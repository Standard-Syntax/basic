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
)

const (
	DefaultMaxChangedFiles = 100
	DefaultMaxFileBytes    = 1 << 20
	DefaultMaxTotalBytes   = 10 << 20
	DefaultTimeout         = 5 * time.Minute
)

type Request struct {
	ExecutionID          string
	Implementation       *reasoningv1.ImplementationRequest
	Proposal             *reasoningv1.ImplementationProposal
	ProposalArtifact     workflow.ArtifactRef
	Lease                workflow.LeaseRef
	ExpectedTaskRevision uint64
}

type Limits struct {
	MaxChangedFiles int
	MaxFileBytes    int64
	MaxTotalBytes   int64
	Timeout         time.Duration
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
	Limits         Limits
}

type ArtifactStore interface {
	Get(context.Context, workflow.ArtifactRef) ([]byte, error)
}

type Applicator interface {
	Apply(context.Context, string, []contracts.FileChange, Limits) error
}

type Result struct {
	ExecutionID string
	BaseCommit  string
	Worktree    string
	Request     contracts.ImplementationRequest
	Proposal    contracts.ImplementationProposal
	Lease       workflow.LeaseRef
	Limits      Limits
}
