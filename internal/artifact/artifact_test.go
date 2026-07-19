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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fixture "github.com/TannerBurns/kama/internal/testfixtures/gguf"
	"golang.org/x/sys/unix"
)

const (
	testModelFile         = "model.gguf"
	testAFile             = "a.gguf"
	testOperationOne      = "operation-one"
	testOperationTwo      = "operation-two"
	testOperationMetadata = "operation"
	testJSONSHAKey        = "sha"
	testJSONFileKey       = "rfilename"
	testJSONSizeKey       = "size"
	testJSONListKey       = "siblings"
)

func TestValidateRelativePathAndNoFollow(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"", ".", "..", "../model.gguf", "/model.gguf", "a//b.gguf", "a/./b.gguf", `a\b.gguf`} {
		if err := ValidateRelativePath(invalid); err == nil {
			t.Errorf("ValidateRelativePath(%q) succeeded", invalid)
		}
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.gguf")
	if err := os.WriteFile(outside, []byte("not a model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, testModelFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegular(root, testModelFile); err == nil || FailureReason(err) != ReasonUnsafePath {
		t.Fatalf("OpenRegular(symlink) error = %v, want UnsafePath", err)
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", testModelFile), []byte("not a model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegular(root, "linked/model.gguf"); err == nil || FailureReason(err) != ReasonUnsafePath {
		t.Fatalf("OpenRegular(symlink directory) error = %v, want UnsafePath", err)
	}
}

func TestOpenRegularRejectsFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(root, "named-pipe.gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := OpenRegular(root, "named-pipe.gguf"); err == nil || FailureReason(err) != ReasonUnsafePath {
		t.Fatalf("OpenRegular(FIFO) = %v, want UnsafePath", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("OpenRegular(FIFO) blocked for %s", elapsed)
	}
}

func TestManifestCanonicalDigestAndExpectations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "b.gguf", fixture.ValidMinimal)
	writeFixture(t, root, testAFile, fixture.ValidMinimal)
	manifest, err := BuildManifest(root, "", "gguf", testAFile, []string{"b.gguf", testAFile})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != FormatGGUF || manifest.Files[0].Path != testAFile || manifest.Files[1].Path != "b.gguf" {
		t.Fatalf("manifest is not normalized: %#v", manifest)
	}
	firstDigest, err := ArtifactDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := ArtifactDigest(manifest)
	if err != nil || firstDigest != secondDigest || len(firstDigest) != 64 {
		t.Fatalf("ManifestDigest = %q, %v; second = %q", firstDigest, err, secondDigest)
	}
	if err := VerifyExpectations(manifest, firstDigest, nil); err != nil {
		t.Fatalf("VerifyExpectations(multi-file digest): %v", err)
	}
	wrong := strings.Repeat("0", 64)
	if err := VerifyExpectations(manifest, wrong, nil); FailureReason(err) != ReasonChecksumMismatch {
		t.Fatalf("VerifyExpectations(wrong) = %v, want ChecksumMismatch", err)
	}
	single := manifest
	single.Entrypoint = testAFile
	single.Files = single.Files[:1]
	if err := VerifyExpectations(single, single.Files[0].SHA256, nil); err != nil {
		t.Fatalf("VerifyExpectations(single-file digest): %v", err)
	}
	singleDigest, err := ArtifactDigest(single)
	if err != nil || singleDigest != single.Files[0].SHA256 {
		t.Fatalf("ArtifactDigest(single) = %q, %v; want file digest %q", singleDigest, err, single.Files[0].SHA256)
	}
}

func TestCanonicalManifestGolden(t *testing.T) {
	t.Parallel()
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Format:        FormatGGUF,
		Entrypoint:    testModelFile,
		Files: []FileRecord{{
			Path: testModelFile, Size: 7,
			SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}},
	}
	payload, err := CanonicalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	const expected = `{"schemaVersion":1,"format":"GGUF","entrypoint":"model.gguf","files":[{"path":"model.gguf","size":7,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}]}`
	if string(payload) != expected {
		t.Fatalf("canonical manifest = %s, want %s", payload, expected)
	}
}

func TestGGUFInspectionAndMissingShard(t *testing.T) {
	t.Parallel()
	payload, err := fixture.Read(fixture.ValidMinimal)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := InspectGGUF(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version != 3 || metadata.Architecture == "" || metadata.ShardCount != 1 {
		t.Fatalf("InspectGGUF() = %#v", metadata)
	}
	malformed, _ := fixture.Read(fixture.MalformedMagic)
	if _, err := InspectGGUF(bytes.NewReader(malformed)); FailureReason(err) != ReasonInvalidGGUF {
		t.Fatalf("InspectGGUF(malformed) = %v, want InvalidGGUF", err)
	}

	root := t.TempDir()
	writeFixture(t, root, "model-00001-of-00002.gguf", fixture.ShardedMetadata)
	manifest, err := BuildManifest(root, "", FormatGGUF, "model-00001-of-00002.gguf", []string{"model-00001-of-00002.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateGGUFSet(root, "", manifest); FailureReason(err) != ReasonMissingShard {
		t.Fatalf("ValidateGGUFSet(incomplete) = %v, want MissingShard", err)
	}
	expanded, err := ExpandStandardShards("dir/model-00001-of-00002.gguf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 2 || expanded[1] != "dir/model-00002-of-00002.gguf" {
		t.Fatalf("ExpandStandardShards() = %v", expanded)
	}
}

func TestDirectCopyPublishAndRecovery(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	cache := t.TempDir()
	writeFixture(t, source, "models/model.gguf", fixture.ValidMinimal)
	directSpec := Spec{
		SchemaVersion: SchemaVersion,
		Mode:          ModeDirect,
		Format:        FormatGGUF,
		Entrypoint:    testModelFile,
		PVC:           &PVCSpec{MountRoot: source, RootPath: "models", SelectedFiles: []string{testModelFile}},
	}
	direct := Execute(context.Background(), directSpec)
	if !direct.Success || direct.Manifest == nil || direct.GGUF == nil || direct.PublishedPath != "models" {
		t.Fatalf("direct result = %#v", direct)
	}
	copySpec := directSpec
	copySpec.Mode = ModeCopy
	copySpec.CacheRoot = cache
	copySpec.OperationID = "artifact-uid-fingerprint"
	first := Execute(context.Background(), copySpec)
	if !first.Success || first.CacheHit || first.PublishedPath == "" || first.BytesTransferred == 0 {
		t.Fatalf("first copy result = %#v", first)
	}
	if first.ValidationMillis < 0 || first.ValidationMillis > first.DurationMillis {
		t.Fatalf("copy validationMillis = %d, durationMillis = %d", first.ValidationMillis, first.DurationMillis)
	}
	publicationDigest, err := ManifestDigest(*first.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ValidatePublished(cache, publicationDigest); err != nil {
		t.Fatalf("ValidatePublished(): %v", err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	second := Execute(context.Background(), copySpec)
	if !second.Success || !second.CacheHit || second.BytesTransferred != 0 || second.ArtifactDigest != first.ArtifactDigest {
		t.Fatalf("recovered copy result = %#v", second)
	}
}

func TestPVCCopyResumesOnlyMatchingPrefixes(t *testing.T) {
	t.Parallel()
	payload, err := fixture.Read(fixture.ValidMinimal)
	if err != nil {
		t.Fatal(err)
	}
	partialLength := len(payload) / 2
	tests := []struct {
		name        string
		staged      func() []byte
		transferred int64
	}{
		{
			name: "matching",
			staged: func() []byte {
				return append([]byte(nil), payload[:partialLength]...)
			},
			transferred: int64(len(payload) - partialLength),
		},
		{
			name: "mismatch",
			staged: func() []byte {
				staged := append([]byte(nil), payload[:partialLength]...)
				staged[len(staged)-1] ^= 0xff
				return staged
			},
			transferred: int64(len(payload)),
		},
		{
			name: "oversize",
			staged: func() []byte {
				return append(append([]byte(nil), payload...), []byte("not-source-data")...)
			},
			transferred: int64(len(payload)),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := t.TempDir()
			cache := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, testModelFile), payload, 0o600); err != nil {
				t.Fatal(err)
			}
			operation := "pvc-resume-" + test.name
			staging, err := EnsureStaging(cache, operation)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(cache, filepath.FromSlash(staging), testModelFile),
				test.staged(),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
			result := Execute(context.Background(), Spec{
				SchemaVersion: SchemaVersion,
				Mode:          ModeCopy,
				OperationID:   operation,
				Format:        FormatGGUF,
				Entrypoint:    testModelFile,
				CacheRoot:     cache,
				PVC:           &PVCSpec{MountRoot: source, SelectedFiles: []string{testModelFile}},
			})
			if !result.Success || result.BytesTransferred != test.transferred {
				t.Fatalf("copy result = %#v, want %d newly transferred bytes", result, test.transferred)
			}
		})
	}
}

func TestPVCCopyInterruptionLeavesDurableResumablePrefix(t *testing.T) {
	t.Parallel()
	payload, err := fixture.Read(fixture.ValidMinimal)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, testModelFile), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	const operation = "pvc-interrupted-resume"
	staging, err := EnsureStaging(cache, operation)
	if err != nil {
		t.Fatal(err)
	}
	partialLength := len(payload) / 2
	destinationPath := path.Join(staging, testModelFile)
	if err := os.WriteFile(
		filepath.Join(cache, filepath.FromSlash(destinationPath)),
		payload[:partialLength],
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	plan, err := prepareRegularCopy(source, testModelFile, cache, destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, cancel := context.WithCancel(context.Background())
	cancel()
	if written, err := plan.copy(interrupted); err == nil || written != 0 {
		t.Fatalf("interrupted copy wrote %d bytes with error %v", written, err)
	}
	if err := plan.close(); err != nil {
		t.Fatal(err)
	}
	partialInfo, err := os.Stat(filepath.Join(cache, filepath.FromSlash(destinationPath)))
	if err != nil {
		t.Fatal(err)
	}
	if partialInfo.Size() != int64(partialLength) {
		t.Fatalf("durable partial size = %d, want %d", partialInfo.Size(), partialLength)
	}

	result := Execute(context.Background(), Spec{
		SchemaVersion: SchemaVersion,
		Mode:          ModeCopy,
		OperationID:   operation,
		Format:        FormatGGUF,
		Entrypoint:    testModelFile,
		CacheRoot:     cache,
		PVC:           &PVCSpec{MountRoot: source, SelectedFiles: []string{testModelFile}},
	})
	if !result.Success || result.BytesTransferred != int64(len(payload)-partialLength) {
		t.Fatalf("resumed copy result = %#v", result)
	}
}

func TestPVCCopyRejectsSourceMutationAfterPreflight(t *testing.T) {
	t.Parallel()
	payload, err := fixture.Read(fixture.ValidMinimal)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	cache := t.TempDir()
	sourcePath := filepath.Join(source, testModelFile)
	if err := os.WriteFile(sourcePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	const operation = "pvc-source-mutation"
	staging, err := EnsureStaging(cache, operation)
	if err != nil {
		t.Fatal(err)
	}
	destinationPath := path.Join(staging, testModelFile)
	plan, err := prepareRegularCopy(source, testModelFile, cache, destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := plan.close(); err != nil {
			t.Errorf("close copy plan: %v", err)
		}
	}()
	if err := plan.validateForTransfer(); err != nil {
		t.Fatalf("validate copy preflight: %v", err)
	}
	changed := append([]byte(nil), payload...)
	changed[len(changed)-1] ^= 0xff
	if err := os.WriteFile(sourcePath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	changedTime := time.Now().Add(time.Minute)
	if err := os.Chtimes(sourcePath, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.copyPrepared(context.Background()); FailureReason(err) != ReasonSourceUnavailable {
		t.Fatalf("copy changed source error = %v, want SourceUnavailable", err)
	}
	destinationInfo, err := os.Stat(filepath.Join(cache, filepath.FromSlash(destinationPath)))
	if err != nil {
		t.Fatal(err)
	}
	if destinationInfo.Size() != 0 {
		t.Fatalf("changed-source staging size = %d, want durable reset", destinationInfo.Size())
	}
}

func TestPVCRootPathDotMeansVolumeRoot(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeFixture(t, source, testModelFile, fixture.ValidMinimal)
	result := Execute(context.Background(), Spec{
		SchemaVersion: SchemaVersion,
		Mode:          ModeDirect,
		Format:        FormatGGUF,
		Entrypoint:    testModelFile,
		PVC:           &PVCSpec{MountRoot: source, RootPath: ".", SelectedFiles: []string{testModelFile}},
	})
	if !result.Success || result.Manifest == nil || result.Manifest.Files[0].Path != testModelFile {
		t.Fatalf("direct rootPath dot result = %#v", result)
	}
}

func TestConcurrentContentAddressedPublication(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	manifests := make([]Manifest, 0, 2)
	for _, operation := range []string{testOperationOne, testOperationTwo} {
		staging, err := EnsureStaging(cache, operation)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, cache, filepath.Join(staging, testModelFile), fixture.ValidMinimal)
		manifest, err := BuildManifest(cache, staging, FormatGGUF, testModelFile, []string{testModelFile})
		if err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, manifest)
	}
	type outcome struct {
		digest string
		hit    bool
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var publishers sync.WaitGroup
	for index, operation := range []string{testOperationOne, testOperationTwo} {
		publishers.Go(func() {
			<-start
			digest, _, hit, err := Publish(cache, ".kama/staging/"+operation, operation, "", manifests[index])
			outcomes <- outcome{digest: digest, hit: hit, err: err}
		})
	}
	close(start)
	publishers.Wait()
	close(outcomes)

	var digest string
	cacheHits := 0
	for result := range outcomes {
		if result.err != nil {
			t.Fatalf("concurrent Publish(): %v", result.err)
		}
		if digest == "" {
			digest = result.digest
		} else if result.digest != digest {
			t.Fatalf("concurrent digests differ: %q and %q", digest, result.digest)
		}
		if result.hit {
			cacheHits++
		}
	}
	if cacheHits != 1 {
		t.Fatalf("cache-hit publishers = %d, want exactly 1", cacheHits)
	}
	publicationDigest, err := ManifestDigest(manifests[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ValidatePublished(cache, publicationDigest); err != nil {
		t.Fatalf("ValidatePublished() after concurrent publishers: %v", err)
	}
	for _, operation := range []string{"operation-one", "operation-two"} {
		if _, err := os.Stat(filepath.Join(cache, ".kama", "staging", operation)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging for %s remains after concurrent publication: %v", operation, err)
		}
	}
}

func TestPublishRecoversCrashBeforeReadyAndOperationMarker(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	prepare := func(operation string) Manifest {
		t.Helper()
		staging, err := EnsureStaging(cache, operation)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, cache, filepath.Join(staging, testModelFile), fixture.ValidMinimal)
		manifest, err := BuildManifest(cache, staging, FormatGGUF, testModelFile, nil)
		if err != nil {
			t.Fatal(err)
		}
		return manifest
	}

	manifest := prepare("crashed-operation")
	digest, published, _, err := Publish(cache, ".kama/staging/crashed-operation", "crashed-operation", "", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cache, filepath.FromSlash(published), readyFilename)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cache, ".kama", "operations", "crashed-operation.json")); err != nil {
		t.Fatal(err)
	}
	publicationDigest, err := ManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredManifest, _, recoveredDigest, recoveredPath, _, found, err := RecoverPublished(cache, "crashed-operation"); err != nil || !found || recoveredDigest != digest || recoveredPath != published || !manifestsEqual(recoveredManifest, manifest) {
		t.Fatalf("intent recovery = digest %q, path %q, found %v, err %v", recoveredDigest, recoveredPath, found, err)
	}

	retryManifest := prepare("retry-operation")
	retryDigest, _, hit, err := Publish(cache, ".kama/staging/retry-operation", "retry-operation", "", retryManifest)
	if err != nil || !hit || retryDigest != digest {
		t.Fatalf("retry Publish() = digest %q, hit %v, err %v", retryDigest, hit, err)
	}
	if _, _, err := ValidatePublished(cache, publicationDigest); err != nil {
		t.Fatalf("ValidatePublished() after recovery: %v", err)
	}
	if _, _, _, _, _, found, err := RecoverPublished(cache, "retry-operation"); err != nil || !found {
		t.Fatalf("RecoverPublished() found=%v, err=%v", found, err)
	}
}

func TestPublicationKeySeparatesSameBytesAtDifferentPaths(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	type publication struct {
		artifactDigest string
		path           string
		manifest       Manifest
	}
	items := make([]publication, 0, 2)
	for index, filename := range []string{"foo.gguf", "bar.gguf"} {
		operation := fmt.Sprintf("different-path-%d", index)
		staging, err := EnsureStaging(cache, operation)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, cache, filepath.Join(staging, filename), fixture.ValidMinimal)
		manifest, err := BuildManifest(cache, staging, FormatGGUF, filename, nil)
		if err != nil {
			t.Fatal(err)
		}
		digest, publishedPath, _, err := Publish(cache, staging, operation, "", manifest)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, publication{artifactDigest: digest, path: publishedPath, manifest: manifest})
	}
	if items[0].artifactDigest != items[1].artifactDigest {
		t.Fatalf("same bytes have different public artifact digests: %#v", items)
	}
	if items[0].path == items[1].path {
		t.Fatalf("path-dependent manifests collided at %q", items[0].path)
	}
	for _, item := range items {
		publicationDigest, err := ManifestDigest(item.manifest)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ValidatePublished(cache, publicationDigest); err != nil {
			t.Fatalf("ValidatePublished(%s): %v", item.path, err)
		}
	}
}

func TestWritableStagingRejectsHardlinkToPublishedBlob(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	staging, err := EnsureStaging(cache, "publish-hardlink-source")
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, cache, filepath.Join(staging, testModelFile), fixture.ValidMinimal)
	manifest, err := BuildManifest(cache, staging, FormatGGUF, testModelFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, publishedPath, _, err := Publish(cache, staging, "publish-hardlink-source", "", manifest)
	if err != nil {
		t.Fatal(err)
	}
	attackStaging, err := EnsureStaging(cache, "publish-hardlink-attack")
	if err != nil {
		t.Fatal(err)
	}
	publishedFile := filepath.Join(cache, filepath.FromSlash(publishedPath), testModelFile)
	attackFile := filepath.Join(cache, filepath.FromSlash(attackStaging), testModelFile)
	if err := os.Link(publishedFile, attackFile); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWritableRegular(cache, path.Join(attackStaging, testModelFile), true); err == nil || FailureReason(err) != ReasonUnsafePath {
		t.Fatalf("OpenWritableRegular(hardlink) = %v, want UnsafePath", err)
	}
	publicationDigest, _ := ManifestDigest(manifest)
	if _, _, err := ValidatePublished(cache, publicationDigest); err != nil {
		t.Fatalf("published content was damaged by hardlink attempt: %v", err)
	}
}

func TestPublishedMetadataRequiresExactCanonicalBytes(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"manifest", "ready", testOperationMetadata} {
		t.Run(target, func(t *testing.T) {
			cache := t.TempDir()
			operation := "exact-metadata-" + target
			staging, err := EnsureStaging(cache, operation)
			if err != nil {
				t.Fatal(err)
			}
			writeFixture(t, cache, filepath.Join(staging, testModelFile), fixture.ValidMinimal)
			manifest, err := BuildManifest(cache, staging, FormatGGUF, testModelFile, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, publishedPath, _, err := Publish(cache, staging, operation, "", manifest)
			if err != nil {
				t.Fatal(err)
			}
			var filename string
			switch target {
			case "manifest":
				filename = filepath.Join(cache, filepath.FromSlash(publishedPath), manifestFilename)
			case "ready":
				filename = filepath.Join(cache, filepath.FromSlash(publishedPath), readyFilename)
			case "operation":
				filename = filepath.Join(cache, ".kama", "operations", operation+".json")
			}
			if err := os.Chmod(filename, 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(filename, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(" "); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if target == testOperationMetadata {
				if _, _, _, _, _, _, err := RecoverPublished(cache, operation); err == nil {
					t.Fatal("RecoverPublished accepted non-canonical operation metadata")
				}
				return
			}
			publicationDigest, _ := ManifestDigest(manifest)
			if _, _, err := ValidatePublished(cache, publicationDigest); err == nil {
				t.Fatalf("ValidatePublished accepted %s with trailing bytes", target)
			}
		})
	}
}

func TestRemoveOperationStateIsArtifactScoped(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	prefix := "artifact-uid-"
	targetOperation := prefix + "0123456789abcdef0123"
	otherOperation := "other-uid-0123456789abcdef0123"
	for _, operation := range []string{targetOperation, otherOperation} {
		if _, err := EnsureStaging(cache, operation); err != nil {
			t.Fatal(err)
		}
		for _, item := range []string{
			path.Join(".kama/intents", operation+".json"),
			path.Join(".kama/operations", operation+".json"),
			path.Join(".kama/revisions", operation+".json"),
			path.Join(".kama/locks", operation+".lock"),
		} {
			if err := MkdirAll(cache, path.Dir(item), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cache, filepath.FromSlash(item)), []byte("transient"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, operation := range []string{targetOperation, otherOperation} {
		for _, directory := range []string{".kama/intents", ".kama/operations", ".kama/revisions"} {
			item := path.Join(directory, "."+operation+".json.tmp-0123456789abcdef")
			if err := os.WriteFile(filepath.Join(cache, filepath.FromSlash(item)), []byte("crash temporary"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := MkdirAll(cache, "blobs/sha256/verified", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOperationState(cache, prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, ".kama", "staging", targetOperation)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target staging still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, ".kama", "staging", otherOperation)); err != nil {
		t.Fatalf("other artifact staging was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, ".kama", "revisions", targetOperation+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target revision pin still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, ".kama", "revisions", otherOperation+".json")); err != nil {
		t.Fatalf("other artifact revision pin was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, ".kama", "revisions", "."+targetOperation+".json.tmp-0123456789abcdef")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target crash temporary still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, ".kama", "revisions", "."+otherOperation+".json.tmp-0123456789abcdef")); err != nil {
		t.Fatalf("other artifact crash temporary was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "blobs", "sha256", "verified")); err != nil {
		t.Fatalf("verified blob was removed: %v", err)
	}
}

func TestFilesystemProbe(t *testing.T) {
	t.Parallel()
	result, err := ProbeFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if result.CapacityBytes == 0 || result.FreeBytes == 0 || !result.Write || !result.Fsync ||
		!result.AtomicRename || !result.DirectoryRename || !result.Mmap || !result.Lock {
		t.Fatalf("ProbeFilesystem() = %#v", result)
	}
}

func TestStorageExhaustionClassificationAndPreflight(t *testing.T) {
	t.Parallel()
	if err := RequireFreeSpace(t.TempDir(), ^uint64(0)); FailureReason(err) != ReasonInsufficientStorage {
		t.Fatalf("RequireFreeSpace() = %v, want InsufficientStorage", err)
	}
	if err := classifyStorageError("write test data", unix.ENOSPC); FailureReason(err) != ReasonInsufficientStorage {
		t.Fatalf("classifyStorageError(ENOSPC) = %v, want InsufficientStorage", err)
	}
	if err := classifyStorageError("write test data", unix.EDQUOT); FailureReason(err) != ReasonInsufficientStorage {
		t.Fatalf("classifyStorageError(EDQUOT) = %v, want InsufficientStorage", err)
	}
}

func TestResultRoundTripAndRedaction(t *testing.T) {
	t.Parallel()
	secret := "hf_deadbeef"
	err := errors.New("GET https://example.test/file?Signature=abc Authorization: Bearer " + secret)
	result := NewFailureResult(ModeHub, err, secret)
	if strings.Contains(result.Message, secret) || strings.Contains(result.Message, "Signature=abc") {
		t.Fatalf("failure message was not redacted: %q", result.Message)
	}
	line, err := MarshalResultLine(result)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseResult(bytes.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Reason != ReasonIOFailure || parsed.Success {
		t.Fatalf("parsed result = %#v", parsed)
	}
	if !bytes.Contains(line, []byte(`"validationMillis":0`)) {
		t.Fatalf("result omits validationMillis: %s", line)
	}
	if _, err := MarshalResultLine(Result{BytesTransferred: -1}); err == nil {
		t.Fatal("MarshalResultLine accepted negative bytesTransferred")
	}
	if summary := MarshalSummary(result); len(summary) >= MaxSummaryBytes {
		t.Fatalf("summary length = %d", len(summary))
	}
	incompleteProbe := Result{SchemaVersion: SchemaVersion, Mode: ModeProbe, Success: true, Probe: &ProbeResult{
		Write: true, Fsync: true, AtomicRename: true, DirectoryRename: true, Mmap: true,
	}}
	if summary := MarshalSummary(incompleteProbe); !bytes.Contains(summary, []byte(`"probePassed":false`)) {
		t.Fatalf("incomplete probe summary reports success: %s", summary)
	}
}

func TestFailedValidationReportsElapsedTime(t *testing.T) {
	t.Parallel()
	payload, err := fixture.Read(fixture.ValidMinimal)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, make([]byte, 16<<20)...)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, testModelFile), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	result := Execute(context.Background(), Spec{
		SchemaVersion:  SchemaVersion,
		Mode:           ModeDirect,
		OperationID:    "timed-failed-validation",
		Format:         FormatGGUF,
		Entrypoint:     testModelFile,
		ExpectedSHA256: strings.Repeat("0", 64),
		PVC:            &PVCSpec{MountRoot: source, SelectedFiles: []string{testModelFile}},
	})
	if result.Success || result.Reason != ReasonChecksumMismatch || result.ValidationMillis <= 0 ||
		result.ValidationMillis > result.DurationMillis {
		t.Fatalf("failed validation result = %#v", result)
	}
}

func TestParseResultRejectsInvalidIdentityAndShape(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"unknown field":       `{"schemaVersion":1,"mode":"hub","success":false,"reason":"IOFailure","extra":true}`,
		"invalid mode":        `{"schemaVersion":1,"mode":"serve","success":false,"reason":"IOFailure"}`,
		"bad operation ID":    `{"schemaVersion":1,"mode":"hub","operationID":"../escape","success":false,"reason":"IOFailure"}`,
		"success with reason": `{"schemaVersion":1,"mode":"hub","success":true,"reason":"IOFailure"}`,
		"failure no reason":   `{"schemaVersion":1,"mode":"hub","success":false}`,
		"negative bytes":      `{"schemaVersion":1,"mode":"hub","success":false,"reason":"IOFailure","bytesTransferred":-1}`,
		"negative duration":   `{"schemaVersion":1,"mode":"hub","success":false,"reason":"IOFailure","durationMillis":-1}`,
		"negative validation": `{"schemaVersion":1,"mode":"hub","success":false,"reason":"IOFailure","validationMillis":-1}`,
		"validation too long": `{"schemaVersion":1,"mode":"hub","success":false,"reason":"IOFailure","validationMillis":2,"durationMillis":1}`,
		"multiple results":    `{"schemaVersion":1,"mode":"hub","success":false,"reason":"IOFailure"} {"schemaVersion":1}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseResult(strings.NewReader(payload)); err == nil {
				t.Fatal("ParseResult() error = nil, want rejection")
			}
		})
	}
}

func TestFileSHA256Fixture(t *testing.T) {
	t.Parallel()
	payload, _ := fixture.Read(fixture.ValidMinimal)
	digest := sha256.Sum256(payload)
	root := t.TempDir()
	writeFixture(t, root, testModelFile, fixture.ValidMinimal)
	manifest, err := BuildManifest(root, "", FormatGGUF, testModelFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Files[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("file digest = %s, want %x", manifest.Files[0].SHA256, digest)
	}
}

func writeFixture(t *testing.T, root, relative string, name fixture.Name) {
	t.Helper()
	payload, err := fixture.Read(name)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func FuzzValidateRelativePath(f *testing.F) {
	for _, seed := range []string{"model.gguf", "dir/model.gguf", "../escape", "/absolute", `dir\model.gguf`, ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_ = ValidateRelativePath(value)
	})
}

func FuzzInspectGGUF(f *testing.F) {
	for _, name := range fixture.Names() {
		payload, err := fixture.Read(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(payload)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = InspectGGUF(bytes.NewReader(payload))
	})
}
