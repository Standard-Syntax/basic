package gateway

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/migration"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"
)

//go:embed migrations/*.sql
var reasoningMigrationFiles embed.FS

func Migrate(ctx context.Context, connectionString string) error {
	return migration.Apply(
		ctx, connectionString, reasoningMigrationFiles, "migrations",
	)
}

type PostgresInvocationRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresInvocationRepository(
	pool *pgxpool.Pool,
) (*PostgresInvocationRepository, error) {
	if pool == nil {
		return nil, errors.New("PostgreSQL pool is required")
	}
	return &PostgresInvocationRepository{pool: pool}, nil
}

func (r *PostgresInvocationRepository) Begin(
	ctx context.Context, start InvocationStart,
) (InvocationHandle, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin reasoning invocation: %w", err)
	}
	handle := &postgresInvocationHandle{tx: tx, start: start}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(
		hashtextextended($1, 557047220213918512))`, start.RequestID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("lock reasoning request: %w", err)
	}
	record, found, err := readInvocation(ctx, tx, start.RequestID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	if found {
		if record.RequestArtifact.SHA256 != start.RequestArtifact.SHA256 {
			_ = tx.Rollback(ctx)
			return nil, ErrInvocationConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit reasoning replay: %w", err)
		}
		handle.tx = nil
		handle.replay = &record
	}
	return handle, nil
}

type postgresInvocationHandle struct {
	tx     pgx.Tx
	start  InvocationStart
	replay *InvocationRecord
}

func (h *postgresInvocationHandle) Replay() (InvocationRecord, bool) {
	if h.replay == nil {
		return InvocationRecord{}, false
	}
	return *h.replay, true
}

func (h *postgresInvocationHandle) Complete(
	ctx context.Context, completion InvocationCompletion,
) (InvocationRecord, error) {
	if h.tx == nil || h.replay != nil {
		return InvocationRecord{}, ErrInvocationState
	}
	details, code, summary, retryable, rejectionTime, err := rejectionColumns(
		completion.Rejection,
	)
	if err != nil {
		return InvocationRecord{}, err
	}
	var proposalURI, proposalDigest *string
	if completion.ProposalArtifact.URI != "" || completion.ProposalArtifact.SHA256 != "" {
		if completion.ProposalArtifact.URI == "" || completion.ProposalArtifact.SHA256 == "" {
			return InvocationRecord{}, ErrInvocationState
		}
		proposalURI = &completion.ProposalArtifact.URI
		proposalDigest = &completion.ProposalArtifact.SHA256
	}
	_, err = h.tx.Exec(ctx, `INSERT INTO reasoning_invocations (
		request_id,request_artifact_uri,request_digest,run_id,task_id,stage,attempt,
		agent_manifest_digest,proposal_artifact_uri,proposal_digest,provider,model,
		started_at,completed_at,input_tokens,output_tokens,provider_requests,
		final_status,rejection_code,rejection_summary,rejection_details,
		rejection_retryable,rejection_timestamp
	) VALUES (
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
		$19,$20,$21,$22,$23
	)`,
		h.start.RequestID, h.start.RequestArtifact.URI, h.start.RequestArtifact.SHA256,
		h.start.RunID, h.start.TaskID, h.start.Stage, h.start.Attempt,
		h.start.AgentManifestDigest, proposalURI, proposalDigest,
		completion.Provider, completion.Model, h.start.StartedAt, completion.CompletedAt,
		completion.Usage.InputTokens, completion.Usage.OutputTokens,
		completion.Usage.ProviderRequests, completion.Status, code, summary, details,
		retryable, rejectionTime,
	)
	if err != nil {
		return InvocationRecord{}, fmt.Errorf("insert reasoning invocation: %w", err)
	}
	if err := h.tx.Commit(ctx); err != nil {
		return InvocationRecord{}, fmt.Errorf("commit reasoning invocation: %w", err)
	}
	h.tx = nil
	return InvocationRecord{
		InvocationStart: h.start, InvocationCompletion: completion,
	}, nil
}

func (h *postgresInvocationHandle) Rollback(ctx context.Context) error {
	if h.tx == nil {
		return nil
	}
	err := h.tx.Rollback(ctx)
	h.tx = nil
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}

func rejectionColumns(
	value *reasoningv1.ProposalRejection,
) ([]byte, *int32, *string, *bool, *time.Time, error) {
	if value == nil {
		return nil, nil, nil, nil, nil, nil
	}
	details, err := json.Marshal(value.GetDetails())
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("marshal rejection details: %w", err)
	}
	code := int32(value.GetCode())
	summary := value.GetSummary()
	retryable := value.GetRetryable()
	var timestamp *time.Time
	if value.GetTimestamp() != nil {
		mapped := value.GetTimestamp().AsTime()
		timestamp = &mapped
	}
	return details, &code, &summary, &retryable, timestamp, nil
}

func readInvocation(
	ctx context.Context, tx pgx.Tx, requestID string,
) (InvocationRecord, bool, error) {
	var record InvocationRecord
	var proposalURI, proposalDigest *string
	var provider, model string
	var taskIDPointer *string
	var status string
	var rejectionCode *int32
	var rejectionSummary *string
	var rejectionDetails []byte
	var rejectionRetryable *bool
	var rejectionTimestamp *time.Time
	err := tx.QueryRow(ctx, `SELECT
		request_id,request_artifact_uri,request_digest,run_id,task_id,stage,attempt,
		agent_manifest_digest,proposal_artifact_uri,proposal_digest,provider,model,
		started_at,completed_at,input_tokens,output_tokens,provider_requests,
		final_status,rejection_code,rejection_summary,rejection_details,
		rejection_retryable,rejection_timestamp
		FROM reasoning_invocations WHERE request_id=$1`, requestID,
	).Scan(
		&record.RequestID, &record.RequestArtifact.URI, &record.RequestArtifact.SHA256,
		&record.RunID, &taskIDPointer, &record.Stage, &record.Attempt,
		&record.AgentManifestDigest, &proposalURI, &proposalDigest, &provider, &model,
		&record.StartedAt, &record.CompletedAt, &record.Usage.InputTokens,
		&record.Usage.OutputTokens, &record.Usage.ProviderRequests, &status,
		&rejectionCode, &rejectionSummary, &rejectionDetails, &rejectionRetryable,
		&rejectionTimestamp,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvocationRecord{}, false, nil
	}
	if err != nil {
		return InvocationRecord{}, false, fmt.Errorf("read reasoning invocation: %w", err)
	}
	record.TaskID = taskIDPointer
	record.Provider = provider
	record.Model = model
	record.Status = FinalStatus(status)
	if proposalURI != nil && proposalDigest != nil {
		record.ProposalArtifact = ArtifactReference{
			URI: *proposalURI, SHA256: *proposalDigest,
		}
	}
	if rejectionCode != nil {
		var details []*reasoningv1.RejectionDetail
		if err := json.Unmarshal(rejectionDetails, &details); err != nil {
			return InvocationRecord{}, false, fmt.Errorf(
				"decode reasoning rejection details: %w", err,
			)
		}
		record.Rejection = &reasoningv1.ProposalRejection{
			Code:    reasoningv1.RejectionCode(*rejectionCode),
			Summary: valueOrEmpty(rejectionSummary), Details: details,
			Retryable: valueOrFalse(rejectionRetryable),
			RequestId: record.RequestID, RunId: record.RunID, TaskId: record.TaskID,
			Attempt: record.Attempt,
		}
		if rejectionTimestamp != nil {
			record.Rejection.Timestamp = timestamppb.New(*rejectionTimestamp)
		}
	}
	return record, true, nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func valueOrFalse(value *bool) bool {
	return value != nil && *value
}
