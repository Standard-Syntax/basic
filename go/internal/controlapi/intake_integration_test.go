//go:build integration

package controlapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/artifact"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type countingArtifacts struct {
	store *artifact.Store
	mu    sync.Mutex
	puts  int
}

func (s *countingArtifacts) Put(
	ctx context.Context, body []byte,
) (workflow.ArtifactRef, error) {
	s.mu.Lock()
	s.puts++
	s.mu.Unlock()
	return s.store.Put(ctx, body)
}

func (s *countingArtifacts) Get(
	ctx context.Context, ref workflow.ArtifactRef,
) ([]byte, error) {
	return s.store.Get(ctx, ref)
}

func (s *countingArtifacts) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

type intakeFixture struct {
	pool        *pgxpool.Pool
	coordinator *RunIntakeCoordinator
	artifacts   *countingArtifacts
	repository  string
	baseCommit  string
	request     RunIntakeRequest
}

func newIntakeFixture(t *testing.T) *intakeFixture {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	if err := workflow.Migrate(t.Context(), databaseURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(arguments ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", repository}, arguments...)...)
		body, commandErr := command.CombinedOutput()
		if commandErr != nil {
			t.Fatalf("git %v: %v: %s", arguments, commandErr, body)
		}
		return string(body)
	}
	git("init", "-q")
	git("config", "user.name", "Atomic Intake Test")
	git("config", "user.email", "atomic-intake@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("bound\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("commit", "-qm", "fixture")
	baseCommit := git("rev-parse", "HEAD")[:40]
	artifactStore, err := artifact.NewStore(filepath.Join(t.TempDir(), "artifacts"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = artifactStore.Close() })
	artifacts := &countingArtifacts{store: artifactStore}
	workflowStore := workflow.NewStore(pool)
	coordinator, err := NewRunIntakeCoordinator(pool, workflowStore, artifacts, repository)
	if err != nil {
		t.Fatal(err)
	}
	key, principal, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Now().UTC().Truncate(time.Microsecond)
	content := json.RawMessage(`{"objective":"atomic"}`)
	requestDigest := runtime.Digest([]byte(`{"wire":"request"}`))
	request := RunIntakeRequest{
		Idempotency: runtime.IdempotencyRequest{
			Key: key, Method: http.MethodPost, Target: "/v1/runs",
			PrincipalID: principal, RequestDigest: requestDigest,
		},
		Command: workflow.CreateRun{Meta: workflow.CommandEnvelope{
			CommandID: key, Actor: workflow.Actor{ID: principal, Kind: workflow.ActorHuman},
			Timestamp: at, CorrelationID: key, CausationID: key,
		}, ID: runID},
		Content: content, BaseCommit: baseCommit,
	}
	return &intakeFixture{
		pool: pool, coordinator: coordinator, artifacts: artifacts,
		repository: repository, baseCommit: baseCommit, request: request,
	}
}

func TestAtomicRunIntakeRollsBackEveryPrecommitFailure(t *testing.T) {
	points := []IntakeFaultPoint{
		FaultAfterReservation, FaultAfterIntakeCAS, FaultAfterRepositoryCAS,
		FaultAfterWorkflow, FaultAfterBinding, FaultAfterResponse, FaultIntakeBeforeCommit,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			fixture := newIntakeFixture(t)
			fixture.coordinator.inject = func(actual IntakeFaultPoint) error {
				if actual == point {
					return errors.New("injected intake failure")
				}
				return nil
			}
			if _, err := fixture.coordinator.Accept(t.Context(), fixture.request); err == nil {
				t.Fatal("injected failure succeeded")
			}
			assertNoAcceptedRun(t, fixture)
			fixture.coordinator.inject = nil
			result, err := fixture.coordinator.Accept(t.Context(), fixture.request)
			if err != nil || result.Replay || result.StatusCode != http.StatusCreated {
				t.Fatalf("retry result=%#v err=%v", result, err)
			}
			assertCompleteRun(t, fixture)
		})
	}
}

func TestAtomicRunIntakeReplaysPostcommitCrashWithoutExternalWork(t *testing.T) {
	fixture := newIntakeFixture(t)
	fixture.coordinator.inject = func(point IntakeFaultPoint) error {
		if point == FaultIntakeAfterCommit {
			return errors.New("connection lost after commit")
		}
		return nil
	}
	if _, err := fixture.coordinator.Accept(t.Context(), fixture.request); err == nil {
		t.Fatal("postcommit failure was not exposed")
	}
	assertCompleteRun(t, fixture)
	puts := fixture.artifacts.count()
	if err := os.Rename(
		filepath.Join(fixture.repository, ".git"), filepath.Join(fixture.repository, ".git-hidden"),
	); err != nil {
		t.Fatal(err)
	}
	fixture.coordinator.inject = nil
	replay, err := fixture.coordinator.Accept(t.Context(), fixture.request)
	if err != nil || !replay.Replay || replay.StatusCode != http.StatusCreated {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if fixture.artifacts.count() != puts {
		t.Fatal("replay published CAS artifacts")
	}
	var stored []byte
	if err := fixture.pool.QueryRow(t.Context(), `SELECT response FROM runtime_api_idempotency
		WHERE idempotency_key=$1`, fixture.request.Idempotency.Key).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(replay.Response) != string(stored) {
		t.Fatalf("replay bytes=%s stored=%s", replay.Response, stored)
	}
	changed := fixture.request
	changed.Idempotency.RequestDigest = runtime.Digest([]byte("changed bytes"))
	if _, err := fixture.coordinator.Accept(t.Context(), changed); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("changed request error=%v", err)
	}
}

func TestPostRunsExactHTTPReplaySkipsGitAndRejectsChangedBytes(t *testing.T) {
	fixture := newIntakeFixture(t)
	token := "atomic-intake-token"
	tokenDigest := sha256.Sum256([]byte(token))
	principal := fixture.request.Idempotency.PrincipalID
	workflowStore := workflow.NewStore(fixture.pool)
	server, err := New(Config{
		ServiceActorID: uuid.NewString(), MaxBodyBytes: 1 << 20,
		Principals: []Principal{{
			ID: principal, TokenSHA256: hex.EncodeToString(tokenDigest[:]),
			Roles: []Role{RoleOperator},
		}},
	}, workflowStore, runtime.NewLedger(fixture.pool), fixture.coordinator,
		fixture.artifacts, runtime.NewBindingRepository(fixture.pool), fakeApproval{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, runID := uuid.NewString(), uuid.NewString()
	body := []byte(`{"run_id":"` + runID + `","base_commit":"` + fixture.baseCommit +
		`","content":{"objective":"atomic"},"decision_timestamp":"2026-07-31T12:00:00Z"}`)
	call := func(value []byte) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(value))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	first := call(body)
	if first.Code != http.StatusCreated || first.Header().Get("Idempotent-Replay") != "" {
		t.Fatalf("first status=%d header=%q body=%s", first.Code,
			first.Header().Get("Idempotent-Replay"), first.Body)
	}
	if err := os.Rename(
		filepath.Join(fixture.repository, ".git"), filepath.Join(fixture.repository, ".git-hidden"),
	); err != nil {
		t.Fatal(err)
	}
	puts := fixture.artifacts.count()
	replay := call(body)
	if replay.Code != http.StatusCreated || replay.Header().Get("Idempotent-Replay") != "true" ||
		replay.Body.String() != first.Body.String() || fixture.artifacts.count() != puts {
		t.Fatalf("replay status=%d header=%q body=%s first=%s puts=%d/%d",
			replay.Code, replay.Header().Get("Idempotent-Replay"), replay.Body,
			first.Body, fixture.artifacts.count(), puts)
	}
	changed := bytes.Replace(body, []byte(`"atomic"`), []byte(`"changed"`), 1)
	conflict := call(changed)
	if conflict.Code != http.StatusConflict || fixture.artifacts.count() != puts {
		t.Fatalf("changed status=%d body=%s puts=%d/%d",
			conflict.Code, conflict.Body, fixture.artifacts.count(), puts)
	}
}

func TestAtomicRunIntakeConcurrentDuplicateAndRunIdentityConflict(t *testing.T) {
	fixture := newIntakeFixture(t)
	type outcome struct {
		result *runtime.IdempotencyResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			result, err := fixture.coordinator.Accept(context.Background(), fixture.request)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	var successes int
	for range 2 {
		got := <-outcomes
		if got.err == nil && got.result != nil {
			successes++
		} else if !errors.Is(got.err, runtime.ErrInProgress) {
			t.Fatalf("duplicate error=%v", got.err)
		}
	}
	if successes == 0 {
		t.Fatal("no duplicate request succeeded")
	}
	assertCompleteRun(t, fixture)

	other := fixture.request
	other.Idempotency.Key = uuid.NewString()
	other.Command.Meta.CommandID = other.Idempotency.Key
	other.Command.Meta.CorrelationID = other.Idempotency.Key
	other.Command.Meta.CausationID = other.Idempotency.Key
	if _, err := fixture.coordinator.Accept(t.Context(), other); !errors.Is(err, workflow.ErrInvalidTransition) {
		t.Fatalf("same run under another key error=%v", err)
	}
	assertCompleteRun(t, fixture)
}

func TestAtomicRunIntakeExpiredGenerationCannotCommitOrAbandon(t *testing.T) {
	fixture := newIntakeFixture(t)
	var takeover uint64
	fixture.coordinator.inject = func(point IntakeFaultPoint) error {
		if point != FaultAfterRepositoryCAS {
			return nil
		}
		if _, err := fixture.pool.Exec(t.Context(), `UPDATE runtime_api_idempotency
			SET reservation_expires_at=now()-interval '1 second'
			WHERE idempotency_key=$1`, fixture.request.Idempotency.Key); err != nil {
			return err
		}
		result, err := fixture.coordinator.ledger.BeginIdempotency(
			t.Context(), fixture.request.Idempotency,
		)
		if err != nil {
			return err
		}
		takeover = result.FencingToken
		return nil
	}
	if _, err := fixture.coordinator.Accept(t.Context(), fixture.request); !errors.Is(err, runtime.ErrStaleFence) {
		t.Fatalf("stale intake error=%v", err)
	}
	if takeover != 2 {
		t.Fatalf("takeover generation=%d", takeover)
	}
	assertNoAcceptedRun(t, fixture)
	var generation uint64
	var completed *time.Time
	if err := fixture.pool.QueryRow(t.Context(), `SELECT reservation_generation,completed_at
		FROM runtime_api_idempotency WHERE idempotency_key=$1`,
		fixture.request.Idempotency.Key).Scan(&generation, &completed); err != nil {
		t.Fatal(err)
	}
	if generation != takeover || completed != nil {
		t.Fatalf("generation=%d completed=%v", generation, completed)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE runtime_api_idempotency
		SET reservation_expires_at=now()-interval '1 second'
		WHERE idempotency_key=$1`, fixture.request.Idempotency.Key); err != nil {
		t.Fatal(err)
	}
	fixture.coordinator.inject = nil
	result, err := fixture.coordinator.Accept(t.Context(), fixture.request)
	if err != nil || result.Replay || result.FencingToken != takeover+1 {
		t.Fatalf("recovered result=%#v error=%v", result, err)
	}
	assertCompleteRun(t, fixture)
}

func assertNoAcceptedRun(t *testing.T, fixture *intakeFixture) {
	t.Helper()
	for table, column := range map[string]string{
		"workflow_runs": "run_id", "workflow_commands": "aggregate_id",
		"workflow_events": "aggregate_id", "runtime_run_bindings": "run_id",
		"runtime_stage_jobs": "run_id",
	} {
		var count int
		query := "SELECT count(*) FROM " + table + " WHERE " + column + "=$1"
		if err := fixture.pool.QueryRow(t.Context(), query, fixture.request.Command.ID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows=%d after rollback", table, count)
		}
	}
	var completed *time.Time
	if err := fixture.pool.QueryRow(t.Context(), `SELECT completed_at
		FROM runtime_api_idempotency WHERE idempotency_key=$1`,
		fixture.request.Idempotency.Key).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if completed != nil {
		t.Fatal("idempotency response completed after rollback")
	}
}

func assertCompleteRun(t *testing.T, fixture *intakeFixture) {
	t.Helper()
	var state string
	var runCount, bindingCount, commandCount, eventCount int
	err := fixture.pool.QueryRow(t.Context(), `SELECT state,
		(SELECT count(*) FROM workflow_runs WHERE run_id=$1),
		(SELECT count(*) FROM runtime_run_bindings WHERE run_id=$1),
		(SELECT count(*) FROM workflow_commands WHERE aggregate_id=$1),
		(SELECT count(*) FROM workflow_events WHERE aggregate_id=$1)
		FROM workflow_runs WHERE run_id=$1`, fixture.request.Command.ID).Scan(
		&state, &runCount, &bindingCount, &commandCount, &eventCount)
	if err != nil {
		t.Fatal(err)
	}
	if state != string(workflow.RunStateDraft) || runCount != 1 || bindingCount != 1 ||
		commandCount != 1 || eventCount != 1 {
		t.Fatalf("state=%s run=%d binding=%d command=%d event=%d",
			state, runCount, bindingCount, commandCount, eventCount)
	}
	var baseCommit, repositoryDigest string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT base_commit,repository_map_digest
		FROM runtime_run_bindings WHERE run_id=$1`, fixture.request.Command.ID).Scan(
		&baseCommit, &repositoryDigest); err != nil {
		t.Fatal(err)
	}
	if baseCommit != fixture.baseCommit || repositoryDigest == "" {
		t.Fatalf("binding base=%s repository_digest=%s", baseCommit, repositoryDigest)
	}
}
