//go:build integration

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresClaimExpiryAndFencing(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	if err := workflow.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	runID, actorID, commandID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Date(1999, time.January, 1, 0, 0, 0, 0, time.UTC)
	_, err = workflow.NewStore(pool).ExecuteRun(ctx, workflow.CreateRun{
		Meta: workflow.CommandEnvelope{
			CommandID: commandID, Actor: workflow.Actor{ID: actorID, Kind: workflow.ActorHuman},
			Timestamp: at, CorrelationID: commandID, CausationID: commandID,
		},
		ID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger(pool)
	claimAt := time.Unix(1, 0).UTC()
	job := Job{
		ID: uuid.NewString(), RunID: runID, Attempt: 1, Stage: "start", AvailableAt: claimAt,
	}
	if err := ledger.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	owners := []string{uuid.NewString(), uuid.NewString()}
	results := make(chan Job, 2)
	var wg sync.WaitGroup
	for _, owner := range owners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, found, claimErr := ledger.Claim(ctx, owner, claimAt, time.Second)
			if claimErr != nil {
				t.Errorf("claim: %v", claimErr)
			} else if found {
				results <- claimed
			}
		}()
	}
	wg.Wait()
	close(results)
	var first Job
	count := 0
	for claimed := range results {
		first = claimed
		count++
	}
	if count != 1 {
		t.Fatalf("claim winners = %d", count)
	}
	second, found, err := ledger.Claim(ctx, owners[1], claimAt.Add(2*time.Second), time.Second)
	if err != nil || !found || second.FencingToken != first.FencingToken+1 {
		t.Fatalf("takeover = %#v, %v, %v", second, found, err)
	}
	digest := Digest([]byte("result"))
	ref := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	if err := ledger.Complete(ctx, job.ID, *first.ClaimOwner, first.FencingToken, ref, at); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale completion = %v", err)
	}
	if err := ledger.Complete(ctx, job.ID, owners[1], second.FencingToken, ref, claimAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ledger.Claim(ctx, owners[0], claimAt.Add(4*time.Second), time.Second); err != nil || found {
		t.Fatalf("terminal reclaim = %v, %v", found, err)
	}
}

func TestPostgresCompleteAndEnqueueIsAtomic(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	if err := workflow.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	runID, actorID, commandID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Date(1998, time.January, 1, 0, 0, 0, 0, time.UTC)
	_, err = workflow.NewStore(pool).ExecuteRun(ctx, workflow.CreateRun{
		Meta: workflow.CommandEnvelope{
			CommandID: commandID, Actor: workflow.Actor{ID: actorID, Kind: workflow.ActorHuman},
			Timestamp: at, CorrelationID: commandID, CausationID: commandID,
		},
		ID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger(pool)
	current := Job{
		ID: uuid.NewString(), RunID: runID, Attempt: 1, Stage: "start", AvailableAt: at,
	}
	if err := ledger.Enqueue(ctx, current); err != nil {
		t.Fatal(err)
	}
	owner := uuid.NewString()
	claimed, found, err := ledger.Claim(ctx, owner, at, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim = %#v, %v, %v", claimed, found, err)
	}
	digest := Digest([]byte("result"))
	result := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	invalidNext := Job{
		ID: uuid.NewString(), RunID: uuid.NewString(), Attempt: 1,
		Stage: "next", AvailableAt: at,
	}
	if err := ledger.CompleteAndEnqueue(
		ctx, claimed.ID, owner, claimed.FencingToken, result, &invalidNext, at,
	); err == nil {
		t.Fatal("invalid successor unexpectedly committed")
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM runtime_stage_jobs WHERE job_id=$1`, claimed.ID).
		Scan(&state); err != nil || state != "CLAIMED" {
		t.Fatalf("source state after rollback = %q, %v", state, err)
	}
	next := Job{
		ID: uuid.NewString(), RunID: runID, Attempt: 1,
		Stage: "next", AvailableAt: at,
	}
	if err := ledger.CompleteAndEnqueue(
		ctx, claimed.ID, owner, claimed.FencingToken, result, &next, at,
	); err != nil {
		t.Fatal(err)
	}
	var sourceState, nextState string
	if err := pool.QueryRow(ctx, `SELECT state FROM runtime_stage_jobs WHERE job_id=$1`, claimed.ID).
		Scan(&sourceState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM runtime_stage_jobs WHERE job_id=$1`, next.ID).
		Scan(&nextState); err != nil {
		t.Fatal(err)
	}
	if sourceState != "COMPLETED" || nextState != "READY" {
		t.Fatalf("states = %q, %q", sourceState, nextState)
	}
}

func TestPostgresTransientRescheduleUsesSeparateCounter(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := t.Context()
	if err := workflow.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	runID, actorID, commandID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Date(1997, time.January, 1, 0, 0, 0, 0, time.UTC)
	_, err = workflow.NewStore(pool).ExecuteRun(ctx, workflow.CreateRun{
		Meta: workflow.CommandEnvelope{
			CommandID: commandID, Actor: workflow.Actor{ID: actorID, Kind: workflow.ActorHuman},
			Timestamp: at, CorrelationID: commandID, CausationID: commandID,
		},
		ID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger(pool)
	job := Job{ID: uuid.NewString(), RunID: runID, Attempt: 1, Stage: "start", AvailableAt: at}
	if err := ledger.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	owner := uuid.NewString()
	claimed, found, err := ledger.Claim(ctx, owner, at, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim=%+v found=%v err=%v", claimed, found, err)
	}
	next := at.Add(time.Second)
	if err := ledger.RescheduleTransient(ctx, job.ID, owner, claimed.FencingToken, next); err != nil {
		t.Fatal(err)
	}
	claimed, found, err = ledger.Claim(ctx, owner, next, time.Minute)
	if err != nil || !found || claimed.RetryCount != 0 || claimed.TransientRescheduleCount != 1 {
		t.Fatalf("reclaim=%+v found=%v err=%v", claimed, found, err)
	}
}

func TestPostgresRuntimeBindingsCheckpointOnceAndRejectMutation(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	if err := workflow.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	runID, actorID, commandID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Now().UTC().Truncate(time.Microsecond)
	_, err = workflow.NewStore(pool).ExecuteRun(ctx, workflow.CreateRun{
		Meta: workflow.CommandEnvelope{
			CommandID: commandID, Actor: workflow.Actor{ID: actorID, Kind: workflow.ActorHuman},
			Timestamp: at, CorrelationID: commandID, CausationID: commandID,
		},
		ID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := NewBindingRepository(pool)
	intake := artifactRef("intake")
	repositoryMap := artifactRef("repository")
	policy := artifactRef("policy")
	binding := RunBinding{
		RunID: runID, Intake: intake,
		BaseCommit:    "0123456789012345678901234567890123456789",
		RepositoryMap: &repositoryMap, CreatedAt: at,
		Policy: &policy, ExecutionImageDigest: "sha256:" + strings.Repeat("a", 64),
		VerificationImageDigest: "sha256:" + strings.Repeat("b", 64),
	}
	if err := repository.CreateRun(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateRun(ctx, binding); err != nil {
		t.Fatalf("exact intake replay: %v", err)
	}
	if err := repository.CheckpointRepository(ctx, runID, repositoryMap); err != nil {
		t.Fatal(err)
	}
	if err := repository.CheckpointRepository(ctx, runID, repositoryMap); err != nil {
		t.Fatalf("exact checkpoint replay: %v", err)
	}
	if err := repository.CheckpointRepository(ctx, runID, artifactRef("changed")); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed checkpoint = %v", err)
	}
	got, err := repository.GetRun(ctx, runID)
	if err != nil || got.RepositoryMap == nil || *got.RepositoryMap != repositoryMap {
		t.Fatalf("binding = %#v, %v", got, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM runtime_run_bindings WHERE run_id=$1`, runID); err == nil {
		t.Fatal("binding deletion succeeded")
	}
}

func TestPostgresRuntimeBindingLegacyRepositoryMapFailsClosed(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	if err := workflow.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	runID, actorID, commandID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Now().UTC().Truncate(time.Microsecond)
	_, err = workflow.NewStore(pool).ExecuteRun(ctx, workflow.CreateRun{
		Meta: workflow.CommandEnvelope{
			CommandID: commandID, Actor: workflow.Actor{ID: actorID, Kind: workflow.ActorHuman},
			Timestamp: at, CorrelationID: commandID, CausationID: commandID,
		},
		ID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	intake := artifactRef("legacy-intake")
	baseCommit := "0123456789012345678901234567890123456789"
	if _, err := pool.Exec(ctx, `INSERT INTO runtime_run_bindings
		(run_id,intake_uri,intake_digest,base_commit,created_at)
		VALUES ($1,$2,$3,$4,$5)`, runID, intake.URI, intake.Digest, baseCommit, at); err != nil {
		t.Fatal(err)
	}
	repository := NewBindingRepository(pool)
	legacy, err := repository.GetRun(ctx, runID)
	if err != nil || legacy.ExecutionImageDigest != "" || legacy.VerificationImageDigest != "" {
		t.Fatalf("read legacy binding = %#v, %v", legacy, err)
	}
	repositoryMap := artifactRef("repository")
	err = repository.CreateRun(ctx, RunBinding{
		RunID: runID, Intake: intake, BaseCommit: baseCommit,
		RepositoryMap: &repositoryMap, Policy: &repositoryMap,
		ExecutionImageDigest:    "sha256:" + strings.Repeat("a", 64),
		VerificationImageDigest: "sha256:" + strings.Repeat("b", 64), CreatedAt: at,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy incomplete binding error=%v", err)
	}
}

func TestPostgresIdempotencyReservationCanBeRecoveredAfterExpiry(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	if err := workflow.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ledger := NewLedger(pool)
	request := IdempotencyRequest{
		Key: uuid.NewString(), Method: "POST", Target: "/v1/runs",
		PrincipalID: uuid.NewString(), RequestDigest: Digest([]byte("request")),
	}
	first, err := ledger.BeginIdempotency(ctx, request)
	if err != nil || first.Replay || first.FencingToken != 1 {
		t.Fatalf("initial reservation = %#v, %v", first, err)
	}
	if _, err := ledger.BeginIdempotency(ctx, request); !errors.Is(err, ErrInProgress) {
		t.Fatalf("live reservation = %v", err)
	}
	if err := ledger.RenewIdempotency(ctx, request.Key, first.FencingToken, time.Minute); err != nil {
		t.Fatalf("renew reservation: %v", err)
	}
	var renewed bool
	if err := pool.QueryRow(ctx, `SELECT reservation_expires_at > now()+interval '50 seconds'
		FROM runtime_api_idempotency WHERE idempotency_key=$1`, request.Key).Scan(&renewed); err != nil || !renewed {
		t.Fatalf("reservation not renewed: renewed=%v err=%v", renewed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runtime_api_idempotency
		SET reservation_expires_at=now()-interval '1 second'
		WHERE idempotency_key=$1`, request.Key); err != nil {
		t.Fatal(err)
	}
	recovered, err := ledger.BeginIdempotency(ctx, request)
	if err != nil || recovered.Replay || recovered.FencingToken != 2 {
		t.Fatalf("recovered reservation = %#v, %v", recovered, err)
	}
	if err := ledger.RenewIdempotency(
		ctx, request.Key, first.FencingToken, time.Minute,
	); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale renewal = %v", err)
	}
	if err := ledger.AbandonIdempotency(
		ctx, request.Key, first.FencingToken,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale abandon = %v", err)
	}
	if err := ledger.AbandonIdempotency(
		ctx, request.Key, recovered.FencingToken,
	); err != nil {
		t.Fatalf("current abandon = %v", err)
	}
	current, err := ledger.BeginIdempotency(ctx, request)
	if err != nil || current.Replay || current.FencingToken != 3 {
		t.Fatalf("post-abandon reservation = %#v, %v", current, err)
	}
	response := json.RawMessage(`{"ok":true}`)
	if err := ledger.CompleteIdempotency(
		ctx, request.Key, recovered.FencingToken, http.StatusCreated, response,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale completion = %v", err)
	}
	if err := ledger.CompleteIdempotency(
		ctx, request.Key, current.FencingToken, http.StatusCreated, response,
	); err != nil {
		t.Fatalf("current completion = %v", err)
	}
	replay, err := ledger.BeginIdempotency(ctx, request)
	var gotResponse, wantResponse any
	gotJSONErr := json.Unmarshal(replay.Response, &gotResponse)
	wantJSONErr := json.Unmarshal(response, &wantResponse)
	if err != nil || gotJSONErr != nil || wantJSONErr != nil || !replay.Replay ||
		replay.StatusCode != http.StatusCreated || !reflect.DeepEqual(gotResponse, wantResponse) {
		t.Fatalf("completed replay = %#v, %v", replay, err)
	}
}

func artifactRef(value string) workflow.ArtifactRef {
	digest := Digest([]byte(value))
	return workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
}
