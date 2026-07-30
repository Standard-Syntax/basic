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
	FakeProviderMode = "fake"
	MiniMaxBaseURL   = "https://api.minimax.io/anthropic"
	MiniMaxModel     = "MiniMax-M3"
	MiniMaxAPIKeyEnv = "ANTHROPIC_API_KEY"
)

type ProviderConfig struct {
	Mode      string `json:"mode"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

func (c ProviderConfig) Normalize() (ProviderConfig, error) {
	if c.Mode == "" {
		c.Mode = MiniMaxMode
	}
	if c.Mode == FakeProviderMode {
		if c.BaseURL != "" || c.Model != "" || c.APIKeyEnv != "" {
			return ProviderConfig{}, errors.New("fake provider does not accept remote configuration")
		}
		return c, nil
	}
	if c.Mode != MiniMaxMode {
		return ProviderConfig{}, errors.New("provider mode must be minimax_anthropic or fake")
	}
	if c.BaseURL == "" {
		c.BaseURL = MiniMaxBaseURL
	}
	if c.Model == "" {
		c.Model = MiniMaxModel
	}
	if c.APIKeyEnv == "" {
		c.APIKeyEnv = MiniMaxAPIKeyEnv
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		c.BaseURL != MiniMaxBaseURL {
		return ProviderConfig{}, errors.New("MiniMax base URL must use the approved HTTPS endpoint")
	}
	if c.Model != MiniMaxModel {
		return ProviderConfig{}, errors.New("MiniMax model must be MiniMax-M3")
	}
	if c.APIKeyEnv != MiniMaxAPIKeyEnv {
		return ProviderConfig{}, errors.New("MiniMax credential source must be ANTHROPIC_API_KEY")
	}
	return c, nil
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
