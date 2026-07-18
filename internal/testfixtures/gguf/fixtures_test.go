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

package gguf

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

type fixtureManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Encoding      string                `json:"encoding"`
	Fixtures      []fixtureManifestItem `json:"fixtures"`
}

type fixtureManifestItem struct {
	Name          string          `json:"name"`
	File          string          `json:"file"`
	DecodedFile   string          `json:"decodedFile"`
	SHA256        string          `json:"sha256"`
	Valid         bool            `json:"valid"`
	Expected      fixtureExpected `json:"expected"`
	ExpectedError string          `json:"expectedError"`
}

type fixtureExpected struct {
	Version       uint32   `json:"version"`
	TensorCount   uint64   `json:"tensorCount"`
	MetadataCount uint64   `json:"metadataCount"`
	MetadataKeys  []string `json:"metadataKeys"`
}

type parsedDocument struct {
	Version      uint32
	TensorCount  uint64
	MetadataKeys []string
	Metadata     map[string]any
}

func TestFixturesMatchManifestAndChecksums(t *testing.T) {
	t.Parallel()

	manifest := readManifest(t)
	checksums := readChecksums(t)
	if manifest.SchemaVersion != 1 || manifest.Encoding != "base64" {
		t.Fatalf("manifest header = schema %d encoding %q, want schema 1 and base64",
			manifest.SchemaVersion, manifest.Encoding)
	}
	if len(manifest.Fixtures) != len(Names()) {
		t.Fatalf("manifest fixture count = %d, want %d", len(manifest.Fixtures), len(Names()))
	}

	for _, item := range manifest.Fixtures {
		t.Run(item.Name, func(t *testing.T) {
			t.Parallel()
			payload, err := Read(Name(item.Name))
			if err != nil {
				t.Fatalf("Read(%q): %v", item.Name, err)
			}
			digest := sha256.Sum256(payload)
			actualDigest := hex.EncodeToString(digest[:])
			if actualDigest != item.SHA256 {
				t.Fatalf("decoded SHA-256 = %s, want %s", actualDigest, item.SHA256)
			}
			if checksums[item.DecodedFile] != actualDigest {
				t.Fatalf("SHA256SUMS value = %q, want %q", checksums[item.DecodedFile], actualDigest)
			}
			if path := fixtureFiles[Name(item.Name)]; path != "testdata/"+item.File {
				t.Fatalf("embedded path = %q, want testdata/%s", path, item.File)
			}

			document, parseError := parseGGUF(payload)
			if !item.Valid {
				if parseError == nil || !strings.Contains(parseError.Error(), item.ExpectedError) {
					t.Fatalf("parse error = %v, want error containing %q", parseError, item.ExpectedError)
				}
				return
			}
			if parseError != nil {
				t.Fatalf("parse valid fixture: %v", parseError)
			}
			assertExpectedDocument(t, document, item.Expected)
		})
	}
}

func TestShardedMetadataValues(t *testing.T) {
	t.Parallel()

	payload, err := Read(ShardedMetadata)
	if err != nil {
		t.Fatalf("Read(): %v", err)
	}
	document, err := parseGGUF(payload)
	if err != nil {
		t.Fatalf("parseGGUF(): %v", err)
	}
	if document.Metadata["split.no"] != uint16(0) ||
		document.Metadata["split.count"] != uint16(2) ||
		document.Metadata["split.tensors.count"] != uint64(0) {
		t.Fatalf("shard metadata = %+v, want shard zero of two with zero tensors", document.Metadata)
	}
}

func TestFixtureProvenanceExcludesModelContent(t *testing.T) {
	t.Parallel()

	provenance, err := os.ReadFile("testdata/PROVENANCE.md")
	if err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	content := string(provenance)
	for _, statement := range []string{"project-owned test data", "zero tensor data", "no model weights", "no third-party model content"} {
		if !strings.Contains(content, statement) {
			t.Fatalf("provenance does not contain %q", statement)
		}
	}
}

func TestReadRejectsUnknownFixture(t *testing.T) {
	t.Parallel()

	if _, err := Read("unknown"); err == nil {
		t.Fatal("Read(unknown) returned nil error")
	}
}

func readManifest(t *testing.T) fixtureManifest {
	t.Helper()
	contents, err := os.ReadFile("testdata/manifest.json")
	if err != nil {
		t.Fatalf("read fixture manifest: %v", err)
	}
	var manifest fixtureManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatalf("decode fixture manifest: %v", err)
	}
	return manifest
}

func readChecksums(t *testing.T) map[string]string {
	t.Helper()
	file, err := os.Open("testdata/SHA256SUMS")
	if err != nil {
		t.Fatalf("open SHA256SUMS: %v", err)
	}
	defer func() { _ = file.Close() }()
	checksums := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			t.Fatalf("invalid SHA256SUMS line %q", scanner.Text())
		}
		checksums[fields[1]] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SHA256SUMS: %v", err)
	}
	return checksums
}

func assertExpectedDocument(t *testing.T, document parsedDocument, expected fixtureExpected) {
	t.Helper()
	if document.Version != expected.Version || document.TensorCount != expected.TensorCount ||
		uint64(len(document.MetadataKeys)) != expected.MetadataCount {
		t.Fatalf("parsed header = version %d tensors %d metadata %d, want version %d tensors %d metadata %d",
			document.Version, document.TensorCount, len(document.MetadataKeys),
			expected.Version, expected.TensorCount, expected.MetadataCount)
	}
	if len(document.MetadataKeys) != len(expected.MetadataKeys) {
		t.Fatalf("metadata keys = %v, want %v", document.MetadataKeys, expected.MetadataKeys)
	}
	for index, key := range expected.MetadataKeys {
		if document.MetadataKeys[index] != key {
			t.Fatalf("metadata keys = %v, want %v", document.MetadataKeys, expected.MetadataKeys)
		}
	}
}

func parseGGUF(payload []byte) (parsedDocument, error) {
	reader := bytes.NewReader(payload)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(reader, magic); err != nil {
		return parsedDocument{}, fmt.Errorf("read GGUF magic: %w", err)
	}
	if string(magic) != "GGUF" {
		return parsedDocument{}, fmt.Errorf("invalid GGUF magic %q", magic)
	}

	document := parsedDocument{Metadata: make(map[string]any)}
	if err := binary.Read(reader, binary.LittleEndian, &document.Version); err != nil {
		return parsedDocument{}, fmt.Errorf("read GGUF version: %w", err)
	}
	if document.Version != 3 {
		return parsedDocument{}, fmt.Errorf("unsupported GGUF version %d", document.Version)
	}
	if err := binary.Read(reader, binary.LittleEndian, &document.TensorCount); err != nil {
		return parsedDocument{}, fmt.Errorf("read tensor count: %w", err)
	}
	var metadataCount uint64
	if err := binary.Read(reader, binary.LittleEndian, &metadataCount); err != nil {
		return parsedDocument{}, fmt.Errorf("read metadata count: %w", err)
	}
	if metadataCount > uint64(reader.Len()) {
		return parsedDocument{}, errors.New("metadata count exceeds remaining payload size")
	}
	document.MetadataKeys = make([]string, 0, int(metadataCount))
	for range metadataCount {
		key, err := readGGUFString(reader)
		if err != nil {
			return parsedDocument{}, fmt.Errorf("read metadata key: %w", err)
		}
		var valueType uint32
		if err := binary.Read(reader, binary.LittleEndian, &valueType); err != nil {
			return parsedDocument{}, fmt.Errorf("read metadata value type: %w", err)
		}
		value, err := readGGUFValue(reader, valueType)
		if err != nil {
			return parsedDocument{}, fmt.Errorf("read metadata value for %q: %w", key, err)
		}
		document.MetadataKeys = append(document.MetadataKeys, key)
		document.Metadata[key] = value
	}
	return document, nil
}

func readGGUFString(reader *bytes.Reader) (string, error) {
	var length uint64
	if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
		return "", err
	}
	if length > uint64(reader.Len()) {
		return "", io.ErrUnexpectedEOF
	}
	value := make([]byte, int(length))
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", err
	}
	return string(value), nil
}

func readGGUFValue(reader *bytes.Reader, valueType uint32) (any, error) {
	switch valueType {
	case 2:
		var value uint16
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 4:
		var value uint32
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 7:
		value, err := reader.ReadByte()
		return value != 0, err
	case 8:
		return readGGUFString(reader)
	case 10:
		var value uint64
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported GGUF value type %d", valueType)
	}
}
