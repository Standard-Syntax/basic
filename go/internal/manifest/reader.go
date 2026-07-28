// Package manifest reads and verifies immutable agent manifests.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/gowebpki/jcs"
)

var (
	lowerDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	agentName   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	version     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	label       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

var validStages = []string{"implementation", "planning", "review", "specification"}
var validCapabilities = []string{"general_reasoning", "independent_review", "strong_coding"}
var validTools = []string{
	"read_repository_file",
	"report_blocker",
	"request_declared_check",
	"search_repository",
}
var validOutputs = []string{
	"implementation_proposal.v1",
	"review_proposal.v1",
	"specification_proposal.v1",
	"task_graph_proposal.v1",
}

// Manifest is a transport-safe representation of agent-manifest v1.
type Manifest struct {
	SchemaVersion string   `json:"schema_version"`
	Agent         Agent    `json:"agent"`
	Stage         string   `json:"stage"`
	Prompt        Prompt   `json:"prompt"`
	Model         Model    `json:"model"`
	Context       Context  `json:"context"`
	Tools         Tools    `json:"tools"`
	Output        Output   `json:"output"`
	Metadata      Metadata `json:"metadata"`
}

type Agent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Prompt struct {
	ArtifactURI string `json:"artifact_uri"`
	SHA256      string `json:"sha256"`
}

type Model struct {
	CapabilityClass     string  `json:"capability_class"`
	Temperature         float64 `json:"temperature"`
	MaximumOutputTokens int     `json:"maximum_output_tokens"`
}

type Context struct {
	IncludeSpecification bool   `json:"include_specification"`
	IncludeTask          bool   `json:"include_task"`
	RepositorySelection  string `json:"repository_selection"`
	MaximumContextTokens int    `json:"maximum_context_tokens"`
}

type Tools struct {
	AllowedRequests  []string `json:"allowed_requests"`
	ArbitraryShell   bool     `json:"arbitrary_shell"`
	ArbitraryNetwork bool     `json:"arbitrary_network"`
	DirectFileWrite  bool     `json:"direct_file_write"`
}

type Output struct {
	Schema string `json:"schema"`
}

type Metadata struct {
	Description string   `json:"description"`
	Labels      []string `json:"labels"`
}

// Read validates a closed manifest, canonicalizes it with RFC 8785, and returns
// its lowercase SHA-256 digest.
func Read(data []byte) (Manifest, []byte, string, error) {
	if err := validateRequiredFields(data); err != nil {
		return Manifest{}, nil, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value Manifest
	if err := decoder.Decode(&value); err != nil {
		return Manifest{}, nil, "", fmt.Errorf("decode manifest: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Manifest{}, nil, "", err
	}
	if err := value.Validate(); err != nil {
		return Manifest{}, nil, "", err
	}
	canonical, err := jcs.Transform(data)
	if err != nil {
		return Manifest{}, nil, "", fmt.Errorf("canonicalize manifest: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return value, canonical, hex.EncodeToString(sum[:]), nil
}

func validateRequiredFields(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	required := map[string][]string{
		"":         {"schema_version", "agent", "stage", "prompt", "model", "context", "tools", "output", "metadata"},
		"agent":    {"name", "version"},
		"prompt":   {"artifact_uri", "sha256"},
		"model":    {"capability_class", "temperature", "maximum_output_tokens"},
		"context":  {"include_specification", "include_task", "repository_selection", "maximum_context_tokens"},
		"tools":    {"allowed_requests", "arbitrary_shell", "arbitrary_network", "direct_file_write"},
		"output":   {"schema"},
		"metadata": {"description", "labels"},
	}
	if err := requireKeys("manifest", root, required[""]); err != nil {
		return err
	}
	for field, keys := range required {
		if field == "" {
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(root[field], &object); err != nil {
			return fmt.Errorf("%s must be an object: %w", field, err)
		}
		if err := requireKeys(field, object, keys); err != nil {
			return err
		}
	}
	return nil
}

func requireKeys(name string, object map[string]json.RawMessage, keys []string) error {
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			return fmt.Errorf("%s missing required field %s", name, key)
		}
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode manifest: trailing JSON value")
		}
		return fmt.Errorf("decode manifest: %w", err)
	}
	return nil
}

// Validate enforces the closed v1 policy without registry or persistence behavior.
func (m Manifest) Validate() error {
	if m.SchemaVersion != "1" {
		return errors.New("schema_version must be 1")
	}
	if !agentName.MatchString(m.Agent.Name) || !version.MatchString(m.Agent.Version) {
		return errors.New("invalid agent name or version")
	}
	if !slices.Contains(validStages, m.Stage) {
		return errors.New("unsupported stage")
	}
	if !lowerDigest.MatchString(m.Prompt.SHA256) ||
		m.Prompt.ArtifactURI != "artifact://sha256/"+m.Prompt.SHA256 {
		return errors.New("invalid prompt artifact URI or digest")
	}
	if !slices.Contains(validCapabilities, m.Model.CapabilityClass) ||
		math.IsNaN(m.Model.Temperature) || math.IsInf(m.Model.Temperature, 0) ||
		m.Model.Temperature < 0 || m.Model.Temperature > 2 ||
		m.Model.MaximumOutputTokens < 1 || m.Model.MaximumOutputTokens > 200_000 {
		return errors.New("invalid model policy")
	}
	if m.Context.RepositorySelection != "kernel_selected" ||
		m.Context.MaximumContextTokens < 1 || m.Context.MaximumContextTokens > 1_000_000 {
		return errors.New("invalid context policy")
	}
	if m.Tools.ArbitraryShell || m.Tools.ArbitraryNetwork || m.Tools.DirectFileWrite {
		return errors.New("unsafe tool permission")
	}
	if !sortedUniqueSubset(m.Tools.AllowedRequests, validTools) {
		return errors.New("invalid tool requests")
	}
	if !slices.Contains(validOutputs, m.Output.Schema) {
		return errors.New("unsupported output schema")
	}
	if len(m.Metadata.Description) > 500 || !sortedUniqueLabels(m.Metadata.Labels) {
		return errors.New("invalid metadata")
	}
	return nil
}

func sortedUniqueSubset(values, allowed []string) bool {
	if !slices.IsSorted(values) {
		return false
	}
	for index, value := range values {
		if !slices.Contains(allowed, value) || (index > 0 && values[index-1] == value) {
			return false
		}
	}
	return true
}

func sortedUniqueLabels(values []string) bool {
	if !slices.IsSorted(values) {
		return false
	}
	for index, value := range values {
		if !label.MatchString(value) || strings.TrimSpace(value) != value ||
			(index > 0 && values[index-1] == value) {
			return false
		}
	}
	return true
}
