package beta

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
)

const (
	MaxEvidenceFiles    = 64
	MaxEvidenceFileSize = 1 << 20
)

// EvidenceCountLimitError reports that an evidence directory contains more
// files than the release contract permits.
type EvidenceCountLimitError struct{}

func (*EvidenceCountLimitError) Error() string {
	return "release evidence directory exceeds file limit"
}

// EvidenceSizeLimitError reports that an evidence file is larger than the
// release contract permits.
type EvidenceSizeLimitError struct{}

func (*EvidenceSizeLimitError) Error() string {
	return "release evidence file exceeds size limit"
}

// DirectoryDigests returns stable relative SHA-256 digests for a bounded
// directory of regular release-evidence files.
func DirectoryDigests(root string) (map[string]string, error) {
	return directoryDigests(root, nil)
}

func directoryDigests(rootPath string, afterWalk func() error) (map[string]string, error) {
	rootInfo, err := os.Lstat(rootPath)
	if err != nil || !rootInfo.IsDir() {
		return nil, errors.New("release evidence root must be a directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, errors.New("release evidence root must be a directory")
	}
	defer root.Close()
	paths := make([]string, 0)
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
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
			return &EvidenceSizeLimitError{}
		}
		paths = append(paths, path)
		if len(paths) > MaxEvidenceFiles {
			return &EvidenceCountLimitError{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if afterWalk != nil {
		if err := afterWalk(); err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	result := make(map[string]string, len(paths))
	for _, path := range paths {
		value, err := digestFile(root, path)
		if err != nil {
			return nil, err
		}
		result[path] = value
	}
	return result, nil
}

func digestFile(root *os.Root, path string) (string, error) {
	file, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("release evidence directory contains a non-regular file")
	}
	if info.Size() > MaxEvidenceFileSize {
		return "", &EvidenceSizeLimitError{}
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, MaxEvidenceFileSize+1))
	if err != nil {
		return "", err
	}
	if written > MaxEvidenceFileSize {
		return "", &EvidenceSizeLimitError{}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
