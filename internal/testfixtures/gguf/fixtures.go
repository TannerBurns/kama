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

// Package gguf exposes small, project-owned GGUF v3 metadata payloads for
// tests. The fixtures intentionally contain no tensors or model weights.
package gguf

import (
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Name identifies an embedded synthetic fixture.
type Name string

const (
	// ValidMinimal is a valid GGUF v3 document with four metadata entries.
	ValidMinimal Name = "valid-minimal"
	// ShardedMetadata is valid shard metadata for shard zero of two.
	ShardedMetadata Name = "sharded-metadata"
	// MalformedMagic has an invalid four-byte GGUF magic value.
	MalformedMagic Name = "malformed-magic"
	// TruncatedMetadata ends partway through its first metadata key.
	TruncatedMetadata Name = "truncated-metadata"
)

//go:embed testdata/*.gguf.b64
var encodedFixtures embed.FS

var fixtureFiles = map[Name]string{
	ValidMinimal:      "testdata/valid-minimal.gguf.b64",
	ShardedMetadata:   "testdata/sharded-metadata.gguf.b64",
	MalformedMagic:    "testdata/malformed-magic.gguf.b64",
	TruncatedMetadata: "testdata/truncated-metadata.gguf.b64",
}

// Names returns all fixture names in a stable order.
func Names() []Name {
	return []Name{ValidMinimal, ShardedMetadata, MalformedMagic, TruncatedMetadata}
}

// Read decodes and returns a fresh copy of the requested GGUF payload.
func Read(name Name) ([]byte, error) {
	path, found := fixtureFiles[name]
	if !found {
		return nil, fmt.Errorf("unknown GGUF fixture %q", name)
	}
	encoded, err := encodedFixtures.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read encoded GGUF fixture %q: %w", name, err)
	}
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(string(encoded)))
	payload, err := io.ReadAll(decoder)
	if err != nil {
		return nil, fmt.Errorf("decode GGUF fixture %q: %w", name, err)
	}
	if len(payload) == 0 {
		return nil, errors.New("decoded GGUF fixture is empty")
	}
	return payload, nil
}
