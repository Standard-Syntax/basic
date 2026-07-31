//go:build integration

package orchestration

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/artifact"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuntimeRecoversClaimUsingPreexistingArtifact(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	ctx := t.Context()
	if err := workflow.Migrate(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	store, err := artifact.NewStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	body := []byte(`{"schema_version":"1","passed":false,"evidence":"preexisting"}`)
	preexisting, err := store.Put(ctx, body)
	if err != nil {
		t.Fatal(err)
	}

	runID := uuid.NewString()
	at := time.Now().UTC().Truncate(time.Microsecond)
	commandID, actorID := uuid.NewString(), uuid.NewString()
	if _, err := workflow.NewStore(pool).ExecuteRun(ctx, workflow.CreateRun{
		Meta: workflow.CommandEnvelope{
			CommandID: commandID,
			Actor:     workflow.Actor{ID: actorID, Kind: workflow.ActorHuman},
			Timestamp: at, CorrelationID: commandID, CausationID: commandID,
		},
		ID: runID,
	}); err != nil {
		t.Fatal(err)
	}

	ledger := runtime.NewLedger(pool)
	job := runtime.Job{
		ID: uuid.NewString(), RunID: runID, Attempt: 1,
		Stage: StageVerification, AvailableAt: at,
	}
	if err := ledger.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	crashedOwner := uuid.NewString()
	claimed, found, err := ledger.Claim(ctx, crashedOwner, at, time.Second)
	if err != nil || !found {
		t.Fatalf("initial claim = %#v, %v, %v", claimed, found, err)
	}

	handlerCalls := 0
	handlers := make(map[string]Handler, len(orderedStages))
	for _, stageName := range orderedStages {
		handlers[stageName] = HandlerFunc(func(
			context.Context, runtime.Job, Identities,
		) (HandlerResult, error) {
			t.Fatalf("unexpected recovery stage")
			return HandlerResult{}, nil
		})
	}
	handlers[StageVerification] = HandlerFunc(func(
		ctx context.Context, recovered runtime.Job, _ Identities,
	) (HandlerResult, error) {
		handlerCalls++
		if recovered.FencingToken != claimed.FencingToken+1 {
			t.Fatalf("recovery fence = %d, want %d",
				recovered.FencingToken, claimed.FencingToken+1)
		}
		got, err := store.Get(ctx, preexisting)
		if err != nil {
			return HandlerResult{}, err
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("preexisting artifact = %q", got)
		}
		return HandlerResult{Artifact: preexisting, Continue: false}, nil
	})

	reconciler, err := New(Config{
		OwnerID: uuid.NewString(), ClaimTTL: time.Second,
		PollInterval: time.Millisecond, MaxRetries: 3, InitialBackoff: time.Millisecond,
	}, ledger, store, handlers, nil)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.now = func() time.Time { return at.Add(2 * time.Second) }
	worked, err := reconciler.Once(ctx)
	if err != nil || !worked || handlerCalls != 1 {
		t.Fatalf("recovery = worked:%t calls:%d err:%v", worked, handlerCalls, err)
	}

	var state, resultURI, resultDigest string
	var fencingToken uint64
	if err := pool.QueryRow(ctx, `SELECT state,result_uri,result_digest,fencing_token
		FROM runtime_stage_jobs WHERE job_id=$1`, job.ID).
		Scan(&state, &resultURI, &resultDigest, &fencingToken); err != nil {
		t.Fatal(err)
	}
	if state != "COMPLETED" || resultURI != preexisting.URI ||
		resultDigest != preexisting.Digest || fencingToken != claimed.FencingToken+1 {
		t.Fatalf("recovered row = state:%s uri:%s digest:%s fence:%d",
			state, resultURI, resultDigest, fencingToken)
	}
	if _, found, err := ledger.Claim(ctx, uuid.NewString(), at.Add(4*time.Second), time.Second); err != nil || found {
		t.Fatalf("completed recovery reclaimed = %t, %v", found, err)
	}
}
