package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	postgresutil "github.com/Standard-Syntax/basic/go/internal/postgres"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

type fakeWorkflow struct {
	runCommands  []workflow.RunCommand
	taskCommands []workflow.TaskCommand
	run          *workflow.Run
	task         *workflow.Task
	taskResult   workflow.CommandResult
}

func (f *fakeWorkflow) ExecuteRun(_ context.Context, command workflow.RunCommand) (workflow.CommandResult, error) {
	f.runCommands = append(f.runCommands, command)
	return workflow.CommandResult{AggregateID: command.RunID(), State: "DRAFT", Revision: 1}, nil
}
func (f *fakeWorkflow) ExecuteTask(_ context.Context, command workflow.TaskCommand) (workflow.CommandResult, error) {
	f.taskCommands = append(f.taskCommands, command)
	if f.taskResult.State != "" {
		return f.taskResult, nil
	}
	return workflow.CommandResult{AggregateID: command.TaskID()}, nil
}
func (f *fakeWorkflow) GetRun(context.Context, string) (workflow.Run, error) {
	if f.run != nil {
		return *f.run, nil
	}
	return workflow.Run{}, workflow.ErrNotFound
}

func (f *fakeWorkflow) GetTask(context.Context, string, string) (workflow.Task, error) {
	if f.task != nil {
		return *f.task, nil
	}
	return workflow.Task{}, workflow.ErrNotFound
}
func (f *fakeWorkflow) ListTasks(context.Context, string) ([]workflow.Task, error) {
	if f.task == nil {
		return nil, nil
	}
	return []workflow.Task{*f.task}, nil
}
func (*fakeWorkflow) ListEvents(context.Context, string, string) ([]workflow.Event, error) {
	return nil, nil
}
func (*fakeWorkflow) ListPendingApprovals(context.Context) ([]workflow.PendingApproval, error) {
	return nil, nil
}

type fakeRuntime struct {
	begins            int
	completes         int
	abandons          int
	intakeHasDeadline bool
	intakeContextErr  error
	stages            []runtime.StageStatus
	enqueued          []runtime.Job
	enqueueErr        error
}

type fakeRunIntake struct {
	workflow *fakeWorkflow
	runtime  *fakeRuntime
	err      error
}

func (f *fakeRunIntake) Accept(
	ctx context.Context, request RunIntakeRequest,
) (*runtime.IdempotencyResult, error) {
	_, f.runtime.intakeHasDeadline = ctx.Deadline()
	f.runtime.intakeContextErr = ctx.Err()
	f.runtime.begins++
	if f.err != nil {
		f.runtime.abandons++
		return nil, f.err
	}
	f.workflow.runCommands = append(f.workflow.runCommands, request.Command)
	f.runtime.completes++
	result := workflow.CommandResult{
		AggregateID: request.Command.ID, State: string(workflow.RunStateDraft), Revision: 1,
	}
	response, _ := json.Marshal(result)
	return &runtime.IdempotencyResult{StatusCode: http.StatusCreated, Response: response}, nil
}

func (f *fakeRuntime) BeginIdempotency(context.Context, runtime.IdempotencyRequest) (*runtime.IdempotencyResult, error) {
	f.begins++
	return &runtime.IdempotencyResult{FencingToken: 1}, nil
}
func (f *fakeRuntime) CompleteIdempotency(
	context.Context, string, uint64, int, json.RawMessage,
) error {
	f.completes++
	return nil
}
func (f *fakeRuntime) AbandonIdempotency(context.Context, string, uint64) error {
	f.abandons++
	return nil
}
func (*fakeRuntime) RenewIdempotency(context.Context, string, uint64, time.Duration) error {
	return nil
}
func (f *fakeRuntime) Enqueue(_ context.Context, job runtime.Job) error {
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	f.enqueued = append(f.enqueued, job)
	return nil
}
func (*fakeRuntime) CancelRun(context.Context, string, time.Time) error {
	return nil
}

func (f *fakeRuntime) RunStages(context.Context, string) ([]runtime.StageStatus, error) {
	return f.stages, nil
}

type fakeArtifacts struct{ putErr error }

func (f fakeArtifacts) Put(_ context.Context, body []byte) (workflow.ArtifactRef, error) {
	if f.putErr != nil {
		return workflow.ArtifactRef{}, f.putErr
	}
	digest := runtime.Digest(body)
	return workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}, nil
}
func (fakeArtifacts) Get(_ context.Context, ref workflow.ArtifactRef) ([]byte, error) {
	return []byte(ref.Digest), nil
}

type fakeBindings struct {
	runs []runtime.RunBinding
	run  *runtime.RunBinding
	err  error
}

func (f *fakeBindings) CreateRun(_ context.Context, binding runtime.RunBinding) error {
	if f.err != nil {
		return f.err
	}
	f.runs = append(f.runs, binding)
	return nil
}
func (f *fakeBindings) GetRun(context.Context, string) (runtime.RunBinding, error) {
	if f.run != nil {
		return *f.run, nil
	}
	return runtime.RunBinding{}, runtime.ErrNotFound
}
func (*fakeBindings) GetTask(context.Context, string, string) (runtime.TaskBinding, error) {
	return runtime.TaskBinding{}, runtime.ErrNotFound
}
func (*fakeBindings) CheckpointSpecification(
	context.Context, string, workflow.ArtifactRef,
) error {
	return nil
}
func (*fakeBindings) CheckpointTaskGraph(
	context.Context, string, workflow.ArtifactRef, runtime.TaskBinding,
) error {
	return nil
}
func (*fakeBindings) CheckpointApproval(context.Context, string, workflow.ArtifactRef) error {
	return nil
}

type fakeApproval struct{}

func (fakeApproval) ApproveTask(context.Context, approval.Request) (approval.Result, error) {
	return approval.Result{}, nil
}

type fakeTaskGraphApproval struct {
	result workflow.CommandResult
	err    error
}

func (f fakeTaskGraphApproval) ApproveTaskGraph(
	context.Context, TaskGraphApprovalRequest,
) (workflow.CommandResult, error) {
	return f.result, f.err
}

type fakePublication struct{}

func (fakePublication) Publish(
	context.Context, publication.Request,
) (publication.Result, error) {
	return publication.Result{}, nil
}

type capturingPublication struct{ request publication.Request }

func (f *capturingPublication) Publish(
	_ context.Context, request publication.Request,
) (publication.Result, error) {
	f.request = request
	return publication.Result{CandidateCommit: request.CandidateCommit}, nil
}

func TestSubmitPublishesOnlyPreviouslyApprovedEvidence(t *testing.T) {
	server, workflowStore, _, _ := testServer(t, RoleOperator)
	ref := func(letter byte) workflow.ArtifactRef {
		digest := strings.Repeat(string(letter), 64)
		return workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	}
	approvalRef, specificationRef := ref('a'), ref('b')
	run := workflow.Run{
		ID: uuid.NewString(), State: workflow.RunStateMergeReady, Revision: 9,
		Approval: &approvalRef,
	}
	task := workflow.Task{
		RunID: run.ID, ID: uuid.NewString(), State: workflow.TaskStateAccepted,
		CandidateCommit: strings.Repeat("c", 40),
	}
	proposal, executionRef, verificationRef, reviewRef := ref('d'), ref('e'), ref('f'), ref('1')
	task.Proposal, task.Execution = &proposal, &executionRef
	task.Verification, task.Review = &verificationRef, &reviewRef
	workflowStore.run, workflowStore.task = &run, &task
	server.bindings = &fakeBindings{run: &runtime.RunBinding{
		RunID: run.ID, BaseCommit: strings.Repeat("2", 40),
		ApprovedSpecification: &specificationRef, CompositeApproval: &approvalRef,
	}}
	publisher := &capturingPublication{}
	server.publication = publisher

	result, err := server.submitRun(t.Context(), uuid.NewString(), time.Now(), run)
	if err != nil {
		t.Fatal(err)
	}
	if result.CandidateCommit != task.CandidateCommit ||
		publisher.request.Approval != approvalRef ||
		publisher.request.ExpectedRunRevision != run.Revision {
		t.Fatalf("result=%+v request=%+v", result, publisher.request)
	}
	unauthorized, unauthorizedWorkflow, _, token := testServer(t, RoleApprover)
	unauthorizedWorkflow.run = &run
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/runs/"+run.ID+"/submit",
		strings.NewReader(`{"decision_timestamp":"2026-08-03T12:00:00Z"}`),
	)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", uuid.NewString())
	request.Header.Set("If-Match", `"9"`)
	response := httptest.NewRecorder()
	unauthorized.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-operator submit status=%d body=%s", response.Code, response.Body)
	}

	run.Approval = nil
	if _, err := server.submitRun(t.Context(), uuid.NewString(), time.Now(), run); !errors.Is(
		err, workflow.ErrInvalidTransition,
	) {
		t.Fatalf("unapproved submit error=%v", err)
	}
}

func testServer(t *testing.T, roles ...Role) (*Server, *fakeWorkflow, *fakeRuntime, string) {
	t.Helper()
	token := "test-secret"
	digest := sha256.Sum256([]byte(token))
	workflowStore := &fakeWorkflow{}
	runtimeLedger := &fakeRuntime{}
	server, err := New(Config{
		ServiceActorID: uuid.NewString(),
		Principals: []Principal{{
			ID: uuid.NewString(), TokenSHA256: hex.EncodeToString(digest[:]), Roles: roles,
		}},
		MaxBodyBytes:  512,
		TrustedChecks: []string{"make-check-v1"},
		Policy:        testPolicy("/tmp/repository", strings.Repeat("a", 40), "https://example.invalid/repository.git"),
	}, workflowStore, runtimeLedger, &fakeRunIntake{
		workflow: workflowStore, runtime: runtimeLedger,
	}, fakeArtifacts{}, &fakeBindings{},
		fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return server, workflowStore, runtimeLedger, token
}

func TestNewRejectsEachRequiredNilDependency(t *testing.T) {
	token := sha256.Sum256([]byte("test-secret"))
	config := Config{
		ServiceActorID: uuid.NewString(),
		Principals: []Principal{{
			ID: uuid.NewString(), TokenSHA256: hex.EncodeToString(token[:]),
			Roles: []Role{RoleOperator},
		}},
		TrustedChecks: []string{"make-check-v1"},
		Policy: testPolicy("/tmp/repository", strings.Repeat("a", 40),
			"https://example.invalid/repository.git"),
	}
	workflowStore := &fakeWorkflow{}
	runtimeLedger := &fakeRuntime{}
	intake := &fakeRunIntake{workflow: workflowStore, runtime: runtimeLedger}
	tests := []struct {
		name string
		want string
		call func() error
	}{
		{name: "workflow", want: "workflow store is required", call: func() error {
			_, err := New(config, nil, runtimeLedger, intake, fakeArtifacts{}, &fakeBindings{}, fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "runtime", want: "runtime ledger is required", call: func() error {
			_, err := New(config, workflowStore, nil, intake, fakeArtifacts{}, &fakeBindings{}, fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "intake", want: "run intake is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, nil, fakeArtifacts{}, &fakeBindings{}, fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "artifacts", want: "artifact store is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, intake, nil, &fakeBindings{}, fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "bindings", want: "binding store is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, intake, fakeArtifacts{}, nil, fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "approval", want: "approval service is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, intake, fakeArtifacts{}, &fakeBindings{}, nil, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "task graph approval", want: "task graph approval service is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, intake, fakeArtifacts{}, &fakeBindings{}, fakeApproval{}, nil, nil, nil)
			return err
		}},
		{name: "typed nil workflow", want: "workflow store is required", call: func() error {
			_, err := New(config, (*fakeWorkflow)(nil), runtimeLedger, intake, fakeArtifacts{}, &fakeBindings{}, fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "typed nil runtime", want: "runtime ledger is required", call: func() error {
			_, err := New(config, workflowStore, (*fakeRuntime)(nil), intake, fakeArtifacts{}, &fakeBindings{}, fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "typed nil intake", want: "run intake is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, (*fakeRunIntake)(nil), fakeArtifacts{}, &fakeBindings{}, fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "typed nil artifacts", want: "artifact store is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, intake, (*fakeArtifacts)(nil), &fakeBindings{}, fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "typed nil bindings", want: "binding store is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, intake, fakeArtifacts{}, (*fakeBindings)(nil), fakeApproval{}, fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "typed nil approval", want: "approval service is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, intake, fakeArtifacts{}, &fakeBindings{}, (*fakeApproval)(nil), fakeTaskGraphApproval{}, nil, nil)
			return err
		}},
		{name: "typed nil task graph approval", want: "task graph approval service is required", call: func() error {
			_, err := New(config, workflowStore, runtimeLedger, intake, fakeArtifacts{}, &fakeBindings{}, fakeApproval{}, (*fakeTaskGraphApproval)(nil), nil, nil)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil || err.Error() != test.want {
				t.Fatalf("error=%v want=%q", err, test.want)
			}
		})
	}
}

func TestNormalizeServerConfigCanonicalizesTrustedCheckOrder(t *testing.T) {
	input := []string{"verify", "build"}
	config, err := normalizeServerConfig(Config{
		ServiceActorID: uuid.NewString(),
		TrustedChecks:  input,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(config.TrustedChecks, ",") != "build,verify" {
		t.Fatalf("trusted checks = %v", config.TrustedChecks)
	}
	if strings.Join(input, ",") != "verify,build" {
		t.Fatalf("caller-owned input mutated: %v", input)
	}
	if _, err := compileTrustedChecks([]string{"build", "build"}); err == nil {
		t.Fatal("duplicate trusted check accepted")
	}
	if config.TaskMaxAttempts != DefaultTaskMaxAttempts {
		t.Fatalf("default task attempts = %d", config.TaskMaxAttempts)
	}
	for _, attempts := range []uint32{1, 11} {
		if _, err := normalizeServerConfig(Config{
			ServiceActorID: uuid.NewString(), TaskMaxAttempts: attempts,
		}); err == nil {
			t.Fatalf("task attempts %d accepted", attempts)
		}
	}
}

func TestCompilePrincipalsRejectsUnknownAndDuplicateRoles(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, roles := range [][]Role{{"unknown"}, {RoleOperator, RoleOperator}} {
		_, err := compilePrincipals([]Principal{{
			ID: uuid.NewString(), TokenSHA256: digest, Roles: roles,
		}})
		if err == nil {
			t.Fatalf("roles %v accepted", roles)
		}
	}
}

func TestTransientDatabaseConflictMapsToRetryable503(t *testing.T) {
	server, _, _, _ := testServer(t, RoleOperator)
	status, body := server.domainError(postgresutil.ErrTransient)
	encoded, _ := json.Marshal(body)
	if status != http.StatusServiceUnavailable || !strings.Contains(string(encoded), "transient_conflict") {
		t.Fatalf("status=%d body=%s", status, encoded)
	}
}

func TestMutationRoutesRejectAllNonPostMethodsBeforeReadingBody(t *testing.T) {
	server, _, ledger, token := testServer(t, RoleOperator)
	for _, method := range []string{http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, "BREW"} {
		request := httptest.NewRequest(method, "/v1/runs", strings.NewReader("not-json"))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s response=%d allow=%q body=%s", method, response.Code, response.Header().Get("Allow"), response.Body)
		}
	}
	if ledger.begins != 0 {
		t.Fatalf("method rejection reserved %d idempotency keys", ledger.begins)
	}
}

func TestRetryTaskQueuesNextAttemptAndExhaustionReturns422(t *testing.T) {
	server, workflowStore, runtimeLedger, token := testServer(t, RoleOperator)
	runID, taskID := uuid.NewString(), uuid.NewString()
	workflowStore.task = &workflow.Task{
		ID: taskID, RunID: runID, Revision: 5, State: workflow.TaskStateReworkRequired,
		CurrentAttempt: 1, MaxAttempts: 2,
	}
	workflowStore.taskResult = workflow.CommandResult{
		AggregateID: taskID, State: string(workflow.TaskStateReady), Revision: 6, Replay: true,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/retry", strings.NewReader(
		`{"run_id":"`+runID+`","decision_timestamp":"2026-08-01T12:00:00Z"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", uuid.NewString())
	request.Header.Set("If-Match", `"5"`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(runtimeLedger.enqueued) != 1 ||
		runtimeLedger.enqueued[0].Attempt != 2 || runtimeLedger.enqueued[0].Stage != orchestration.StageStart {
		t.Fatalf("response=%d %s jobs=%+v", response.Code, response.Body, runtimeLedger.enqueued)
	}

	runtimeLedger.enqueued = nil
	workflowStore.task.CurrentAttempt = 2
	workflowStore.taskResult.State = string(workflow.TaskStateFailed)
	request = httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/retry", strings.NewReader(
		`{"run_id":"`+runID+`","decision_timestamp":"2026-08-01T12:01:00Z"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", uuid.NewString())
	request.Header.Set("If-Match", `"5"`)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || len(runtimeLedger.enqueued) != 0 {
		t.Fatalf("exhausted response=%d %s jobs=%+v", response.Code, response.Body, runtimeLedger.enqueued)
	}
}

func TestRetryTaskSameKeyReplayRepairsMissedEnqueue(t *testing.T) {
	server, workflowStore, runtimeLedger, token := testServer(t, RoleOperator)
	runID, taskID, key := uuid.NewString(), uuid.NewString(), uuid.NewString()
	workflowStore.task = &workflow.Task{
		ID: taskID, RunID: runID, Revision: 6, State: workflow.TaskStateReady,
		CurrentAttempt: 1, MaxAttempts: 2,
	}
	workflowStore.taskResult = workflow.CommandResult{
		AggregateID: taskID, State: string(workflow.TaskStateReady), Revision: 6, Replay: true,
	}
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/retry", strings.NewReader(
			`{"run_id":"`+runID+`","decision_timestamp":"2026-08-01T12:00:00Z"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("If-Match", `"5"`)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	runtimeLedger.enqueueErr = errors.New("injected enqueue failure")
	if response := call(); response.Code != http.StatusInternalServerError {
		t.Fatalf("first response=%d %s", response.Code, response.Body)
	}
	runtimeLedger.enqueueErr = nil
	if response := call(); response.Code != http.StatusOK || len(runtimeLedger.enqueued) != 1 {
		t.Fatalf("repair response=%d %s jobs=%+v", response.Code, response.Body, runtimeLedger.enqueued)
	}
	if len(workflowStore.taskCommands) != 2 || !workflowStore.taskResult.Replay {
		t.Fatalf("workflow commands=%d result=%+v", len(workflowStore.taskCommands), workflowStore.taskResult)
	}
}

func testPolicy(root, commit, remoteURL string) beta.Policy {
	return beta.Policy{Version: beta.PolicyVersion,
		Repository:    beta.Repository{Owner: "owner", Name: "repository", Root: root, Remote: "origin", RemoteURL: remoteURL, BaseBranch: "main", BaseCommit: commit},
		Paths:         beta.Paths{Readable: []string{"docs"}, Writable: []string{"docs"}, Prohibited: []string{"secrets"}},
		TrustedChecks: []string{"make-check-v1"}, Limits: beta.Limits{MaximumTasks: 1, MaximumChangedFiles: 4, MaximumFileBytes: 1024, MaximumTotalBytes: 4096, ExecutionConcurrency: 1, VerificationConcurrency: 1},
		Images: beta.Images{Execution: "sha256:" + strings.Repeat("a", 64), Verification: "sha256:" + strings.Repeat("b", 64)}}
}

func TestHealthAndAuthentication(t *testing.T) {
	server, _, _, token := testServer(t, RoleOperator)
	for _, test := range []struct {
		path   string
		token  string
		status int
	}{{"/healthz", "", 200}, {"/readyz", "", 200}, {"/readyz", token, 200}} {
		request := httptest.NewRequest(http.MethodGet, test.path, http.NoBody)
		if test.token != "" {
			request.Header.Set("Authorization", "Bearer "+test.token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s status = %d body=%s", test.path, response.Code, response.Body)
		}
	}
}

func TestGetRunExposesTerminalFailureArtifactWithoutBody(t *testing.T) {
	server, workflowStore, runtimeLedger, token := testServer(t, RoleOperator)
	runID := uuid.NewString()
	workflowStore.run = &workflow.Run{ID: runID, Revision: 3}
	runtimeLedger.stages = []runtime.StageStatus{{Attempt: 1, Stage: "verification", State: "FAILED",
		Failure: &workflow.ArtifactRef{URI: "artifact://sha256/failure", Digest: strings.Repeat("a", 64)}}}
	request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID, http.NoBody)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"FAILED"`) ||
		!strings.Contains(response.Body.String(), strings.Repeat("a", 64)) || strings.Contains(response.Body.String(), "proposal") {
		t.Fatalf("terminal run response = %d %s", response.Code, response.Body.String())
	}
}

func TestCreateRunRequiresRoleStrictJSONAndIdempotency(t *testing.T) {
	server, workflowStore, runtimeLedger, token := testServer(t, RoleOperator)
	runID, key := uuid.NewString(), uuid.NewString()
	body := `{"run_id":"` + runID + `","base_commit":"0123456789012345678901234567890123456789",` +
		`"content":{"objective":"fix"},"decision_timestamp":"2026-07-29T12:00:00Z"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(workflowStore.runCommands) != 1 ||
		runtimeLedger.begins != 1 || runtimeLedger.completes != 1 || !runtimeLedger.intakeHasDeadline {
		t.Fatalf("response=%d %s commands=%d runtime=%#v",
			response.Code, response.Body, len(workflowStore.runCommands), runtimeLedger)
	}
	command := workflowStore.runCommands[0].(workflow.CreateRun)
	if command.ID != runID || command.Meta.Actor.Kind != workflow.ActorHuman {
		t.Fatalf("command = %#v", command)
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"run_id":"`+runID+`","decision_timestamp":"2026-07-29T12:00:00Z","unknown":true}`))
	bad.Header.Set("Authorization", "Bearer "+token)
	bad.Header.Set("Idempotency-Key", uuid.NewString())
	badResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("strict JSON status = %d: %s", badResponse.Code, badResponse.Body)
	}
}

func TestRunIntakeSurvivesClientCancellation(t *testing.T) {
	server, _, runtimeLedger, token := testServer(t, RoleOperator)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := `{"run_id":"` + uuid.NewString() + `","base_commit":"0123456789012345678901234567890123456789",` +
		`"content":{"objective":"fix"},"decision_timestamp":"2026-08-01T12:00:00Z"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", uuid.NewString())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || runtimeLedger.intakeContextErr != nil {
		t.Fatalf("response=%d %s intake context=%v", response.Code, response.Body, runtimeLedger.intakeContextErr)
	}
}

func TestCreateRunReportsRecoverableIntakeFailure(t *testing.T) {
	body := `{"run_id":"` + uuid.NewString() +
		`","base_commit":"0123456789012345678901234567890123456789",` +
		`"content":{"objective":"fix"},"decision_timestamp":"2026-07-29T12:00:00Z"}`
	tests := []string{"artifact", "binding", "transaction"}
	for _, test := range tests {
		t.Run(test, func(t *testing.T) {
			server, workflowStore, runtimeLedger, token := testServer(t, RoleOperator)
			server.intake.(*fakeRunIntake).err = errors.New(test + " unavailable")
			request := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Idempotency-Key", uuid.NewString())
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError ||
				runtimeLedger.abandons != 1 || runtimeLedger.completes != 0 ||
				len(workflowStore.runCommands) != 0 {
				t.Fatalf(
					"status=%d runtime=%#v commands=%d",
					response.Code, runtimeLedger, len(workflowStore.runCommands),
				)
			}
		})
	}
}

func TestCreateRunRejectsMissingKeyAndWrongRole(t *testing.T) {
	for _, roles := range [][]Role{{RoleOperator}, {RoleApprover}} {
		server, _, _, token := testServer(t, roles...)
		request := httptest.NewRequest(http.MethodPost, "/v1/runs",
			strings.NewReader(`{"run_id":"`+uuid.NewString()+
				`","base_commit":"0123456789012345678901234567890123456789",`+
				`"content":{"objective":"fix"},"decision_timestamp":"2026-07-29T12:00:00Z"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		if roles[0] == RoleApprover {
			request.Header.Set("Idempotency-Key", uuid.NewString())
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		want := http.StatusBadRequest
		if roles[0] == RoleApprover {
			want = http.StatusForbidden
		}
		if response.Code != want {
			t.Fatalf("roles=%v status=%d body=%s", roles, response.Code, response.Body)
		}
	}
}
