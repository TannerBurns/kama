// Copyright 2026 Kama Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const fillBlockSize = 64 << 10

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: m1-functional-helper <verify-tree|ready|fill> [flags]")
	}

	var err error
	switch os.Args[1] {
	case "verify-tree":
		err = verifyTree(os.Args[2:])
	case "ready":
		err = ready(os.Args[2:])
	case "fill":
		err = fill(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func verifyTree(args []string) (returnedErr error) {
	flags := flag.NewFlagSet("verify-tree", flag.ContinueOnError)
	root := flags.String("root", "", "root of the immutable publication")
	expectedFile := flags.String("expected-file", "", "relative GGUF path")
	expectedSHA := flags.String("expected-sha256", "", "expected lowercase SHA-256")
	readyFile := flags.String("ready-file", "", "readiness marker to create after verification")
	hold := flags.Duration("hold", 0, "time to remain alive after verification")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *root == "" || *expectedFile == "" || len(*expectedSHA) != sha256.Size*2 {
		return errors.New("root, expected-file, and a 64-character expected-sha256 are required")
	}
	if filepath.IsAbs(*expectedFile) || strings.Contains(filepath.ToSlash(*expectedFile), "../") {
		return errors.New("expected-file must be a safe relative path")
	}

	expectedPath := filepath.Join(*root, filepath.FromSlash(*expectedFile))
	mapped, closeMapped, err := mmapRegular(expectedPath)
	if err != nil {
		return fmt.Errorf("mmap expected file: %w", err)
	}
	defer func() { returnedErr = errors.Join(returnedErr, closeMapped()) }()
	fileDigest := fmt.Sprintf("%x", sha256.Sum256(mapped))
	if fileDigest != strings.ToLower(*expectedSHA) {
		return fmt.Errorf("file digest = %s, want %s", fileDigest, strings.ToLower(*expectedSHA))
	}
	treeDigest, files, err := digestTree(*root)
	if err != nil {
		return err
	}
	if files == 0 {
		return errors.New("publication tree contains no regular files")
	}

	if _, err := fmt.Fprintf(os.Stdout, "M1_FILE_SHA256=%s\n", fileDigest); err != nil {
		return fmt.Errorf("write file digest: %w", err)
	}
	if _, err := fmt.Fprintf(os.Stdout, "M1_TREE_DIGEST=%s\n", treeDigest); err != nil {
		return fmt.Errorf("write tree digest: %w", err)
	}
	if _, err := fmt.Fprintf(os.Stdout, "M1_MMAP_HELD=true files=%d\n", files); err != nil {
		return fmt.Errorf("write mmap result: %w", err)
	}
	if *readyFile != "" {
		if err := os.WriteFile(*readyFile, []byte("ready\n"), 0o600); err != nil {
			return fmt.Errorf("write readiness marker: %w", err)
		}
	}
	if *hold > 0 {
		time.Sleep(*hold)
	}
	return nil
}

func ready(args []string) error {
	flags := flag.NewFlagSet("ready", flag.ContinueOnError)
	path := flags.String("file", "", "readiness marker")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("file is required")
	}
	payload, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	if string(payload) != "ready\n" {
		return errors.New("readiness marker has unexpected content")
	}
	return nil
}

func fill(args []string) error {
	flags := flag.NewFlagSet("fill", flag.ContinueOnError)
	root := flags.String("root", "", "filesystem root to exhaust")
	releaseBytes := flags.Int64("release-bytes", 512<<10, "bytes to release after observing ENOSPC")
	maxFreeBytes := flags.Uint64("max-free-bytes", 900<<10, "maximum acceptable free space after release")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *root == "" || *releaseBytes < fillBlockSize || *maxFreeBytes == 0 {
		return errors.New("root, release-bytes >= 65536, and max-free-bytes > 0 are required")
	}
	if err := os.MkdirAll(*root, 0o750); err != nil {
		return fmt.Errorf("prepare fill root: %w", err)
	}
	fillPath := filepath.Join(*root, ".m1-functional-fill")
	file, err := os.OpenFile(fillPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create fill file: %w", err)
	}
	defer func() { _ = file.Close() }()

	block := make([]byte, fillBlockSize)
	var written int64
	var fullErr error
	for {
		count, writeErr := file.Write(block)
		written += int64(count)
		if writeErr != nil {
			fullErr = writeErr
			break
		}
		if count != len(block) {
			fullErr = syscall.ENOSPC
			break
		}
	}
	if !errors.Is(fullErr, syscall.ENOSPC) {
		return fmt.Errorf("fill stopped without ENOSPC after %d bytes: %w", written, fullErr)
	}
	if written <= *releaseBytes {
		return fmt.Errorf("fill wrote only %d bytes; cannot release %d", written, *releaseBytes)
	}
	if err := file.Truncate(written - *releaseBytes); err != nil {
		return fmt.Errorf("release bounded free space: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync fill file: %w", err)
	}
	_, free, err := filesystemSpace(*root)
	if err != nil {
		return err
	}
	if free == 0 || free > *maxFreeBytes {
		return fmt.Errorf("free space after ENOSPC recovery = %d, want 1..%d", free, *maxFreeBytes)
	}
	if _, err := fmt.Fprintf(
		os.Stdout, "M1_ENOSPC_OBSERVED=true bytes_written=%d bytes_free=%d\n", written, free,
	); err != nil {
		return fmt.Errorf("write ENOSPC result: %w", err)
	}
	return nil
}

func mmapRegular(path string) ([]byte, func() error, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, errors.New("path is not a regular file")
	}
	if info.Size() == 0 {
		return nil, file.Close, nil
	}
	if info.Size() > int64(^uint(0)>>1) {
		_ = file.Close()
		return nil, nil, errors.New("file is too large to mmap on this architecture")
	}
	payload, err := syscall.Mmap(int(file.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	closeMapped := func() error {
		return errors.Join(syscall.Munmap(payload), file.Close())
	}
	return payload, closeMapped, nil
}

func mmapDigest(path string) (string, error) {
	payload, closeMapped, err := mmapRegular(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	if err := closeMapped(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sum), nil
}

func digestTree(root string) (string, int, error) {
	hasher := sha256.New()
	files := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("publication contains symlink %q", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("publication contains non-regular entry %q", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fileDigest, err := mmapDigest(path)
		if err != nil {
			return fmt.Errorf("mmap %q: %w", relative, err)
		}
		_, _ = hasher.Write([]byte(filepath.ToSlash(relative)))
		_, _ = hasher.Write([]byte{0})
		if err := binary.Write(hasher, binary.BigEndian, uint64(info.Size())); err != nil {
			return err
		}
		_, _ = hasher.Write([]byte(fileDigest))
		files++
		return nil
	})
	if err != nil {
		return "", 0, fmt.Errorf("digest publication tree: %w", err)
	}
	return fmt.Sprintf("%x", hasher.Sum(nil)), files, nil
}

func filesystemSpace(root string) (capacity, free uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return 0, 0, fmt.Errorf("inspect filesystem space: %w", err)
	}
	return stat.Blocks * uint64(stat.Bsize), stat.Bavail * uint64(stat.Bsize), nil
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "m1-functional-helper: "+format+"\n", args...)
	os.Exit(1)
}
