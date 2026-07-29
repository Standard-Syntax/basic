// Package approval applies authenticated human task decisions after trusted review.
package approval

import (
	"context"
	"errors"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

var (
	ErrInvalidRequest    = errors.New("invalid task approval request")
	ErrUnauthorized      = errors.New("task approval principal is unauthorized")
	ErrElevatedRole      = errors.New("elevated approval role is required")
	ErrArtifactIntegrity = errors.New("task approval artifact integrity check failed")
)

type Role string

const (
	RoleApprover         Role = "approver"
	RoleElevatedApprover Role = "elevated_approver"
)

type Principal struct {
	ID    string `json:"id"`
	Roles []Role `json:"roles"`
}

type Request struct {
	ApprovalID                  string
	DecisionTimestamp           time.Time
	Principal                   Principal
	RunID                       string
	TaskID                      string
	CandidateCommit             string
	ApprovedSpecificationDigest string
	ApprovedTaskDigest          string
	ActualChangedPaths          []string
	ExclusiveResourceLabels     []string
	Implementation              workflow.ArtifactRef
	Execution                   workflow.ArtifactRef
	Verification                workflow.ArtifactRef
	Review                      workflow.ArtifactRef
	ExpectedTaskRevision        uint64
}

type Result struct {
	ApprovalID       string
	Decision         string
	ApprovalArtifact workflow.ArtifactRef
	Elevated         bool
	RiskReasons      []string
	Replay           bool
}

type ArtifactStore interface {
	Get(context.Context, workflow.ArtifactRef) ([]byte, error)
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
}

type WorkflowStore interface {
	ExecuteTask(context.Context, workflow.TaskCommand) (workflow.CommandResult, error)
}

type TaskApproval struct {
	SchemaVersion               string               `json:"schema_version"`
	ApprovalID                  string               `json:"approval_id"`
	Approver                    Principal            `json:"approver"`
	Decision                    string               `json:"decision"`
	DecisionTimestamp           string               `json:"decision_timestamp"`
	Reason                      string               `json:"reason,omitempty"`
	Elevated                    bool                 `json:"elevated"`
	RiskReasons                 []string             `json:"risk_reasons"`
	RunID                       string               `json:"run_id"`
	TaskID                      string               `json:"task_id"`
	CandidateCommit             string               `json:"candidate_commit"`
	ApprovedSpecificationDigest string               `json:"approved_specification_digest"`
	ApprovedTaskDigest          string               `json:"approved_task_digest"`
	Implementation              workflow.ArtifactRef `json:"implementation"`
	Execution                   workflow.ArtifactRef `json:"execution"`
	Verification                workflow.ArtifactRef `json:"verification"`
	Review                      workflow.ArtifactRef `json:"review"`
}
