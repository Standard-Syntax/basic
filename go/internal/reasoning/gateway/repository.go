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

func MigrationSource() migration.Source {
	return migration.Source{Files: reasoningMigrationFiles, Directory: "migrations"}
}

func Migrate(ctx context.Context, connectionString string) error {
	return migration.Apply(
		ctx, connectionString, reasoningMigrationFiles, "migrations",
	)
}

type PostgresInvocationRepository struct {
	pool   *pgxpool.Pool
	inject func(RepositoryFaultPoint) error
}

type RepositoryFaultPoint string

const (
	FaultBeforeInvocationInsert RepositoryFaultPoint = "before_invocation_insert"
	FaultBeforeInvocationCommit RepositoryFaultPoint = "before_invocation_commit"
)

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
	for {
		result, err := r.pool.Exec(ctx, `INSERT INTO reasoning_invocations (
			request_id,request_artifact_uri,request_digest,run_id,task_id,stage,attempt,
			agent_manifest_digest,started_at,input_tokens,output_tokens,
			provider_requests,state
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,0,0,'in_progress')
		ON CONFLICT (request_id) DO NOTHING`,
			start.RequestID, start.RequestArtifact.URI, start.RequestArtifact.SHA256,
			start.RunID, start.TaskID, start.Stage, start.Attempt,
			start.AgentManifestDigest, start.StartedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("reserve reasoning invocation: %w", err)
		}
		if result.RowsAffected() == 1 {
			return &postgresInvocationHandle{
				repository: r, start: start, active: true,
			}, nil
		}

		var requestDigest, state string
		if err := r.pool.QueryRow(ctx, `SELECT request_digest,state
			FROM reasoning_invocations WHERE request_id=$1`,
			start.RequestID,
		).Scan(&requestDigest, &state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("read reasoning reservation: %w", err)
		}
		if requestDigest != start.RequestArtifact.SHA256 {
			return nil, ErrInvocationConflict
		}
		if state != "in_progress" {
			record, found, err := readInvocation(ctx, r.pool, start.RequestID)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			return &postgresInvocationHandle{replay: &record}, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

type postgresInvocationHandle struct {
	repository *PostgresInvocationRepository
	start      InvocationStart
	replay     *InvocationRecord
	active     bool
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
	if !h.active || h.repository == nil || h.replay != nil {
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
	var responseURI, responseDigest *string
	if completion.ProviderResponseArtifact.URI != "" ||
		completion.ProviderResponseArtifact.SHA256 != "" {
		if completion.ProviderResponseArtifact.URI == "" ||
			completion.ProviderResponseArtifact.SHA256 == "" {
			return InvocationRecord{}, ErrInvocationState
		}
		responseURI = &completion.ProviderResponseArtifact.URI
		responseDigest = &completion.ProviderResponseArtifact.SHA256
	}
	if err := h.fault(FaultBeforeInvocationInsert); err != nil {
		return InvocationRecord{}, err
	}
	tx, err := h.repository.pool.Begin(ctx)
	if err != nil {
		return InvocationRecord{}, fmt.Errorf("begin reasoning completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `UPDATE reasoning_invocations SET
		proposal_artifact_uri=$3,proposal_digest=$4,provider=$5,model=$6,
		completed_at=$7,input_tokens=$8,output_tokens=$9,provider_requests=$10,
		state='completed',final_status=$11,rejection_code=$12,rejection_summary=$13,
		rejection_details=$14,rejection_retryable=$15,rejection_timestamp=$16,
		provider_response_artifact_uri=$17,provider_response_digest=$18,
		provider_request_id=$19
		WHERE request_id=$1 AND request_digest=$2 AND state='in_progress'`,
		h.start.RequestID, h.start.RequestArtifact.SHA256, proposalURI, proposalDigest,
		completion.Provider, completion.Model, completion.CompletedAt,
		completion.Usage.InputTokens, completion.Usage.OutputTokens,
		completion.Usage.ProviderRequests, completion.Status, code, summary, details,
		retryable, rejectionTime, responseURI, responseDigest,
		nullableString(completion.ProviderRequestID),
	)
	if err != nil {
		return InvocationRecord{}, fmt.Errorf("finalize reasoning invocation: %w", err)
	}
	if result.RowsAffected() != 1 {
		return InvocationRecord{}, ErrInvocationState
	}
	if err := h.fault(FaultBeforeInvocationCommit); err != nil {
		return InvocationRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return InvocationRecord{}, fmt.Errorf("commit reasoning invocation: %w", err)
	}
	h.active = false
	return InvocationRecord{
		InvocationStart: h.start, InvocationCompletion: completion,
	}, nil
}

func (h *postgresInvocationHandle) fault(point RepositoryFaultPoint) error {
	if h.repository != nil && h.repository.inject != nil {
		return h.repository.inject(point)
	}
	return nil
}

func (h *postgresInvocationHandle) Rollback(ctx context.Context) error {
	if !h.active || h.repository == nil {
		return nil
	}
	result, err := h.repository.pool.Exec(ctx, `DELETE FROM reasoning_invocations
		WHERE request_id=$1 AND request_digest=$2 AND state='in_progress'`,
		h.start.RequestID, h.start.RequestArtifact.SHA256,
	)
	if err != nil {
		return fmt.Errorf("release reasoning invocation: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrInvocationState
	}
	h.active = false
	return nil
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
	ctx context.Context, pool *pgxpool.Pool, requestID string,
) (InvocationRecord, bool, error) {
	var record InvocationRecord
	var proposalURI, proposalDigest *string
	var responseURI, responseDigest, providerRequestID *string
	var provider, model string
	var taskIDPointer *string
	var status string
	var rejectionCode *int32
	var rejectionSummary *string
	var rejectionDetails []byte
	var rejectionRetryable *bool
	var rejectionTimestamp *time.Time
	err := pool.QueryRow(ctx, `SELECT
		request_id,request_artifact_uri,request_digest,run_id,task_id,stage,attempt,
		agent_manifest_digest,proposal_artifact_uri,proposal_digest,provider,model,
		started_at,completed_at,input_tokens,output_tokens,provider_requests,
		final_status,rejection_code,rejection_summary,rejection_details,
		rejection_retryable,rejection_timestamp,provider_response_artifact_uri,
		provider_response_digest,provider_request_id
		FROM reasoning_invocations WHERE request_id=$1`, requestID,
	).Scan(
		&record.RequestID, &record.RequestArtifact.URI, &record.RequestArtifact.SHA256,
		&record.RunID, &taskIDPointer, &record.Stage, &record.Attempt,
		&record.AgentManifestDigest, &proposalURI, &proposalDigest, &provider, &model,
		&record.StartedAt, &record.CompletedAt, &record.Usage.InputTokens,
		&record.Usage.OutputTokens, &record.Usage.ProviderRequests, &status,
		&rejectionCode, &rejectionSummary, &rejectionDetails, &rejectionRetryable,
		&rejectionTimestamp, &responseURI, &responseDigest, &providerRequestID,
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
	if responseURI != nil && responseDigest != nil {
		record.ProviderResponseArtifact = ArtifactReference{
			URI: *responseURI, SHA256: *responseDigest,
		}
	}
	record.ProviderRequestID = valueOrEmpty(providerRequestID)
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

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
