// Package execution applies authorized implementation proposals in isolated worktrees.
package execution

import (
	"context"
	"errors"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

var (
	ErrInvalidRequest    = errors.New("invalid execution request")
	ErrArtifactIntegrity = errors.New("execution artifact integrity check failed")
	ErrUnsafePath        = errors.New("unsafe execution path")
	ErrLimitExceeded     = errors.New("execution limit exceeded")
	ErrExecutionConflict = errors.New("execution ID already has different content")
)

const (
	DefaultMaxChangedFiles = 100
	DefaultMaxFileBytes    = 1 << 20
	DefaultMaxTotalBytes   = 10 << 20
	DefaultTimeout         = 5 * time.Minute
	DefaultMaxConcurrent   = 4
	DefaultMaxWorktrees    = 8
)

type Request struct {
	ExecutionID          string
	ExecutionTimestamp   time.Time
	Implementation       *reasoningv1.ImplementationRequest
	Proposal             *reasoningv1.ImplementationProposal
	ProposalArtifact     workflow.ArtifactRef
	Lease                workflow.LeaseRef
	ExpectedTaskRevision uint64
}

type Limits struct {
	MaxChangedFiles int           `json:"max_changed_files"`
	MaxFileBytes    int64         `json:"max_file_bytes"`
	MaxTotalBytes   int64         `json:"max_total_bytes"`
	Timeout         time.Duration `json:"timeout_nanoseconds"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxChangedFiles: DefaultMaxChangedFiles,
		MaxFileBytes:    DefaultMaxFileBytes,
		MaxTotalBytes:   DefaultMaxTotalBytes,
		Timeout:         DefaultTimeout,
	}
}

type Config struct {
	RepositoryRoot string
	WorktreeRoot   string
	WorkerImage    string
	UID            int
	GID            int
	ActorID        string
	AuthorName     string
	AuthorEmail    string
	Limits         Limits
	MaxConcurrent  int
	MaxWorktrees   int
}

type ArtifactStore interface {
	Get(context.Context, workflow.ArtifactRef) ([]byte, error)
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
}

type WorkflowStore interface {
	ExecuteTask(context.Context, workflow.TaskCommand) (workflow.CommandResult, error)
}

type ExecutionStart struct {
	ExecutionID    string
	RequestDigest  string
	Timestamp      time.Time
	ReservationTTL time.Duration
}

type ExecutionHandle interface {
	Replay() (Result, bool)
	FinalTransitionTime(context.Context, time.Time) (time.Time, error)
	Complete(context.Context, Result) error
}

type ExecutionLedger interface {
	Begin(context.Context, ExecutionStart) (ExecutionHandle, error)
}

type Applicator interface {
	Apply(context.Context, string, []contracts.FileChange, Limits) error
}

type Result struct {
	ExecutionID     string
	BaseCommit      string
	CandidateCommit string
	CandidateRef    string
	ReportArtifact  workflow.ArtifactRef
	Lease           workflow.LeaseRef
	Limits          Limits
	ActualDiff      []DiffEntry
	Replay          bool
}

type DiffEntry struct {
	Operation    contracts.FileOperation `json:"operation"`
	Path         string                  `json:"path"`
	Mode         string                  `json:"mode,omitempty"`
	BeforeSHA256 string                  `json:"before_sha256"`
	AfterSHA256  string                  `json:"after_sha256"`
}

type ExecutionReport struct {
	SchemaVersion   string               `json:"schema_version"`
	ExecutionID     string               `json:"execution_id"`
	ExecutedAt      string               `json:"executed_at"`
	RunID           string               `json:"run_id"`
	TaskID          string               `json:"task_id"`
	Attempt         uint32               `json:"attempt"`
	Proposal        workflow.ArtifactRef `json:"proposal"`
	Lease           workflow.LeaseRef    `json:"lease"`
	BaseCommit      string               `json:"base_commit"`
	CandidateCommit string               `json:"candidate_commit"`
	CandidateRef    string               `json:"candidate_ref"`
	Limits          Limits               `json:"limits"`
	ActualDiff      []DiffEntry          `json:"actual_diff"`
}
