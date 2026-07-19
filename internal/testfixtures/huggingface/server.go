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

// Package huggingface implements a deterministic, Hub-compatible HTTP fixture.
// It is intentionally small: it supports the model metadata and resolve routes
// used by Kama's importer, including private repositories and byte ranges.
package huggingface

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	gguffixtures "github.com/TannerBurns/kama/internal/testfixtures/gguf"
)

const (
	// PublicRepository is the unauthenticated repository provided by DefaultConfig.
	PublicRepository = "kama/public-gguf"
	// PrivateRepository is the authenticated repository provided by DefaultConfig.
	PrivateRepository = "kama/private-gguf"
	// DefaultRevision is a stable full-length synthetic Hub commit.
	DefaultRevision = "0123456789abcdef0123456789abcdef01234567"
	// DefaultToken grants access to PrivateRepository.
	DefaultToken = "kama-test-token"
	// maxFaultPause prevents a forgotten test fault from pinning a fixture
	// handler indefinitely. Kind observes and interrupts the pause immediately.
	maxFaultPause = 30 * time.Second
	statusKey     = "status"
	errorKey      = "error"
)

// File is one immutable repository file.
type File struct {
	Data []byte
	ETag string
}

// Repository is one model repository at one immutable revision.
type Repository struct {
	Revision string
	Private  bool
	Token    string
	Files    map[string]File
}

// Config contains all repositories exposed by a Server.
type Config struct {
	Repositories map[string]Repository
}

// DefaultConfig returns public and private repositories containing the same
// project-owned, zero-tensor GGUF fixture.
func DefaultConfig() (Config, error) {
	payload, err := gguffixtures.Read(gguffixtures.ValidMinimal)
	if err != nil {
		return Config{}, fmt.Errorf("read GGUF fixture: %w", err)
	}
	digest := sha256.Sum256(payload)
	file := File{Data: payload, ETag: `"` + hex.EncodeToString(digest[:]) + `"`}
	return Config{Repositories: map[string]Repository{
		PublicRepository: {
			Revision: DefaultRevision,
			Files:    map[string]File{"model.gguf": file},
		},
		PrivateRepository: {
			Revision: DefaultRevision,
			Private:  true,
			Token:    DefaultToken,
			Files:    map[string]File{"model.gguf": file},
		},
	}}, nil
}

// Server implements the subset of the Hugging Face Hub HTTP API used by Kama.
type Server struct {
	config Config

	mu        sync.Mutex
	downloads map[string]int
	transfers map[string]*transferCounters
	fault     *pauseFault
}

type transferCounters struct {
	Attempts     int `json:"attempts"`
	Ranges       int `json:"ranges"`
	Pauses       int `json:"pauses"`
	ActivePauses int `json:"activePauses"`
	Completions  int `json:"completions"`
}

type pauseFault struct {
	Target          string
	PauseAfterBytes int64
	Remaining       int
	Release         chan struct{}
	Released        bool
}

type pauseFaultStatus struct {
	Target          string `json:"target"`
	PauseAfterBytes int64  `json:"pauseAfterBytes"`
	Remaining       int    `json:"remaining"`
}

type pauseFaultRequest struct {
	Target          string `json:"target"`
	PauseAfterBytes int64  `json:"pauseAfterBytes"`
}

// New validates config and returns a fixture server.
func New(config Config) (*Server, error) {
	if len(config.Repositories) == 0 {
		return nil, errors.New("at least one repository is required")
	}
	for name, repository := range config.Repositories {
		if name == "" || strings.Count(name, "/") != 1 {
			return nil, fmt.Errorf("repository name %q must be owner/name", name)
		}
		if len(repository.Revision) != 40 {
			return nil, fmt.Errorf("repository %q revision must be a full 40-character commit", name)
		}
		if repository.Private && repository.Token == "" {
			return nil, fmt.Errorf("private repository %q requires a token", name)
		}
		if len(repository.Files) == 0 {
			return nil, fmt.Errorf("repository %q requires at least one file", name)
		}
	}
	return &Server{
		config:    config,
		downloads: make(map[string]int),
		transfers: make(map[string]*transferCounters),
	}, nil
}

// Handler returns the fixture HTTP handler.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/health":
		writeJSON(writer, http.StatusOK, map[string]string{statusKey: "ok"})
	case request.Method == http.MethodGet && request.URL.Path == "/state":
		s.handleState(writer)
	case request.Method == http.MethodPut && request.URL.Path == "/state/reset":
		s.resetState()
		writeJSON(writer, http.StatusOK, map[string]string{statusKey: "reset"})
	case request.Method == http.MethodPut && request.URL.Path == "/state/fault":
		s.handleFault(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.EscapedPath(), "/api/models/"):
		s.handleMetadata(writer, request)
	case (request.Method == http.MethodHead || request.Method == http.MethodGet) &&
		strings.Contains(request.URL.EscapedPath(), "/resolve/"):
		s.handleResolve(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (s *Server) handleState(writer http.ResponseWriter) {
	s.mu.Lock()
	downloads := make(map[string]int, len(s.downloads))
	maps.Copy(downloads, s.downloads)
	transfers := make(map[string]transferCounters, len(s.transfers))
	for key, value := range s.transfers {
		transfers[key] = *value
	}
	var fault *pauseFaultStatus
	if s.fault != nil {
		fault = &pauseFaultStatus{
			Target:          s.fault.Target,
			PauseAfterBytes: s.fault.PauseAfterBytes,
			Remaining:       s.fault.Remaining,
		}
	}
	s.mu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{
		"downloads": downloads,
		"transfers": transfers,
		"fault":     fault,
	})
}

func (s *Server) handleFault(writer http.ResponseWriter, request *http.Request) {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096))
	decoder.DisallowUnknownFields()
	var requested pauseFaultRequest
	if err := decoder.Decode(&requested); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{errorKey: "invalid fault request"})
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{errorKey: "invalid fault request"})
		return
	}
	fileSize, found := s.targetSize(requested.Target)
	if !found || requested.PauseAfterBytes <= 0 || requested.PauseAfterBytes >= fileSize {
		writeJSON(writer, http.StatusBadRequest, map[string]string{errorKey: "fault target or byte boundary is invalid"})
		return
	}
	s.mu.Lock()
	s.releaseFaultLocked()
	s.fault = &pauseFault{
		Target:          requested.Target,
		PauseAfterBytes: requested.PauseAfterBytes,
		Remaining:       1,
		Release:         make(chan struct{}),
	}
	s.mu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]string{statusKey: "armed"})
}

func (s *Server) targetSize(target string) (int64, bool) {
	for repositoryName, repository := range s.config.Repositories {
		for filename, file := range repository.Files {
			if repositoryName+"/"+filename == target {
				return int64(len(file.Data)), true
			}
		}
	}
	return 0, false
}

func (s *Server) resetState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseFaultLocked()
	s.fault = nil
	s.downloads = make(map[string]int)
	s.transfers = make(map[string]*transferCounters)
}

func (s *Server) releaseFaultLocked() {
	if s.fault != nil && !s.fault.Released {
		close(s.fault.Release)
		s.fault.Released = true
	}
}

type sibling struct {
	Filename string `json:"rfilename"`
	Size     int64  `json:"size"`
}

func (s *Server) handleMetadata(writer http.ResponseWriter, request *http.Request) {
	escaped := strings.TrimPrefix(request.URL.EscapedPath(), "/api/models/")
	repositoryPart, revisionPart, found := strings.Cut(escaped, "/revision/")
	if !found {
		http.NotFound(writer, request)
		return
	}
	repositoryName, err := url.PathUnescape(repositoryPart)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{errorKey: "invalid repository path"})
		return
	}
	revision, err := url.PathUnescape(revisionPart)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{errorKey: "invalid revision"})
		return
	}
	repository, ok := s.authorizedRepository(writer, request, repositoryName)
	if !ok {
		return
	}
	if revision != repository.Revision && revision != "main" {
		writeJSON(writer, http.StatusNotFound, map[string]string{errorKey: "revision not found"})
		return
	}
	files := make([]sibling, 0, len(repository.Files))
	for path, file := range repository.Files {
		files = append(files, sibling{Filename: path, Size: int64(len(file.Data))})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"sha": repository.Revision, "siblings": files})
}

func (s *Server) handleResolve(writer http.ResponseWriter, request *http.Request) {
	escaped := strings.TrimPrefix(request.URL.EscapedPath(), "/")
	repositoryPart, remainder, found := strings.Cut(escaped, "/resolve/")
	if !found {
		http.NotFound(writer, request)
		return
	}
	revisionPart, filePart, found := strings.Cut(remainder, "/")
	if !found {
		http.NotFound(writer, request)
		return
	}
	repositoryName, err := url.PathUnescape(repositoryPart)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{errorKey: "invalid repository path"})
		return
	}
	revision, err := url.PathUnescape(revisionPart)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{errorKey: "invalid revision"})
		return
	}
	filename, err := url.PathUnescape(filePart)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{errorKey: "invalid filename"})
		return
	}
	repository, ok := s.authorizedRepository(writer, request, repositoryName)
	if !ok {
		return
	}
	if revision != repository.Revision {
		writeJSON(writer, http.StatusNotFound, map[string]string{errorKey: "revision not found"})
		return
	}
	file, found := repository.Files[filename]
	if !found {
		http.NotFound(writer, request)
		return
	}

	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("ETag", file.ETag)
	writer.Header().Set("Content-Type", "application/octet-stream")
	if request.Method == http.MethodHead {
		writer.Header().Set("Content-Length", strconv.Itoa(len(file.Data)))
		writer.WriteHeader(http.StatusOK)
		return
	}

	start, partial, rangeErr := rangeStart(request.Header.Get("Range"), int64(len(file.Data)))
	if rangeErr != nil {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(file.Data)))
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if partial && request.Header.Get("If-Range") != "" && request.Header.Get("If-Range") != file.ETag {
		start = 0
		partial = false
	}
	body := file.Data[start:]
	target := repositoryName + "/" + filename
	counters, fault := s.startTransfer(target, partial, int64(len(body)))
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if partial {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(file.Data)-1, len(file.Data)))
		writer.WriteHeader(http.StatusPartialContent)
	} else {
		writer.WriteHeader(http.StatusOK)
	}
	if fault != nil {
		boundary := int(fault.PauseAfterBytes)
		written, writeErr := writer.Write(body[:boundary])
		if writeErr != nil || written != boundary {
			return
		}
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		s.markPaused(counters, 1)
		pauseTimer := time.NewTimer(maxFaultPause)
		defer pauseTimer.Stop()
		select {
		case <-request.Context().Done():
			s.markPaused(counters, -1)
			return
		case <-fault.Release:
			s.markPaused(counters, -1)
			return
		case <-pauseTimer.C:
			s.markPaused(counters, -1)
			return
		}
	}
	written, writeErr := writer.Write(body)
	if writeErr != nil || written != len(body) || request.Context().Err() != nil {
		return
	}
	s.mu.Lock()
	s.downloads[target]++
	counters.Completions++
	s.mu.Unlock()
}

func (s *Server) startTransfer(target string, ranged bool, bodySize int64) (*transferCounters, *pauseFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counters := s.transfers[target]
	if counters == nil {
		counters = &transferCounters{}
		s.transfers[target] = counters
	}
	counters.Attempts++
	if ranged {
		counters.Ranges++
	}
	if s.fault == nil || s.fault.Target != target || s.fault.Remaining == 0 ||
		s.fault.PauseAfterBytes >= bodySize {
		return counters, nil
	}
	s.fault.Remaining--
	return counters, s.fault
}

func (s *Server) markPaused(counters *transferCounters, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if delta > 0 {
		counters.Pauses += delta
	}
	counters.ActivePauses += delta
}

func (s *Server) authorizedRepository(
	writer http.ResponseWriter,
	request *http.Request,
	name string,
) (Repository, bool) {
	repository, found := s.config.Repositories[name]
	if !found {
		http.NotFound(writer, request)
		return Repository{}, false
	}
	if repository.Private && request.Header.Get("Authorization") != "Bearer "+repository.Token {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{errorKey: "unauthorized"})
		return Repository{}, false
	}
	return repository, true
}

func rangeStart(header string, size int64) (int64, bool, error) {
	if header == "" {
		return 0, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, false, errors.New("unsupported range")
	}
	value := strings.TrimSuffix(strings.TrimPrefix(header, "bytes="), "-")
	start, err := strconv.ParseInt(value, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, false, errors.New("range is outside the file")
	}
	return start, true, nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
