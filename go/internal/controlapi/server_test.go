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
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

type fakeWorkflow struct {
	runCommands  []workflow.RunCommand
	taskCommands []workflow.TaskCommand
	run          *workflow.Run
}

func (f *fakeWorkflow) ExecuteRun(_ context.Context, command workflow.RunCommand) (workflow.CommandResult, error) {
	f.runCommands = append(f.runCommands, command)
	return workflow.CommandResult{AggregateID: command.RunID(), State: "DRAFT", Revision: 1}, nil
}
func (f *fakeWorkflow) ExecuteTask(_ context.Context, command workflow.TaskCommand) (workflow.CommandResult, error) {
	f.taskCommands = append(f.taskCommands, command)
	return workflow.CommandResult{AggregateID: command.TaskID()}, nil
}
func (f *fakeWorkflow) GetRun(context.Context, string) (workflow.Run, error) {
	if f.run != nil {
		return *f.run, nil
	}
	return workflow.Run{}, workflow.ErrNotFound
}
func (*fakeWorkflow) GetTask(context.Context, string, string) (workflow.Task, error) {
	return workflow.Task{}, workflow.ErrNotFound
}
func (*fakeWorkflow) ListTasks(context.Context, string) ([]workflow.Task, error) {
	return nil, nil
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
	stages            []runtime.StageStatus
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
func (*fakeRuntime) Enqueue(context.Context, runtime.Job) error { return nil }
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
	err  error
}

func (f *fakeBindings) CreateRun(_ context.Context, binding runtime.RunBinding) error {
	if f.err != nil {
		return f.err
	}
	f.runs = append(f.runs, binding)
	return nil
}
func (*fakeBindings) GetRun(context.Context, string) (runtime.RunBinding, error) {
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

type fakePublication struct{}

func (fakePublication) Publish(
	context.Context, publication.Request,
) (publication.Result, error) {
	return publication.Result{}, nil
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
		fakeApproval{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return server, workflowStore, runtimeLedger, token
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
