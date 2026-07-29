// Package review reconstructs trusted evidence and records advisory task review.
package review

import (
	"context"
	"errors"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

var (
	ErrInvalidRequest    = errors.New("invalid review request")
	ErrArtifactIntegrity = errors.New("review artifact integrity check failed")
	ErrPolicyViolation   = errors.New("review policy violation")
)

type Request struct {
	ReviewID                string
	ReviewTimestamp         time.Time
	Review                  *reasoningv1.ReviewRequest
	ExecutionArtifact       workflow.ArtifactRef
	VerificationArtifact    workflow.ArtifactRef
	ExclusiveResourceLabels []string
	ExpectedTaskRevision    uint64
}

type Result struct {
	ReviewID        string
	CandidateCommit string
	ReportArtifact  workflow.ArtifactRef
	Recommendation  reasoningv1.ReviewRecommendation
	Passed          bool
	Replay          bool
}

type ArtifactStore interface {
	Get(context.Context, workflow.ArtifactRef) ([]byte, error)
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
}

type ReviewGateway interface {
	ProposeReview(context.Context, *reasoningv1.ReviewRequest) (gateway.ReviewOutcome, error)
}

type WorkflowStore interface {
	ExecuteTask(context.Context, workflow.TaskCommand) (workflow.CommandResult, error)
}

type Config struct {
	ActorID string
}

type RiskAssessment struct {
	BlockingFindingIDs []string `json:"blocking_finding_ids"`
	UnrequestedChanges []string `json:"unrequested_changes"`
	ExclusiveResources []string `json:"exclusive_resources"`
}

type ReviewReport struct {
	SchemaVersion                string                        `json:"schema_version"`
	ReviewID                     string                        `json:"review_id"`
	ReviewedAt                   string                        `json:"reviewed_at"`
	RunID                        string                        `json:"run_id"`
	TaskID                       string                        `json:"task_id"`
	Attempt                      uint32                        `json:"attempt"`
	CandidateCommit              string                        `json:"candidate_commit"`
	ApprovedSpecificationDigest  string                        `json:"approved_specification_digest"`
	ApprovedTaskDigest           string                        `json:"approved_task_digest"`
	ImplementationProposalDigest string                        `json:"implementation_proposal_digest"`
	Request                      workflow.ArtifactRef          `json:"request"`
	Proposal                     workflow.ArtifactRef          `json:"proposal"`
	Execution                    workflow.ArtifactRef          `json:"execution"`
	Verification                 workflow.ArtifactRef          `json:"verification"`
	Findings                     []*reasoningv1.ReviewFinding  `json:"findings"`
	RequiredActions              []*reasoningv1.RequiredAction `json:"required_actions"`
	Recommendation               string                        `json:"recommendation"`
	Risk                         RiskAssessment                `json:"risk_assessment"`
	Passed                       bool                          `json:"passed"`
}
