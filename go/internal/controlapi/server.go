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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	DefaultMaxBodyBytes int64 = 1 << 20
	runIntakeTimeout          = 30 * time.Second
)

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
	ListPendingApprovals(context.Context) ([]workflow.PendingApproval, error)
}

type IdempotencyLedger interface {
	BeginIdempotency(context.Context, runtime.IdempotencyRequest) (*runtime.IdempotencyResult, error)
	CompleteIdempotency(context.Context, string, uint64, int, json.RawMessage) error
	AbandonIdempotency(context.Context, string, uint64) error
	Enqueue(context.Context, runtime.Job) error
	CancelRun(context.Context, string, time.Time) error
	RunStages(context.Context, string) ([]runtime.StageStatus, error)
}

type ArtifactStore interface {
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
	Get(context.Context, workflow.ArtifactRef) ([]byte, error)
}

type BindingStore interface {
	CreateRun(context.Context, runtime.RunBinding) error
	GetRun(context.Context, string) (runtime.RunBinding, error)
	GetTask(context.Context, string, string) (runtime.TaskBinding, error)
	CheckpointSpecification(context.Context, string, workflow.ArtifactRef) error
	CheckpointTaskGraph(
		context.Context, string, workflow.ArtifactRef, runtime.TaskBinding,
	) error
	CheckpointApproval(context.Context, string, workflow.ArtifactRef) error
}

type ApprovalService interface {
	ApproveTask(context.Context, approval.Request) (approval.Result, error)
}

type PublicationService interface {
	Publish(context.Context, publication.Request) (publication.Result, error)
}

type Config struct {
	Principals     []Principal
	ServiceActorID string
	MaxBodyBytes   int64
	TrustedChecks  []string
	Policy         beta.Policy
	Ready          func(context.Context) error
}

type Server struct {
	config      Config
	workflow    WorkflowStore
	runtime     IdempotencyLedger
	intake      RunIntake
	artifacts   ArtifactStore
	bindings    BindingStore
	approval    ApprovalService
	publication PublicationService
	principals  []principalDigest
	checks      map[string]struct{}
	logger      *slog.Logger
	requests    atomic.Uint64
	failures    atomic.Uint64
	elapsedNS   atomic.Uint64
}

type principalDigest struct {
	principal Principal
	digest    [sha256.Size]byte
}

func New(
	config Config, workflowStore WorkflowStore, runtimeLedger IdempotencyLedger,
	runIntake RunIntake, artifacts ArtifactStore, bindings BindingStore, approvalService ApprovalService,
	publicationService PublicationService, logger *slog.Logger,
) (*Server, error) {
	normalized, err := normalizeServerConfig(config)
	if err != nil {
		return nil, err
	}
	principals, err := compilePrincipals(normalized.Principals)
	if err != nil {
		return nil, err
	}
	if len(principals) == 0 || bindings == nil || artifacts == nil || runIntake == nil ||
		approvalService == nil {
		return nil, errors.New("at least one principal is required")
	}
	checks, err := compileTrustedChecks(normalized.TrustedChecks)
	if err != nil {
		return nil, err
	}
	if err := normalized.Policy.Validate(); err != nil {
		return nil, err
	}
	if !slices.Equal(normalized.TrustedChecks, normalized.Policy.TrustedChecks) {
		return nil, errors.New("trusted checks do not match beta policy")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		config: normalized, workflow: workflowStore, runtime: runtimeLedger, intake: runIntake,
		artifacts: artifacts, bindings: bindings, approval: approvalService,
		publication: publicationService, principals: principals,
		checks: checks, logger: logger,
	}, nil
}

func normalizeServerConfig(config Config) (Config, error) {
	if _, err := uuid.Parse(config.ServiceActorID); err != nil {
		return Config{}, errors.New("invalid service actor ID")
	}
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = DefaultMaxBodyBytes
	}
	config.TrustedChecks = slices.Clone(config.TrustedChecks)
	slices.Sort(config.TrustedChecks)
	return config, nil
}

func compilePrincipals(values []Principal) ([]principalDigest, error) {
	seen := make(map[string]struct{})
	principals := make([]principalDigest, 0, len(values))
	for _, principal := range values {
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
	return principals, nil
}

func compileTrustedChecks(values []string) (map[string]struct{}, error) {
	checks := make(map[string]struct{}, len(values))
	for _, check := range values {
		if check == "" || strings.TrimSpace(check) != check {
			return nil, errors.New("trusted check IDs must be non-empty")
		}
		if _, exists := checks[check]; exists {
			return nil, errors.New("trusted check IDs must be unique")
		}
		checks[check] = struct{}{}
	}
	return checks, nil
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if request.URL.Path == "/readyz" {
		if s.config.Ready != nil {
			ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
			err := s.config.Ready(ctx)
			cancel()
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	principal, ok := s.authenticate(request)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "valid bearer token required")
		return
	}
	counted := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	started := time.Now()
	defer func() {
		elapsed := time.Since(started)
		s.requests.Add(1)
		s.elapsedNS.Add(uint64(elapsed))
		if counted.status >= http.StatusBadRequest {
			s.failures.Add(1)
		}
		attributes := []any{
			"method", request.Method, "path", request.URL.Path,
			"status", counted.status, "elapsed_nanoseconds", elapsed.Nanoseconds(),
			"principal_id", principal.ID,
			"idempotency_key", request.Header.Get("Idempotency-Key"),
		}
		if runID, _, ok := parseRunPath(request.URL.Path); ok {
			attributes = append(attributes, "run_id", runID)
		}
		if taskID, _, ok := parseTaskPath(request.URL.Path); ok {
			attributes = append(attributes, "task_id", taskID)
		}
		s.logger.Info("control API request", attributes...)
	}()
	w = counted
	if request.URL.Path == "/metrics" {
		writeJSON(w, http.StatusOK, map[string]uint64{
			"requests_total":                    s.requests.Load(),
			"failures_total":                    s.failures.Load(),
			"request_elapsed_nanoseconds_total": s.elapsedNS.Load(),
		})
		return
	}
	if request.Method == http.MethodGet {
		s.handleGet(w, request, principal)
		return
	}
	s.handleMutation(w, request, principal)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
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
		approvals, err := s.workflow.ListPendingApprovals(request.Context())
		if err != nil {
			s.writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"approvals": approvals})
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
		stages, err := s.runtime.RunStages(request.Context(), runID)
		if err != nil {
			s.writeDomainError(w, err)
			return
		}
		w.Header().Set("ETag", revisionETag(run.Revision))
		writeJSON(w, http.StatusOK, map[string]any{"run": run, "tasks": tasks, "stages": stages})
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
	RunID        string          `json:"run_id,omitempty"`
	BaseCommit   string          `json:"base_commit,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	Request      json.RawMessage `json:"request,omitempty"`
	Proposal     json.RawMessage `json:"proposal,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Terminal     bool            `json:"terminal,omitempty"`
	DecisionTime time.Time       `json:"decision_timestamp"`
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
	if request.URL.Path == "/v1/runs" {
		s.handleRunIntake(w, request, principal, body, idem)
		return
	}
	reservation, err := s.runtime.BeginIdempotency(request.Context(), idem)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	if reservation.Replay {
		w.Header().Set("Idempotent-Replay", "true")
		writeRawJSON(w, reservation.StatusCode, reservation.Response)
		return
	}
	status, response, err := s.applyMutation(request, principal, key, body)
	if err != nil {
		status, response = s.domainError(err)
		if status >= http.StatusInternalServerError {
			if abandonErr := s.runtime.AbandonIdempotency(
				request.Context(), key, reservation.FencingToken,
			); abandonErr != nil {
				s.writeDomainError(w, abandonErr)
				return
			}
			writeJSON(w, status, response)
			return
		}
		encoded, marshalErr := json.Marshal(response)
		if marshalErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", "encode response")
			return
		}
		if completeErr := s.runtime.CompleteIdempotency(
			request.Context(), key, reservation.FencingToken, status, encoded,
		); completeErr != nil {
			s.writeDomainError(w, completeErr)
			return
		}
		writeRawJSON(w, status, encoded)
		return
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "encode response")
		return
	}
	if err := s.runtime.CompleteIdempotency(
		request.Context(), key, reservation.FencingToken, status, encoded,
	); err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeRawJSON(w, status, encoded)
}

func (s *Server) handleRunIntake(
	w http.ResponseWriter, request *http.Request, principal Principal, body mutationBody,
	idempotency runtime.IdempotencyRequest,
) {
	if !hasRole(principal, RoleOperator) {
		s.writeDomainError(w, workflow.ErrUnauthorized)
		return
	}
	if body.DecisionTime.IsZero() {
		writeError(w, http.StatusBadRequest, "invalid_request", "decision_timestamp is required")
		return
	}
	if _, err := uuid.Parse(body.RunID); err != nil || len(body.BaseCommit) != 40 ||
		len(body.Content) == 0 || string(body.Content) == "null" {
		s.writeDomainError(w, workflow.ErrInvalid)
		return
	}
	for _, value := range body.BaseCommit {
		if !strings.ContainsRune("0123456789abcdef", value) {
			s.writeDomainError(w, workflow.ErrInvalid)
			return
		}
	}
	intakeContext, cancel := context.WithTimeout(request.Context(), runIntakeTimeout)
	defer cancel()
	result, err := s.intake.Accept(intakeContext, RunIntakeRequest{
		Idempotency: idempotency, Content: body.Content, BaseCommit: body.BaseCommit,
		Command: workflow.CreateRun{
			Meta: s.envelope(
				idempotency.Key, principal.ID, workflow.ActorHuman, 0, body.DecisionTime,
			),
			ID: body.RunID,
		},
	})
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeRawJSON(w, result.StatusCode, result.Response)
}

func (s *Server) applyMutation(
	request *http.Request, principal Principal, key string, body mutationBody,
) (int, any, error) {
	if body.DecisionTime.IsZero() {
		return 0, nil, errors.New("decision_timestamp is required")
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
		var specificationRequest reasoningv1.SpecificationRequest
		var specificationProposal reasoningv1.SpecificationProposal
		if err := decodeProtoPair(
			body.Request, body.Proposal, &specificationRequest, &specificationProposal,
		); err != nil {
			return 0, nil, workflow.ErrInvalid
		}
		ref, err := runtime.BuildApprovedSpecification(
			ctx, s.artifacts, &specificationRequest, &specificationProposal,
		)
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
		if err == nil {
			err = s.bindings.CheckpointSpecification(ctx, run.ID, *run.Specification)
		}
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
		var planningRequest reasoningv1.TaskPlanningRequest
		var graphProposal reasoningv1.TaskGraphProposal
		if err := decodeProtoPair(
			body.Request, body.Proposal, &planningRequest, &graphProposal,
		); err != nil {
			return 0, nil, workflow.ErrInvalid
		}
		ref, _, _, err := runtime.BuildApprovedTaskGraph(
			ctx, s.artifacts, &planningRequest, &graphProposal, s.checks, s.config.Policy,
		)
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
		graphBody, err := s.artifacts.Get(ctx, *run.TaskGraph)
		if err != nil {
			return 0, nil, err
		}
		var graph reasoningv1.TaskGraphProposal
		if err := proto.Unmarshal(graphBody, &graph); err != nil ||
			len(graph.GetTasks()) != 1 || len(graph.GetTasks()[0].GetDependencies()) != 0 {
			return 0, nil, workflow.ErrInvalid
		}
		task := graph.GetTasks()[0]
		taskBody, err := proto.MarshalOptions{Deterministic: true}.Marshal(task)
		if err != nil {
			return 0, nil, err
		}
		taskRef, err := s.artifacts.Put(ctx, taskBody)
		if err != nil {
			return 0, nil, err
		}
		definition := workflow.TaskDefinition{ID: task.GetTaskId(), MaxAttempts: 1}
		result, err := s.workflow.ExecuteRun(ctx, workflow.ApproveTaskGraph{
			Meta: humanMeta("approve-task-graph", run.Revision), ID: run.ID,
			TaskGraph: *run.TaskGraph, Tasks: []workflow.TaskDefinition{definition},
		})
		if err == nil {
			taskID := definition.ID
			err = s.bindings.CheckpointTaskGraph(ctx, run.ID, *run.TaskGraph,
				runtime.TaskBinding{RunID: run.ID, TaskID: taskID, ApprovedTask: taskRef})
		}
		if err == nil {
			taskID := definition.ID
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
		result, publicationResult, err := s.compositeApprove(
			ctx, principal, key, body.DecisionTime, run,
		)
		response := map[string]any{"result": result}
		if publicationResult == nil {
			response["publication"] = map[string]string{
				"status": "configuration_blocked", "reason": "publication is not configured",
			}
		} else {
			response["publication"] = publicationResult
		}
		return http.StatusOK, response, err
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
) (approval.Result, *publication.Result, error) {
	tasks, err := s.workflow.ListTasks(ctx, run.ID)
	if err != nil || len(tasks) != 1 {
		return approval.Result{}, nil, workflow.ErrInvalid
	}
	task := tasks[0]
	if task.Execution == nil || task.Verification == nil || task.Review == nil ||
		task.Proposal == nil || task.CandidateCommit == "" ||
		run.Specification == nil || run.TaskGraph == nil {
		return approval.Result{}, nil, workflow.ErrInvalidTransition
	}
	runBinding, err := s.bindings.GetRun(ctx, run.ID)
	if err != nil {
		return approval.Result{}, nil, err
	}
	if runBinding.ApprovedSpecification == nil {
		return approval.Result{}, nil, workflow.ErrInvalidTransition
	}
	taskBinding, err := s.bindings.GetTask(ctx, run.ID, task.ID)
	if err != nil {
		return approval.Result{}, nil, err
	}
	executionBody, err := s.artifacts.Get(ctx, *task.Execution)
	if err != nil {
		return approval.Result{}, nil, err
	}
	var executionReport execution.ExecutionReport
	if err := json.Unmarshal(executionBody, &executionReport); err != nil {
		return approval.Result{}, nil, err
	}
	changedPaths := make([]string, 0, len(executionReport.ActualDiff))
	for _, entry := range executionReport.ActualDiff {
		changedPaths = append(changedPaths, entry.Path)
	}
	taskBody, err := s.artifacts.Get(ctx, taskBinding.ApprovedTask)
	if err != nil {
		return approval.Result{}, nil, err
	}
	var plannedTask reasoningv1.PlannedTask
	if err := proto.Unmarshal(taskBody, &plannedTask); err != nil {
		return approval.Result{}, nil, err
	}
	roles := make([]approval.Role, 0, len(principal.Roles))
	if hasRole(principal, RoleApprover) {
		roles = append(roles, approval.RoleApprover)
	}
	if hasRole(principal, RoleElevatedApprover) {
		roles = append(roles, approval.RoleElevatedApprover)
	}
	approvalResult, err := s.approval.ApproveTask(ctx, approval.Request{
		ApprovalID: stableStepID(key, "approve-task"), DecisionTimestamp: at,
		Principal: approval.Principal{ID: principal.ID, Roles: roles},
		RunID:     run.ID, TaskID: task.ID, CandidateCommit: task.CandidateCommit,
		ApprovedSpecificationDigest: runBinding.ApprovedSpecification.Digest,
		ApprovedTaskDigest:          taskBinding.ApprovedTask.Digest,
		ActualChangedPaths:          changedPaths,
		ExclusiveResourceLabels:     plannedTask.GetExclusiveResources(),
		Implementation:              *task.Proposal, Execution: *task.Execution,
		Verification: *task.Verification, Review: *task.Review,
		ExpectedTaskRevision: task.Revision,
	})
	if err != nil {
		return approval.Result{}, nil, err
	}
	if err := s.bindings.CheckpointApproval(
		ctx, run.ID, approvalResult.ApprovalArtifact,
	); err != nil {
		return approval.Result{}, nil, err
	}
	if task.State == workflow.TaskStateAwaitingApproval {
		task.State = workflow.TaskStateAccepted
	}
	run, err = s.workflow.GetRun(ctx, run.ID)
	if err != nil {
		return approval.Result{}, nil, err
	}
	steps := []struct {
		state workflow.RunState
		make  func(workflow.Run) workflow.RunCommand
	}{
		{workflow.RunStateExecuting, func(value workflow.Run) workflow.RunCommand {
			return workflow.RecordRunExecution{
				Meta: s.envelope(stableStepID(key, "aggregate-execution"), s.config.ServiceActorID,
					workflow.ActorExecutionService, value.Revision, at),
				ID: value.ID, Execution: *task.Execution, CandidateCommit: task.CandidateCommit,
			}
		}},
		{workflow.RunStateVerifying, func(value workflow.Run) workflow.RunCommand {
			return workflow.RecordRunVerification{
				Meta: s.envelope(stableStepID(key, "aggregate-verification"), s.config.ServiceActorID,
					workflow.ActorVerificationService, value.Revision, at),
				ID: value.ID, CandidateCommit: task.CandidateCommit, Evidence: *task.Verification,
			}
		}},
		{workflow.RunStateReviewing, func(value workflow.Run) workflow.RunCommand {
			return workflow.RecordRunReview{
				Meta: s.envelope(stableStepID(key, "aggregate-review"), s.config.ServiceActorID,
					workflow.ActorReviewService, value.Revision, at),
				ID: value.ID, CandidateCommit: task.CandidateCommit, Review: *task.Review,
			}
		}},
	}
	for _, step := range steps {
		if run.State == step.state {
			_, err = s.workflow.ExecuteRun(ctx, step.make(run))
			if err != nil {
				return approval.Result{}, nil, err
			}
			run, err = s.workflow.GetRun(ctx, run.ID)
			if err != nil {
				return approval.Result{}, nil, err
			}
		}
	}
	if run.State != workflow.RunStateAwaitingApproval || run.Review == nil {
		return approval.Result{}, nil, workflow.ErrInvalidTransition
	}
	_, err = s.workflow.ExecuteRun(ctx, workflow.ApproveRun{
		Meta: s.envelope(stableStepID(key, "approve-run"), principal.ID,
			workflow.ActorHuman, run.Revision, at),
		ID: run.ID, CandidateCommit: task.CandidateCommit,
		Review: *run.Review, Approval: approvalResult.ApprovalArtifact,
	})
	if err != nil {
		return approval.Result{}, nil, err
	}
	if s.publication == nil {
		return approvalResult, nil, nil
	}
	run, err = s.workflow.GetRun(ctx, run.ID)
	if err != nil {
		return approval.Result{}, nil, err
	}
	publicationResult, err := s.publication.Publish(ctx, publication.Request{
		PublicationID: stableStepID(key, "publish"), PublicationTimestamp: at,
		RunID: run.ID, BaseCommit: runBinding.BaseCommit,
		CandidateCommit: task.CandidateCommit,
		Specification:   *runBinding.ApprovedSpecification,
		Implementation:  *task.Proposal, Execution: *task.Execution,
		Verification: *task.Verification, Review: *task.Review,
		Approval: approvalResult.ApprovalArtifact, ExpectedRunRevision: run.Revision,
	})
	if err != nil {
		return approval.Result{}, nil, err
	}
	return approvalResult, &publicationResult, nil
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

func decodeProtoPair(
	requestBody, proposalBody json.RawMessage, request, proposal proto.Message,
) error {
	if len(requestBody) == 0 || len(proposalBody) == 0 ||
		string(requestBody) == "null" || string(proposalBody) == "null" {
		return workflow.ErrInvalid
	}
	options := protojson.UnmarshalOptions{DiscardUnknown: false}
	if err := options.Unmarshal(requestBody, request); err != nil {
		return err
	}
	return options.Unmarshal(proposalBody, proposal)
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
	status, response := s.domainError(err)
	writeJSON(w, status, response)
}

func (s *Server) domainError(err error) (int, any) {
	code, message, status := "internal", "request failed", http.StatusInternalServerError
	switch {
	case errors.Is(err, workflow.ErrUnauthorized):
		code, message, status = "forbidden", "principal lacks required role", http.StatusForbidden
	case errors.Is(err, workflow.ErrNotFound), errors.Is(err, runtime.ErrNotFound):
		code, message, status = "not_found", "resource not found", http.StatusNotFound
	case errors.Is(err, workflow.ErrRevisionConflict):
		code, message, status = "revision_conflict", "ETag revision conflict", http.StatusPreconditionFailed
	case errors.Is(err, runtime.ErrInProgress):
		code, message, status = "idempotency_in_progress", "request is still processing", http.StatusConflict
	case errors.Is(err, workflow.ErrCommandConflict), errors.Is(err, runtime.ErrConflict),
		errors.Is(err, runtime.ErrStaleFence), errors.Is(err, runtime.ErrTerminal):
		code, message, status = "idempotency_conflict", "idempotency identity conflict", http.StatusConflict
	case errors.Is(err, workflow.ErrInvalid), errors.Is(err, workflow.ErrInvalidTransition):
		code, message, status = "invalid_transition", err.Error(), http.StatusUnprocessableEntity
	default:
		s.logger.Error("control API request failed", "error", err)
	}
	return status, map[string]any{"error": map[string]string{"code": code, "message": message}}
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
