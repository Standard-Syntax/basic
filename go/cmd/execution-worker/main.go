// Command execution-worker is the private, network-disabled file applicator.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"golang.org/x/sys/unix"
)

type preparedChange struct {
	change contracts.FileChange
	dirFD  int
	leaf   string
	stat   unix.Stat_t
	mode   uint32
}

func main() {
	if err := run(os.Stdin); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(input io.Reader) error {
	var request struct {
		Changes []contracts.FileChange `json:"changes"`
		Limits  execution.Limits       `json:"limits"`
	}
	decoder := json.NewDecoder(io.LimitReader(input, execution.DefaultMaxTotalBytes*6+1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if err := validateRequest(request.Changes, request.Limits); err != nil {
		return err
	}
	rootFD, err := unix.Open("/workspace", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer unix.Close(rootFD)
	prepared := make([]preparedChange, 0, len(request.Changes))
	defer func() {
		for _, item := range prepared {
			_ = unix.Close(item.dirFD)
		}
	}()
	for _, change := range request.Changes {
		item, err := prepare(rootFD, change, request.Limits.MaxFileBytes)
		if err != nil {
			return err
		}
		prepared = append(prepared, item)
	}
	for _, item := range prepared {
		if err := apply(item); err != nil {
			return err
		}
	}
	return nil
}

func validateRequest(changes []contracts.FileChange, limits execution.Limits) error {
	if limits.MaxChangedFiles < 1 || limits.MaxFileBytes < 1 || limits.MaxTotalBytes < 1 ||
		limits.MaxChangedFiles > execution.DefaultMaxChangedFiles ||
		limits.MaxFileBytes > execution.DefaultMaxFileBytes ||
		limits.MaxTotalBytes > execution.DefaultMaxTotalBytes {
		return errors.New("invalid worker limits")
	}
	if len(changes) > limits.MaxChangedFiles {
		return errors.New("too many changed files")
	}
	var total int64
	for _, change := range changes {
		if change.ReplacementContent == nil {
			continue
		}
		size := int64(len(*change.ReplacementContent))
		if size > limits.MaxFileBytes {
			return errors.New("replacement exceeds per-file limit")
		}
		total += size
		if total > limits.MaxTotalBytes {
			return errors.New("replacement content exceeds total limit")
		}
	}
	return nil
}

func prepare(
	rootFD int, change contracts.FileChange, maxFileBytes int64,
) (preparedChange, error) {
	if change.Path == "" || path.IsAbs(change.Path) || path.Clean(change.Path) != change.Path ||
		strings.HasPrefix(change.Path, "../") || strings.Contains(change.Path, `\`) {
		return preparedChange{}, errors.New("unsafe change path")
	}
	parts := strings.Split(change.Path, "/")
	for _, part := range parts {
		if part == ".git" || part == "" || part == "." || part == ".." {
			return preparedChange{}, errors.New("unsafe change path")
		}
	}
	dirFD, err := unix.Dup(rootFD)
	if err != nil {
		return preparedChange{}, err
	}
	for _, component := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(
			dirFD, component, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0,
		)
		_ = unix.Close(dirFD)
		if openErr != nil {
			return preparedChange{}, fmt.Errorf("open path ancestor: %w", openErr)
		}
		dirFD = next
	}
	item := preparedChange{change: change, dirFD: dirFD, leaf: parts[len(parts)-1]}
	var stat unix.Stat_t
	statErr := unix.Fstatat(dirFD, item.leaf, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if change.Operation == contracts.FileCreate {
		if statErr == nil || !errors.Is(statErr, unix.ENOENT) {
			_ = unix.Close(dirFD)
			return preparedChange{}, errors.New("create target already exists")
		}
		item.mode = 0o644
		return item, nil
	}
	if statErr != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		(stat.Mode&0o777 != 0o644 && stat.Mode&0o777 != 0o755) {
		_ = unix.Close(dirFD)
		return preparedChange{}, errors.New("target is not a supported regular file")
	}
	fd, err := unix.Openat(dirFD, item.leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(dirFD)
		return preparedChange{}, fmt.Errorf("open target: %w", err)
	}
	file := os.NewFile(uintptr(fd), item.leaf)
	body, readErr := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		_ = unix.Close(dirFD)
		return preparedChange{}, errors.New("read target")
	}
	if int64(len(body)) > maxFileBytes {
		_ = unix.Close(dirFD)
		return preparedChange{}, errors.New("target exceeds per-file limit")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != change.ExpectedOriginalSHA256 {
		_ = unix.Close(dirFD)
		return preparedChange{}, errors.New("target digest mismatch")
	}
	item.stat, item.mode = stat, stat.Mode&0o777
	return item, nil
}

func apply(item preparedChange) error {
	if item.change.Operation != contracts.FileCreate {
		var current unix.Stat_t
		if err := unix.Fstatat(
			item.dirFD, item.leaf, &current, unix.AT_SYMLINK_NOFOLLOW,
		); err != nil || current.Dev != item.stat.Dev || current.Ino != item.stat.Ino ||
			current.Mode&unix.S_IFMT != unix.S_IFREG {
			return errors.New("target changed after preflight")
		}
	}
	if item.change.Operation == contracts.FileDelete {
		return unix.Unlinkat(item.dirFD, item.leaf, 0)
	}
	if item.change.ReplacementContent == nil {
		return errors.New("replacement content is required")
	}
	temporary := ".harness-" + item.leaf + ".tmp"
	fd, err := unix.Openat(
		item.dirFD, temporary,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		item.mode,
	)
	if err != nil {
		return fmt.Errorf("create replacement: %w", err)
	}
	file := os.NewFile(uintptr(fd), temporary)
	_, writeErr := file.WriteString(*item.change.ReplacementContent)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = unix.Unlinkat(item.dirFD, temporary, 0)
		return errors.New("write replacement")
	}
	if err := unix.Renameat(item.dirFD, temporary, item.dirFD, item.leaf); err != nil {
		_ = unix.Unlinkat(item.dirFD, temporary, 0)
		return fmt.Errorf("publish replacement: %w", err)
	}
	return nil
}
