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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	fixture "github.com/TannerBurns/kama/internal/testfixtures/gguf"
)

func TestPrivateHubImportResumeAndRecovery(t *testing.T) {
	t.Parallel()
	payload, err := fixture.Read(fixture.ValidMinimal)
	if err != nil {
		t.Fatal(err)
	}
	const (
		token  = "private-fixture-token"
		commit = "0123456789abcdef0123456789abcdef01234567"
		etag   = `"fixture-v1"`
	)
	var resolutions, downloads, rangeDownloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasPrefix(request.URL.Path, "/api/models/acme/tiny/revision/"):
			resolutions.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				testJSONSHAKey:  commit,
				testJSONListKey: []map[string]any{{testJSONFileKey: testModelFile, testJSONSizeKey: len(payload)}},
			})
		case request.URL.Path == "/acme/tiny/resolve/"+commit+"/model.gguf":
			writer.Header().Set("ETag", etag)
			writer.Header().Set("Accept-Ranges", "bytes")
			writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			if request.Method == http.MethodHead {
				return
			}
			downloads.Add(1)
			offset := 0
			if value := request.Header.Get("Range"); value != "" {
				_, _ = fmt.Sscanf(value, "bytes=%d-", &offset)
				rangeDownloads.Add(1)
				if offset >= len(payload) {
					writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				writer.Header().Set("Content-Length", strconv.Itoa(len(payload)-offset))
				writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(payload)-1, len(payload)))
				writer.WriteHeader(http.StatusPartialContent)
			}
			_, _ = writer.Write(payload[offset:])
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cache := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := hubTestSpec(cache, tokenFile, server.URL)
	staging, err := EnsureStaging(cache, spec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	partialLength := len(payload) / 2
	partialPath := filepath.Join(cache, filepath.FromSlash(staging), testModelFile)
	if err := os.WriteFile(partialPath, payload[:partialLength], 0o600); err != nil {
		t.Fatal(err)
	}
	partialDigest := sha256.Sum256(payload[:partialLength])
	resumePayload, _ := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"etag":          etag,
		testJSONSizeKey: len(payload),
		"partialSize":   partialLength,
		"partialSHA256": hex.EncodeToString(partialDigest[:]),
	})
	if err := os.WriteFile(partialPath+".resume.json", resumePayload, 0o600); err != nil {
		t.Fatal(err)
	}

	first := Execute(context.Background(), spec)
	if !first.Success || first.ResolvedRevision != commit || first.BytesTransferred != int64(len(payload)-partialLength) {
		t.Fatalf("first Hub result = %#v", first)
	}
	if downloads.Load() != 1 || rangeDownloads.Load() != 1 {
		t.Fatalf("downloads = %d, ranges = %d; want 1, 1", downloads.Load(), rangeDownloads.Load())
	}
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	second := Execute(context.Background(), spec)
	if !second.Success || !second.CacheHit || second.BytesTransferred != 0 || second.ResolvedRevision != commit {
		t.Fatalf("recovered Hub result = %#v", second)
	}
	if downloads.Load() != 1 || resolutions.Load() != 1 {
		t.Fatalf("recovery contacted Hub: resolutions=%d downloads=%d", resolutions.Load(), downloads.Load())
	}
}

func TestHubRetryUsesDurablyPinnedRevisionAfterMutableRevisionMoves(t *testing.T) {
	t.Parallel()
	payload, err := fixture.Read(fixture.ValidMinimal)
	if err != nil {
		t.Fatal(err)
	}
	const (
		firstCommit  = "0123456789abcdef0123456789abcdef01234567"
		secondCommit = "abcdef0123456789abcdef0123456789abcdef01"
		etag         = `"moving-revision-v1"`
	)
	partialLength := len(payload) / 2
	var mainResolutions, pinnedResolutions, downloads, secondCommitRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/api/models/acme/tiny/revision/"):
			revision := strings.TrimPrefix(request.URL.Path, "/api/models/acme/tiny/revision/")
			commit := firstCommit
			switch revision {
			case "main":
				if mainResolutions.Add(1) > 1 {
					commit = secondCommit
				}
			case firstCommit:
				pinnedResolutions.Add(1)
			case secondCommit:
				secondCommitRequests.Add(1)
				commit = secondCommit
			default:
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				testJSONSHAKey:  commit,
				testJSONListKey: []map[string]any{{testJSONFileKey: testModelFile, testJSONSizeKey: len(payload)}},
			})
		case request.URL.Path == "/acme/tiny/resolve/"+firstCommit+"/model.gguf":
			writer.Header().Set("ETag", etag)
			writer.Header().Set("Accept-Ranges", "bytes")
			if request.Method == http.MethodHead {
				writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				return
			}
			attempt := downloads.Add(1)
			offset := 0
			if value := request.Header.Get("Range"); value != "" {
				_, _ = fmt.Sscanf(value, "bytes=%d-", &offset)
				writer.Header().Set("Content-Length", strconv.Itoa(len(payload)-offset))
				writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(payload)-1, len(payload)))
				writer.WriteHeader(http.StatusPartialContent)
			} else {
				writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			}
			if attempt == 1 {
				_, _ = writer.Write(payload[:partialLength])
				return
			}
			_, _ = writer.Write(payload[offset:])
		case request.URL.Path == "/acme/tiny/resolve/"+secondCommit+"/model.gguf":
			secondCommitRequests.Add(1)
			writer.WriteHeader(http.StatusInternalServerError)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cache := t.TempDir()
	spec := hubTestSpec(cache, "", server.URL)
	first := Execute(context.Background(), spec)
	if first.Success {
		t.Fatalf("interrupted Hub transfer unexpectedly succeeded: %#v", first)
	}
	pin, found, err := readHubRevisionPin(cache, spec.OperationID, server.URL, *spec.Hub, spec.Entrypoint)
	if err != nil || !found || pin != firstCommit {
		t.Fatalf("revision pin = %q, found %v, err %v", pin, found, err)
	}

	second := Execute(context.Background(), spec)
	if !second.Success || second.ResolvedRevision != firstCommit ||
		second.BytesTransferred != int64(len(payload)-partialLength) {
		t.Fatalf("retried Hub result = %#v", second)
	}
	if mainResolutions.Load() != 1 || pinnedResolutions.Load() != 1 || downloads.Load() != 2 {
		t.Fatalf(
			"main resolutions=%d pinned resolutions=%d downloads=%d, want 1, 1, 2",
			mainResolutions.Load(), pinnedResolutions.Load(), downloads.Load(),
		)
	}
	if secondCommitRequests.Load() != 0 {
		t.Fatalf("retry contacted moved commit %q %d times", secondCommit, secondCommitRequests.Load())
	}
}

func TestHubRedirectDoesNotForwardAuthorization(t *testing.T) {
	t.Parallel()
	payload, _ := fixture.Read(fixture.ValidMinimal)
	const (
		token  = "origin-only-token"
		commit = "abcdef0123456789abcdef0123456789abcdef01"
	)
	var leaked atomic.Bool
	cdn := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		writer.Header().Set("ETag", `"cdn-v1"`)
		writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		if request.Method != http.MethodHead {
			_, _ = writer.Write(payload)
		}
	}))
	defer cdn.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/models/") {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				testJSONSHAKey:  commit,
				testJSONListKey: []map[string]any{{testJSONFileKey: testModelFile, testJSONSizeKey: len(payload)}},
			})
			return
		}
		http.Redirect(writer, request, cdn.URL+"/blob", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	result := Execute(context.Background(), hubTestSpec(t.TempDir(), tokenFile, origin.URL))
	if !result.Success {
		t.Fatalf("Hub redirect result = %#v", result)
	}
	if leaked.Load() {
		t.Fatal("Authorization header was forwarded to a cross-origin redirect")
	}
}

func TestValidateRepositoryMatchesHubRepoIDSyntax(t *testing.T) {
	t.Parallel()
	for _, repository := range []string{"gpt2", "owner/model", "_legacy", "owner/model_name"} {
		if err := validateRepository(repository); err != nil {
			t.Errorf("validateRepository(%q): %v", repository, err)
		}
	}
	for _, repository := range []string{
		"", ".model", "model-", "owner/model.git", "owner/model/extra", "owner/model--variant",
	} {
		if err := validateRepository(repository); err == nil {
			t.Errorf("validateRepository(%q) accepted an invalid repo_id", repository)
		}
	}
}

func TestHubURLsSupportUnnamespacedRepository(t *testing.T) {
	t.Parallel()
	const commit = "0123456789abcdef0123456789abcdef01234567"
	client, err := NewHubClient("https://hub.example/base", "", HTTPClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := client.resolutionURL("gpt2", "refs/pr/1"),
		"https://hub.example/base/api/models/gpt2/revision/refs%2Fpr%2F1"; got != want {
		t.Fatalf("resolution URL = %q, want %q", got, want)
	}
	if got, want := client.downloadURL("gpt2", commit, "models/model.gguf"),
		"https://hub.example/base/gpt2/resolve/"+commit+"/models/model.gguf"; got != want {
		t.Fatalf("download URL = %q, want %q", got, want)
	}
}

func hubTestSpec(cache, tokenFile, endpoint string) Spec {
	return Spec{
		SchemaVersion: SchemaVersion,
		Mode:          ModeHub,
		OperationID:   "hub-operation-fingerprint",
		Format:        FormatGGUF,
		Entrypoint:    testModelFile,
		CacheRoot:     cache,
		HubEndpoint:   endpoint,
		Hub: &HubSpec{
			Repository:    "acme/tiny",
			Revision:      "main",
			FileSelectors: []string{"*.gguf"},
			TokenFile:     tokenFile,
		},
		HTTP: HTTPClientOptions{AllowHTTP: true},
	}
}
