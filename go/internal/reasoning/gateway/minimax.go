package gateway

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
)

const (
	MiniMaxMode      = "minimax_anthropic"
	MiniMaxBaseURL   = "https://api.minimax.io/anthropic"
	MiniMaxModel     = "MiniMax-M2.7"
	MiniMaxAPIKeyEnv = "ANTHROPIC_API_KEY"
)

type ProviderConfig struct {
	Mode      string `json:"mode"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
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
	if normalized.APIKeyEnv == "" {
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
	if normalized.APIKeyEnv != MiniMaxAPIKeyEnv {
		return ProviderConfig{}, errors.New("MiniMax credential source must be ANTHROPIC_API_KEY")
	}
	return normalized, nil
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
