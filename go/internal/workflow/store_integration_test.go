//go:build integration

package workflow

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if err := Migrate(t.Context(), connectionString); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), connectionString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func uniqueEnvelope(kind ActorKind, revision uint64) CommandEnvelope {
	return CommandEnvelope{
		CommandID: uuid.NewString(), Actor: Actor{ID: uuid.NewString(), Kind: kind},
		ExpectedRevision: revision, Timestamp: time.Now().UTC(),
		CorrelationID: uuid.NewString(), CausationID: uuid.NewString(),
	}
}

func executeRun(t *testing.T, store *Store, command RunCommand) CommandResult {
	t.Helper()
	result, err := store.ExecuteRun(t.Context(), command)
	if err != nil {
		t.Fatalf("execute %T: %v", command, err)
	}
	return result
}

func assertRunCommandTypeConflict(
	t *testing.T, store *Store, command ProposeSpecification,
) {
	t.Helper()
	typeConflict := ApproveSpecification{
		Meta: command.Meta, ID: command.ID, Specification: command.Specification,
	}
	if _, err := store.ExecuteRun(t.Context(), typeConflict); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("same-shaped run command type conflict error = %v", err)
	}
}

func assertTaskCommandTypeConflict(t *testing.T, store *Store, command LeaseTask) {
	t.Helper()
	typeConflict := ReleaseTaskLease{
		Meta: command.Meta, Run: command.Run, ID: command.ID, Lease: command.Lease,
	}
	if _, err := store.ExecuteTask(t.Context(), typeConflict); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("same-shaped task command type conflict error = %v", err)
	}
}

func createPersistedRun(t *testing.T, store *Store) (string, uint64) {
	t.Helper()
	id := uuid.NewString()
	result := executeRun(t, store, CreateRun{
		Meta: uniqueEnvelope(ActorHuman, 0), ID: id,
	})
	return id, result.Revision
}

func TestStoreCommandReplayConflictAndRollback(t *testing.T) {
	pool := integrationPool(t)
	store := NewStore(pool)
	runID, revision := createPersistedRun(t, store)
	command := ProposeSpecification{
		Meta: uniqueEnvelope(ActorWorkflowService, revision), ID: runID,
		Specification: testSpec,
	}
	first := executeRun(t, store, command)
	replay := executeRun(t, store, command)
	if !replay.Replay || replay.Revision != first.Revision {
		t.Fatalf("unexpected replay: %#v", replay)
	}
	conflict := command
	conflict.Specification = artifact("artifact://specifications/conflict", 'b')
	if _, err := store.ExecuteRun(t.Context(), conflict); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("conflicting command error = %v", err)
	}
	assertRunCommandTypeConflict(t, store, command)

	for _, point := range []FaultPoint{FaultBeforeEvents, FaultBeforeSnapshot, FaultBeforeCommit} {
		t.Run(string(point), func(t *testing.T) {
			id := uuid.NewString()
			failing := NewStore(pool)
			failing.inject = func(actual FaultPoint) error {
				if actual == point {
					return errors.New("injected failure")
				}
				return nil
			}
			meta := uniqueEnvelope(ActorHuman, 0)
			if _, err := failing.ExecuteRun(t.Context(), CreateRun{Meta: meta, ID: id}); err == nil {
				t.Fatal("injected failure succeeded")
			}
			for table, filter := range map[string][2]string{
				"workflow_runs":     {"run_id", id},
				"workflow_commands": {"command_id", meta.CommandID},
				"workflow_events":   {"command_id", meta.CommandID},
			} {
				var count int
				query := "SELECT count(*) FROM " + table + " WHERE " + filter[0] + "=$1"
				if err := pool.QueryRow(t.Context(), query, filter[1]).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("%s retained %d rows", table, count)
				}
			}
		})
	}
}

func TestStoreConcurrentRevisionHasExactlyOneWinner(t *testing.T) {
	pool := integrationPool(t)
	store := NewStore(pool)
	runID, revision := createPersistedRun(t, store)
	commands := []ProposeSpecification{
		{Meta: uniqueEnvelope(ActorWorkflowService, revision), ID: runID, Specification: testSpec},
		{Meta: uniqueEnvelope(ActorWorkflowService, revision), ID: runID,
			Specification: artifact("artifact://specifications/other", 'c')},
	}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for _, command := range commands {
		wait.Add(1)
		go func(command ProposeSpecification) {
			defer wait.Done()
			_, err := store.ExecuteRun(context.Background(), command)
			results <- err
		}(command)
	}
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent commands = %d", successes)
	}
	var revisionAfter, events int
	if err := pool.QueryRow(t.Context(),
		`SELECT revision FROM workflow_runs WHERE run_id=$1`, runID).Scan(&revisionAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM workflow_events WHERE aggregate_id=$1`, runID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if revisionAfter != 2 || events != 2 {
		t.Fatalf("revision=%d events=%d", revisionAfter, events)
	}
}

func TestStoreFullLifecycleAndAppendOnlyEvents(t *testing.T) {
	pool := integrationPool(t)
	store := NewStore(pool)
	runID, revision := createPersistedRun(t, store)
	specResult := executeRun(t, store, ProposeSpecification{
		Meta: uniqueEnvelope(ActorWorkflowService, revision), ID: runID, Specification: testSpec,
	})
	approvedSpec := executeRun(t, store, ApproveSpecification{
		Meta: uniqueEnvelope(ActorHuman, specResult.Revision), ID: runID, Specification: testSpec,
	})
	graph := artifact("artifact://task-graphs/integration", 'd')
	graphReview := executeRun(t, store, ProposeTaskGraph{
		Meta: uniqueEnvelope(ActorWorkflowService, approvedSpec.Revision), ID: runID, TaskGraph: graph,
	})
	taskID := uuid.NewString()
	ready := executeRun(t, store, ApproveTaskGraph{
		Meta: uniqueEnvelope(ActorHuman, graphReview.Revision), ID: runID, TaskGraph: graph,
		Tasks: []TaskDefinition{{ID: taskID, MaxAttempts: 2}},
	})
	executingRun := executeRun(t, store, StartRun{
		Meta: uniqueEnvelope(ActorWorkflowService, ready.Revision), ID: runID,
	})

	taskRevision := uint64(1)
	lease := LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour).UTC(), FencingToken: 1,
	}
	taskCommands := []TaskCommand{
		LeaseTask{Meta: uniqueEnvelope(ActorWorkflowService, taskRevision), Run: runID, ID: taskID, Lease: lease},
	}
	proposal := artifact("artifact://proposals/integration", 'a')
	execution := artifact("artifact://executions/integration", 'b')
	verification := artifact("artifact://verifications/integration", 'c')
	review := artifact("artifact://reviews/integration", 'd')
	approval := artifact("artifact://approvals/integration", 'e')
	commit := "0123456789012345678901234567890123456789"
	taskCommands = append(taskCommands,
		StartReasoning{Meta: uniqueEnvelope(ActorReasoningService, 2), Run: runID, ID: taskID, Lease: lease},
		AcceptTaskProposal{Meta: uniqueEnvelope(ActorExecutionService, 3), Run: runID, ID: taskID, Proposal: proposal, Lease: lease},
		RecordTaskExecution{
			Meta: uniqueEnvelope(ActorExecutionService, 4), Run: runID, ID: taskID,
			Proposal: proposal, Execution: execution, CandidateCommit: commit, Lease: lease,
		},
		RecordTaskVerification{
			Meta: uniqueEnvelope(ActorVerificationService, 5), Run: runID, ID: taskID,
			CandidateCommit: commit, Evidence: verification, Passed: true,
		},
		RecordTaskReview{
			Meta: uniqueEnvelope(ActorReviewService, 6), Run: runID, ID: taskID,
			CandidateCommit: commit, Review: review, Passed: true,
		},
		ApproveTask{
			Meta: uniqueEnvelope(ActorHuman, 7), Run: runID, ID: taskID,
			CandidateCommit: commit, Review: review, Approval: approval,
		},
	)
	leased := taskCommands[0].(LeaseTask)
	if _, err := store.ExecuteTask(t.Context(), leased); err != nil {
		t.Fatalf("execute %T: %v", leased, err)
	}
	assertTaskCommandTypeConflict(t, store, leased)
	for _, command := range taskCommands[1:] {
		if _, err := store.ExecuteTask(t.Context(), command); err != nil {
			t.Fatalf("execute %T: %v", command, err)
		}
	}

	runExecution := artifact("artifact://run-executions/integration", '1')
	runVerification := artifact("artifact://run-verifications/integration", '2')
	runReview := artifact("artifact://run-reviews/integration", '3')
	runApproval := artifact("artifact://run-approvals/integration", '4')
	publication := artifact("artifact://publications/integration", '6')
	merge := artifact("artifact://merges/integration", '5')
	runCommands := []RunCommand{
		RecordRunExecution{
			Meta: uniqueEnvelope(ActorExecutionService, executingRun.Revision), ID: runID,
			Execution: runExecution, CandidateCommit: commit,
		},
		RecordRunVerification{
			Meta: uniqueEnvelope(ActorVerificationService, executingRun.Revision+1), ID: runID,
			CandidateCommit: commit, Evidence: runVerification,
		},
		RecordRunReview{
			Meta: uniqueEnvelope(ActorReviewService, executingRun.Revision+2), ID: runID,
			CandidateCommit: commit, Review: runReview,
		},
		ApproveRun{
			Meta: uniqueEnvelope(ActorHuman, executingRun.Revision+3), ID: runID,
			CandidateCommit: commit, Review: runReview, Approval: runApproval,
		},
		RecordDraftPullRequest{
			Meta: uniqueEnvelope(ActorPublicationService, executingRun.Revision+4), ID: runID,
			CandidateCommit: commit, Approval: runApproval, Publication: publication,
		},
		RecordMerge{
			Meta: uniqueEnvelope(ActorMergeService, executingRun.Revision+5), ID: runID,
			CandidateCommit: commit, Approval: runApproval, Merge: merge,
		},
	}
	var final CommandResult
	for _, command := range runCommands {
		final = executeRun(t, store, command)
	}
	if final.State != string(RunStateMerged) {
		t.Fatalf("final state = %s", final.State)
	}
	rows, err := pool.Query(t.Context(), `SELECT revision FROM workflow_events
		WHERE aggregate_type='RUN' AND aggregate_id=$1 ORDER BY revision`, runID)
	if err != nil {
		t.Fatal(err)
	}
	expectedRevision := uint64(1)
	for rows.Next() {
		var revision uint64
		if err := rows.Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if revision != expectedRevision {
			t.Fatalf("event revision=%d want=%d", revision, expectedRevision)
		}
		expectedRevision++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if expectedRevision != final.Revision+1 {
		t.Fatalf("event stream ended at %d, aggregate revision=%d", expectedRevision-1, final.Revision)
	}
	var eventID string
	if err := pool.QueryRow(t.Context(),
		`SELECT event_id FROM workflow_events WHERE aggregate_id=$1 ORDER BY sequence LIMIT 1`,
		runID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE workflow_events SET event_type='TAMPERED' WHERE event_id=$1`, eventID); err == nil {
		t.Fatal("event update succeeded")
	}
	if _, err := pool.Exec(t.Context(),
		`DELETE FROM workflow_events WHERE event_id=$1`, eventID); err == nil {
		t.Fatal("event delete succeeded")
	}
}

func TestMigrationsAreRepeatableAndDigestTracked(t *testing.T) {
	pool := integrationPool(t)
	connectionString := os.Getenv("TEST_DATABASE_URL")
	var wait sync.WaitGroup
	errorsFound := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsFound <- Migrate(context.Background(), connectionString)
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
	var count, invalidDigests int
	if err := pool.QueryRow(t.Context(), `SELECT count(*),
		count(*) FILTER (WHERE digest !~ '^[a-f0-9]{64}$')
		FROM schema_migrations WHERE version IN (1,2,3,4,5,8,14,15)`,
	).Scan(&count, &invalidDigests); err != nil {
		t.Fatal(err)
	}
	if count != 8 || invalidDigests != 0 {
		t.Fatalf("migration rows=%d invalid digests=%d", count, invalidDigests)
	}
}

func TestStoreDependencyReadinessAndCancellationCascade(t *testing.T) {
	pool := integrationPool(t)
	store := NewStore(pool)

	setup := func(t *testing.T, taskIDs []string, dependencies []TaskDependency) (string, CommandResult) {
		t.Helper()
		runID, revision := createPersistedRun(t, store)
		proposed := executeRun(t, store, ProposeSpecification{
			Meta: uniqueEnvelope(ActorWorkflowService, revision), ID: runID,
			Specification: testSpec,
		})
		specApproved := executeRun(t, store, ApproveSpecification{
			Meta: uniqueEnvelope(ActorHuman, proposed.Revision), ID: runID,
			Specification: testSpec,
		})
		graph := artifact("artifact://task-graphs/"+runID, '6')
		graphProposed := executeRun(t, store, ProposeTaskGraph{
			Meta: uniqueEnvelope(ActorWorkflowService, specApproved.Revision),
			ID:   runID, TaskGraph: graph,
		})
		definitions := make([]TaskDefinition, len(taskIDs))
		for index, taskID := range taskIDs {
			definitions[index] = TaskDefinition{ID: taskID, MaxAttempts: 2}
		}
		ready := executeRun(t, store, ApproveTaskGraph{
			Meta: uniqueEnvelope(ActorHuman, graphProposed.Revision), ID: runID,
			TaskGraph: graph, Tasks: definitions, Dependencies: dependencies,
		})
		return runID, ready
	}

	rootID, dependentID := uuid.NewString(), uuid.NewString()
	runID, ready := setup(t, []string{rootID, dependentID}, []TaskDependency{
		{TaskID: dependentID, DependsOnID: rootID},
	})
	executeRun(t, store, StartRun{
		Meta: uniqueEnvelope(ActorWorkflowService, ready.Revision), ID: runID,
	})
	lease := LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour).UTC(), FencingToken: 1,
	}
	proposal := artifact("artifact://dependency/proposal", 'a')
	execution := artifact("artifact://dependency/execution", 'b')
	verification := artifact("artifact://dependency/verification", 'c')
	review := artifact("artifact://dependency/review", 'd')
	approval := artifact("artifact://dependency/approval", 'e')
	commit := "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	commands := []TaskCommand{
		LeaseTask{Meta: uniqueEnvelope(ActorWorkflowService, 1), Run: runID, ID: rootID, Lease: lease},
		StartReasoning{Meta: uniqueEnvelope(ActorReasoningService, 2), Run: runID, ID: rootID, Lease: lease},
		AcceptTaskProposal{Meta: uniqueEnvelope(ActorExecutionService, 3), Run: runID, ID: rootID, Proposal: proposal, Lease: lease},
		RecordTaskExecution{
			Meta: uniqueEnvelope(ActorExecutionService, 4), Run: runID, ID: rootID,
			Proposal: proposal, Execution: execution, CandidateCommit: commit, Lease: lease,
		},
		RecordTaskVerification{
			Meta: uniqueEnvelope(ActorVerificationService, 5), Run: runID, ID: rootID,
			CandidateCommit: commit, Evidence: verification, Passed: true,
		},
		RecordTaskReview{
			Meta: uniqueEnvelope(ActorReviewService, 6), Run: runID, ID: rootID,
			CandidateCommit: commit, Review: review, Passed: true,
		},
		ApproveTask{
			Meta: uniqueEnvelope(ActorHuman, 7), Run: runID, ID: rootID,
			CandidateCommit: commit, Review: review, Approval: approval,
		},
	}
	for _, command := range commands {
		if _, err := store.ExecuteTask(t.Context(), command); err != nil {
			t.Fatalf("execute %T: %v", command, err)
		}
	}
	var state TaskState
	var revision uint64
	if err := pool.QueryRow(t.Context(),
		`SELECT state,revision FROM workflow_tasks WHERE task_id=$1`, dependentID,
	).Scan(&state, &revision); err != nil {
		t.Fatal(err)
	}
	if state != TaskStateReady || revision != 2 {
		t.Fatalf("dependent state=%s revision=%d", state, revision)
	}

	cancelTaskA, cancelTaskB := uuid.NewString(), uuid.NewString()
	cancelRunID, cancelReady := setup(t, []string{cancelTaskA, cancelTaskB}, nil)
	cancelled := executeRun(t, store, CancelRun{
		Meta: uniqueEnvelope(ActorHuman, cancelReady.Revision),
		ID:   cancelRunID, Reason: "operator cancelled",
	})
	if cancelled.State != string(RunStateCancelled) || len(cancelled.EventIDs) != 3 {
		t.Fatalf("unexpected cancellation result: %#v", cancelled)
	}
	var cancelledTasks int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workflow_tasks
		WHERE run_id=$1 AND state='CANCELLED'`, cancelRunID).Scan(&cancelledTasks); err != nil {
		t.Fatal(err)
	}
	if cancelledTasks != 2 {
		t.Fatalf("cancelled tasks = %d", cancelledTasks)
	}
}
