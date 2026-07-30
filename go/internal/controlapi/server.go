// Package controlapi exposes the authenticated Phase 11 HTTP control plane.
package controlapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

const DefaultMaxBodyBytes int64 = 1 << 20

type Role string

const (
	RoleOperator         Role = "operator"
	RoleApprover         Role = "approver"
	RoleElevatedApprover Role = "elevated_approver"
)

type Principal struct {
	ID          string `json:"id"`
	TokenSHA256 string `json:"token_sha256"`
	Roles       []Role `json:"roles"`
}

type WorkflowStore interface {
	ExecuteRun(context.Context, workflow.RunCommand) (workflow.CommandResult, error)
	ExecuteTask(context.Context, workflow.TaskCommand) (workflow.CommandResult, error)
	GetRun(context.Context, string) (workflow.Run, error)
	GetTask(context.Context, string, string) (workflow.Task, error)
	ListTasks(context.Context, string) ([]workflow.Task, error)
	ListEvents(context.Context, string, string) ([]workflow.Event, error)
}

type IdempotencyLedger interface {
	BeginIdempotency(context.Context, runtime.IdempotencyRequest) (*runtime.IdempotencyResult, error)
	CompleteIdempotency(context.Context, string, int, json.RawMessage) error
	Enqueue(context.Context, runtime.Job) error
	CancelRun(context.Context, string, time.Time) error
}

type ArtifactStore interface {
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
}

type Config struct {
	Principals     []Principal
	ServiceActorID string
	MaxBodyBytes   int64
}

type Server struct {
	config     Config
	workflow   WorkflowStore
	runtime    IdempotencyLedger
	artifacts  ArtifactStore
	principals []principalDigest
	logger     *slog.Logger
}

type principalDigest struct {
	principal Principal
	digest    [sha256.Size]byte
}

func New(
	config Config, workflowStore WorkflowStore, runtimeLedger IdempotencyLedger,
	artifacts ArtifactStore, logger *slog.Logger,
) (*Server, error) {
	if _, err := uuid.Parse(config.ServiceActorID); err != nil {
		return nil, errors.New("invalid service actor ID")
	}
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = DefaultMaxBodyBytes
	}
	seen := make(map[string]struct{})
	principals := make([]principalDigest, 0, len(config.Principals))
	for _, principal := range config.Principals {
		if _, err := uuid.Parse(principal.ID); err != nil {
			return nil, errors.New("invalid principal ID")
		}
		if _, exists := seen[principal.ID]; exists {
			return nil, errors.New("duplicate principal ID")
		}
		seen[principal.ID] = struct{}{}
		decoded, err := hex.DecodeString(principal.TokenSHA256)
		if err != nil || len(decoded) != sha256.Size || len(principal.Roles) == 0 {
			return nil, errors.New("invalid principal token digest or roles")
		}
		var digest [sha256.Size]byte
		copy(digest[:], decoded)
		principals = append(principals, principalDigest{principal: principal, digest: digest})
	}
	if len(principals) == 0 {
		return nil, errors.New("at least one principal is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		config: config, workflow: workflowStore, runtime: runtimeLedger,
		artifacts: artifacts, principals: principals, logger: logger,
	}, nil
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	principal, ok := s.authenticate(request)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "valid bearer token required")
		return
	}
	if request.URL.Path == "/readyz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	if request.Method == http.MethodGet {
		s.handleGet(w, request, principal)
		return
	}
	s.handleMutation(w, request, principal)
}

func (s *Server) authenticate(request *http.Request) (Principal, bool) {
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) == len("Bearer ") {
		return Principal{}, false
	}
	sum := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
	match := -1
	for index := range s.principals {
		equal := subtle.ConstantTimeCompare(sum[:], s.principals[index].digest[:])
		match = subtle.ConstantTimeSelect(equal, index, match)
	}
	if match < 0 {
		return Principal{}, false
	}
	return s.principals[match].principal, true
}

func (s *Server) handleGet(w http.ResponseWriter, request *http.Request, principal Principal) {
	_ = principal
	if request.URL.Path == "/v1/approvals" {
		if request.URL.Query().Get("status") != "pending" {
			writeError(w, http.StatusBadRequest, "invalid_request", "status must be pending")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"approvals": []any{}})
		return
	}
	runID, suffix, ok := parseRunPath(request.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	if suffix == "" {
		run, err := s.workflow.GetRun(request.Context(), runID)
		if err != nil {
			s.writeDomainError(w, err)
			return
		}
		tasks, err := s.workflow.ListTasks(request.Context(), runID)
		if err != nil {
			s.writeDomainError(w, err)
			return
		}
		w.Header().Set("ETag", revisionETag(run.Revision))
		writeJSON(w, http.StatusOK, map[string]any{"run": run, "tasks": tasks})
		return
	}
	if suffix == "/events" {
		events, err := s.workflow.ListEvents(request.Context(), "RUN", runID)
		if err != nil {
			s.writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": events})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "route not found")
}

type mutationBody struct {
	RunID        string                    `json:"run_id,omitempty"`
	Content      json.RawMessage           `json:"content,omitempty"`
	Reason       string                    `json:"reason,omitempty"`
	Terminal     bool                      `json:"terminal,omitempty"`
	Tasks        []workflow.TaskDefinition `json:"tasks,omitempty"`
	Dependencies []workflow.TaskDependency `json:"dependencies,omitempty"`
	DecisionTime time.Time                 `json:"decision_timestamp"`
}

func (s *Server) handleMutation(w http.ResponseWriter, request *http.Request, principal Principal) {
	key := request.Header.Get("Idempotency-Key")
	if _, err := uuid.Parse(key); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "UUID Idempotency-Key required")
		return
	}
	var body mutationBody
	raw, err := decodeStrict(request, s.config.MaxBodyBytes, &body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	idem := runtime.IdempotencyRequest{
		Key: key, Method: request.Method, Target: request.URL.RequestURI(),
		PrincipalID: principal.ID, RequestDigest: runtime.Digest(raw),
	}
	replay, err := s.runtime.BeginIdempotency(request.Context(), idem)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	if replay != nil {
		w.Header().Set("Idempotent-Replay", "true")
		writeRawJSON(w, replay.StatusCode, replay.Response)
		return
	}
	status, response, err := s.applyMutation(request, principal, key, body)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "encode response")
		return
	}
	if err := s.runtime.CompleteIdempotency(request.Context(), key, status, encoded); err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeRawJSON(w, status, encoded)
}

func (s *Server) applyMutation(
	request *http.Request, principal Principal, key string, body mutationBody,
) (int, any, error) {
	if body.DecisionTime.IsZero() {
		return 0, nil, errors.New("decision_timestamp is required")
	}
	if request.URL.Path == "/v1/runs" {
		return s.createRun(request.Context(), principal, key, body)
	}
	taskID, taskSuffix, taskRoute := parseTaskPath(request.URL.Path)
	if taskRoute && taskSuffix == "/retry" {
		return s.retryTask(request, principal, key, body, taskID)
	}
	runID, suffix, runRoute := parseRunPath(request.URL.Path)
	if !runRoute {
		return 0, nil, workflow.ErrNotFound
	}
	run, err := s.workflow.GetRun(request.Context(), runID)
	if err != nil {
		return 0, nil, err
	}
	expected, err := requiredRevision(request.Header.Get("If-Match"))
	if err != nil || expected != run.Revision {
		return 0, nil, workflow.ErrRevisionConflict
	}
	return s.applyRunMutation(request.Context(), principal, key, suffix, body, run)
}

func (s *Server) createRun(
	ctx context.Context, principal Principal, key string, body mutationBody,
) (int, any, error) {
	if !hasRole(principal, RoleOperator) {
		return 0, nil, workflow.ErrUnauthorized
	}
	if _, err := uuid.Parse(body.RunID); err != nil {
		return 0, nil, workflow.ErrInvalid
	}
	result, err := s.workflow.ExecuteRun(ctx, workflow.CreateRun{
		Meta: s.envelope(key, principal.ID, workflow.ActorHuman, 0, body.DecisionTime),
		ID:   body.RunID,
	})
	return http.StatusCreated, result, err
}

func (s *Server) retryTask(
	request *http.Request, principal Principal, key string, body mutationBody, taskID string,
) (int, any, error) {
	if !hasRole(principal, RoleOperator) {
		return 0, nil, workflow.ErrUnauthorized
	}
	if _, err := uuid.Parse(body.RunID); err != nil {
		return 0, nil, workflow.ErrInvalid
	}
	task, err := s.workflow.GetTask(request.Context(), body.RunID, taskID)
	if err != nil {
		return 0, nil, err
	}
	expected, err := requiredRevision(request.Header.Get("If-Match"))
	if err != nil || expected != task.Revision {
		return 0, nil, workflow.ErrRevisionConflict
	}
	result, err := s.workflow.ExecuteTask(request.Context(), workflow.RetryTask{
		Meta: s.envelope(
			key, s.config.ServiceActorID, workflow.ActorWorkflowService, expected, body.DecisionTime,
		),
		Run: task.RunID, ID: task.ID,
	})
	return http.StatusOK, result, err
}

func (s *Server) applyRunMutation( // skipcq: GO-R1005 -- explicit route-to-command audit table
	ctx context.Context, principal Principal, key, suffix string, body mutationBody, run workflow.Run,
) (int, any, error) {
	serviceMeta := func(step string, revision uint64) workflow.CommandEnvelope {
		return s.envelope(stableStepID(key, step), s.config.ServiceActorID,
			workflow.ActorWorkflowService, revision, body.DecisionTime)
	}
	humanMeta := func(step string, revision uint64) workflow.CommandEnvelope {
		return s.envelope(stableStepID(key, step), principal.ID,
			workflow.ActorHuman, revision, body.DecisionTime)
	}
	switch suffix {
	case "/specification":
		if !hasRole(principal, RoleOperator) {
			return 0, nil, workflow.ErrUnauthorized
		}
		ref, err := s.storeContent(ctx, body.Content)
		if err != nil {
			return 0, nil, err
		}
		result, err := s.workflow.ExecuteRun(ctx, workflow.ProposeSpecification{
			Meta: serviceMeta("propose-specification", run.Revision), ID: run.ID, Specification: ref,
		})
		return http.StatusOK, result, err
	case "/specification/approve":
		if !hasApprovalRole(principal, false) || run.Specification == nil {
			return 0, nil, workflow.ErrUnauthorized
		}
		result, err := s.workflow.ExecuteRun(ctx, workflow.ApproveSpecification{
			Meta: humanMeta("approve-specification", run.Revision), ID: run.ID,
			Specification: *run.Specification,
		})
		return http.StatusOK, result, err
	case "/specification/reject":
		if !hasApprovalRole(principal, false) || run.Specification == nil {
			return 0, nil, workflow.ErrUnauthorized
		}
		result, err := s.workflow.ExecuteRun(ctx, workflow.RejectSpecification{
			Meta: humanMeta("reject-specification", run.Revision), ID: run.ID,
			Specification: *run.Specification, Reason: body.Reason, Terminal: body.Terminal,
		})
		return http.StatusOK, result, err
	case "/task-graph":
		if !hasRole(principal, RoleOperator) {
			return 0, nil, workflow.ErrUnauthorized
		}
		ref, err := s.storeContent(ctx, body.Content)
		if err != nil {
			return 0, nil, err
		}
		result, err := s.workflow.ExecuteRun(ctx, workflow.ProposeTaskGraph{
			Meta: serviceMeta("propose-task-graph", run.Revision), ID: run.ID, TaskGraph: ref,
		})
		return http.StatusOK, result, err
	case "/task-graph/approve":
		if !hasApprovalRole(principal, false) || run.TaskGraph == nil {
			return 0, nil, workflow.ErrUnauthorized
		}
		if len(body.Tasks) != 1 || len(body.Dependencies) != 0 {
			return 0, nil, workflow.ErrInvalid
		}
		result, err := s.workflow.ExecuteRun(ctx, workflow.ApproveTaskGraph{
			Meta: humanMeta("approve-task-graph", run.Revision), ID: run.ID,
			TaskGraph: *run.TaskGraph, Tasks: body.Tasks,
		})
		if err == nil {
			taskID := body.Tasks[0].ID
			err = s.runtime.Enqueue(ctx, runtime.Job{
				ID:    orchestration.StableID(run.ID, taskID, "1", orchestration.StageStart, "job"),
				RunID: run.ID, TaskID: &taskID, Attempt: 1,
				Stage: orchestration.StageStart, AvailableAt: body.DecisionTime.UTC(),
			})
		}
		return http.StatusOK, result, err
	case "/task-graph/reject":
		if !hasApprovalRole(principal, false) || run.TaskGraph == nil {
			return 0, nil, workflow.ErrUnauthorized
		}
		result, err := s.workflow.ExecuteRun(ctx, workflow.RejectTaskGraph{
			Meta: humanMeta("reject-task-graph", run.Revision), ID: run.ID,
			TaskGraph: *run.TaskGraph, Reason: body.Reason, Terminal: body.Terminal,
		})
		return http.StatusOK, result, err
	case "/approval":
		if !hasApprovalRole(principal, false) {
			return 0, nil, workflow.ErrUnauthorized
		}
		result, err := s.compositeApprove(ctx, principal, key, body.DecisionTime, run)
		return http.StatusOK, result, err
	case "/reject":
		if !hasApprovalRole(principal, false) || run.Review == nil {
			return 0, nil, workflow.ErrUnauthorized
		}
		result, err := s.workflow.ExecuteRun(ctx, workflow.RejectRun{
			Meta: humanMeta("reject-run", run.Revision), ID: run.ID,
			CandidateCommit: run.CandidateCommit, Review: *run.Review, Reason: body.Reason,
		})
		return http.StatusOK, result, err
	case "/cancel":
		if !hasRole(principal, RoleOperator) {
			return 0, nil, workflow.ErrUnauthorized
		}
		result, err := s.workflow.ExecuteRun(ctx, workflow.CancelRun{
			Meta: serviceMeta("cancel-run", run.Revision), ID: run.ID, Reason: body.Reason,
		})
		if err == nil {
			err = s.runtime.CancelRun(ctx, run.ID, body.DecisionTime)
		}
		return http.StatusOK, result, err
	default:
		return 0, nil, workflow.ErrNotFound
	}
}

func (s *Server) compositeApprove( // skipcq: GO-R1005 -- restart-safe ordered approval checkpoints
	ctx context.Context, principal Principal, key string, at time.Time, run workflow.Run,
) (workflow.CommandResult, error) {
	tasks, err := s.workflow.ListTasks(ctx, run.ID)
	if err != nil || len(tasks) != 1 {
		return workflow.CommandResult{}, workflow.ErrInvalid
	}
	task := tasks[0]
	if task.Execution == nil || task.Verification == nil || task.Review == nil ||
		task.CandidateCommit == "" || run.Specification == nil || run.TaskGraph == nil {
		return workflow.CommandResult{}, workflow.ErrInvalidTransition
	}
	approvalBody, err := json.Marshal(map[string]any{
		"schema_version": "composite_run_approval.v1", "principal_id": principal.ID,
		"run_id": run.ID, "task_id": task.ID, "candidate_commit": task.CandidateCommit,
		"specification": run.Specification, "task_graph": run.TaskGraph,
		"execution": task.Execution, "verification": task.Verification, "review": task.Review,
		"decision_timestamp": at.UTC(),
	})
	if err != nil {
		return workflow.CommandResult{}, err
	}
	approvalRef, err := s.artifacts.Put(ctx, approvalBody)
	if err != nil {
		return workflow.CommandResult{}, err
	}
	if task.State == workflow.TaskStateAwaitingApproval {
		_, err = s.workflow.ExecuteTask(ctx, workflow.ApproveTask{
			Meta: s.envelope(stableStepID(key, "approve-task"), principal.ID,
				workflow.ActorHuman, task.Revision, at),
			Run: run.ID, ID: task.ID, CandidateCommit: task.CandidateCommit,
			Review: *task.Review, Approval: approvalRef,
		})
		if err != nil {
			return workflow.CommandResult{}, err
		}
	}
	run, err = s.workflow.GetRun(ctx, run.ID)
	if err != nil {
		return workflow.CommandResult{}, err
	}
	steps := []struct {
		state workflow.RunState
		make  func(workflow.Run) workflow.RunCommand
	}{
		{workflow.RunStateExecuting, func(value workflow.Run) workflow.RunCommand {
			return workflow.RecordRunExecution{
				Meta: s.envelope(stableStepID(key, "aggregate-execution"), s.config.ServiceActorID,
					workflow.ActorWorkflowService, value.Revision, at),
				ID: value.ID, Execution: *task.Execution, CandidateCommit: task.CandidateCommit,
			}
		}},
		{workflow.RunStateVerifying, func(value workflow.Run) workflow.RunCommand {
			return workflow.RecordRunVerification{
				Meta: s.envelope(stableStepID(key, "aggregate-verification"), s.config.ServiceActorID,
					workflow.ActorWorkflowService, value.Revision, at),
				ID: value.ID, CandidateCommit: task.CandidateCommit, Evidence: *task.Verification,
			}
		}},
		{workflow.RunStateReviewing, func(value workflow.Run) workflow.RunCommand {
			return workflow.RecordRunReview{
				Meta: s.envelope(stableStepID(key, "aggregate-review"), s.config.ServiceActorID,
					workflow.ActorWorkflowService, value.Revision, at),
				ID: value.ID, CandidateCommit: task.CandidateCommit, Review: *task.Review,
			}
		}},
	}
	for _, step := range steps {
		if run.State == step.state {
			_, err = s.workflow.ExecuteRun(ctx, step.make(run))
			if err != nil {
				return workflow.CommandResult{}, err
			}
			run, err = s.workflow.GetRun(ctx, run.ID)
			if err != nil {
				return workflow.CommandResult{}, err
			}
		}
	}
	if run.State != workflow.RunStateAwaitingApproval || run.Review == nil {
		return workflow.CommandResult{}, workflow.ErrInvalidTransition
	}
	return s.workflow.ExecuteRun(ctx, workflow.ApproveRun{
		Meta: s.envelope(stableStepID(key, "approve-run"), principal.ID,
			workflow.ActorHuman, run.Revision, at),
		ID: run.ID, CandidateCommit: task.CandidateCommit,
		Review: *run.Review, Approval: approvalRef,
	})
}

func (*Server) envelope(
	commandID, actorID string, kind workflow.ActorKind, revision uint64, at time.Time,
) workflow.CommandEnvelope {
	return workflow.CommandEnvelope{
		CommandID: commandID, Actor: workflow.Actor{ID: actorID, Kind: kind},
		ExpectedRevision: revision, Timestamp: at.UTC(),
		CorrelationID: commandID, CausationID: commandID,
	}
}

func (s *Server) storeContent(ctx context.Context, content json.RawMessage) (workflow.ArtifactRef, error) {
	if len(content) == 0 || string(content) == "null" {
		return workflow.ArtifactRef{}, workflow.ErrInvalid
	}
	return s.artifacts.Put(ctx, content)
}

func stableStepID(key, step string) string {
	return uuid.NewSHA1(uuid.MustParse(key), []byte(step)).String()
}

func parseRunPath(value string) (string, string, bool) {
	const prefix = "/v1/runs/"
	if !strings.HasPrefix(value, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(value, prefix)
	id, suffix, _ := strings.Cut(rest, "/")
	if _, err := uuid.Parse(id); err != nil {
		return "", "", false
	}
	if suffix != "" {
		suffix = "/" + suffix
	}
	return id, suffix, true
}

func parseTaskPath(value string) (string, string, bool) {
	const prefix = "/v1/tasks/"
	if !strings.HasPrefix(value, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(value, prefix)
	id, suffix, _ := strings.Cut(rest, "/")
	if _, err := uuid.Parse(id); err != nil {
		return "", "", false
	}
	if suffix != "" {
		suffix = "/" + suffix
	}
	return id, suffix, true
}

func requiredRevision(value string) (uint64, error) {
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return 0, errors.New("quoted ETag required")
	}
	return strconv.ParseUint(value[1:len(value)-1], 10, 64)
}

func revisionETag(revision uint64) string { return fmt.Sprintf("\"%d\"", revision) }

func hasRole(principal Principal, role Role) bool {
	for _, candidate := range principal.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

func hasApprovalRole(principal Principal, elevated bool) bool {
	if elevated {
		return hasRole(principal, RoleElevatedApprover)
	}
	return hasRole(principal, RoleApprover) || hasRole(principal, RoleElevatedApprover)
}

func decodeStrict(request *http.Request, limit int64, destination any) ([]byte, error) {
	reader := io.LimitReader(request.Body, limit+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, errors.New("request body exceeds limit")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("request body exceeds limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("request body has trailing content")
	}
	return body, nil
}

func (s *Server) writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workflow.ErrUnauthorized):
		writeError(w, http.StatusForbidden, "forbidden", "principal lacks required role")
	case errors.Is(err, workflow.ErrNotFound), errors.Is(err, runtime.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, workflow.ErrRevisionConflict):
		writeError(w, http.StatusPreconditionFailed, "revision_conflict", "ETag revision conflict")
	case errors.Is(err, runtime.ErrInProgress):
		writeError(w, http.StatusConflict, "idempotency_in_progress", "request is still processing")
	case errors.Is(err, workflow.ErrCommandConflict), errors.Is(err, runtime.ErrConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency identity conflict")
	case errors.Is(err, workflow.ErrInvalid), errors.Is(err, workflow.ErrInvalidTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	default:
		s.logger.Error("control API request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "request failed")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		http.Error(w, `{"error":{"code":"internal","message":"encode response"}}`, 500)
		return
	}
	writeRawJSON(w, status, encoded)
}

func writeRawJSON(w http.ResponseWriter, status int, encoded []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}
