package gateway

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	MiniMaxMode      = "minimax_anthropic"
	MiniMaxBaseURL   = "https://api.minimax.io/anthropic"
	MiniMaxModel     = "MiniMax-M2.7"
	MiniMaxAPIKeyEnv = "ANTHROPIC_API_KEY"
)

type ProviderConfig struct {
	Mode       string `json:"mode"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	APIKeyEnv  string `json:"api_key_env"`
	APIKeyFile string `json:"api_key_file,omitempty"`
}

func (c ProviderConfig) Normalize() (ProviderConfig, error) {
	normalized := c
	if normalized.Mode == "" {
		normalized.Mode = MiniMaxMode
	}
	if normalized.Mode != MiniMaxMode {
		return ProviderConfig{}, errors.New("provider mode must be minimax_anthropic")
	}
	return normalizeMiniMaxConfig(normalized)
}

func normalizeMiniMaxConfig(normalized ProviderConfig) (ProviderConfig, error) {
	if normalized.BaseURL == "" {
		normalized.BaseURL = MiniMaxBaseURL
	}
	if normalized.Model == "" {
		normalized.Model = MiniMaxModel
	}
	if normalized.APIKeyEnv == "" && normalized.APIKeyFile == "" {
		normalized.APIKeyEnv = MiniMaxAPIKeyEnv
	}
	parsed, err := url.Parse(normalized.BaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		normalized.BaseURL != MiniMaxBaseURL {
		return ProviderConfig{}, errors.New("MiniMax base URL must use the approved HTTPS endpoint")
	}
	if normalized.Model != MiniMaxModel {
		return ProviderConfig{}, errors.New("MiniMax model must be MiniMax-M2.7")
	}
	fileSource := normalized.APIKeyFile != ""
	if fileSource == (normalized.APIKeyEnv != "") {
		return ProviderConfig{}, errors.New("exactly one MiniMax credential source is required")
	}
	if fileSource {
		if !filepath.IsAbs(normalized.APIKeyFile) || filepath.Clean(normalized.APIKeyFile) != normalized.APIKeyFile {
			return ProviderConfig{}, errors.New("MiniMax credential file must be clean and absolute")
		}
	} else if normalized.APIKeyEnv != MiniMaxAPIKeyEnv {
		return ProviderConfig{}, errors.New("MiniMax credential source must be ANTHROPIC_API_KEY")
	}
	return normalized, nil
}

// FileCredentialSource validates and rereads an owner-only credential for
// every provider invocation so rotation requires no service restart.
type FileCredentialSource struct{ Path string }

func (s FileCredentialSource) Credential(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(s.Path) || filepath.Clean(s.Path) != s.Path {
		return "", ErrCredentialUnavailable
	}
	info, err := os.Lstat(s.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", ErrCredentialUnavailable
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return "", ErrCredentialUnavailable
	}
	file, err := os.Open(s.Path)
	if err != nil {
		return "", ErrCredentialUnavailable
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		return "", ErrCredentialUnavailable
	}
	value := strings.TrimSpace(string(body))
	if value == "" {
		return "", ErrCredentialUnavailable
	}
	return value, nil
}

type EnvironmentCredentialSource struct {
	Name string
}

func (s EnvironmentCredentialSource) Credential(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.Name != MiniMaxAPIKeyEnv {
		return "", ErrCredentialUnavailable
	}
	value := os.Getenv(s.Name)
	if strings.TrimSpace(value) == "" {
		return "", ErrCredentialUnavailable
	}
	return value, nil
}

func MiniMaxModels() StaticCapabilityModels {
	return StaticCapabilityModels{
		"strong_coding":      MiniMaxModel,
		"independent_review": MiniMaxModel,
	}
}
