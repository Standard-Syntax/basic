package publication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var ErrCredentialPermissions = errors.New("unsafe GitHub credential file")

// FileCredential reads the token for each request and never retains it.
type FileCredential struct{ path string }

func NewFileCredential(path string) (*FileCredential, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, ErrCredentialPermissions
	}
	source := &FileCredential{path: path}
	if _, err := source.Token(context.Background()); err != nil {
		return nil, err
	}
	return source, nil
}

func (s *FileCredential) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Lstat(s.path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o600 {
		return "", ErrCredentialPermissions
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return "", ErrCredentialPermissions
	}
	file, err := os.Open(s.path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	body := make([]byte, 4097)
	count, err := file.Read(body)
	if err != nil {
		return "", err
	}
	if count == len(body) {
		return "", ErrCredentialPermissions
	}
	token := strings.TrimSpace(string(body[:count]))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", ErrCredentialPermissions
	}
	return token, nil
}
