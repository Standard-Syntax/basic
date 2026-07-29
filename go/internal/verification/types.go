// Package verification independently verifies candidate commits against trusted checks.
package verification

import (
	"context"
	"errors"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

var (
	ErrInvalidRequest       = errors.New("invalid verification request")
	ErrArtifactIntegrity    = errors.New("verification artifact integrity check failed")
	ErrVerificationConflict = errors.New("verification ID already has different content")
	ErrOutputLimit          = errors.New("verification output limit exceeded")
	ErrWorkerResponse       = errors.New("invalid verification worker response")
)

const (
	DefaultCheckTimeout   = 10 * time.Minute
	DefaultMaxChecks      = 16
	DefaultMaxConcurrent  = 2
	DefaultMaxWorkspaces  = 4
	DefaultMaxOutputBytes = 1 << 20
)

type Request struct {
	VerificationID        string
	VerificationTimestamp time.Time
	Implementation        *reasoningv1.ImplementationRequest
	ExecutionArtifact     workflow.ArtifactRef
	CandidateCommit       string
	ExpectedTaskRevision  uint64
	Requirements          []AcceptanceRequirement
}

type AcceptanceRequirement struct {
	CriterionID string   `json:"criterion_id"`
	CheckIDs    []string `json:"check_ids"`
}

type ResourceLimits struct {
	CPUs        int   `json:"cpus"`
	MemoryBytes int64 `json:"memory_bytes"`
	PIDs        int   `json:"pids"`
	OutputBytes int64 `json:"output_bytes"`
}

type CheckDefinition struct {
	ID               string         `json:"id"`
	CommandReference string         `json:"command_reference"`
	Argv             []string       `json:"argv"`
	Timeout          time.Duration  `json:"timeout_nanoseconds"`
	Limits           ResourceLimits `json:"limits"`
}

type ResolvedPlan struct {
	Checks       []CheckDefinition       `json:"checks"`
	Requirements []AcceptanceRequirement `json:"requirements"`
}

type CheckResult struct {
	CheckID          string               `json:"check_id"`
	CommandReference string               `json:"command_reference"`
	Argv             []string             `json:"argv"`
	ImageID          string               `json:"image_id"`
	CandidateCommit  string               `json:"candidate_commit"`
	StartedAt        string               `json:"started_at"`
	FinishedAt       string               `json:"finished_at"`
	ExitCode         int                  `json:"exit_code"`
	TimedOut         bool                 `json:"timed_out"`
	Output           workflow.ArtifactRef `json:"output"`
	OutputDigest     string               `json:"output_digest"`
	WallTimeNanos    int64                `json:"wall_time_nanoseconds"`
	UserTimeNanos    int64                `json:"user_time_nanoseconds"`
	SystemTimeNanos  int64                `json:"system_time_nanoseconds"`
	PeakRSSBytes     int64                `json:"peak_rss_bytes"`
	Passed           bool                 `json:"passed"`
}

type CriterionCoverage struct {
	CriterionID string   `json:"criterion_id"`
	CheckIDs    []string `json:"check_ids"`
	Covered     bool     `json:"covered"`
	Passed      bool     `json:"passed"`
}

type VerificationReport struct {
	SchemaVersion   string               `json:"schema_version"`
	VerificationID  string               `json:"verification_id"`
	VerifiedAt      string               `json:"verified_at"`
	RunID           string               `json:"run_id"`
	TaskID          string               `json:"task_id"`
	Attempt         uint32               `json:"attempt"`
	Execution       workflow.ArtifactRef `json:"execution"`
	BaseCommit      string               `json:"base_commit"`
	CandidateCommit string               `json:"candidate_commit"`
	ImageID         string               `json:"image_id"`
	Checks          []CheckResult        `json:"checks"`
	Coverage        []CriterionCoverage  `json:"coverage"`
	Passed          bool                 `json:"passed"`
}

type Result struct {
	VerificationID  string
	CandidateCommit string
	ReportArtifact  workflow.ArtifactRef
	Coverage        []CriterionCoverage
	Passed          bool
	Replay          bool
}

type ArtifactStore interface {
	Get(context.Context, workflow.ArtifactRef) ([]byte, error)
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
}

type WorkflowStore interface {
	ExecuteTask(context.Context, workflow.TaskCommand) (workflow.CommandResult, error)
}

type WorkspacePreparer interface {
	Prepare(context.Context, string, string) (workspace string, cleanup func() error, err error)
}

type CheckExecutor interface {
	ImageID(context.Context) (string, error)
	Run(context.Context, string, CheckDefinition) (ExecutionMeasurement, error)
}

type ExecutionMeasurement struct {
	StartedAt       time.Time
	FinishedAt      time.Time
	ExitCode        int
	TimedOut        bool
	Output          []byte
	WallTime        time.Duration
	UserTime        time.Duration
	SystemTime      time.Duration
	PeakRSSBytes    int64
	OutputTruncated bool
}

type VerificationStart struct {
	VerificationID string
	RequestDigest  string
	Timestamp      time.Time
	ReservationTTL time.Duration
}

type VerificationEvidence struct {
	ReportArtifact  workflow.ArtifactRef
	CandidateCommit string
	Coverage        []CriterionCoverage
	Passed          bool
}

type VerificationHandle interface {
	Replay() (Result, bool)
	Evidence() (VerificationEvidence, bool)
	SaveEvidence(context.Context, VerificationEvidence) error
	FinalTransitionTime(context.Context, time.Time) (time.Time, error)
	Complete(context.Context, Result) error
}

type VerificationLedger interface {
	Begin(context.Context, VerificationStart) (VerificationHandle, error)
}
