// Package contracts maps untrusted Protobuf transports into validated domain values.
package contracts

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Stage string

const (
	StageSpecification  Stage = "specification"
	StagePlanning       Stage = "planning"
	StageImplementation Stage = "implementation"
	StageReview         Stage = "review"
)

type Envelope struct {
	SchemaVersion       string
	RequestID           string
	RunID               string
	TaskID              *string
	Stage               Stage
	Attempt             uint32
	CreatedAt           time.Time
	ExpiresAt           time.Time
	MaximumInputTokens  uint64
	MaximumOutputTokens uint64
	MaximumRequests     uint32
	InputArtifacts      []Artifact
	AgentManifestDigest string
}

type Artifact struct {
	URI    string
	SHA256 string
}

func MapEnvelope(value *reasoningv1.ReasoningRequestEnvelope, expected Stage) (Envelope, error) {
	if value == nil {
		return Envelope{}, errors.New("envelope is required")
	}
	stage, err := validateEnvelopeHeader(value, expected)
	if err != nil {
		return Envelope{}, err
	}
	created, expires, err := mapEnvelopeTimes(value)
	if err != nil {
		return Envelope{}, err
	}
	if err := validateEnvelopeBudget(value); err != nil {
		return Envelope{}, err
	}
	if !digestPattern.MatchString(value.GetAgentManifestDigest()) {
		return Envelope{}, errors.New("invalid agent manifest digest")
	}
	artifacts, err := mapArtifacts(value.GetInputArtifacts())
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		SchemaVersion:       value.GetSchemaVersion(),
		RequestID:           value.GetRequestId(),
		RunID:               value.GetRunId(),
		TaskID:              value.TaskId,
		Stage:               stage,
		Attempt:             value.GetAttempt(),
		CreatedAt:           created,
		ExpiresAt:           expires,
		MaximumInputTokens:  value.GetBudget().GetMaximumInputTokens(),
		MaximumOutputTokens: value.GetBudget().GetMaximumOutputTokens(),
		MaximumRequests:     value.GetBudget().GetMaximumProviderRequests(),
		InputArtifacts:      artifacts,
		AgentManifestDigest: value.GetAgentManifestDigest(),
	}, nil
}

func validateEnvelopeHeader(
	value *reasoningv1.ReasoningRequestEnvelope, expected Stage,
) (Stage, error) {
	stage, err := mapStage(value.GetStage())
	if err != nil || stage != expected {
		return "", errors.New("unexpected reasoning stage")
	}
	if err := validateAuthority(value.GetAuthority()); err != nil {
		return "", err
	}
	if value.GetSchemaVersion() != "1" || value.GetRequestId() == "" || value.GetRunId() == "" {
		return "", errors.New("invalid envelope identity")
	}
	return stage, nil
}

func mapEnvelopeTimes(value *reasoningv1.ReasoningRequestEnvelope) (time.Time, time.Time, error) {
	if value.GetAttempt() == 0 || value.GetCreatedAt() == nil || value.GetExpiresAt() == nil {
		return time.Time{}, time.Time{}, errors.New("invalid attempt or timestamps")
	}
	if err := value.GetCreatedAt().CheckValid(); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("created_at: %w", err)
	}
	if err := value.GetExpiresAt().CheckValid(); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("expires_at: %w", err)
	}
	created := value.GetCreatedAt().AsTime()
	expires := value.GetExpiresAt().AsTime()
	if !expires.After(created) {
		return time.Time{}, time.Time{}, errors.New("expires_at must follow created_at")
	}
	return created, expires, nil
}

func validateEnvelopeBudget(value *reasoningv1.ReasoningRequestEnvelope) error {
	if value.GetBudget() == nil || value.GetBudget().GetMaximumInputTokens() == 0 ||
		value.GetBudget().GetMaximumOutputTokens() == 0 ||
		value.GetBudget().GetMaximumProviderRequests() == 0 {
		return errors.New("positive reasoning budget is required")
	}
	return nil
}

func mapArtifacts(values []*reasoningv1.ArtifactDigest) ([]Artifact, error) {
	artifacts := make([]Artifact, 0, len(values))
	for _, artifact := range values {
		if !digestPattern.MatchString(artifact.GetSha256()) ||
			artifact.GetArtifactUri() != "artifact://sha256/"+artifact.GetSha256() {
			return nil, errors.New("invalid input artifact")
		}
		artifacts = append(artifacts, Artifact{URI: artifact.GetArtifactUri(), SHA256: artifact.GetSha256()})
	}
	return artifacts, nil
}

func validateAuthority(value *reasoningv1.AuthorityConstraints) error {
	if value == nil || value.GetMode() != reasoningv1.AuthorityMode_AUTHORITY_MODE_PROPOSAL_ONLY ||
		value.GetMayMutateKernelState() || value.GetMayExecuteCommands() ||
		value.GetMayModifyFiles() || value.GetMayExpandScope() || value.GetMayApproveWork() {
		return errors.New("authority violation: proposal_only with all capabilities false is required")
	}
	return nil
}

func mapStage(value reasoningv1.ReasoningStage) (Stage, error) {
	switch value {
	case reasoningv1.ReasoningStage_REASONING_STAGE_SPECIFICATION:
		return StageSpecification, nil
	case reasoningv1.ReasoningStage_REASONING_STAGE_PLANNING:
		return StagePlanning, nil
	case reasoningv1.ReasoningStage_REASONING_STAGE_IMPLEMENTATION:
		return StageImplementation, nil
	case reasoningv1.ReasoningStage_REASONING_STAGE_REVIEW:
		return StageReview, nil
	default:
		return "", errors.New("unknown reasoning stage")
	}
}

func validateProposalIdentity(
	value *reasoningv1.ProposalIdentity, request Envelope, expected Stage,
) error {
	stage, err := mapStage(value.GetStage())
	if err != nil || stage != expected || value.GetSchemaVersion() != request.SchemaVersion ||
		value.GetRequestId() != request.RequestID || value.GetRunId() != request.RunID ||
		value.GetAttempt() != request.Attempt ||
		value.GetAgentManifestDigest() != request.AgentManifestDigest {
		return errors.New("proposal request identity mismatch")
	}
	if (value.TaskId == nil) != (request.TaskID == nil) ||
		(value.TaskId != nil && *value.TaskId != *request.TaskID) ||
		len(value.GetInputArtifactDigests()) != len(request.InputArtifacts) {
		return errors.New("proposal request identity mismatch")
	}
	for index, artifact := range request.InputArtifacts {
		if value.GetInputArtifactDigests()[index] != artifact.SHA256 {
			return errors.New("proposal request identity mismatch")
		}
	}
	return nil
}
