// Package orchestration drives restart-safe deterministic runtime stages.
package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

const (
	StageStart                 = "start"
	StageImplementationRequest = "implementation_request"
	StageReasoning             = "implementation_reasoning"
	StageExecution             = "execution"
	StageVerification          = "verification"
	StageReview                = "review"
	StageAwaitingApproval      = "awaiting_approval"
)

var orderedStages = []string{
	StageStart, StageImplementationRequest, StageReasoning, StageExecution,
	StageVerification, StageReview, StageAwaitingApproval,
}

type JobLedger interface {
	Claim(context.Context, string, time.Time, time.Duration) (runtime.Job, bool, error)
	CompleteAndEnqueue(
		context.Context, string, string, uint64, workflow.ArtifactRef, *runtime.Job, time.Time,
	) error
	Retry(context.Context, string, string, uint64, time.Time) error
	Fail(context.Context, string, string, uint64, workflow.ArtifactRef, time.Time) error
}

type ArtifactStore interface {
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
}

type Handler interface {
	Handle(context.Context, runtime.Job, Identities) (workflow.ArtifactRef, error)
}

type HandlerFunc func(context.Context, runtime.Job, Identities) (workflow.ArtifactRef, error)

func (f HandlerFunc) Handle(
	ctx context.Context, job runtime.Job, ids Identities,
) (workflow.ArtifactRef, error) {
	return f(ctx, job, ids)
}

type Identities struct {
	ActivityID     string `json:"activity_id"`
	InvocationID   string `json:"invocation_id"`
	ExecutionID    string `json:"execution_id"`
	VerificationID string `json:"verification_id"`
	ReviewID       string `json:"review_id"`
	CommandID      string `json:"command_id"`
}

type Config struct {
	OwnerID        string
	ClaimTTL       time.Duration
	PollInterval   time.Duration
	MaxRetries     uint32
	InitialBackoff time.Duration
}

type Reconciler struct {
	config    Config
	ledger    JobLedger
	artifacts ArtifactStore
	handlers  map[string]Handler
	logger    *slog.Logger
	now       func() time.Time
}

func New(
	config Config, ledger JobLedger, artifacts ArtifactStore,
	handlers map[string]Handler, logger *slog.Logger,
) (*Reconciler, error) {
	if _, err := uuid.Parse(config.OwnerID); err != nil || config.ClaimTTL <= 0 ||
		config.PollInterval <= 0 || config.MaxRetries == 0 || config.InitialBackoff <= 0 {
		return nil, errors.New("invalid reconciler configuration")
	}
	for _, stage := range orderedStages {
		if handlers[stage] == nil {
			return nil, fmt.Errorf("missing stage handler %q", stage)
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		config: config, ledger: ledger, artifacts: artifacts,
		handlers: handlers, logger: logger, now: time.Now,
	}, nil
}

func (r *Reconciler) Run(ctx context.Context) error {
	timer := time.NewTicker(r.config.PollInterval)
	defer timer.Stop()
	for {
		worked, err := r.Once(ctx)
		if err != nil {
			r.logger.Error("runtime reconciliation failed", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Reconciler) Once(ctx context.Context) (bool, error) {
	now := r.now().UTC()
	job, found, err := r.ledger.Claim(ctx, r.config.OwnerID, now, r.config.ClaimTTL)
	if err != nil || !found {
		return false, err
	}
	ids := StableIdentities(job)
	logger := r.logger.With(
		"run_id", job.RunID, "stage", job.Stage, "attempt", job.Attempt,
		"job_id", job.ID, "fencing_token", job.FencingToken,
	)
	if job.TaskID != nil {
		logger = logger.With("task_id", *job.TaskID)
	}
	var result workflow.ArtifactRef
	var handleErr error
	handler, ok := r.handlers[job.Stage]
	if !ok {
		handleErr = fmt.Errorf("no handler registered for stage %q", job.Stage)
	} else {
		handlerCtx, cancel := context.WithTimeout(ctx, r.config.ClaimTTL)
		result, handleErr = handler.Handle(handlerCtx, job, ids)
		cancel()
	}
	if handleErr == nil {
		var nextJob *runtime.Job
		if next, ok := nextStage(job.Stage); ok {
			nextJob = &runtime.Job{
				ID:    StableID(job.RunID, taskValue(job.TaskID), fmt.Sprint(job.Attempt), next, "job"),
				RunID: job.RunID, TaskID: job.TaskID, Attempt: job.Attempt,
				Stage: next, AvailableAt: r.now().UTC(),
			}
		}
		if err := r.ledger.CompleteAndEnqueue(
			ctx, job.ID, r.config.OwnerID, job.FencingToken, result, nextJob, r.now(),
		); err != nil {
			return true, err
		}
		logger.Info("runtime stage completed", "result_digest", result.Digest)
		return true, nil
	}
	if errors.Is(handleErr, context.Canceled) || errors.Is(handleErr, context.DeadlineExceeded) {
		return true, handleErr
	}
	if job.RetryCount+1 < r.config.MaxRetries {
		backoff := r.config.InitialBackoff << job.RetryCount
		if backoff > time.Minute {
			backoff = time.Minute
		}
		err := r.ledger.Retry(
			ctx, job.ID, r.config.OwnerID, job.FencingToken, r.now().Add(backoff),
		)
		logger.Warn("runtime stage scheduled for retry", "error", handleErr, "backoff", backoff)
		return true, err
	}
	body, marshalErr := json.Marshal(struct {
		SchemaVersion string      `json:"schema_version"`
		Job           runtime.Job `json:"job"`
		Error         string      `json:"error"`
	}{SchemaVersion: "runtime_failure.v1", Job: job, Error: handleErr.Error()})
	if marshalErr != nil {
		return true, marshalErr
	}
	failure, err := r.artifacts.Put(ctx, body)
	if err != nil {
		return true, err
	}
	if err := r.ledger.Fail(
		ctx, job.ID, r.config.OwnerID, job.FencingToken, failure, r.now(),
	); err != nil {
		return true, err
	}
	logger.Error("runtime stage exhausted retries", "failure_digest", failure.Digest)
	return true, nil
}

func StableIdentities(job runtime.Job) Identities {
	parts := []string{job.RunID, taskValue(job.TaskID), fmt.Sprint(job.Attempt), job.Stage}
	return Identities{
		ActivityID:     StableID(append(parts, "activity")...),
		InvocationID:   StableID(append(parts, "invocation")...),
		ExecutionID:    StableID(append(parts, "execution")...),
		VerificationID: StableID(append(parts, "verification")...),
		ReviewID:       StableID(append(parts, "review")...),
		CommandID:      StableID(append(parts, "command")...),
	}
}

func StableID(parts ...string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%q", parts))).String()
}

func nextStage(stage string) (string, bool) {
	for index, value := range orderedStages {
		if value == stage && index+1 < len(orderedStages) {
			return orderedStages[index+1], true
		}
	}
	return "", false
}

func taskValue(task *string) string {
	if task == nil {
		return "-"
	}
	return *task
}
