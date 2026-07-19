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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const defaultHubEndpoint = "https://huggingface.co"

var (
	fullCommitPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
	repositoryPart    = regexp.MustCompile(`^[A-Za-z0-9_](?:[A-Za-z0-9._-]*[A-Za-z0-9_])?$`)
)

// ValidHubCommit reports whether value is a full immutable Hub commit ID.
func ValidHubCommit(value string) bool {
	return fullCommitPattern.MatchString(value)
}

// RemoteFile is one selected file reported by the Hub model API.
type RemoteFile struct {
	Path        string `json:"path"`
	Size        *int64 `json:"size,omitempty"`
	ETag        string `json:"-"`
	ResumeBytes int64  `json:"-"`
	preflighted bool
}

type resumeMetadata struct {
	SchemaVersion int    `json:"schemaVersion"`
	ETag          string `json:"etag"`
	Size          *int64 `json:"size,omitempty"`
	PartialSize   int64  `json:"partialSize,omitempty"`
	PartialSHA256 string `json:"partialSHA256,omitempty"`
}

// hubRevisionPin records the immutable commit selected for one operation
// before any file preflight or transfer. A retry of a mutable revision must
// resolve and download through this commit instead of observing a moved tag.
type hubRevisionPin struct {
	SchemaVersion     int      `json:"schemaVersion"`
	HubEndpoint       string   `json:"hubEndpoint"`
	Repository        string   `json:"repository"`
	RequestedRevision string   `json:"requestedRevision"`
	ResolvedRevision  string   `json:"resolvedRevision"`
	Entrypoint        string   `json:"entrypoint"`
	FileSelectors     []string `json:"fileSelectors"`
}

// HubResolution is an immutable commit and its selected file set.
type HubResolution struct {
	Commit string       `json:"commit"`
	Files  []RemoteFile `json:"files"`
}

// HubClient implements the minimal public Hugging Face HTTP contract used by
// the importer. It does not expose signed redirect URLs or response bodies.
type HubClient struct {
	endpoint         *url.URL
	client           *http.Client
	token            string
	userAgent        string
	maxResponseBytes int64
}

func readHubRevisionPin(cacheRoot, operationID, endpoint string, spec HubSpec, entrypoint string) (string, bool, error) {
	if err := validateOperationID(operationID); err != nil {
		return "", false, err
	}
	var pin hubRevisionPin
	found, err := readCanonicalMetadata(
		cacheRoot,
		path.Join(".kama/revisions", operationID+".json"),
		MaxSpecBytes,
		&pin,
	)
	if err != nil || !found {
		return "", found, err
	}
	valid := pin.SchemaVersion == SchemaVersion && ValidHubCommit(pin.ResolvedRevision) &&
		pin.HubEndpoint == endpoint && pin.Repository == spec.Repository &&
		pin.RequestedRevision == spec.Revision && pin.Entrypoint == entrypoint &&
		slices.Equal(pin.FileSelectors, spec.FileSelectors)
	if !valid {
		return "", false, failure(
			ReasonPublicationConflict,
			"validate pinned Hub revision",
			errors.New("stored revision pin describes a different or invalid Hub source"),
		)
	}
	return pin.ResolvedRevision, true, nil
}

func writeHubRevisionPin(cacheRoot, operationID, endpoint string, spec HubSpec, entrypoint, commit string) error {
	if err := validateOperationID(operationID); err != nil {
		return err
	}
	if !ValidHubCommit(commit) {
		return failure(ReasonSourceUnavailable, "pin Hub revision", errors.New("resolved revision is not immutable"))
	}
	payload, err := json.Marshal(hubRevisionPin{
		SchemaVersion:     SchemaVersion,
		HubEndpoint:       endpoint,
		Repository:        spec.Repository,
		RequestedRevision: spec.Revision,
		ResolvedRevision:  commit,
		Entrypoint:        entrypoint,
		FileSelectors:     slices.Clone(spec.FileSelectors),
	})
	if err != nil {
		return failure(ReasonIOFailure, "encode pinned Hub revision", err)
	}
	return writeAtomic(cacheRoot, path.Join(".kama/revisions", operationID+".json"), payload, 0o640)
}

// NewHubClient validates endpoint policy and configures credential-safe
// redirect handling. An HTTP endpoint is accepted only for explicit test or
// operator configuration with AllowHTTP=true.
func NewHubClient(endpoint, token string, options HTTPClientOptions) (*HubClient, error) {
	if endpoint == "" {
		endpoint = defaultHubEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, failure(ReasonInvalidSpec, "validate Hub endpoint", errors.New("hub endpoint is not a valid URL"))
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, failure(ReasonInvalidSpec, "validate Hub endpoint", errors.New("hub endpoint must contain only scheme, host, and optional path"))
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !options.AllowHTTP) {
		return nil, failure(ReasonInvalidSpec, "validate Hub endpoint", errors.New("hub endpoint must use HTTPS"))
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	maxRedirects := options.MaxRedirects
	if maxRedirects == 0 {
		maxRedirects = 10
	}
	if maxRedirects < 0 || maxRedirects > 20 {
		return nil, failure(ReasonInvalidSpec, "validate HTTP options", errors.New("maxRedirects must be between 0 and 20"))
	}
	requestTimeout := options.Timeout
	if requestTimeout == 0 {
		requestTimeout = 6 * time.Hour
	}
	maxResponseBytes := options.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = 16 << 20
	}
	if maxResponseBytes < 1024 || maxResponseBytes > 64<<20 {
		return nil, failure(ReasonInvalidSpec, "validate HTTP options", errors.New("maxResponseBytes must be between 1 KiB and 64 MiB"))
	}
	userAgent := options.UserAgent
	if userAgent == "" {
		userAgent = "kama-importer"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 45 * time.Second
	client := &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
	}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many Hub redirects")
		}
		if request.URL.Scheme != "https" && (request.URL.Scheme != "http" || !options.AllowHTTP) {
			return errors.New("hub redirect downgraded transport security")
		}
		if len(via) > 0 && !sameOrigin(request.URL, via[0].URL) {
			request.Header.Del("Authorization")
		}
		return nil
	}
	return &HubClient{
		endpoint:         parsed,
		client:           client,
		token:            strings.TrimSpace(token),
		userAgent:        userAgent,
		maxResponseBytes: maxResponseBytes,
	}, nil
}

// ReadTokenFile reads a Secret volume file without ever returning its contents
// in an error.
func ReadTokenFile(filename string) (string, error) {
	if filename == "" {
		return "", nil
	}
	file, err := os.Open(filename)
	if err != nil {
		return "", failure(ReasonUnauthorized, "read Hub token file", errors.New("token file is unavailable"))
	}
	defer func() { _ = file.Close() }()
	payload, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return "", failure(ReasonUnauthorized, "read Hub token file", errors.New("token file could not be read"))
	}
	token := strings.TrimSpace(string(payload))
	if token == "" {
		return "", failure(ReasonUnauthorized, "read Hub token file", errors.New("token file is empty"))
	}
	return token, nil
}

// Resolve calls the model API, pins revision to a full commit, applies POSIX
// selectors, and expands a standard shard entrypoint to the complete set.
//
//nolint:gocyclo // Resolution validates each remote trust boundary in one flow.
func (client *HubClient) Resolve(ctx context.Context, repository, revision, entrypoint string, selectors []string) (HubResolution, error) {
	if err := validateRepository(repository); err != nil {
		return HubResolution{}, err
	}
	if revision == "" || len(revision) > 255 || strings.ContainsRune(revision, '\x00') {
		return HubResolution{}, failure(ReasonInvalidSpec, "validate Hub revision", errors.New("revision must contain 1-255 characters"))
	}
	if err := ValidateRelativePath(entrypoint); err != nil {
		return HubResolution{}, failure(ReasonUnsafePath, "validate Hub entrypoint", err)
	}
	if len(selectors) == 0 {
		return HubResolution{}, failure(ReasonInvalidSpec, "validate Hub selectors", errors.New("at least one file selector is required"))
	}
	if len(selectors) > MaxSelectedFiles {
		return HubResolution{}, failure(ReasonInvalidSpec, "validate Hub selectors", fmt.Errorf("selector count exceeds %d", MaxSelectedFiles))
	}
	for _, selector := range selectors {
		if err := validateSelector(selector); err != nil {
			return HubResolution{}, err
		}
	}

	endpoint := client.resolutionURL(repository, revision)
	request, err := client.newRequest(ctx, http.MethodGet, endpoint)
	if err != nil {
		return HubResolution{}, failure(ReasonInvalidSpec, "create Hub resolution request", err)
	}
	response, err := client.client.Do(request)
	if err != nil {
		return HubResolution{}, failure(ReasonSourceUnavailable, "resolve Hub revision", err)
	}
	defer func() { _ = response.Body.Close() }()
	if err := responseStatusError("resolve Hub revision", response.StatusCode); err != nil {
		return HubResolution{}, err
	}
	type sibling struct {
		Filename string `json:"rfilename"`
		Size     *int64 `json:"size"`
		LFS      *struct {
			Size int64 `json:"size"`
		} `json:"lfs"`
	}
	var model struct {
		SHA      string    `json:"sha"`
		Siblings []sibling `json:"siblings"`
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, client.maxResponseBytes+1))
	if err != nil || int64(len(payload)) > client.maxResponseBytes {
		return HubResolution{}, failure(ReasonSourceUnavailable, "decode Hub model metadata", errors.New("model metadata response exceeds the configured limit"))
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&model); err != nil {
		return HubResolution{}, failure(ReasonSourceUnavailable, "decode Hub model metadata", errors.New("model metadata response is invalid"))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return HubResolution{}, failure(ReasonSourceUnavailable, "decode Hub model metadata", errors.New("model metadata response contains trailing data"))
	}
	if !ValidHubCommit(model.SHA) {
		return HubResolution{}, failure(ReasonSourceUnavailable, "resolve Hub revision", errors.New("model API did not return a full immutable commit"))
	}
	if ValidHubCommit(revision) && model.SHA != revision {
		return HubResolution{}, failure(ReasonSourceUnavailable, "resolve Hub revision", errors.New("model API returned a different commit than the pinned revision"))
	}
	available := make(map[string]RemoteFile, len(model.Siblings))
	for _, item := range model.Siblings {
		if err := ValidateRelativePath(item.Filename); err != nil {
			return HubResolution{}, failure(ReasonUnsafePath, "validate Hub file metadata", errors.New("model API returned an unsafe filename"))
		}
		size := item.Size
		if size == nil && item.LFS != nil {
			value := item.LFS.Size
			size = &value
		}
		if size != nil && *size < 0 {
			return HubResolution{}, failure(ReasonSourceUnavailable, "validate Hub file metadata", errors.New("model API returned a negative file size"))
		}
		available[item.Filename] = RemoteFile{Path: item.Filename, Size: size}
	}

	selected := make(map[string]struct{})
	for name := range available {
		for _, selector := range selectors {
			matched, matchErr := path.Match(selector, name)
			if matchErr != nil {
				return HubResolution{}, failure(ReasonInvalidSpec, "match Hub selector", errors.New("file selector syntax is invalid"))
			}
			if matched {
				selected[name] = struct{}{}
				break
			}
		}
	}
	if _, found := selected[entrypoint]; !found {
		return HubResolution{}, failure(ReasonInvalidSpec, "select Hub files", errors.New("entrypoint is not matched by a file selector"))
	}
	shards, err := shardNames(entrypoint)
	if err != nil {
		return HubResolution{}, err
	}
	for _, name := range shards {
		if _, found := available[name]; !found {
			return HubResolution{}, failure(ReasonMissingShard, "resolve Hub shards", fmt.Errorf("required shard %q is absent", name))
		}
		selected[name] = struct{}{}
	}
	if len(selected) > MaxSelectedFiles {
		return HubResolution{}, failure(ReasonInvalidSpec, "select Hub files", fmt.Errorf("selected %d files, maximum is %d", len(selected), MaxSelectedFiles))
	}
	result := HubResolution{Commit: model.SHA, Files: make([]RemoteFile, 0, len(selected))}
	for name := range selected {
		if !strings.EqualFold(path.Ext(name), ".gguf") {
			return HubResolution{}, failure(ReasonInvalidGGUF, "select Hub files", fmt.Errorf("selected file %q is not GGUF", name))
		}
		result.Files = append(result.Files, available[name])
	}
	slices.SortFunc(result.Files, func(left, right RemoteFile) int { return strings.Compare(left.Path, right.Path) })
	return result, nil
}

// Preflight resolves an authoritative size and ETag and validates any durable
// partial checkpoint before aggregate free-space accounting.
func (client *HubClient) Preflight(
	ctx context.Context,
	repository, commit string,
	remote RemoteFile,
	destinationRoot, destinationPath string,
) (RemoteFile, error) {
	if err := validateRepository(repository); err != nil {
		return RemoteFile{}, err
	}
	if !ValidHubCommit(commit) {
		return RemoteFile{}, failure(ReasonInvalidSpec, "validate Hub commit", errors.New("commit is not a full immutable hexadecimal ID"))
	}
	if err := ValidateRelativePath(remote.Path); err != nil {
		return RemoteFile{}, failure(ReasonUnsafePath, "validate Hub file path", err)
	}
	if err := ValidateRelativePath(destinationPath); err != nil {
		return RemoteFile{}, failure(ReasonUnsafePath, "validate Hub destination path", err)
	}
	downloadURL := client.downloadURL(repository, commit, remote.Path)
	headRequest, err := client.newRequest(ctx, http.MethodHead, downloadURL)
	if err != nil {
		return RemoteFile{}, failure(ReasonInvalidSpec, "create Hub preflight request", err)
	}
	headResponse, headErr := client.client.Do(headRequest)
	if headErr != nil {
		return RemoteFile{}, failure(ReasonSourceUnavailable, "preflight Hub file", headErr)
	}
	_ = headResponse.Body.Close()
	if headResponse.StatusCode != http.StatusMethodNotAllowed && headResponse.StatusCode != http.StatusNotImplemented {
		if err := responseStatusError("inspect Hub file", headResponse.StatusCode); err != nil {
			return RemoteFile{}, err
		}
		remote.ETag = strings.TrimSpace(headResponse.Header.Get("ETag"))
		if headResponse.ContentLength >= 0 {
			size := headResponse.ContentLength
			if remote.Size != nil && *remote.Size != size {
				return RemoteFile{}, failure(ReasonSourceUnavailable, "inspect Hub file", errors.New("hub API and file endpoint report different sizes"))
			}
			remote.Size = &size
		}
	}
	if remote.Size == nil {
		return RemoteFile{}, failure(ReasonSourceUnavailable, "preflight Hub file", errors.New("file endpoint did not report a content size"))
	}
	checkpoint, err := validateResumeCheckpoint(
		destinationRoot,
		destinationPath,
		destinationPath+".resume.json",
		remote.ETag,
		remote.Size,
	)
	if err != nil {
		return RemoteFile{}, err
	}
	remote.ResumeBytes = checkpoint
	remote.preflighted = true
	return remote, nil
}

// Download resumes a file only from a byte checkpoint whose ETag, size, and
// SHA-256 were durably recorded. It returns bytes transferred in this call.
//
//nolint:gocyclo // Download keeps resume state transitions adjacent and auditable.
func (client *HubClient) Download(ctx context.Context, repository, commit string, remote RemoteFile, destinationRoot, destinationPath string) (int64, error) {
	var err error
	if !remote.preflighted {
		remote, err = client.Preflight(ctx, repository, commit, remote, destinationRoot, destinationPath)
		if err != nil {
			return 0, err
		}
	}
	parent := path.Dir(destinationPath)
	if parent != "." {
		if err := MkdirAll(destinationRoot, parent, 0o750); err != nil {
			return 0, err
		}
	}
	resumePath := destinationPath + ".resume.json"
	partialSize, err := validateResumeCheckpoint(
		destinationRoot,
		destinationPath,
		resumePath,
		remote.ETag,
		remote.Size,
	)
	if err != nil {
		return 0, err
	}
	if partialSize == *remote.Size {
		return 0, nil
	}
	if partialSize == 0 {
		if err := writeResumeCheckpoint(destinationRoot, resumePath, remote.ETag, remote.Size, 0, ""); err != nil {
			return 0, err
		}
	}
	downloadURL := client.downloadURL(repository, commit, remote.Path)
	request, err := client.newRequest(ctx, http.MethodGet, downloadURL)
	if err != nil {
		return 0, failure(ReasonInvalidSpec, "create Hub download request", err)
	}
	if partialSize > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", partialSize))
		request.Header.Set("If-Range", remote.ETag)
	}
	response, err := client.client.Do(request)
	if err != nil {
		return 0, failure(ReasonSourceUnavailable, "download Hub file", err)
	}
	defer func() { _ = response.Body.Close() }()
	if err := responseStatusError("download Hub file", response.StatusCode); err != nil {
		return 0, err
	}
	if responseETag := strings.TrimSpace(response.Header.Get("ETag")); remote.ETag != "" &&
		responseETag != "" && responseETag != remote.ETag {
		return 0, failure(ReasonSourceUnavailable, "download Hub file", errors.New("server changed ETag after preflight"))
	}
	appendMode := false
	if partialSize > 0 && response.StatusCode == http.StatusPartialContent {
		end, _, valid := parseContentRange(response.Header.Get("Content-Range"), partialSize, remote.Size)
		if !valid {
			return 0, failure(ReasonSourceUnavailable, "resume Hub file", errors.New("server returned an invalid Content-Range"))
		}
		if response.ContentLength >= 0 && response.ContentLength != end-partialSize+1 {
			return 0, failure(ReasonSourceUnavailable, "resume Hub file", errors.New("Content-Length does not match Content-Range"))
		}
		appendMode = true
	} else if response.StatusCode != http.StatusOK {
		return 0, failure(ReasonSourceUnavailable, "download Hub file", fmt.Errorf("unexpected HTTP status %d", response.StatusCode))
	} else if response.ContentLength >= 0 {
		if *remote.Size != response.ContentLength {
			return 0, failure(ReasonSourceUnavailable, "download Hub file", errors.New("Content-Length differs from preflight size"))
		}
		partialSize = 0
	}
	destination, err := OpenWritableRegular(destinationRoot, destinationPath, !appendMode)
	if err != nil {
		return 0, err
	}
	if appendMode {
		if _, err := destination.Seek(partialSize, io.SeekStart); err != nil {
			_ = destination.Close()
			return 0, failure(ReasonIOFailure, "seek partial Hub file", err)
		}
	} else {
		partialSize = 0
	}
	hasher := sha256.New()
	if partialSize > 0 {
		if _, err := destination.Seek(0, io.SeekStart); err != nil {
			_ = destination.Close()
			return 0, failure(ReasonIOFailure, "seek partial Hub file", err)
		}
		if _, err := io.CopyN(hasher, destination, partialSize); err != nil {
			_ = destination.Close()
			return 0, failure(ReasonIOFailure, "hash partial Hub file", err)
		}
		if _, err := destination.Seek(partialSize, io.SeekStart); err != nil {
			_ = destination.Close()
			return 0, failure(ReasonIOFailure, "seek partial Hub file", err)
		}
	}
	buffer := make([]byte, 1<<20)
	written := int64(0)
	checkpointAt := partialSize
	for {
		remaining := *remote.Size - partialSize - written
		if remaining == 0 {
			var extra [1]byte
			read, readErr := response.Body.Read(extra[:])
			if read > 0 {
				checkpointErr := persistDownloadCheckpoint(
					destination, destinationRoot, resumePath, remote, partialSize+written, hasher,
				)
				closeErr := destination.Close()
				return written, failure(ReasonSourceUnavailable, "validate Hub transfer", errors.Join(checkpointErr, closeErr, errors.New("response exceeded the preflight size")))
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				checkpointErr := persistDownloadCheckpoint(
					destination, destinationRoot, resumePath, remote, partialSize+written, hasher,
				)
				closeErr := destination.Close()
				return written, failure(ReasonSourceUnavailable, "download Hub file", errors.Join(readErr, checkpointErr, closeErr))
			}
			continue
		}
		readBuffer := buffer
		if int64(len(readBuffer)) > remaining {
			readBuffer = readBuffer[:remaining]
		}
		read, readErr := response.Body.Read(readBuffer)
		if read > 0 {
			stored, writeErr := destination.Write(buffer[:read])
			if stored > 0 {
				_, _ = hasher.Write(buffer[:stored])
				written += int64(stored)
			}
			if writeErr != nil || stored != read {
				checkpointErr := persistDownloadCheckpoint(
					destination, destinationRoot, resumePath, remote, partialSize+written, hasher,
				)
				closeErr := destination.Close()
				return written, classifyStorageError("store Hub file", errors.Join(writeErr, checkpointErr, closeErr, io.ErrShortWrite))
			}
			if partialSize+written-checkpointAt >= 8<<20 {
				if err := persistDownloadCheckpoint(destination, destinationRoot, resumePath, remote, partialSize+written, hasher); err != nil {
					_ = destination.Close()
					return written, err
				}
				checkpointAt = partialSize + written
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		checkpointErr := persistDownloadCheckpoint(
			destination, destinationRoot, resumePath, remote, partialSize+written, hasher,
		)
		closeErr := destination.Close()
		if checkpointErr != nil || closeErr != nil {
			return written, classifyStorageError("store interrupted Hub file", errors.Join(checkpointErr, closeErr))
		}
		return written, failure(ReasonSourceUnavailable, "download Hub file", readErr)
	}
	if err := persistDownloadCheckpoint(destination, destinationRoot, resumePath, remote, partialSize+written, hasher); err != nil {
		_ = destination.Close()
		return written, err
	}
	info, statErr := destination.Stat()
	closeErr := destination.Close()
	if statErr != nil || closeErr != nil {
		return written, classifyStorageError("finish Hub file", errors.Join(statErr, closeErr))
	}
	if info.Size() != *remote.Size {
		return written, failure(ReasonSourceUnavailable, "validate Hub transfer", fmt.Errorf("downloaded %d of %d bytes", info.Size(), *remote.Size))
	}
	return written, nil
}

func persistDownloadCheckpoint(
	destination *os.File,
	destinationRoot, resumePath string,
	remote RemoteFile,
	partialSize int64,
	hasher io.Writer,
) error {
	if err := destination.Sync(); err != nil {
		return classifyStorageError("fsync partial Hub file", err)
	}
	digest, ok := hasher.(interface{ Sum([]byte) []byte })
	if !ok {
		return failure(ReasonIOFailure, "checkpoint Hub file", errors.New("SHA-256 state is unavailable"))
	}
	return writeResumeCheckpoint(
		destinationRoot,
		resumePath,
		remote.ETag,
		remote.Size,
		partialSize,
		hex.EncodeToString(digest.Sum(nil)),
	)
}

func writeResumeCheckpoint(root, resumePath, etag string, size *int64, partialSize int64, digest string) error {
	metadata, err := json.Marshal(resumeMetadata{
		SchemaVersion: SchemaVersion,
		ETag:          etag,
		Size:          size,
		PartialSize:   partialSize,
		PartialSHA256: digest,
	})
	if err != nil {
		return failure(ReasonIOFailure, "encode Hub resume checkpoint", err)
	}
	return writeAtomic(root, resumePath, metadata, 0o600)
}

func validateResumeCheckpoint(root, destinationPath, resumePath, etag string, size *int64) (int64, error) {
	file, err := OpenRegular(root, destinationPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return 0, failure(ReasonIOFailure, "inspect partial Hub file", statErr)
	}
	stored, metadataValid, metadataErr := readResumeCheckpoint(root, resumePath)
	if metadataErr != nil {
		_ = file.Close()
		return 0, metadataErr
	}
	valid := metadataValid && stored.SchemaVersion == SchemaVersion &&
		stored.ETag != "" && etag != "" && stored.ETag == etag && stored.Size != nil && size != nil &&
		*stored.Size == *size && stored.PartialSize > 0 && stored.PartialSize <= *size &&
		info.Size() >= stored.PartialSize && validSHA256(stored.PartialSHA256)
	if valid {
		hasher := sha256.New()
		written, hashErr := io.CopyN(hasher, file, stored.PartialSize)
		valid = hashErr == nil && written == stored.PartialSize &&
			hex.EncodeToString(hasher.Sum(nil)) == stored.PartialSHA256
	}
	closeErr := file.Close()
	if closeErr != nil {
		return 0, failure(ReasonIOFailure, "close partial Hub file", closeErr)
	}
	checkpoint := int64(0)
	if valid {
		checkpoint = stored.PartialSize
	}
	if info.Size() != checkpoint {
		writable, openErr := OpenWritableRegular(root, destinationPath, false)
		if openErr != nil {
			return 0, openErr
		}
		truncateErr := writable.Truncate(checkpoint)
		syncErr := writable.Sync()
		closeErr := writable.Close()
		if truncateErr != nil || syncErr != nil || closeErr != nil {
			return 0, classifyStorageError("reset partial Hub file", errors.Join(truncateErr, syncErr, closeErr))
		}
	}
	return checkpoint, nil
}

func readResumeCheckpoint(root, resumePath string) (resumeMetadata, bool, error) {
	file, err := OpenRegular(root, resumePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return resumeMetadata{}, false, nil
		}
		return resumeMetadata{}, false, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	var metadata resumeMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return resumeMetadata{}, false, nil
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return resumeMetadata{}, false, nil
	}
	return metadata, true, nil
}

func (client *HubClient) resolutionURL(repository, revision string) string {
	return strings.TrimRight(client.endpoint.String(), "/") + "/api/models/" +
		escapeRepository(repository) + "/revision/" + url.PathEscape(revision)
}

func (client *HubClient) downloadURL(repository, commit, filename string) string {
	fileParts := strings.Split(filename, "/")
	for index := range fileParts {
		fileParts[index] = url.PathEscape(fileParts[index])
	}
	return strings.TrimRight(client.endpoint.String(), "/") + "/" +
		escapeRepository(repository) + "/resolve/" + url.PathEscape(commit) + "/" + strings.Join(fileParts, "/")
}

func escapeRepository(repository string) string {
	parts := strings.Split(repository, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func (client *HubClient) newRequest(ctx context.Context, method, endpoint string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", client.userAgent)
	if client.token != "" {
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
	return request, nil
}

func validateRepository(repository string) error {
	parts := strings.Split(repository, "/")
	invalid := len(repository) == 0 || len(repository) > 96 || len(parts) > 2 ||
		strings.Contains(repository, "--") || strings.Contains(repository, "..") ||
		strings.HasSuffix(strings.ToLower(repository), ".git")
	for _, part := range parts {
		if !repositoryPart.MatchString(part) {
			invalid = true
		}
	}
	if invalid {
		return failure(ReasonInvalidSpec, "validate Hub repository", errors.New("repository must be repo_name or namespace/repo_name using Hugging Face repo_id syntax"))
	}
	return nil
}

func validateSelector(selector string) error {
	if selector == "" || strings.HasPrefix(selector, "/") || strings.Contains(selector, "\\") || strings.ContainsRune(selector, '\x00') {
		return failure(ReasonUnsafePath, "validate Hub selector", errors.New("selector must be a relative POSIX pattern"))
	}
	for component := range strings.SplitSeq(selector, "/") {
		if component == "" || component == "." || component == ".." {
			return failure(ReasonUnsafePath, "validate Hub selector", errors.New("selector contains an unsafe path component"))
		}
	}
	if _, err := path.Match(selector, "probe"); err != nil {
		return failure(ReasonInvalidSpec, "validate Hub selector", errors.New("selector syntax is invalid"))
	}
	return nil
}

func responseStatusError(operation string, status int) error {
	switch status {
	case http.StatusOK, http.StatusPartialContent:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return failure(ReasonUnauthorized, operation, fmt.Errorf("hub returned HTTP status %d", status))
	case http.StatusNotFound:
		return failure(ReasonSourceUnavailable, operation, errors.New("hub source was not found"))
	default:
		return failure(ReasonSourceUnavailable, operation, fmt.Errorf("hub returned HTTP status %d", status))
	}
}

func parseContentRange(header string, offset int64, size *int64) (end, total int64, valid bool) {
	if !strings.HasPrefix(header, "bytes ") {
		return 0, 0, false
	}
	value := strings.TrimPrefix(header, "bytes ")
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return 0, 0, false
	}
	rangeParts := strings.Split(parts[0], "-")
	if len(rangeParts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(rangeParts[0], 10, 64)
	if err != nil || start != offset {
		return 0, 0, false
	}
	end, err = strconv.ParseInt(rangeParts[1], 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	total = -1
	if parts[1] != "*" {
		total, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || total <= end || (size != nil && total != *size) {
			return 0, 0, false
		}
	}
	return end, total, true
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}
