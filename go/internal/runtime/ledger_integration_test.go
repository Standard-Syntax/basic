//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
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
	ledger := NewLedger(pool)
	job := Job{
		ID: uuid.NewString(), RunID: runID, Attempt: 1, Stage: "start", AvailableAt: at,
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
			claimed, found, claimErr := ledger.Claim(ctx, owner, at, time.Second)
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
	second, found, err := ledger.Claim(ctx, owners[1], at.Add(2*time.Second), time.Second)
	if err != nil || !found || second.FencingToken != first.FencingToken+1 {
		t.Fatalf("takeover = %#v, %v, %v", second, found, err)
	}
	digest := Digest([]byte("result"))
	ref := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	if err := ledger.Complete(ctx, job.ID, *first.ClaimOwner, first.FencingToken, ref, at); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale completion = %v", err)
	}
	if err := ledger.Complete(ctx, job.ID, owners[1], second.FencingToken, ref, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ledger.Claim(ctx, owners[0], at.Add(4*time.Second), time.Second); err != nil || found {
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
