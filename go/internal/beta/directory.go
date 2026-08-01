package beta

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

const (
	MaxEvidenceFiles    = 64
	MaxEvidenceFileSize = 1 << 20
)

// DirectoryDigests returns stable relative SHA-256 digests for a bounded
// directory of regular release-evidence files.
func DirectoryDigests(root string) (map[string]string, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("release evidence root must be a directory")
	}
	paths := make([]string, 0)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("release evidence directory contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("release evidence directory contains a non-regular file")
		}
		if info.Size() > MaxEvidenceFileSize {
			return errors.New("release evidence file exceeds size limit")
		}
		paths = append(paths, path)
		if len(paths) > MaxEvidenceFiles {
			return errors.New("release evidence directory exceeds file limit")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	result := make(map[string]string, len(paths))
	for _, path := range paths {
		value, err := digestFile(path)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		result[filepath.ToSlash(relative)] = value
	}
	return result, nil
}

func digestFile(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return "", errors.New("open release evidence file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("release evidence directory contains a non-regular file")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, MaxEvidenceFileSize+1))
	if err != nil {
		return "", err
	}
	if written > MaxEvidenceFileSize {
		return "", errors.New("release evidence file exceeds size limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
