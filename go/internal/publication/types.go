// Package publication publishes an approved candidate as one immutable branch and draft pull request.
package publication

import (
	"context"
	"errors"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

var (
	ErrInvalidRequest      = errors.New("invalid publication request")
	ErrArtifactIntegrity   = errors.New("publication artifact integrity check failed")
	ErrBaseDrift           = errors.New("reviewed base commit no longer matches remote base")
	ErrBranchConflict      = errors.New("publication branch already has different content")
	ErrPullRequestConflict = errors.New("pull request identity conflicts with publication")
	ErrPublicationConflict = errors.New("publication ID already has different content")
	ErrPublicationState    = errors.New("invalid publication ledger state")
	ErrResponseLimit       = errors.New("publication response limit exceeded")
)

const (
	DefaultBranchPrefix     = "harness/"
	DefaultMaxArtifactBytes = 1 << 20
	DefaultMaxBodyBytes     = 64 << 10
	DefaultTimeout          = 30 * time.Second
)

type Config struct {
	RepositoryRoot   string
	RepositoryOwner  string
	RepositoryName   string
	Remote           string
	BaseBranch       string
	BranchPrefix     string
	ActorID          string
	APIEndpoint      string
	APIVersion       string
	MaxArtifactBytes int64
	MaxBodyBytes     int64
	Timeout          time.Duration
}

type Request struct {
	PublicationID        string
	PublicationTimestamp time.Time
	RunID                string
	BaseCommit           string
	CandidateCommit      string
	Specification        workflow.ArtifactRef
	Implementation       workflow.ArtifactRef
	Execution            workflow.ArtifactRef
	Verification         workflow.ArtifactRef
	Review               workflow.ArtifactRef
	Approval             workflow.ArtifactRef
	ExpectedRunRevision  uint64
}

type Result struct {
	PublicationID       string
	Branch              string
	CandidateCommit     string
	PullRequestNumber   int64
	PullRequestURL      string
	PublicationArtifact workflow.ArtifactRef
	Replay              bool
}

type DraftPullRequest struct {
	Number int64  `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	Marker string `json:"marker,omitempty"`
}

type DraftPullRequestInput struct {
	Owner  string
	Repo   string
	Head   string
	Base   string
	Title  string
	Body   string
	Marker string
}

type ArtifactStore interface {
	Get(context.Context, workflow.ArtifactRef) ([]byte, error)
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
}

type WorkflowStore interface {
	ExecuteRun(context.Context, workflow.RunCommand) (workflow.CommandResult, error)
}

type GitPublisher interface {
	BaseHead(context.Context) (string, error)
	BranchHead(context.Context, string) (string, bool, error)
	Publish(context.Context, string, string) (bool, error)
}

type CredentialSource interface {
	Token(context.Context) (string, error)
}

type PullRequestClient interface {
	FindDraft(context.Context, DraftPullRequestInput) (DraftPullRequest, bool, error)
	CreateDraft(context.Context, DraftPullRequestInput) (DraftPullRequest, error)
}

type PublicationStart struct {
	PublicationID        string
	RequestDigest        string
	RequestedAt          time.Time
	Repository           string
	BaseBranch           string
	HeadBranch           string
	BaseCommit           string
	CandidateCommit      string
	SpecificationDigest  string
	ImplementationDigest string
	ExecutionDigest      string
	VerificationDigest   string
	ReviewDigest         string
	ApprovalDigest       string
	ExpectedRunRevision  uint64
}

type BranchCheckpoint struct {
	Branch          string
	CandidateCommit string
}

type PullRequestCheckpoint struct {
	Branch            string
	CandidateCommit   string
	PullRequestNumber int64
	PullRequestURL    string
}

type PublicationHandle interface {
	Replay() (Result, bool)
	Branch() (BranchCheckpoint, bool)
	SaveBranch(context.Context, BranchCheckpoint) error
	PullRequest() (PullRequestCheckpoint, bool)
	SavePullRequest(context.Context, PullRequestCheckpoint) error
	Complete(context.Context, Result) error
	Rollback(context.Context) error
}

type PublicationLedger interface {
	Begin(context.Context, PublicationStart) (PublicationHandle, error)
}

type DraftPullRequestArtifact struct {
	SchemaVersion     string               `json:"schema_version"`
	PublicationID     string               `json:"publication_id"`
	PublishedAt       string               `json:"published_at"`
	RepositoryOwner   string               `json:"repository_owner"`
	RepositoryName    string               `json:"repository_name"`
	BaseBranch        string               `json:"base_branch"`
	HeadBranch        string               `json:"head_branch"`
	BaseCommit        string               `json:"base_commit"`
	CandidateCommit   string               `json:"candidate_commit"`
	PullRequestNumber int64                `json:"pull_request_number"`
	PullRequestURL    string               `json:"pull_request_url"`
	Draft             bool                 `json:"draft"`
	Specification     workflow.ArtifactRef `json:"specification"`
	Implementation    workflow.ArtifactRef `json:"implementation"`
	Execution         workflow.ArtifactRef `json:"execution"`
	Verification      workflow.ArtifactRef `json:"verification"`
	Review            workflow.ArtifactRef `json:"review"`
	Approval          workflow.ArtifactRef `json:"approval"`
}
