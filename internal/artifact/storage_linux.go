//go:build linux

/*
Copyright 2026 Kama Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package artifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type operationMarker struct {
	SchemaVersion     int    `json:"schemaVersion"`
	ArtifactDigest    string `json:"artifactDigest"`
	PublicationDigest string `json:"publicationDigest"`
	ResolvedRevision  string `json:"resolvedRevision,omitempty"`
}

type operationIntent struct {
	SchemaVersion     int      `json:"schemaVersion"`
	ArtifactDigest    string   `json:"artifactDigest"`
	PublicationDigest string   `json:"publicationDigest"`
	ResolvedRevision  string   `json:"resolvedRevision,omitempty"`
	Manifest          Manifest `json:"manifest"`
}

// OperationLock serializes retries that share an operation ID. Its advisory
// lock is released by the kernel if an importer process exits unexpectedly.
type OperationLock struct {
	file *os.File
}

// Close releases an operation lock.
func (lock *OperationLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}

// AcquireOperationLock obtains the stable cache-local lock for an operation.
func AcquireOperationLock(ctx context.Context, cacheRoot, operationID string) (*OperationLock, error) {
	if err := validateOperationID(operationID); err != nil {
		return nil, err
	}
	if err := MkdirAll(cacheRoot, ".kama/locks", 0o750); err != nil {
		return nil, err
	}
	file, err := OpenWritableRegular(cacheRoot, path.Join(".kama/locks", operationID+".lock"), false)
	if err != nil {
		return nil, err
	}
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &OperationLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, failure(ReasonIOFailure, "acquire artifact operation lock", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, failure(ReasonIOFailure, "acquire artifact operation lock", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// EnsureStaging creates and returns the stable cache-relative staging path for
// an operation. Retries reuse partial downloads in this directory.
func EnsureStaging(cacheRoot, operationID string) (string, error) {
	if err := validateOperationID(operationID); err != nil {
		return "", err
	}
	relative := path.Join(".kama/staging", operationID)
	if err := MkdirAll(cacheRoot, relative, 0o750); err != nil {
		return "", err
	}
	return relative, nil
}

// ValidateOperationPrefix validates the artifact-UID prefix used for scoped
// transient cleanup.
func ValidateOperationPrefix(prefix string) error {
	base := strings.TrimSuffix(prefix, "-")
	if !strings.HasSuffix(prefix, "-") || base == "" || len(prefix) > 108 || !operationIDPattern.MatchString(base) {
		return failure(ReasonInvalidSpec, "validate operation prefix", errors.New("operationPrefix must be a bounded operation identifier ending in a hyphen"))
	}
	return nil
}

// RemoveOperationState deletes only transient state whose operation identifier
// starts with prefix. Verified blobs are deliberately outside the enumerated
// paths and cannot be removed by this function.
func RemoveOperationState(cacheRoot, prefix string) error {
	if err := ValidateOperationPrefix(prefix); err != nil {
		return err
	}
	type transientArea struct {
		directory string
		suffix    string
		tree      bool
	}
	const metadataSuffix = ".json"
	for _, area := range []transientArea{
		{directory: ".kama/staging", tree: true},
		{directory: ".kama/intents", suffix: metadataSuffix},
		{directory: ".kama/operations", suffix: metadataSuffix},
		{directory: ".kama/revisions", suffix: metadataSuffix},
		{directory: ".kama/locks", suffix: ".lock"},
	} {
		directory, err := OpenDirectory(cacheRoot, area.directory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		entries, readErr := directory.ReadDir(-1)
		closeErr := directory.Close()
		if readErr != nil || closeErr != nil {
			return failure(ReasonIOFailure, "enumerate transient artifact state", errors.Join(readErr, closeErr))
		}
		for _, entry := range entries {
			operation := entry.Name()
			if area.suffix != "" {
				var recognized bool
				operation, recognized = transientMetadataOperation(operation, area.suffix)
				if !recognized {
					continue
				}
			}
			if !strings.HasPrefix(operation, prefix) || !operationIDPattern.MatchString(operation) {
				continue
			}
			relative := path.Join(area.directory, entry.Name())
			if area.tree {
				if err := RemoveTreeNoFollow(cacheRoot, relative); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				continue
			}
			if err := RemoveNoFollow(cacheRoot, relative); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func transientMetadataOperation(name, suffix string) (string, bool) {
	if operation, found := strings.CutSuffix(name, suffix); found {
		return operation, operationIDPattern.MatchString(operation)
	}
	marker := suffix + ".tmp-"
	if !strings.HasPrefix(name, ".") {
		return "", false
	}
	trimmed := strings.TrimPrefix(name, ".")
	markerIndex := strings.LastIndex(trimmed, marker)
	if markerIndex <= 0 {
		return "", false
	}
	operation := trimmed[:markerIndex]
	random := trimmed[markerIndex+len(marker):]
	if !operationIDPattern.MatchString(operation) || len(random) != 16 {
		return "", false
	}
	if _, err := hex.DecodeString(random); err != nil {
		return "", false
	}
	return operation, true
}

const copyCheckpointBytes = int64(8 << 20)

// regularCopyPlan keeps the source and staging descriptors used during
// preflight open through transfer. Resume credit therefore applies to the
// exact files that are copied after the aggregate free-space check.
type regularCopyPlan struct {
	source          *os.File
	destination     *os.File
	destinationRoot string
	destinationDir  string
	sourceSize      int64
	resumeBytes     int64
	sourceSnapshot  unix.Stat_t
}

// prepareRegularCopy validates an existing staging file byte-for-byte against
// the source before returning resume credit. Invalid or oversize staging is
// durably reset to zero so callers charge the full source size to free space.
func prepareRegularCopy(sourceRoot, sourcePath, destinationRoot, destinationPath string) (*regularCopyPlan, error) {
	source, err := OpenRegular(sourceRoot, sourcePath)
	if err != nil {
		return nil, err
	}
	var sourceSnapshot unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &sourceSnapshot); err != nil {
		_ = source.Close()
		return nil, failure(ReasonIOFailure, "inspect PVC source identity", err)
	}
	if sourceSnapshot.Size < 0 {
		_ = source.Close()
		return nil, failure(ReasonIOFailure, "inspect PVC source", errors.New("source file has a negative size"))
	}
	parent := path.Dir(destinationPath)
	if parent != "." {
		if err := MkdirAll(destinationRoot, parent, 0o750); err != nil {
			_ = source.Close()
			return nil, err
		}
	}
	destination, err := OpenWritableRegular(destinationRoot, destinationPath, false)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	destinationInfo, err := destination.Stat()
	if err != nil {
		_ = destination.Close()
		_ = source.Close()
		return nil, failure(ReasonIOFailure, "inspect PVC copy staging", err)
	}
	if destinationInfo.Size() < 0 {
		_ = destination.Close()
		_ = source.Close()
		return nil, failure(ReasonIOFailure, "inspect PVC copy staging", errors.New("staging file has a negative size"))
	}

	resumeBytes := destinationInfo.Size()
	validPrefix := resumeBytes <= sourceSnapshot.Size
	if validPrefix && resumeBytes > 0 {
		validPrefix, err = regularFilePrefixesEqual(source, destination, resumeBytes)
		if err != nil {
			_ = destination.Close()
			_ = source.Close()
			return nil, err
		}
	}
	if !validPrefix {
		resumeBytes = 0
		truncateErr := destination.Truncate(0)
		_, seekDestinationErr := destination.Seek(0, io.SeekStart)
		_, seekSourceErr := source.Seek(0, io.SeekStart)
		if truncateErr != nil || seekDestinationErr != nil || seekSourceErr != nil {
			_ = destination.Close()
			_ = source.Close()
			return nil, classifyStorageError(
				"reset PVC copy staging",
				errors.Join(truncateErr, seekDestinationErr, seekSourceErr),
			)
		}
	} else {
		_, seekDestinationErr := destination.Seek(resumeBytes, io.SeekStart)
		_, seekSourceErr := source.Seek(resumeBytes, io.SeekStart)
		if seekDestinationErr != nil || seekSourceErr != nil {
			_ = destination.Close()
			_ = source.Close()
			return nil, failure(ReasonIOFailure, "seek PVC copy staging", errors.Join(seekDestinationErr, seekSourceErr))
		}
	}

	plan := &regularCopyPlan{
		source:          source,
		destination:     destination,
		destinationRoot: destinationRoot,
		destinationDir:  parent,
		sourceSize:      sourceSnapshot.Size,
		resumeBytes:     resumeBytes,
		sourceSnapshot:  sourceSnapshot,
	}
	if err := plan.syncCheckpoint(); err != nil {
		_ = plan.close()
		return nil, err
	}
	return plan, nil
}

func regularFilePrefixesEqual(source, destination *os.File, size int64) (bool, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return false, failure(ReasonIOFailure, "seek PVC source prefix", err)
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return false, failure(ReasonIOFailure, "seek PVC staging prefix", err)
	}
	sourceBuffer := make([]byte, 1<<20)
	destinationBuffer := make([]byte, len(sourceBuffer))
	for remaining := size; remaining > 0; {
		chunk := min(int64(len(sourceBuffer)), remaining)
		sourceChunk := sourceBuffer[:chunk]
		destinationChunk := destinationBuffer[:chunk]
		if _, err := io.ReadFull(source, sourceChunk); err != nil {
			return false, failure(ReasonIOFailure, "read PVC source prefix", err)
		}
		if _, err := io.ReadFull(destination, destinationChunk); err != nil {
			return false, failure(ReasonIOFailure, "read PVC staging prefix", err)
		}
		if !bytes.Equal(sourceChunk, destinationChunk) {
			return false, nil
		}
		remaining -= chunk
	}
	return true, nil
}

func (plan *regularCopyPlan) syncCheckpoint() error {
	if err := plan.destination.Sync(); err != nil {
		return classifyStorageError("fsync partial PVC copy", err)
	}
	if err := syncDirectory(plan.destinationRoot, plan.destinationDir); err != nil {
		return err
	}
	return nil
}

func (plan *regularCopyPlan) validateForTransfer() error {
	var sourceStat unix.Stat_t
	sourceStatErr := unix.Fstat(int(plan.source.Fd()), &sourceStat)
	destinationInfo, destinationStatErr := plan.destination.Stat()
	if sourceStatErr != nil || destinationStatErr != nil {
		return failure(ReasonIOFailure, "reinspect PVC copy", errors.Join(sourceStatErr, destinationStatErr))
	}
	if !sameRegularFileSnapshot(plan.sourceSnapshot, sourceStat) {
		return failure(ReasonSourceUnavailable, "reinspect PVC source", errors.New("source changed after copy preflight"))
	}
	if destinationInfo.Size() != plan.resumeBytes {
		return failure(ReasonIOFailure, "reinspect PVC copy staging", errors.New("staging changed after copy preflight"))
	}
	equal, err := regularFilePrefixesEqual(plan.source, plan.destination, plan.resumeBytes)
	if err != nil {
		return err
	}
	if !equal {
		return failure(ReasonIOFailure, "reinspect PVC copy staging", errors.New("staging prefix changed after copy preflight"))
	}
	if _, err := plan.source.Seek(plan.resumeBytes, io.SeekStart); err != nil {
		return failure(ReasonIOFailure, "seek PVC source", err)
	}
	if _, err := plan.destination.Seek(plan.resumeBytes, io.SeekStart); err != nil {
		return failure(ReasonIOFailure, "seek PVC copy staging", err)
	}
	return nil
}

func (plan *regularCopyPlan) copy(ctx context.Context) (int64, error) {
	if err := plan.validateForTransfer(); err != nil {
		return 0, err
	}
	return plan.copyPrepared(ctx)
}

func (plan *regularCopyPlan) copyPrepared(ctx context.Context) (int64, error) {
	remaining := plan.sourceSize - plan.resumeBytes
	written := int64(0)
	checkpointAt := int64(0)
	buffer := make([]byte, 1<<20)
	for remaining > 0 {
		select {
		case <-ctx.Done():
			if err := plan.syncCheckpoint(); err != nil {
				return written, err
			}
			return written, failure(ReasonIOFailure, "interrupt PVC copy", ctx.Err())
		default:
		}

		chunk := min(int64(len(buffer)), remaining)
		read, readErr := plan.source.Read(buffer[:chunk])
		if read > 0 {
			stored, writeErr := plan.destination.Write(buffer[:read])
			written += int64(stored)
			remaining -= int64(stored)
			if writeErr != nil || stored != read {
				checkpointErr := plan.syncCheckpoint()
				return written, classifyStorageError(
					"copy artifact file",
					errors.Join(writeErr, checkpointErr, io.ErrShortWrite),
				)
			}
			if written-checkpointAt >= copyCheckpointBytes {
				if err := plan.syncCheckpoint(); err != nil {
					return written, err
				}
				checkpointAt = written
			}
		}
		if readErr != nil {
			checkpointErr := plan.syncCheckpoint()
			if errors.Is(readErr, io.EOF) && remaining > 0 {
				return written, failure(
					ReasonSourceUnavailable,
					"copy PVC source",
					errors.Join(checkpointErr, errors.New("source became shorter during transfer")),
				)
			}
			if !errors.Is(readErr, io.EOF) {
				return written, failure(ReasonIOFailure, "copy PVC source", errors.Join(readErr, checkpointErr))
			}
		}
		if read == 0 && readErr == nil {
			continue
		}
	}
	if err := plan.syncCheckpoint(); err != nil {
		return written, err
	}
	var sourceStat unix.Stat_t
	if err := unix.Fstat(int(plan.source.Fd()), &sourceStat); err != nil {
		return written, failure(ReasonIOFailure, "reinspect copied PVC source", err)
	}
	if !sameRegularFileSnapshot(plan.sourceSnapshot, sourceStat) {
		resetErr := plan.resetDestination()
		return written, errors.Join(
			failure(ReasonSourceUnavailable, "copy PVC source", errors.New("source changed during transfer")),
			resetErr,
		)
	}
	return written, nil
}

func sameRegularFileSnapshot(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Size == right.Size &&
		left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func (plan *regularCopyPlan) resetDestination() error {
	truncateErr := plan.destination.Truncate(0)
	_, destinationSeekErr := plan.destination.Seek(0, io.SeekStart)
	_, sourceSeekErr := plan.source.Seek(0, io.SeekStart)
	if truncateErr != nil || destinationSeekErr != nil || sourceSeekErr != nil {
		return classifyStorageError(
			"reset changed PVC copy staging",
			errors.Join(truncateErr, destinationSeekErr, sourceSeekErr),
		)
	}
	plan.resumeBytes = 0
	return plan.syncCheckpoint()
}

func (plan *regularCopyPlan) close() error {
	if plan == nil {
		return nil
	}
	var destinationErr, sourceErr error
	if plan.destination != nil {
		destinationErr = plan.destination.Close()
		plan.destination = nil
	}
	if plan.source != nil {
		sourceErr = plan.source.Close()
		plan.source = nil
	}
	if destinationErr != nil || sourceErr != nil {
		return classifyStorageError("close PVC copy files", errors.Join(destinationErr, sourceErr))
	}
	return nil
}

// CopyRegularFile securely resumes one file between mounted roots and durably
// fsyncs the destination before returning. Callers that aggregate free-space
// requirements should retain the prepared plan through their capacity check.
func CopyRegularFile(sourceRoot, sourcePath, destinationRoot, destinationPath string) (int64, error) {
	plan, err := prepareRegularCopy(sourceRoot, sourcePath, destinationRoot, destinationPath)
	if err != nil {
		return 0, err
	}
	written, copyErr := plan.copy(context.Background())
	closeErr := plan.close()
	if copyErr != nil {
		return written, copyErr
	}
	if closeErr != nil {
		return written, closeErr
	}
	return written, nil
}

// FilesystemSpace reports filesystem capacity and current unprivileged free
// space, not merely provisioned PVC capacity.
func FilesystemSpace(root string) (capacity, free uint64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return 0, 0, failure(ReasonIOFailure, "inspect filesystem capacity", err)
	}
	return stat.Blocks * uint64(stat.Bsize), stat.Bavail * uint64(stat.Bsize), nil
}

// RequireFreeSpace fails before transfer when the known source size exceeds
// currently available space.
func RequireFreeSpace(root string, required uint64) error {
	_, free, err := FilesystemSpace(root)
	if err != nil {
		return err
	}
	if required > free {
		return failure(ReasonInsufficientStorage, "preflight artifact storage", fmt.Errorf("%d bytes required, %d bytes available", required, free))
	}
	return nil
}

// RequirePublicationSpace reserves payload bytes plus bounded room for resume
// checkpoints, the canonical manifest, READY, the operation marker, and
// filesystem directory/block allocation. Publication must not consume the last
// available block and then fail after an otherwise successful transfer.
func RequirePublicationSpace(root string, payloadBytes uint64, fileCount int) error {
	if fileCount < 1 || fileCount > MaxSelectedFiles {
		return failure(ReasonInvalidSpec, "preflight artifact storage", errors.New("publication file count is outside supported bounds"))
	}
	const fixedReserve = uint64(1 << 20)
	const perFileReserve = uint64(64 << 10)
	overhead := fixedReserve + uint64(fileCount)*perFileReserve
	if payloadBytes > ^uint64(0)-overhead {
		return failure(ReasonInsufficientStorage, "preflight artifact storage", errors.New("publication size plus metadata reserve overflows"))
	}
	return RequireFreeSpace(root, payloadBytes+overhead)
}

// ProbeFilesystem verifies the minimum filesystem behavior required for a
// writable cache: durable write/fsync, same-directory atomic rename, mmap, and
// free-space reporting.
//
//nolint:gocyclo // Each filesystem primitive needs distinct cleanup and a bounded failure reason.
func ProbeFilesystem(root string) (result ProbeResult, returnedErr error) {
	capacity, free, err := FilesystemSpace(root)
	if err != nil {
		return ProbeResult{}, err
	}
	result.CapacityBytes = capacity
	result.FreeBytes = free
	if err := MkdirAll(root, ".kama/probes", 0o750); err != nil {
		return result, err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return result, failure(ReasonIOFailure, "create probe name", err)
	}
	first := path.Join(".kama/probes", "probe-"+suffix+".tmp")
	second := path.Join(".kama/probes", "probe-"+suffix+".dat")
	defer func() {
		removeErr := errors.Join(RemoveNoFollow(root, first), RemoveNoFollow(root, second))
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && returnedErr == nil {
			returnedErr = failure(ReasonIOFailure, "remove filesystem probe", removeErr)
		}
	}()

	file, err := CreateWritableRegular(root, first)
	if err != nil {
		return result, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return result, failure(ReasonIOFailure, "lock filesystem probe", err)
	}
	contender, err := OpenWritableRegular(root, first, false)
	if err != nil {
		_ = file.Close()
		return result, err
	}
	contenderErr := unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	_ = contender.Close()
	if !errors.Is(contenderErr, unix.EWOULDBLOCK) && !errors.Is(contenderErr, unix.EAGAIN) {
		_ = file.Close()
		return result, failure(ReasonIOFailure, "validate filesystem lock", errors.New("independent lock acquisition was not excluded"))
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		_ = file.Close()
		return result, failure(ReasonIOFailure, "unlock filesystem probe", err)
	}
	result.Lock = true
	payload := make([]byte, os.Getpagesize())
	for index := range payload {
		payload[index] = byte(index % 251)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return result, classifyStorageError("write filesystem probe", err)
	}
	result.Write = true
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return result, classifyStorageError("fsync filesystem probe", err)
	}
	result.Fsync = true
	if err := file.Close(); err != nil {
		return result, classifyStorageError("close filesystem probe", err)
	}
	if err := RenameNoFollow(root, first, second); err != nil {
		return result, err
	}
	result.AtomicRename = true
	if err := syncDirectory(root, ".kama/probes"); err != nil {
		return result, err
	}
	readable, err := OpenRegular(root, second)
	if err != nil {
		return result, err
	}
	mapped, err := unix.Mmap(int(readable.Fd()), 0, len(payload), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = readable.Close()
		return result, failure(ReasonIOFailure, "mmap filesystem probe", err)
	}
	if len(mapped) != len(payload) || mapped[0] != payload[0] || mapped[len(mapped)-1] != payload[len(payload)-1] {
		_ = unix.Munmap(mapped)
		_ = readable.Close()
		return result, failure(ReasonIOFailure, "validate filesystem mmap", errors.New("mapped data differs from durable write"))
	}
	result.Mmap = true
	if err := unix.Munmap(mapped); err != nil {
		_ = readable.Close()
		return result, failure(ReasonIOFailure, "unmap filesystem probe", err)
	}
	if err := readable.Close(); err != nil {
		return result, failure(ReasonIOFailure, "close filesystem probe", err)
	}
	if err := MkdirAll(root, ".kama/staging", 0o750); err != nil {
		return result, err
	}
	if err := MkdirAll(root, "blobs/sha256", 0o750); err != nil {
		return result, err
	}
	directorySource := path.Join(".kama/staging", "probe-directory-"+suffix)
	directoryTarget := path.Join("blobs/sha256", ".probe-directory-"+suffix)
	defer func() {
		removeErr := errors.Join(RemoveTreeNoFollow(root, directorySource), RemoveTreeNoFollow(root, directoryTarget))
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && returnedErr == nil {
			returnedErr = failure(ReasonIOFailure, "remove directory-rename probe", removeErr)
		}
	}()
	if err := MkdirAll(root, directorySource, 0o750); err != nil {
		return result, err
	}
	if err := RenameNoFollow(root, directorySource, directoryTarget); err != nil {
		return result, err
	}
	if err := syncDirectory(root, ".kama/staging"); err != nil {
		return result, err
	}
	if err := syncDirectory(root, "blobs/sha256"); err != nil {
		return result, err
	}
	result.DirectoryRename = true
	return result, nil
}

// RecoverPublished validates an operation marker and every byte of its ready
// artifact. If a process stopped between completed validation and the final
// marker, its durable intent is used to finish staging/publication without
// contacting the source again.
func RecoverPublished(cacheRoot, operationID string) (manifest Manifest, metadata GGUFMetadata, digest, publishedPath, resolvedRevision string, found bool, err error) {
	if err := validateOperationID(operationID); err != nil {
		return Manifest{}, GGUFMetadata{}, "", "", "", false, err
	}
	var marker operationMarker
	markerFound, err := readCanonicalMetadata(cacheRoot, path.Join(".kama/operations", operationID+".json"), 1024, &marker)
	if err != nil {
		return Manifest{}, GGUFMetadata{}, "", "", "", false, err
	}
	if markerFound {
		if err := validateOperationIdentity(marker.SchemaVersion, marker.ArtifactDigest, marker.PublicationDigest, marker.ResolvedRevision); err != nil {
			return Manifest{}, GGUFMetadata{}, "", "", "", false, err
		}
		manifest, metadata, err = ValidatePublished(cacheRoot, marker.PublicationDigest)
		if err != nil {
			return Manifest{}, GGUFMetadata{}, "", "", "", false, err
		}
		actualArtifactDigest, digestErr := ArtifactDigest(manifest)
		if digestErr != nil || actualArtifactDigest != marker.ArtifactDigest {
			return Manifest{}, GGUFMetadata{}, "", "", "", false, failure(ReasonPublicationConflict, "validate operation marker", errors.Join(digestErr, errors.New("artifact digest does not match operation marker")))
		}
		return manifest, metadata, marker.ArtifactDigest, publishedRelative(marker.PublicationDigest), marker.ResolvedRevision, true, nil
	}

	var intent operationIntent
	intentFound, err := readCanonicalMetadata(cacheRoot, path.Join(".kama/intents", operationID+".json"), MaxResultBytes, &intent)
	if err != nil || !intentFound {
		return Manifest{}, GGUFMetadata{}, "", "", "", false, err
	}
	if err := validateOperationIdentity(intent.SchemaVersion, intent.ArtifactDigest, intent.PublicationDigest, intent.ResolvedRevision); err != nil {
		return Manifest{}, GGUFMetadata{}, "", "", "", false, err
	}
	if err := ValidateManifest(intent.Manifest); err != nil {
		return Manifest{}, GGUFMetadata{}, "", "", "", false, failure(ReasonPublicationConflict, "validate operation intent", err)
	}
	artifactDigest, digestErr := ArtifactDigest(intent.Manifest)
	publicationDigest, publicationErr := ManifestDigest(intent.Manifest)
	if digestErr != nil || publicationErr != nil || artifactDigest != intent.ArtifactDigest || publicationDigest != intent.PublicationDigest {
		return Manifest{}, GGUFMetadata{}, "", "", "", false, failure(ReasonPublicationConflict, "validate operation intent", errors.Join(digestErr, publicationErr, errors.New("intent digests do not match its manifest")))
	}

	// A rename may have committed while READY or the operation marker did not.
	manifest, metadata, finalErr := validatePublishedContent(cacheRoot, intent.PublicationDigest)
	if finalErr == nil {
		if !manifestsEqual(manifest, intent.Manifest) {
			return Manifest{}, GGUFMetadata{}, "", "", "", false, failure(ReasonPublicationConflict, "recover operation intent", errors.New("published manifest differs from operation intent"))
		}
		if err := finalizePublication(cacheRoot, operationID, intent.ArtifactDigest, intent.PublicationDigest, intent.ResolvedRevision); err != nil {
			return Manifest{}, GGUFMetadata{}, "", "", "", false, err
		}
		return manifest, metadata, intent.ArtifactDigest, publishedRelative(intent.PublicationDigest), intent.ResolvedRevision, true, nil
	}
	finalDirectory, finalStatErr := OpenDirectory(cacheRoot, publishedRelative(intent.PublicationDigest))
	if finalStatErr == nil {
		_ = finalDirectory.Close()
		return Manifest{}, GGUFMetadata{}, "", "", "", false, failure(ReasonPublicationConflict, "recover operation intent", finalErr)
	}
	if !errors.Is(finalStatErr, os.ErrNotExist) {
		return Manifest{}, GGUFMetadata{}, "", "", "", false, failure(ReasonPublicationConflict, "recover operation intent", finalStatErr)
	}

	staging := path.Join(".kama/staging", operationID)
	metadata, err = validateManifestContent(cacheRoot, staging, intent.Manifest)
	if err != nil {
		return Manifest{}, GGUFMetadata{}, "", "", "", false, failure(ReasonPublicationConflict, "recover staged operation intent", err)
	}
	recoveredDigest, recoveredPath, _, err := Publish(cacheRoot, staging, operationID, intent.ResolvedRevision, intent.Manifest)
	if err != nil {
		return Manifest{}, GGUFMetadata{}, "", "", "", false, err
	}
	return intent.Manifest, metadata, recoveredDigest, recoveredPath, intent.ResolvedRevision, true, nil
}

// ValidatePublished verifies READY, the canonical publication-manifest digest,
// every file digest, and GGUF/shard structure.
func ValidatePublished(cacheRoot, publicationDigest string) (Manifest, GGUFMetadata, error) {
	manifest, metadata, err := validatePublishedContent(cacheRoot, publicationDigest)
	if err != nil {
		return Manifest{}, GGUFMetadata{}, err
	}
	ready, err := OpenRegular(cacheRoot, path.Join(publishedRelative(publicationDigest), readyFilename))
	if err != nil {
		return Manifest{}, GGUFMetadata{}, failure(ReasonPublicationConflict, "open READY marker", err)
	}
	expected := []byte(publicationDigest + "\n")
	payload, readErr := io.ReadAll(io.LimitReader(ready, int64(len(expected)+1)))
	closeErr := ready.Close()
	if readErr != nil || closeErr != nil {
		return Manifest{}, GGUFMetadata{}, failure(ReasonPublicationConflict, "read READY marker", errors.Join(readErr, closeErr))
	}
	if !bytes.Equal(payload, expected) {
		return Manifest{}, GGUFMetadata{}, failure(ReasonPublicationConflict, "validate READY marker", errors.New("READY content does not exactly match publication directory"))
	}
	return manifest, metadata, nil
}

// Publish atomically moves a fully prepared staging directory into the
// content-addressed blob tree, writes READY last, and records operation
// recovery metadata. stagingRelative must be below .kama/staging.
func Publish(cacheRoot, stagingRelative, operationID, resolvedRevision string, manifest Manifest) (artifactDigest, publishedPath string, cacheHit bool, err error) {
	if err := validateOperationID(operationID); err != nil {
		return "", "", false, err
	}
	if !strings.HasPrefix(stagingRelative, ".kama/staging/") {
		return "", "", false, failure(ReasonUnsafePath, "validate staging path", errors.New("staging directory is outside .kama/staging"))
	}
	artifactDigest, err = ArtifactDigest(manifest)
	if err != nil {
		return "", "", false, err
	}
	publicationDigest, err := ManifestDigest(manifest)
	if err != nil {
		return "", "", false, err
	}
	if resolvedRevision != "" && !ValidHubCommit(resolvedRevision) {
		return "", "", false, failure(ReasonInvalidSpec, "validate publication revision", errors.New("resolved revision is not a full immutable Hub commit"))
	}
	intent, _ := json.Marshal(operationIntent{
		SchemaVersion:     SchemaVersion,
		ArtifactDigest:    artifactDigest,
		PublicationDigest: publicationDigest,
		ResolvedRevision:  resolvedRevision,
		Manifest:          manifest,
	})
	if err := MkdirAll(cacheRoot, ".kama/intents", 0o750); err != nil {
		return "", "", false, err
	}
	if err := writeAtomic(cacheRoot, path.Join(".kama/intents", operationID+".json"), intent, 0o640); err != nil {
		return "", "", false, err
	}
	// Completed Hub checkpoints remain durable until the intent above commits.
	// Remove them before staging becomes immutable publication content.
	for _, record := range manifest.Files {
		resumePath := path.Join(stagingRelative, record.Path) + ".resume.json"
		if err := RemoveNoFollow(cacheRoot, resumePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", false, err
		}
	}
	manifestPayload, _ := CanonicalManifest(manifest)
	if err := writeAtomic(cacheRoot, path.Join(stagingRelative, manifestFilename), manifestPayload, 0o640); err != nil {
		return "", "", false, err
	}
	if err := syncArtifactTree(cacheRoot, stagingRelative, manifest); err != nil {
		return "", "", false, err
	}
	if err := MkdirAll(cacheRoot, "blobs/sha256", 0o750); err != nil {
		return "", "", false, err
	}
	publishedPath = publishedRelative(publicationDigest)
	existingManifest, _, existingErr := validatePublishedContent(cacheRoot, publicationDigest)
	if existingErr == nil {
		existingPayload, existingMarshalErr := CanonicalManifest(existingManifest)
		if existingMarshalErr != nil || !bytes.Equal(existingPayload, manifestPayload) {
			return "", "", false, failure(
				ReasonPublicationConflict,
				"publish artifact",
				errors.New("artifact digest already exists with a different manifest"),
			)
		}
		cacheHit = true
	} else {
		// An existing directory without READY can be finalized only if all
		// content is already valid. Any other content is never overwritten.
		existingDirectory, statErr := OpenDirectory(cacheRoot, publishedPath)
		if statErr == nil {
			_ = existingDirectory.Close()
			return "", "", false, failure(ReasonPublicationConflict, "publish artifact", existingErr)
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", "", false, failure(ReasonPublicationConflict, "inspect artifact destination", statErr)
		}
		if renameErr := RenameNoFollow(cacheRoot, stagingRelative, publishedPath); renameErr != nil {
			if FailureReason(renameErr) == ReasonInsufficientStorage {
				return "", "", false, renameErr
			}
			retryManifest, _, retryErr := validatePublishedContent(cacheRoot, publicationDigest)
			if retryErr != nil {
				return "", "", false, failure(ReasonPublicationConflict, "atomically publish artifact", errors.Join(renameErr, retryErr))
			}
			retryPayload, retryMarshalErr := CanonicalManifest(retryManifest)
			if retryMarshalErr != nil || !bytes.Equal(retryPayload, manifestPayload) {
				return "", "", false, failure(
					ReasonPublicationConflict,
					"atomically publish artifact",
					errors.New("concurrent publisher used the same digest for a different manifest"),
				)
			}
			cacheHit = true
		} else if syncErr := syncDirectory(cacheRoot, path.Dir(stagingRelative)); syncErr != nil {
			return "", "", false, syncErr
		}
	}
	if err := finalizePublication(cacheRoot, operationID, artifactDigest, publicationDigest, resolvedRevision); err != nil {
		return "", "", false, err
	}
	if cacheHit {
		if err := RemoveTreeNoFollow(cacheRoot, stagingRelative); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", false, err
		}
	}
	return artifactDigest, publishedPath, cacheHit, nil
}

func validatePublishedContent(cacheRoot, publicationDigest string) (Manifest, GGUFMetadata, error) {
	if !validSHA256(publicationDigest) {
		return Manifest{}, GGUFMetadata{}, failure(ReasonInvalidSpec, "validate publication digest", errors.New("digest is not lowercase SHA-256"))
	}
	prefix := publishedRelative(publicationDigest)
	manifest, err := readManifest(cacheRoot, path.Join(prefix, manifestFilename))
	if err != nil {
		return Manifest{}, GGUFMetadata{}, err
	}
	actualDigest, err := ManifestDigest(manifest)
	if err != nil {
		return Manifest{}, GGUFMetadata{}, err
	}
	if actualDigest != publicationDigest {
		return Manifest{}, GGUFMetadata{}, failure(ReasonPublicationConflict, "validate published manifest", errors.New("canonical manifest digest does not match publication directory"))
	}
	metadata, err := validateManifestContent(cacheRoot, prefix, manifest)
	if err != nil {
		return Manifest{}, GGUFMetadata{}, err
	}
	return manifest, metadata, nil
}

func validateManifestContent(cacheRoot, prefix string, manifest Manifest) (GGUFMetadata, error) {
	if err := ValidateManifest(manifest); err != nil {
		return GGUFMetadata{}, err
	}
	for _, expected := range manifest.Files {
		file, err := OpenRegular(cacheRoot, path.Join(prefix, expected.Path))
		if err != nil {
			return GGUFMetadata{}, err
		}
		actual, hashErr := HashFile(expected.Path, file)
		closeErr := file.Close()
		if hashErr != nil || closeErr != nil {
			return GGUFMetadata{}, errors.Join(hashErr, closeErr)
		}
		if actual.Size != expected.Size || actual.SHA256 != expected.SHA256 {
			return GGUFMetadata{}, failure(ReasonPublicationConflict, "validate published file", fmt.Errorf("%q differs from manifest", expected.Path))
		}
	}
	metadata, err := ValidateGGUFSet(cacheRoot, prefix, manifest)
	if err != nil {
		return GGUFMetadata{}, err
	}
	return metadata, nil
}

func finalizePublication(cacheRoot, operationID, artifactDigest, publicationDigest, resolvedRevision string) error {
	if err := syncDirectory(cacheRoot, "blobs/sha256"); err != nil {
		return err
	}
	publishedPath := publishedRelative(publicationDigest)
	if err := writeAtomic(cacheRoot, path.Join(publishedPath, readyFilename), []byte(publicationDigest+"\n"), 0o440); err != nil {
		return err
	}
	if err := syncDirectory(cacheRoot, publishedPath); err != nil {
		return err
	}
	marker, _ := json.Marshal(operationMarker{
		SchemaVersion:     SchemaVersion,
		ArtifactDigest:    artifactDigest,
		PublicationDigest: publicationDigest,
		ResolvedRevision:  resolvedRevision,
	})
	if err := MkdirAll(cacheRoot, ".kama/operations", 0o750); err != nil {
		return err
	}
	return writeAtomic(cacheRoot, path.Join(".kama/operations", operationID+".json"), marker, 0o640)
}

func validateOperationIdentity(schemaVersion int, artifactDigest, publicationDigest, resolvedRevision string) error {
	if schemaVersion != SchemaVersion || !validSHA256(artifactDigest) || !validSHA256(publicationDigest) ||
		(resolvedRevision != "" && !ValidHubCommit(resolvedRevision)) {
		return failure(ReasonPublicationConflict, "validate operation metadata", errors.New("operation identity is invalid"))
	}
	return nil
}

func readCanonicalMetadata(root, relativePath string, limit int64, destination any) (bool, error) {
	file, err := OpenRegular(root, relativePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return false, failure(ReasonIOFailure, "read operation metadata", errors.Join(readErr, closeErr))
	}
	if int64(len(payload)) > limit {
		return false, failure(ReasonPublicationConflict, "validate operation metadata", errors.New("operation metadata exceeds size limit"))
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return false, failure(ReasonPublicationConflict, "decode operation metadata", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("operation metadata contains multiple JSON values")
		}
		return false, failure(ReasonPublicationConflict, "validate operation metadata", err)
	}
	canonical, err := json.Marshal(destination)
	if err != nil || !bytes.Equal(payload, canonical) {
		return false, failure(ReasonPublicationConflict, "validate operation metadata", errors.Join(err, errors.New("operation metadata is not canonical")))
	}
	return true, nil
}

func manifestsEqual(left, right Manifest) bool {
	leftPayload, leftErr := CanonicalManifest(left)
	rightPayload, rightErr := CanonicalManifest(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

func syncArtifactTree(root, prefix string, manifest Manifest) error {
	directories := map[string]struct{}{prefix: {}}
	for _, record := range manifest.Files {
		file, err := OpenRegular(root, path.Join(prefix, record.Path))
		if err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return classifyStorageError("fsync artifact file", err)
		}
		if err := file.Close(); err != nil {
			return classifyStorageError("close artifact file", err)
		}
		directory := path.Dir(path.Join(prefix, record.Path))
		for strings.HasPrefix(directory, prefix) {
			directories[directory] = struct{}{}
			if directory == prefix {
				break
			}
			directory = path.Dir(directory)
		}
	}
	for directory := range directories {
		if err := syncDirectory(root, directory); err != nil {
			return err
		}
	}
	return nil
}

func writeAtomic(root, relativePath string, payload []byte, mode os.FileMode) error {
	parent := path.Dir(relativePath)
	if parent != "." {
		if err := MkdirAll(root, parent, 0o750); err != nil {
			return err
		}
	}
	suffix, err := randomSuffix()
	if err != nil {
		return failure(ReasonIOFailure, "create temporary file name", err)
	}
	temporary := path.Join(parent, "."+path.Base(relativePath)+".tmp-"+suffix)
	file, err := CreateWritableRegular(root, temporary)
	if err != nil {
		return err
	}
	defer func() { _ = RemoveNoFollow(root, temporary) }()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return failure(ReasonIOFailure, "set artifact metadata permissions", err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return classifyStorageError("write artifact metadata", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return classifyStorageError("fsync artifact metadata", err)
	}
	if err := file.Close(); err != nil {
		return classifyStorageError("close artifact metadata", err)
	}
	if err := RenameNoFollow(root, temporary, relativePath); err != nil {
		return err
	}
	return syncDirectory(root, parent)
}

func syncDirectory(root, relativePath string) error {
	if relativePath == "." {
		relativePath = ""
	}
	directory, err := OpenDirectory(root, relativePath)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return classifyStorageError("fsync artifact directory", err)
	}
	return nil
}

func validateOperationID(operationID string) error {
	if !operationIDPattern.MatchString(operationID) {
		return failure(ReasonInvalidSpec, "validate operation ID", errors.New("operationID must be 1-128 letters, digits, dots, underscores, or hyphens and start with a letter or digit"))
	}
	return nil
}

func publishedRelative(digest string) string {
	return path.Join("blobs/sha256", digest)
}

func classifyStorageError(operation string, err error) error {
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
		return failure(ReasonInsufficientStorage, operation, err)
	}
	return failure(ReasonIOFailure, operation, err)
}

func randomSuffix() (string, error) {
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(randomBytes[:]), nil
}
