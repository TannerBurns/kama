//go:build linux

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
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// ValidateRelativePath enforces a portable, POSIX relative artifact path.
func ValidateRelativePath(value string) error {
	if value == "" {
		return errors.New("path is empty")
	}
	if strings.ContainsRune(value, '\x00') {
		return errors.New("path contains NUL")
	}
	if strings.Contains(value, "\\") {
		return errors.New("path contains a backslash")
	}
	if strings.HasPrefix(value, "/") || path.IsAbs(value) {
		return errors.New("path is absolute")
	}
	if path.Clean(value) != value || value == "." {
		return errors.New("path is not clean")
	}
	for part := range strings.SplitSeq(value, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("path contains an unsafe component")
		}
	}
	return nil
}

// OpenRegular opens relativePath below root one descriptor at a time using
// O_NOFOLLOW. This prevents symlink swaps from escaping an adopted PVC root.
func OpenRegular(root, relativePath string) (*os.File, error) {
	if err := ValidateRelativePath(relativePath); err != nil {
		return nil, failure(ReasonUnsafePath, "validate path", err)
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, failure(ReasonUnsafePath, "open validation root", err)
	}
	currentFD := rootFD
	components := strings.Split(relativePath, "/")
	for index, component := range components {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if index != len(components)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			// Opening a FIFO read-only blocks until a writer appears. Nonblocking
			// open lets fstat reject every non-regular final component promptly.
			flags |= unix.O_NONBLOCK
		}
		nextFD, openErr := unix.Openat(currentFD, component, flags, 0)
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		if openErr != nil {
			_ = unix.Close(rootFD)
			return nil, failure(ReasonUnsafePath, "open artifact path", openErr)
		}
		currentFD = nextFD
	}
	_ = unix.Close(rootFD)

	var stat unix.Stat_t
	if err := unix.Fstat(currentFD, &stat); err != nil {
		_ = unix.Close(currentFD)
		return nil, failure(ReasonIOFailure, "inspect artifact file", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(currentFD)
		return nil, failure(ReasonUnsafePath, "inspect artifact file", fmt.Errorf("%q is not a regular file", relativePath))
	}
	return os.NewFile(uintptr(currentFD), relativePath), nil
}

// OpenDirectory opens relativePath below root without following symlinks.
func OpenDirectory(root, relativePath string) (*os.File, error) {
	if relativePath == "" {
		fd, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, failure(ReasonUnsafePath, "open validation root", err)
		}
		return os.NewFile(uintptr(fd), root), nil
	}
	if err := ValidateRelativePath(relativePath); err != nil {
		return nil, failure(ReasonUnsafePath, "validate directory path", err)
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, failure(ReasonUnsafePath, "open validation root", err)
	}
	currentFD := rootFD
	for component := range strings.SplitSeq(relativePath, "/") {
		nextFD, openErr := unix.Openat(currentFD, component,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		if openErr != nil {
			_ = unix.Close(rootFD)
			return nil, failure(ReasonUnsafePath, "open artifact directory", openErr)
		}
		currentFD = nextFD
	}
	_ = unix.Close(rootFD)
	return os.NewFile(uintptr(currentFD), relativePath), nil
}

// MkdirAll creates a relative directory hierarchy below root without following
// existing symlinks.
func MkdirAll(root, relativePath string, mode os.FileMode) error {
	if err := ValidateRelativePath(relativePath); err != nil {
		return failure(ReasonUnsafePath, "validate directory path", err)
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return failure(ReasonUnsafePath, "open directory root", err)
	}
	defer func() { _ = unix.Close(rootFD) }()
	currentFD := rootFD
	for component := range strings.SplitSeq(relativePath, "/") {
		created := false
		if err := unix.Mkdirat(currentFD, component, uint32(mode.Perm())); err == nil {
			created = true
		} else if !errors.Is(err, unix.EEXIST) {
			if currentFD != rootFD {
				_ = unix.Close(currentFD)
			}
			return classifyStorageError("create artifact directory", err)
		}
		// A file fsync does not make the directory entry linking a newly
		// created directory durable. Sync each parent at creation time so the
		// staging and publication hierarchy survives a storage restart.
		if created {
			if err := unix.Fsync(currentFD); err != nil {
				if currentFD != rootFD {
					_ = unix.Close(currentFD)
				}
				return classifyStorageError("fsync artifact directory parent", err)
			}
		}
		nextFD, err := unix.Openat(currentFD, component,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		if err != nil {
			return failure(ReasonUnsafePath, "open artifact directory", err)
		}
		currentFD = nextFD
	}
	if currentFD != rootFD {
		return unix.Close(currentFD)
	}
	return nil
}

// OpenWritableRegular opens or creates a regular file below root without
// following symlinks. parent directories must already exist.
func OpenWritableRegular(root, relativePath string, truncate bool) (*os.File, error) {
	return openWritableRegular(root, relativePath, truncate, false)
}

// CreateWritableRegular exclusively creates a new regular file below root.
// It is used for random temporary files so an attacker cannot pre-create or
// hard-link a chosen pathname before metadata is written.
func CreateWritableRegular(root, relativePath string) (*os.File, error) {
	return openWritableRegular(root, relativePath, false, true)
}

func openWritableRegular(root, relativePath string, truncate, exclusive bool) (*os.File, error) {
	if err := ValidateRelativePath(relativePath); err != nil {
		return nil, failure(ReasonUnsafePath, "validate writable path", err)
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, failure(ReasonUnsafePath, "open writable root", err)
	}
	currentFD := rootFD
	components := strings.Split(relativePath, "/")
	for _, component := range components[:len(components)-1] {
		nextFD, openErr := unix.Openat(currentFD, component,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		if openErr != nil {
			_ = unix.Close(rootFD)
			return nil, failure(ReasonUnsafePath, "open writable directory", openErr)
		}
		currentFD = nextFD
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CREAT
	if exclusive {
		flags |= unix.O_EXCL
	}
	fd, openErr := unix.Openat(currentFD, components[len(components)-1], flags, 0o640)
	if currentFD != rootFD {
		_ = unix.Close(currentFD)
	}
	_ = unix.Close(rootFD)
	if openErr != nil {
		if errors.Is(openErr, unix.ELOOP) {
			return nil, failure(ReasonUnsafePath, "open writable artifact file", openErr)
		}
		return nil, classifyStorageError("open writable artifact file", openErr)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, failure(ReasonIOFailure, "inspect writable artifact file", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, failure(ReasonUnsafePath, "inspect writable artifact file", errors.New("destination is not a regular file"))
	}
	// A hard link from deterministic staging into an already-published blob
	// must never let a retry truncate verified content.
	if stat.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, failure(ReasonUnsafePath, "inspect writable artifact file", errors.New("destination has multiple hard links"))
	}
	if truncate {
		if err := unix.Ftruncate(fd, 0); err != nil {
			_ = unix.Close(fd)
			return nil, classifyStorageError("truncate writable artifact file", err)
		}
	}
	return os.NewFile(uintptr(fd), relativePath), nil
}

// RenameNoFollow renames two paths below root using securely opened parent
// descriptors. Intermediate symlink replacements cannot redirect the rename.
func RenameNoFollow(root, oldRelativePath, newRelativePath string) error {
	oldParent, oldBase, err := openParent(root, oldRelativePath)
	if err != nil {
		return err
	}
	defer func() { _ = oldParent.Close() }()
	newParent, newBase, err := openParent(root, newRelativePath)
	if err != nil {
		return err
	}
	defer func() { _ = newParent.Close() }()
	if err := unix.Renameat(int(oldParent.Fd()), oldBase, int(newParent.Fd()), newBase); err != nil {
		return classifyStorageError("rename artifact path", err)
	}
	return nil
}

// RemoveNoFollow unlinks one non-directory entry below root through a securely
// opened parent descriptor. It never follows the target if it is a symlink.
func RemoveNoFollow(root, relativePath string) error {
	parent, base, err := openParent(root, relativePath)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if err := unix.Unlinkat(int(parent.Fd()), base, 0); err != nil {
		return classifyStorageError("remove artifact path", err)
	}
	if err := parent.Sync(); err != nil {
		return classifyStorageError("fsync removed artifact path parent", err)
	}
	return nil
}

// RemoveTreeNoFollow recursively removes one directory below root through
// directory descriptors. Symlinks are unlinked as entries and are never
// followed, including if a path is concurrently replaced.
func RemoveTreeNoFollow(root, relativePath string) error {
	parent, base, err := openParent(root, relativePath)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	directoryFD, err := unix.Openat(int(parent.Fd()), base,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return classifyStorageError("open artifact tree", err)
	}
	directory := os.NewFile(uintptr(directoryFD), relativePath)
	if err := removeDirectoryContents(directory); err != nil {
		_ = directory.Close()
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return classifyStorageError("fsync removed artifact tree", err)
	}
	if err := directory.Close(); err != nil {
		return classifyStorageError("close removed artifact tree", err)
	}
	if err := unix.Unlinkat(int(parent.Fd()), base, unix.AT_REMOVEDIR); err != nil {
		return classifyStorageError("remove artifact tree", err)
	}
	if err := parent.Sync(); err != nil {
		return classifyStorageError("fsync artifact tree parent", err)
	}
	return nil
}

func removeDirectoryContents(directory *os.File) error {
	for {
		entries, err := directory.ReadDir(128)
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, io.EOF) {
			return classifyStorageError("read artifact tree", err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\x00') {
				return failure(ReasonUnsafePath, "read artifact tree", errors.New("directory contains an invalid entry name"))
			}
			var stat unix.Stat_t
			if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return classifyStorageError("inspect artifact tree entry", err)
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				childFD, err := unix.Openat(int(directory.Fd()), name,
					unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
				if err != nil {
					return failure(ReasonUnsafePath, "open artifact tree entry", err)
				}
				child := os.NewFile(uintptr(childFD), name)
				if err := removeDirectoryContents(child); err != nil {
					_ = child.Close()
					return err
				}
				if err := child.Close(); err != nil {
					return classifyStorageError("close artifact tree entry", err)
				}
				if err := unix.Unlinkat(int(directory.Fd()), name, unix.AT_REMOVEDIR); err != nil {
					return classifyStorageError("remove artifact tree directory", err)
				}
				continue
			}
			if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
				return classifyStorageError("remove artifact tree entry", err)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func openParent(root, relativePath string) (*os.File, string, error) {
	if err := ValidateRelativePath(relativePath); err != nil {
		return nil, "", failure(ReasonUnsafePath, "validate artifact path", err)
	}
	parentPath := path.Dir(relativePath)
	if parentPath == "." {
		parentPath = ""
	}
	parent, err := OpenDirectory(root, parentPath)
	if err != nil {
		return nil, "", err
	}
	return parent, path.Base(relativePath), nil
}
