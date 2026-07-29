package gateway

import (
	"context"
	"errors"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
)

var (
	ErrInvocationConflict = errors.New("reasoning request ID conflict")
	ErrInvocationState    = errors.New("invalid reasoning invocation state")
)

type FinalStatus string

const (
	StatusAccepted FinalStatus = "accepted"
	StatusRejected FinalStatus = "rejected"
)

type InvocationStart struct {
	RequestID           string
	RequestArtifact     ArtifactReference
	RunID               string
	TaskID              *string
	Stage               string
	Attempt             uint32
	AgentManifestDigest string
	StartedAt           time.Time
}

type InvocationCompletion struct {
	ProposalArtifact         ArtifactReference
	ProviderResponseArtifact ArtifactReference
	ProviderRequestID        string
	Provider                 string
	Model                    string
	CompletedAt              time.Time
	Usage                    Usage
	Status                   FinalStatus
	Rejection                *reasoningv1.ProposalRejection
}

type InvocationRecord struct {
	InvocationStart
	InvocationCompletion
}

type InvocationHandle interface {
	Replay() (InvocationRecord, bool)
	Complete(context.Context, InvocationCompletion) (InvocationRecord, error)
	Rollback(context.Context) error
}

type InvocationRepository interface {
	Begin(context.Context, InvocationStart) (InvocationHandle, error)
}
