package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

type fakeWorkflow struct {
	runCommands  []workflow.RunCommand
	taskCommands []workflow.TaskCommand
}

func (f *fakeWorkflow) ExecuteRun(_ context.Context, command workflow.RunCommand) (workflow.CommandResult, error) {
	f.runCommands = append(f.runCommands, command)
	return workflow.CommandResult{AggregateID: command.RunID(), State: "DRAFT", Revision: 1}, nil
}
func (f *fakeWorkflow) ExecuteTask(_ context.Context, command workflow.TaskCommand) (workflow.CommandResult, error) {
	f.taskCommands = append(f.taskCommands, command)
	return workflow.CommandResult{AggregateID: command.TaskID()}, nil
}
func (*fakeWorkflow) GetRun(context.Context, string) (workflow.Run, error) {
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
func (f *fakeWorkflow) ListPendingApprovals(context.Context) ([]workflow.PendingApproval, error) {
	return nil, nil
}

type fakeRuntime struct {
	begins    int
	completes int
}

func (f *fakeRuntime) BeginIdempotency(context.Context, runtime.IdempotencyRequest) (*runtime.IdempotencyResult, error) {
	f.begins++
	return nil, nil
}
func (f *fakeRuntime) CompleteIdempotency(context.Context, string, int, json.RawMessage) error {
	f.completes++
	return nil
}
func (*fakeRuntime) Enqueue(context.Context, runtime.Job) error { return nil }
func (*fakeRuntime) CancelRun(context.Context, string, time.Time) error {
	return nil
}

type fakeArtifacts struct{}

func (fakeArtifacts) Put(_ context.Context, body []byte) (workflow.ArtifactRef, error) {
	digest := runtime.Digest(body)
	return workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}, nil
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
		MaxBodyBytes: 512,
	}, workflowStore, runtimeLedger, fakeArtifacts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return server, workflowStore, runtimeLedger, token
}

func TestHealthAndAuthentication(t *testing.T) {
	server, _, _, token := testServer(t, RoleOperator)
	for _, test := range []struct {
		path   string
		token  string
		status int
	}{{"/healthz", "", 200}, {"/readyz", "", 401}, {"/readyz", token, 200}} {
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

func TestCreateRunRequiresRoleStrictJSONAndIdempotency(t *testing.T) {
	server, workflowStore, runtimeLedger, token := testServer(t, RoleOperator)
	runID, key := uuid.NewString(), uuid.NewString()
	body := `{"run_id":"` + runID + `","decision_timestamp":"2026-07-29T12:00:00Z"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(workflowStore.runCommands) != 1 ||
		runtimeLedger.begins != 1 || runtimeLedger.completes != 1 {
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

func TestCreateRunRejectsMissingKeyAndWrongRole(t *testing.T) {
	for _, roles := range [][]Role{{RoleOperator}, {RoleApprover}} {
		server, _, _, token := testServer(t, roles...)
		request := httptest.NewRequest(http.MethodPost, "/v1/runs",
			strings.NewReader(`{"run_id":"`+uuid.NewString()+`","decision_timestamp":"2026-07-29T12:00:00Z"}`))
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
