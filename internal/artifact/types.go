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

// Package artifact implements Kama's durable artifact format and importer.
// It deliberately has no dependency on the Kubernetes API so the same
// validation and result contract can be used by Jobs and controllers.
package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

const (
	// FormatGGUF is the only M1 artifact format.
	FormatGGUF = "GGUF"
	// SchemaVersion is the on-disk manifest and importer result schema.
	SchemaVersion = 1
	// MaxSelectedFiles bounds API status, importer output, and work per artifact.
	MaxSelectedFiles = 128
	// MaxResultBytes bounds the full JSON line consumed from retained Pod logs.
	MaxResultBytes = 256 << 10
	// MaxSummaryBytes is below Kubernetes' termination message limit.
	MaxSummaryBytes = 4 << 10
)

// Mode is an importer operation.
type Mode string

const (
	// ModeUnknown is used only when a malformed spec cannot be decoded far
	// enough to identify its requested operation.
	ModeUnknown Mode = "unknown"
	ModeHub     Mode = "hub"
	ModeCopy    Mode = "copy"
	ModeDirect  Mode = "direct"
	ModeProbe   Mode = "probe"
	ModeCleanup Mode = "cleanup"
)

// ValidMode reports whether mode is part of the importer result contract.
func ValidMode(mode Mode) bool {
	switch mode {
	case ModeUnknown, ModeHub, ModeCopy, ModeDirect, ModeProbe, ModeCleanup:
		return true
	default:
		return false
	}
}

// Reason is a stable, non-sensitive failure category suitable for conditions
// and metrics.
type Reason string

const (
	ReasonInvalidSpec         Reason = "InvalidSpec"
	ReasonUnsafePath          Reason = "UnsafePath"
	ReasonSourceUnavailable   Reason = "SourceUnavailable"
	ReasonUnauthorized        Reason = "Unauthorized"
	ReasonChecksumMismatch    Reason = "ChecksumMismatch"
	ReasonInvalidGGUF         Reason = "InvalidGGUF"
	ReasonMissingShard        Reason = "MissingShard"
	ReasonInsufficientStorage Reason = "InsufficientStorage"
	ReasonIOFailure           Reason = "IOFailure"
	ReasonPublicationConflict Reason = "PublicationConflict"
)

// ValidReason reports whether reason is a bounded public failure category.
func ValidReason(reason Reason) bool {
	switch reason {
	case ReasonInvalidSpec, ReasonUnsafePath, ReasonSourceUnavailable, ReasonUnauthorized,
		ReasonChecksumMismatch, ReasonInvalidGGUF, ReasonMissingShard, ReasonInsufficientStorage,
		ReasonIOFailure, ReasonPublicationConflict:
		return true
	default:
		return false
	}
}

// Error carries a stable reason while retaining an internal error chain. Its
// public message is sanitized before it is put in a Result.
type Error struct {
	Reason Reason
	Op     string
	Err    error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Op == "" {
		return e.Err.Error()
	}
	return e.Op + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func failure(reason Reason, op string, err error) error {
	if err == nil {
		err = errors.New("operation failed")
	}
	return &Error{Reason: reason, Op: op, Err: err}
}

// FailureReason extracts a stable reason from err.
func FailureReason(err error) Reason {
	var artifactError *Error
	if errors.As(err, &artifactError) {
		return artifactError.Reason
	}
	return ReasonIOFailure
}

// Spec is the complete, versioned input mounted into an importer Job. Source
// credentials are file references, never inline values.
type Spec struct {
	SchemaVersion  int               `json:"schemaVersion"`
	Mode           Mode              `json:"mode"`
	OperationID    string            `json:"operationID,omitempty"`
	Format         string            `json:"format,omitempty"`
	Entrypoint     string            `json:"entrypoint,omitempty"`
	ExpectedSHA256 string            `json:"expectedSHA256,omitempty"`
	ExpectedSize   *int64            `json:"expectedSize,omitempty"`
	CacheRoot      string            `json:"cacheRoot,omitempty"`
	HubEndpoint    string            `json:"hubEndpoint,omitempty"`
	Hub            *HubSpec          `json:"hub,omitempty"`
	PVC            *PVCSpec          `json:"pvc,omitempty"`
	Probe          *ProbeSpec        `json:"probe,omitempty"`
	Cleanup        *CleanupSpec      `json:"cleanup,omitempty"`
	HTTP           HTTPClientOptions `json:"http,omitempty"`
}

// HubSpec identifies one immutable-or-resolvable Hugging Face source.
type HubSpec struct {
	Repository    string   `json:"repository"`
	Revision      string   `json:"revision"`
	FileSelectors []string `json:"fileSelectors"`
	TokenFile     string   `json:"tokenFile,omitempty"`
}

// PVCSpec identifies the source mount and a path within it. RootPath and all
// selected files are interpreted as POSIX relative paths.
type PVCSpec struct {
	MountRoot     string   `json:"mountRoot"`
	RootPath      string   `json:"rootPath,omitempty"`
	SelectedFiles []string `json:"selectedFiles,omitempty"`
}

// ProbeSpec identifies the filesystem mount to validate.
type ProbeSpec struct {
	Root string `json:"root"`
}

// CleanupSpec identifies one artifact UID operation prefix. Cleanup removes
// transient operation state only; verified blob publications are never in its
// path set.
type CleanupSpec struct {
	OperationPrefix string `json:"operationPrefix"`
}

// HTTPClientOptions controls bounded networking behavior. Zero values select
// secure defaults.
type HTTPClientOptions struct {
	Timeout          time.Duration `json:"timeout,omitempty"`
	MaxRedirects     int           `json:"maxRedirects,omitempty"`
	AllowHTTP        bool          `json:"allowHTTP,omitempty"`
	UserAgent        string        `json:"userAgent,omitempty"`
	MaxResponseBytes int64         `json:"maxResponseBytes,omitempty"`
}

// GGUFMetadata is the serving-relevant subset of a GGUF v3 header.
type GGUFMetadata struct {
	Version      uint32 `json:"version"`
	Architecture string `json:"architecture,omitempty"`
	Quantization string `json:"quantization,omitempty"`
	ShardCount   uint32 `json:"shardCount,omitempty"`
	TensorCount  uint64 `json:"tensorCount,omitempty"`
}

// ProbeResult reports observable filesystem capacity and required behavior.
type ProbeResult struct {
	CapacityBytes   uint64 `json:"capacityBytes"`
	FreeBytes       uint64 `json:"freeBytes"`
	Write           bool   `json:"write"`
	Fsync           bool   `json:"fsync"`
	AtomicRename    bool   `json:"atomicRename"`
	DirectoryRename bool   `json:"directoryRename"`
	Mmap            bool   `json:"mmap"`
	Lock            bool   `json:"lock"`
}

// Result is the stable Job-to-controller contract. It is emitted as one JSON
// line and is intentionally bounded by MaxSelectedFiles and MaxResultBytes.
type Result struct {
	SchemaVersion    int           `json:"schemaVersion"`
	Mode             Mode          `json:"mode"`
	OperationID      string        `json:"operationID,omitempty"`
	Success          bool          `json:"success"`
	Reason           Reason        `json:"reason,omitempty"`
	Message          string        `json:"message,omitempty"`
	ResolvedRevision string        `json:"resolvedRevision,omitempty"`
	ArtifactDigest   string        `json:"artifactDigest,omitempty"`
	Manifest         *Manifest     `json:"manifest,omitempty"`
	GGUF             *GGUFMetadata `json:"gguf,omitempty"`
	PublishedPath    string        `json:"publishedPath,omitempty"`
	BytesTransferred int64         `json:"bytesTransferred,omitempty"`
	ValidationMillis int64         `json:"validationMillis"`
	CacheHit         bool          `json:"cacheHit,omitempty"`
	Probe            *ProbeResult  `json:"probe,omitempty"`
	DurationMillis   int64         `json:"durationMillis"`
}

// NewFailureResult converts an error to a safe result. secrets are removed
// from the message in addition to generic credential and URL redaction.
func NewFailureResult(mode Mode, err error, secrets ...string) Result {
	if !ValidMode(mode) || mode == "" {
		mode = ModeUnknown
	}
	message := "artifact operation failed"
	if err != nil {
		message = Sanitize(err.Error(), secrets...)
	}
	return Result{
		SchemaVersion: SchemaVersion,
		Mode:          mode,
		Success:       false,
		Reason:        FailureReason(err),
		Message:       truncateUTF8(message, 1024),
	}
}

// MarshalResultLine returns one bounded JSON line.
func MarshalResultLine(result Result) ([]byte, error) {
	result.SchemaVersion = SchemaVersion
	result.Message = truncateUTF8(Sanitize(result.Message), 1024)
	if err := validateResultMeasurements(result); err != nil {
		return nil, err
	}
	if result.Manifest != nil && len(result.Manifest.Files) > MaxSelectedFiles {
		return nil, fmt.Errorf("result has %d files, maximum is %d", len(result.Manifest.Files), MaxSelectedFiles)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode importer result: %w", err)
	}
	if len(payload)+1 > MaxResultBytes {
		return nil, fmt.Errorf("importer result is %d bytes, maximum is %d", len(payload)+1, MaxResultBytes)
	}
	return append(payload, '\n'), nil
}

// ParseResult reads exactly one bounded Result JSON value. It rejects unknown
// fields so incompatible importer/controller versions fail explicitly.
//
//nolint:gocyclo // The Job result is an untrusted tagged union validated exhaustively here.
func ParseResult(reader io.Reader) (Result, error) {
	limited := io.LimitReader(reader, MaxResultBytes+1)
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var result Result
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decode importer result: %w", err)
	}
	if result.SchemaVersion != SchemaVersion {
		return Result{}, fmt.Errorf("unsupported result schemaVersion %d", result.SchemaVersion)
	}
	if !ValidMode(result.Mode) {
		return Result{}, fmt.Errorf("unsupported result mode %q", result.Mode)
	}
	if result.OperationID != "" {
		if err := validateOperationID(result.OperationID); err != nil {
			return Result{}, fmt.Errorf("invalid result operationID: %w", err)
		}
	}
	if result.Success && result.Reason != "" {
		return Result{}, errors.New("successful importer result contains a failure reason")
	}
	if result.Success && result.Mode == ModeUnknown {
		return Result{}, errors.New("successful importer result has unknown mode")
	}
	if !result.Success && !ValidReason(result.Reason) {
		return Result{}, fmt.Errorf("failed importer result has invalid reason %q", result.Reason)
	}
	if err := validateResultMeasurements(result); err != nil {
		return Result{}, err
	}
	if result.ArtifactDigest != "" && !validSHA256(result.ArtifactDigest) {
		return Result{}, errors.New("importer result artifactDigest is not lowercase SHA-256")
	}
	if result.ResolvedRevision != "" && !ValidHubCommit(result.ResolvedRevision) {
		return Result{}, errors.New("importer result resolvedRevision is not a full immutable Hub commit")
	}
	if result.Manifest != nil && len(result.Manifest.Files) > MaxSelectedFiles {
		return Result{}, fmt.Errorf("result has %d files, maximum is %d", len(result.Manifest.Files), MaxSelectedFiles)
	}
	if result.Manifest != nil {
		if err := ValidateManifest(*result.Manifest); err != nil {
			return Result{}, fmt.Errorf("invalid result manifest: %w", err)
		}
	}
	if result.PublishedPath != "" && result.PublishedPath != "." {
		if err := ValidateRelativePath(result.PublishedPath); err != nil {
			return Result{}, fmt.Errorf("invalid result publishedPath: %w", err)
		}
	}
	if result.Success {
		switch result.Mode {
		case ModeProbe:
			if result.Probe == nil || result.Manifest != nil || result.GGUF != nil || result.ArtifactDigest != "" || result.PublishedPath != "" {
				return Result{}, errors.New("successful probe result has an invalid shape")
			}
		case ModeCleanup:
			if result.Probe != nil || result.Manifest != nil || result.GGUF != nil || result.ArtifactDigest != "" || result.PublishedPath != "" {
				return Result{}, errors.New("successful cleanup result has an invalid shape")
			}
		case ModeHub, ModeCopy, ModeDirect:
			if result.Manifest == nil || result.GGUF == nil || result.ArtifactDigest == "" || result.Probe != nil {
				return Result{}, errors.New("successful artifact result has an invalid shape")
			}
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Result{}, errors.New("importer result contains multiple JSON values")
		}
		return Result{}, fmt.Errorf("decode trailing importer result data: %w", err)
	}
	return result, nil
}

func validateResultMeasurements(result Result) error {
	if result.BytesTransferred < 0 {
		return errors.New("importer result bytesTransferred must not be negative")
	}
	if result.DurationMillis < 0 {
		return errors.New("importer result duration must not be negative")
	}
	if result.ValidationMillis < 0 {
		return errors.New("importer result validationMillis must not be negative")
	}
	if result.ValidationMillis > result.DurationMillis {
		return errors.New("importer result validationMillis exceeds total duration")
	}
	return nil
}

// MarshalSummary returns a compact termination message that stays within the
// Kubernetes 4 KiB termination-message limit. Detailed file records remain in
// the full stdout Result.
func MarshalSummary(result Result) []byte {
	type summary struct {
		SchemaVersion  int    `json:"schemaVersion"`
		Mode           Mode   `json:"mode"`
		OperationID    string `json:"operationID,omitempty"`
		Success        bool   `json:"success"`
		Reason         Reason `json:"reason,omitempty"`
		Message        string `json:"message,omitempty"`
		ArtifactDigest string `json:"artifactDigest,omitempty"`
		Files          int    `json:"files,omitempty"`
		Bytes          int64  `json:"bytes,omitempty"`
		CacheHit       bool   `json:"cacheHit,omitempty"`
		Probe          *bool  `json:"probePassed,omitempty"`
	}
	item := summary{
		SchemaVersion:  SchemaVersion,
		Mode:           result.Mode,
		OperationID:    result.OperationID,
		Success:        result.Success,
		Reason:         result.Reason,
		Message:        truncateUTF8(Sanitize(result.Message), 512),
		ArtifactDigest: result.ArtifactDigest,
		Bytes:          result.BytesTransferred,
		CacheHit:       result.CacheHit,
	}
	if result.Manifest != nil {
		item.Files = len(result.Manifest.Files)
	}
	if result.Probe != nil {
		passed := result.Probe.Write && result.Probe.Fsync && result.Probe.AtomicRename &&
			result.Probe.DirectoryRename && result.Probe.Mmap && result.Probe.Lock
		item.Probe = &passed
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return []byte(`{"schemaVersion":1,"success":false,"reason":"IOFailure"}`)
	}
	if len(payload) >= MaxSummaryBytes {
		item.Message = "artifact operation failed; see importer logs"
		payload, _ = json.Marshal(item)
	}
	return payload
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
