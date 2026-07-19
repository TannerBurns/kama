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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
)

const (
	manifestFilename = "manifest.json"
	readyFilename    = "READY"
)

// FileRecord is one immutable file in a canonical Manifest.
type FileRecord struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is Kama's immutable on-disk content description. Files must be
// sorted by Path. Source URLs and credentials are intentionally excluded.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Format        string       `json:"format"`
	Entrypoint    string       `json:"entrypoint"`
	Files         []FileRecord `json:"files"`
}

// BuildManifest securely opens and hashes selected files below root/prefix.
// The paths and entrypoint in the returned manifest remain relative to prefix.
func BuildManifest(root, prefix, format, entrypoint string, selected []string) (Manifest, error) {
	if prefix == "." {
		prefix = ""
	}
	selected, err := NormalizeSelectedFiles(entrypoint, selected)
	if err != nil {
		return Manifest{}, err
	}
	if !strings.EqualFold(format, FormatGGUF) {
		return Manifest{}, failure(ReasonInvalidSpec, "validate format", fmt.Errorf("unsupported format %q", format))
	}
	if prefix != "" {
		if err := ValidateRelativePath(prefix); err != nil {
			return Manifest{}, failure(ReasonUnsafePath, "validate source root path", err)
		}
	}

	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Format:        FormatGGUF,
		Entrypoint:    entrypoint,
		Files:         make([]FileRecord, 0, len(selected)),
	}
	for _, relativePath := range selected {
		sourcePath := relativePath
		if prefix != "" {
			sourcePath = path.Join(prefix, relativePath)
		}
		file, openErr := OpenRegular(root, sourcePath)
		if openErr != nil {
			return Manifest{}, openErr
		}
		record, hashErr := HashFile(relativePath, file)
		closeErr := file.Close()
		if hashErr != nil {
			return Manifest{}, hashErr
		}
		if closeErr != nil {
			return Manifest{}, failure(ReasonIOFailure, "close artifact file", closeErr)
		}
		manifest.Files = append(manifest.Files, record)
	}
	return manifest, nil
}

// HashFile streams a regular file into SHA-256 without loading it into memory.
func HashFile(relativePath string, file *os.File) (FileRecord, error) {
	if err := ValidateRelativePath(relativePath); err != nil {
		return FileRecord{}, failure(ReasonUnsafePath, "validate manifest path", err)
	}
	info, err := file.Stat()
	if err != nil {
		return FileRecord{}, failure(ReasonIOFailure, "inspect artifact file", err)
	}
	if !info.Mode().IsRegular() {
		return FileRecord{}, failure(ReasonUnsafePath, "inspect artifact file", fmt.Errorf("%q is not regular", relativePath))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return FileRecord{}, failure(ReasonIOFailure, "seek artifact file", err)
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, file)
	if err != nil {
		return FileRecord{}, failure(ReasonIOFailure, "hash artifact file", err)
	}
	if written != info.Size() {
		return FileRecord{}, failure(ReasonIOFailure, "hash artifact file", fmt.Errorf("file changed while reading: read %d of %d bytes", written, info.Size()))
	}
	return FileRecord{Path: relativePath, Size: written, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// NormalizeSelectedFiles validates, de-duplicates, and sorts file paths.
func NormalizeSelectedFiles(entrypoint string, selected []string) ([]string, error) {
	if err := ValidateRelativePath(entrypoint); err != nil {
		return nil, failure(ReasonUnsafePath, "validate entrypoint", err)
	}
	if len(selected) == 0 {
		selected = []string{entrypoint}
	}
	if len(selected) > MaxSelectedFiles {
		return nil, failure(ReasonInvalidSpec, "validate selected files", fmt.Errorf("selected %d files, maximum is %d", len(selected), MaxSelectedFiles))
	}
	unique := make(map[string]struct{}, len(selected)+1)
	for _, name := range selected {
		if err := ValidateRelativePath(name); err != nil {
			return nil, failure(ReasonUnsafePath, "validate selected file", fmt.Errorf("%q: %w", name, err))
		}
		if name == manifestFilename || name == readyFilename {
			return nil, failure(ReasonUnsafePath, "validate selected file", fmt.Errorf("%q is reserved", name))
		}
		if !strings.EqualFold(path.Ext(name), ".gguf") {
			return nil, failure(ReasonInvalidGGUF, "validate selected file", fmt.Errorf("%q is not a GGUF file", name))
		}
		unique[name] = struct{}{}
	}
	if _, found := unique[entrypoint]; !found {
		return nil, failure(ReasonInvalidSpec, "validate selected files", errors.New("entrypoint is not selected"))
	}
	selected = selected[:0]
	for name := range unique {
		selected = append(selected, name)
	}
	if len(selected) > MaxSelectedFiles {
		return nil, failure(ReasonInvalidSpec, "validate selected files", fmt.Errorf("selected %d files, maximum is %d", len(selected), MaxSelectedFiles))
	}
	slices.Sort(selected)
	return selected, nil
}

// CanonicalManifest returns the deterministic JSON bytes used for the artifact
// digest. It rejects rather than silently normalizing a non-canonical manifest.
func CanonicalManifest(manifest Manifest) ([]byte, error) {
	if err := ValidateManifest(manifest); err != nil {
		return nil, err
	}
	return json.Marshal(manifest)
}

// ManifestDigest computes SHA-256 over the canonical manifest JSON.
func ManifestDigest(manifest Manifest) (string, error) {
	payload, err := CanonicalManifest(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

// ArtifactDigest is the content identity used for publication. A single-file
// artifact uses that file's SHA-256 directly; a multi-file artifact uses the
// SHA-256 of its canonical manifest.
func ArtifactDigest(manifest Manifest) (string, error) {
	if err := ValidateManifest(manifest); err != nil {
		return "", err
	}
	if len(manifest.Files) == 1 {
		return manifest.Files[0].SHA256, nil
	}
	return ManifestDigest(manifest)
}

// ValidateManifest validates the immutable schema and canonical ordering.
func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion {
		return failure(ReasonInvalidSpec, "validate manifest", fmt.Errorf("unsupported schemaVersion %d", manifest.SchemaVersion))
	}
	if manifest.Format != FormatGGUF {
		return failure(ReasonInvalidSpec, "validate manifest", fmt.Errorf("unsupported format %q", manifest.Format))
	}
	if err := ValidateRelativePath(manifest.Entrypoint); err != nil {
		return failure(ReasonUnsafePath, "validate manifest entrypoint", err)
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > MaxSelectedFiles {
		return failure(ReasonInvalidSpec, "validate manifest", fmt.Errorf("manifest file count %d is outside 1..%d", len(manifest.Files), MaxSelectedFiles))
	}
	entrypointFound := false
	previous := ""
	for index, record := range manifest.Files {
		if err := ValidateRelativePath(record.Path); err != nil {
			return failure(ReasonUnsafePath, "validate manifest file", err)
		}
		if index > 0 && record.Path <= previous {
			return failure(ReasonInvalidSpec, "validate manifest", errors.New("manifest files are not strictly sorted"))
		}
		if record.Size < 0 {
			return failure(ReasonInvalidSpec, "validate manifest", fmt.Errorf("negative size for %q", record.Path))
		}
		if !validSHA256(record.SHA256) {
			return failure(ReasonInvalidSpec, "validate manifest", fmt.Errorf("invalid SHA-256 for %q", record.Path))
		}
		entrypointFound = entrypointFound || record.Path == manifest.Entrypoint
		previous = record.Path
	}
	if !entrypointFound {
		return failure(ReasonInvalidSpec, "validate manifest", errors.New("manifest does not contain its entrypoint"))
	}
	return nil
}

// VerifyExpectations applies the single-file digest and multi-file canonical
// manifest digest rules, plus the aggregate expected size.
func VerifyExpectations(manifest Manifest, expectedSHA256 string, expectedSize *int64) error {
	var aggregate int64
	for _, record := range manifest.Files {
		if record.Size > int64(^uint64(0)>>1)-aggregate {
			return failure(ReasonInvalidSpec, "verify artifact size", errors.New("aggregate size overflows int64"))
		}
		aggregate += record.Size
	}
	if expectedSize != nil && aggregate != *expectedSize {
		return failure(ReasonChecksumMismatch, "verify artifact size", fmt.Errorf("aggregate size is %d bytes, expected %d", aggregate, *expectedSize))
	}
	if expectedSHA256 == "" {
		return nil
	}
	if !validSHA256(expectedSHA256) {
		return failure(ReasonInvalidSpec, "verify expected digest", errors.New("expectedSHA256 must be 64 lowercase hexadecimal characters"))
	}
	actual, err := ArtifactDigest(manifest)
	if err != nil {
		return err
	}
	if actual != expectedSHA256 {
		return failure(ReasonChecksumMismatch, "verify artifact digest", fmt.Errorf("computed SHA-256 %s does not match expected value", actual))
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func readManifest(root, relativePath string) (Manifest, error) {
	file, err := OpenRegular(root, relativePath)
	if err != nil {
		return Manifest{}, err
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, MaxResultBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return Manifest{}, failure(ReasonIOFailure, "read artifact manifest", errors.Join(readErr, closeErr))
	}
	if len(payload) > MaxResultBytes {
		return Manifest{}, failure(ReasonPublicationConflict, "validate artifact manifest", errors.New("manifest exceeds size limit"))
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, failure(ReasonIOFailure, "decode artifact manifest", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("manifest contains multiple JSON values")
		}
		return Manifest{}, failure(ReasonPublicationConflict, "validate artifact manifest", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	canonical, err := CanonicalManifest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	if !bytes.Equal(payload, canonical) {
		return Manifest{}, failure(ReasonPublicationConflict, "validate artifact manifest", errors.New("manifest encoding is not canonical"))
	}
	return manifest, nil
}
