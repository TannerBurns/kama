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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"
	"time"
)

const MaxSpecBytes = 128 << 10

// DecodeSpec parses one bounded, strict importer spec.
func DecodeSpec(reader io.Reader) (Spec, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, MaxSpecBytes+1))
	decoder.DisallowUnknownFields()
	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, failure(ReasonInvalidSpec, "decode importer spec", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("spec contains multiple JSON values")
		}
		return Spec{}, failure(ReasonInvalidSpec, "decode importer spec", err)
	}
	if err := ValidateSpec(spec); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// ValidateSpec enforces the mode union before any storage or network access.
//
//nolint:gocyclo // The mode union is clearer as one exhaustive validation function.
func ValidateSpec(spec Spec) error {
	if spec.SchemaVersion != SchemaVersion {
		return failure(ReasonInvalidSpec, "validate importer spec", fmt.Errorf("unsupported schemaVersion %d", spec.SchemaVersion))
	}
	if spec.ExpectedSize != nil && *spec.ExpectedSize <= 0 {
		return failure(ReasonInvalidSpec, "validate expected size", errors.New("expectedSize must be greater than zero"))
	}
	if spec.ExpectedSHA256 != "" && !validSHA256(spec.ExpectedSHA256) {
		return failure(ReasonInvalidSpec, "validate expected digest", errors.New("expectedSHA256 must be 64 lowercase hexadecimal characters"))
	}
	switch spec.Mode {
	case ModeHub:
		if spec.Hub == nil || spec.PVC != nil || spec.Probe != nil || spec.Cleanup != nil {
			return failure(ReasonInvalidSpec, "validate source union", errors.New("hub mode requires only hub configuration"))
		}
		if spec.CacheRoot == "" || spec.OperationID == "" {
			return failure(ReasonInvalidSpec, "validate Hub import", errors.New("cacheRoot and operationID are required"))
		}
		if spec.Hub.Repository == "" || spec.Hub.Revision == "" || len(spec.Hub.FileSelectors) == 0 {
			return failure(ReasonInvalidSpec, "validate Hub import", errors.New("repository, revision, and fileSelectors are required"))
		}
	case ModeCopy:
		if spec.PVC == nil || spec.Hub != nil || spec.Probe != nil || spec.Cleanup != nil {
			return failure(ReasonInvalidSpec, "validate source union", errors.New("copy mode requires only PVC configuration"))
		}
		if spec.CacheRoot == "" || spec.OperationID == "" || spec.PVC.MountRoot == "" {
			return failure(ReasonInvalidSpec, "validate PVC copy", errors.New("cacheRoot, operationID, and PVC mountRoot are required"))
		}
	case ModeDirect:
		if spec.PVC == nil || spec.Hub != nil || spec.Probe != nil || spec.Cleanup != nil {
			return failure(ReasonInvalidSpec, "validate source union", errors.New("direct mode requires only PVC configuration"))
		}
		if spec.CacheRoot != "" || spec.PVC.MountRoot == "" {
			return failure(ReasonInvalidSpec, "validate direct PVC", errors.New("PVC mountRoot is required and cacheRoot is forbidden"))
		}
	case ModeProbe:
		if spec.Probe == nil || spec.Hub != nil || spec.PVC != nil || spec.Cleanup != nil {
			return failure(ReasonInvalidSpec, "validate source union", errors.New("probe mode requires only probe configuration"))
		}
		if spec.Probe.Root == "" || spec.CacheRoot != "" {
			return failure(ReasonInvalidSpec, "validate filesystem probe", errors.New("probe root is required and cacheRoot is forbidden"))
		}
		if spec.Format != "" || spec.Entrypoint != "" || spec.ExpectedSHA256 != "" || spec.ExpectedSize != nil {
			return failure(ReasonInvalidSpec, "validate filesystem probe", errors.New("artifact fields are forbidden in probe mode"))
		}
		return nil
	case ModeCleanup:
		if spec.Cleanup == nil || spec.Hub != nil || spec.PVC != nil || spec.Probe != nil {
			return failure(ReasonInvalidSpec, "validate source union", errors.New("cleanup mode requires only cleanup configuration"))
		}
		if spec.CacheRoot == "" || spec.Cleanup.OperationPrefix == "" {
			return failure(ReasonInvalidSpec, "validate artifact cleanup", errors.New("cacheRoot and operationPrefix are required"))
		}
		if spec.Format != "" || spec.Entrypoint != "" || spec.ExpectedSHA256 != "" || spec.ExpectedSize != nil {
			return failure(ReasonInvalidSpec, "validate artifact cleanup", errors.New("artifact fields are forbidden in cleanup mode"))
		}
		return ValidateOperationPrefix(spec.Cleanup.OperationPrefix)
	default:
		return failure(ReasonInvalidSpec, "validate importer mode", fmt.Errorf("unsupported mode %q", spec.Mode))
	}
	if !strings.EqualFold(spec.Format, FormatGGUF) {
		return failure(ReasonInvalidSpec, "validate artifact format", errors.New("format must be GGUF"))
	}
	if err := ValidateRelativePath(spec.Entrypoint); err != nil {
		return failure(ReasonUnsafePath, "validate artifact entrypoint", err)
	}
	if spec.OperationID != "" {
		if err := validateOperationID(spec.OperationID); err != nil {
			return err
		}
	}
	if spec.PVC != nil {
		if spec.PVC.RootPath != "" && spec.PVC.RootPath != "." {
			if err := ValidateRelativePath(spec.PVC.RootPath); err != nil {
				return failure(ReasonUnsafePath, "validate PVC rootPath", err)
			}
		}
		if len(spec.PVC.SelectedFiles) > MaxSelectedFiles {
			return failure(ReasonInvalidSpec, "validate PVC selected files", fmt.Errorf("selected %d files, maximum is %d", len(spec.PVC.SelectedFiles), MaxSelectedFiles))
		}
	}
	return nil
}

// Execute runs one validated importer operation and always returns a structured
// Result. The caller decides how to emit it and translate Success to exit code.
func Execute(ctx context.Context, spec Spec) Result {
	started := time.Now()
	if err := ValidateSpec(spec); err != nil {
		result := NewFailureResult(spec.Mode, err)
		result.DurationMillis = time.Since(started).Milliseconds()
		return result
	}
	var result Result
	var err error
	switch spec.Mode {
	case ModeHub:
		result, err = executeHub(ctx, spec)
	case ModeCopy:
		result, err = executeCopy(ctx, spec)
	case ModeDirect:
		result, err = executeDirect(spec)
	case ModeProbe:
		result, err = executeProbe(spec)
	case ModeCleanup:
		result, err = executeCleanup(spec)
	}
	if err != nil {
		validationMillis := result.ValidationMillis
		result = NewFailureResult(spec.Mode, err)
		result.ValidationMillis = validationMillis
	}
	result.SchemaVersion = SchemaVersion
	if ValidMode(spec.Mode) && spec.Mode != ModeUnknown {
		result.Mode = spec.Mode
	} else {
		result.Mode = ModeUnknown
	}
	result.OperationID = spec.OperationID
	result.DurationMillis = time.Since(started).Milliseconds()
	return result
}

func executeHub(ctx context.Context, spec Spec) (Result, error) {
	lock, err := AcquireOperationLock(ctx, spec.CacheRoot, spec.OperationID)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = lock.Close() }()
	if recovered, found, err := recoverResult(spec); err != nil {
		return recovered, err
	} else if found {
		return recovered, nil
	}
	token, err := ReadTokenFile(spec.Hub.TokenFile)
	if err != nil {
		return Result{}, err
	}
	client, err := NewHubClient(spec.HubEndpoint, token, spec.HTTP)
	if err != nil {
		return Result{}, err
	}
	resolvedRevision, pinned, err := readHubRevisionPin(
		spec.CacheRoot,
		spec.OperationID,
		client.endpoint.String(),
		*spec.Hub,
		spec.Entrypoint,
	)
	if err != nil {
		return Result{}, err
	}
	revision := spec.Hub.Revision
	if pinned {
		revision = resolvedRevision
	}
	resolution, err := client.Resolve(ctx, spec.Hub.Repository, revision, spec.Entrypoint, spec.Hub.FileSelectors)
	if err != nil {
		return Result{}, err
	}
	if !pinned {
		if err := writeHubRevisionPin(
			spec.CacheRoot,
			spec.OperationID,
			client.endpoint.String(),
			*spec.Hub,
			spec.Entrypoint,
			resolution.Commit,
		); err != nil {
			return Result{}, err
		}
	}
	staging, err := EnsureStaging(spec.CacheRoot, spec.OperationID)
	if err != nil {
		return Result{}, err
	}
	var required uint64
	for index := range resolution.Files {
		remote, err := client.Preflight(
			ctx,
			spec.Hub.Repository,
			resolution.Commit,
			resolution.Files[index],
			spec.CacheRoot,
			path.Join(staging, resolution.Files[index].Path),
		)
		if err != nil {
			return Result{}, err
		}
		resolution.Files[index] = remote
		remaining := *remote.Size - remote.ResumeBytes
		if remaining < 0 || uint64(remaining) > math.MaxUint64-required {
			return Result{}, failure(ReasonSourceUnavailable, "preflight Hub import", errors.New("selected file sizes overflow"))
		}
		required += uint64(remaining)
	}
	if err := RequirePublicationSpace(spec.CacheRoot, required, len(resolution.Files)); err != nil {
		return Result{}, err
	}
	var transferred int64
	selected := make([]string, 0, len(resolution.Files))
	for _, remote := range resolution.Files {
		written, err := client.Download(ctx, spec.Hub.Repository, resolution.Commit, remote, spec.CacheRoot, path.Join(staging, remote.Path))
		if err != nil {
			return Result{}, err
		}
		if written > math.MaxInt64-transferred {
			return Result{}, failure(ReasonIOFailure, "count Hub transfer", errors.New("transferred byte count overflow"))
		}
		transferred += written
		selected = append(selected, remote.Path)
	}
	validationStarted := time.Now()
	manifest, metadata, err := validateStaged(spec, spec.CacheRoot, staging, selected)
	validationMillis := time.Since(validationStarted).Milliseconds()
	if err != nil {
		return Result{ValidationMillis: validationMillis}, err
	}
	digest, publishedPath, cacheHit, err := Publish(spec.CacheRoot, staging, spec.OperationID, resolution.Commit, manifest)
	if err != nil {
		return Result{ValidationMillis: validationMillis}, err
	}
	return successfulResult(
		spec.Mode, manifest, metadata, digest, publishedPath, resolution.Commit,
		transferred, validationMillis, cacheHit,
	), nil
}

func executeCopy(ctx context.Context, spec Spec) (Result, error) {
	lock, err := AcquireOperationLock(ctx, spec.CacheRoot, spec.OperationID)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = lock.Close() }()
	if recovered, found, err := recoverResult(spec); err != nil {
		return recovered, err
	} else if found {
		return recovered, nil
	}
	selected, err := ExpandStandardShards(spec.Entrypoint, spec.PVC.SelectedFiles)
	if err != nil {
		return Result{}, err
	}
	staging, err := EnsureStaging(spec.CacheRoot, spec.OperationID)
	if err != nil {
		return Result{}, err
	}
	plans := make([]*regularCopyPlan, 0, len(selected))
	defer func() {
		for _, plan := range plans {
			_ = plan.close()
		}
	}()
	var required uint64
	for _, name := range selected {
		plan, err := prepareRegularCopy(
			spec.PVC.MountRoot,
			withPrefix(spec.PVC.RootPath, name),
			spec.CacheRoot,
			path.Join(staging, name),
		)
		if err != nil {
			if isStandardShard(spec.Entrypoint) && errors.Is(err, os.ErrNotExist) {
				return Result{}, failure(ReasonMissingShard, "open PVC shard", fmt.Errorf("required shard %q is absent", name))
			}
			return Result{}, err
		}
		plans = append(plans, plan)
		additional := uint64(plan.sourceSize - plan.resumeBytes)
		if additional > math.MaxUint64-required {
			return Result{}, failure(ReasonInvalidSpec, "preflight PVC copy", errors.New("selected file sizes overflow"))
		}
		required += additional
	}
	if err := RequirePublicationSpace(spec.CacheRoot, required, len(selected)); err != nil {
		return Result{}, err
	}
	var transferred int64
	for _, plan := range plans {
		written, copyErr := plan.copy(ctx)
		closeErr := plan.close()
		if copyErr != nil || closeErr != nil {
			return Result{}, errors.Join(copyErr, closeErr)
		}
		if written > math.MaxInt64-transferred {
			return Result{}, failure(ReasonIOFailure, "count PVC copy", errors.New("copied byte count overflow"))
		}
		transferred += written
	}
	validationStarted := time.Now()
	manifest, metadata, err := validateStaged(spec, spec.CacheRoot, staging, selected)
	validationMillis := time.Since(validationStarted).Milliseconds()
	if err != nil {
		return Result{ValidationMillis: validationMillis}, err
	}
	digest, publishedPath, cacheHit, err := Publish(spec.CacheRoot, staging, spec.OperationID, "", manifest)
	if err != nil {
		return Result{ValidationMillis: validationMillis}, err
	}
	return successfulResult(
		spec.Mode, manifest, metadata, digest, publishedPath, "",
		transferred, validationMillis, cacheHit,
	), nil
}

func executeDirect(spec Spec) (Result, error) {
	selected, err := ExpandStandardShards(spec.Entrypoint, spec.PVC.SelectedFiles)
	if err != nil {
		return Result{}, err
	}
	validationStarted := time.Now()
	manifest, err := BuildManifest(spec.PVC.MountRoot, spec.PVC.RootPath, spec.Format, spec.Entrypoint, selected)
	if err != nil {
		validationMillis := time.Since(validationStarted).Milliseconds()
		if isStandardShard(spec.Entrypoint) && errors.Is(err, os.ErrNotExist) {
			return Result{ValidationMillis: validationMillis}, failure(ReasonMissingShard, "open PVC shard", err)
		}
		return Result{ValidationMillis: validationMillis}, err
	}
	metadata, err := ValidateGGUFSet(spec.PVC.MountRoot, spec.PVC.RootPath, manifest)
	if err != nil {
		return Result{ValidationMillis: time.Since(validationStarted).Milliseconds()}, err
	}
	if err := VerifyExpectations(manifest, spec.ExpectedSHA256, spec.ExpectedSize); err != nil {
		return Result{ValidationMillis: time.Since(validationStarted).Milliseconds()}, err
	}
	digest, err := ArtifactDigest(manifest)
	if err != nil {
		return Result{ValidationMillis: time.Since(validationStarted).Milliseconds()}, err
	}
	validationMillis := time.Since(validationStarted).Milliseconds()
	return successfulResult(
		spec.Mode, manifest, metadata, digest, spec.PVC.RootPath, "",
		0, validationMillis, false,
	), nil
}

func executeProbe(spec Spec) (Result, error) {
	probe, err := ProbeFilesystem(spec.Probe.Root)
	if err != nil {
		return Result{}, err
	}
	return Result{SchemaVersion: SchemaVersion, Mode: ModeProbe, Success: true, Probe: &probe}, nil
}

func executeCleanup(spec Spec) (Result, error) {
	if err := RemoveOperationState(spec.CacheRoot, spec.Cleanup.OperationPrefix); err != nil {
		return Result{}, err
	}
	return Result{SchemaVersion: SchemaVersion, Mode: ModeCleanup, Success: true}, nil
}

func validateStaged(spec Spec, root, prefix string, selected []string) (Manifest, GGUFMetadata, error) {
	manifest, err := BuildManifest(root, prefix, spec.Format, spec.Entrypoint, selected)
	if err != nil {
		return Manifest{}, GGUFMetadata{}, err
	}
	metadata, err := ValidateGGUFSet(root, prefix, manifest)
	if err != nil {
		return Manifest{}, GGUFMetadata{}, err
	}
	if err := VerifyExpectations(manifest, spec.ExpectedSHA256, spec.ExpectedSize); err != nil {
		return Manifest{}, GGUFMetadata{}, err
	}
	return manifest, metadata, nil
}

func recoverResult(spec Spec) (Result, bool, error) {
	validationStarted := time.Now()
	manifest, metadata, digest, publishedPath, revision, found, err := RecoverPublished(spec.CacheRoot, spec.OperationID)
	if err != nil {
		return Result{ValidationMillis: time.Since(validationStarted).Milliseconds()}, found, err
	}
	if !found {
		return Result{}, false, nil
	}
	if manifest.Entrypoint != spec.Entrypoint || !strings.EqualFold(manifest.Format, spec.Format) {
		return Result{ValidationMillis: time.Since(validationStarted).Milliseconds()}, false,
			failure(ReasonPublicationConflict, "recover artifact", errors.New("operation marker describes a different artifact"))
	}
	if err := VerifyExpectations(manifest, spec.ExpectedSHA256, spec.ExpectedSize); err != nil {
		return Result{ValidationMillis: time.Since(validationStarted).Milliseconds()}, false, err
	}
	validationMillis := time.Since(validationStarted).Milliseconds()
	return successfulResult(
		spec.Mode, manifest, metadata, digest, publishedPath, revision,
		0, validationMillis, true,
	), true, nil
}

func successfulResult(
	mode Mode,
	manifest Manifest,
	metadata GGUFMetadata,
	digest, publishedPath, revision string,
	transferred, validationMillis int64,
	cacheHit bool,
) Result {
	return Result{
		SchemaVersion:    SchemaVersion,
		Mode:             mode,
		Success:          true,
		ResolvedRevision: revision,
		ArtifactDigest:   digest,
		Manifest:         &manifest,
		GGUF:             &metadata,
		PublishedPath:    publishedPath,
		BytesTransferred: transferred,
		ValidationMillis: validationMillis,
		CacheHit:         cacheHit,
	}
}

func withPrefix(prefix, name string) string {
	if prefix == "" || prefix == "." {
		return name
	}
	return path.Join(prefix, name)
}

func isStandardShard(entrypoint string) bool {
	return standardShardPattern.MatchString(path.Base(entrypoint))
}
