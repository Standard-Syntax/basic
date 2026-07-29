package verification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Config struct {
	ActorID        string
	Catalog        Catalog
	MaxConcurrent  int
	MaxWorkspaces  int
	ReservationTTL time.Duration
}

type Service struct {
	config     Config
	artifacts  ArtifactStore
	workflow   WorkflowStore
	workspaces WorkspacePreparer
	executor   CheckExecutor
	ledger     VerificationLedger
	now        func() time.Time
	executions chan struct{}
	workspace  chan struct{}
}

func NewService(
	config Config,
	artifacts ArtifactStore,
	workflowStore WorkflowStore,
	workspaces WorkspacePreparer,
	executor CheckExecutor,
	ledger VerificationLedger,
) (*Service, error) {
	if !servicePortsPresent(artifacts, workflowStore, workspaces, executor, ledger) {
		return nil, errors.New(
			"artifact, workflow, workspace, executor, and verification ledger ports are required",
		)
	}
	var err error
	config, err = normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return &Service{
		config: config, artifacts: artifacts, workflow: workflowStore,
		workspaces: workspaces, executor: executor, ledger: ledger, now: time.Now,
		executions: make(chan struct{}, config.MaxConcurrent),
		workspace:  make(chan struct{}, config.MaxWorkspaces),
	}, nil
}

func servicePortsPresent(
	artifacts ArtifactStore,
	workflowStore WorkflowStore,
	workspaces WorkspacePreparer,
	executor CheckExecutor,
	ledger VerificationLedger,
) bool {
	return artifacts != nil && workflowStore != nil && workspaces != nil &&
		executor != nil && ledger != nil
}

func normalizeConfig(config Config) (Config, error) {
	if _, err := uuid.Parse(config.ActorID); err != nil {
		return Config{}, errors.New("verification actor ID is required")
	}
	if len(config.Catalog.definitions) == 0 {
		config.Catalog = DefaultCatalog()
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = DefaultMaxConcurrent
	}
	if config.MaxWorkspaces == 0 {
		config.MaxWorkspaces = DefaultMaxWorkspaces
	}
	if config.ReservationTTL == 0 {
		config.ReservationTTL = DefaultCheckTimeout*time.Duration(DefaultMaxChecks) + time.Minute
	}
	if config.MaxConcurrent < 1 || config.MaxConcurrent > DefaultMaxConcurrent ||
		config.MaxWorkspaces < 1 || config.MaxWorkspaces > DefaultMaxWorkspaces ||
		config.ReservationTTL <= 0 {
		return Config{}, errors.New("invalid verification service limits")
	}
	return config, nil
}

func (s *Service) Verify(ctx context.Context, request Request) (Result, error) {
	mapped, report, plan, err := s.validateRequest(ctx, request)
	if err != nil {
		return Result{}, err
	}
	digest, err := verificationRequestDigest(request)
	if err != nil {
		return Result{}, err
	}
	handle, err := s.ledger.Begin(ctx, VerificationStart{
		VerificationID: request.VerificationID, RequestDigest: digest,
		Timestamp: request.VerificationTimestamp, ReservationTTL: s.config.ReservationTTL,
	})
	if err != nil {
		return Result{}, err
	}
	if replay, ok := handle.Replay(); ok {
		replay.Replay = true
		return replay, nil
	}
	evidence, ready := handle.Evidence()
	if !ready {
		release, err := s.acquireCapacity(ctx)
		if err != nil {
			return Result{}, err
		}
		evidence, err = s.collectEvidence(ctx, request, mapped, report, plan)
		release()
		if err != nil {
			return Result{}, err
		}
		if err := handle.SaveEvidence(ctx, evidence); err != nil {
			return Result{}, fmt.Errorf("checkpoint verification evidence: %w", err)
		}
	}
	return s.recordWorkflow(ctx, request, mapped, evidence, handle)
}

func (s *Service) validateRequest(
	ctx context.Context, request Request,
) (contracts.ImplementationRequest, execution.ExecutionReport, ResolvedPlan, error) {
	if !requestHeaderValid(request) {
		return contracts.ImplementationRequest{}, execution.ExecutionReport{}, ResolvedPlan{},
			ErrInvalidRequest
	}
	if err := request.ExecutionArtifact.Validate(); err != nil {
		return contracts.ImplementationRequest{}, execution.ExecutionReport{}, ResolvedPlan{},
			fmt.Errorf("%w: execution artifact: %v", ErrInvalidRequest, err)
	}
	mapped, err := contracts.MapImplementationRequestAt(request.Implementation, s.now().UTC())
	if err != nil {
		return contracts.ImplementationRequest{}, execution.ExecutionReport{}, ResolvedPlan{},
			fmt.Errorf("%w: implementation request: %v", ErrInvalidRequest, err)
	}
	plan, err := s.config.Catalog.Resolve(mapped, request.Requirements)
	if err != nil {
		return contracts.ImplementationRequest{}, execution.ExecutionReport{}, ResolvedPlan{}, err
	}
	report, err := s.loadExecutionReport(ctx, request.ExecutionArtifact)
	if err != nil {
		return contracts.ImplementationRequest{}, execution.ExecutionReport{}, ResolvedPlan{}, err
	}
	if !executionReportMatches(report, mapped, request.CandidateCommit) {
		return contracts.ImplementationRequest{}, execution.ExecutionReport{}, ResolvedPlan{},
			fmt.Errorf("%w: execution report binding", ErrInvalidRequest)
	}
	return mapped, report, plan, nil
}

func requestHeaderValid(request Request) bool {
	_, err := uuid.Parse(request.VerificationID)
	return err == nil &&
		!request.VerificationTimestamp.IsZero() &&
		request.Implementation != nil &&
		request.ExpectedTaskRevision > 0 &&
		commitPattern.MatchString(request.CandidateCommit)
}

func executionReportMatches(
	report execution.ExecutionReport,
	request contracts.ImplementationRequest,
	candidateCommit string,
) bool {
	return report.SchemaVersion == "1" &&
		report.RunID == request.Envelope.RunID &&
		report.TaskID == request.ApprovedTaskID &&
		report.Attempt == request.Envelope.Attempt &&
		report.BaseCommit == request.BaseCommit &&
		report.CandidateCommit == candidateCommit
}

func (s *Service) loadExecutionReport(
	ctx context.Context, artifact workflow.ArtifactRef,
) (execution.ExecutionReport, error) {
	body, err := s.artifacts.Get(ctx, artifact)
	if err != nil {
		return execution.ExecutionReport{}, fmt.Errorf("load execution artifact: %w", err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != artifact.Digest {
		return execution.ExecutionReport{}, ErrArtifactIntegrity
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var report execution.ExecutionReport
	if err := decoder.Decode(&report); err != nil {
		return execution.ExecutionReport{}, fmt.Errorf("%w: decode execution report", ErrArtifactIntegrity)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return execution.ExecutionReport{}, fmt.Errorf("%w: execution report trailer", ErrArtifactIntegrity)
	}
	return report, nil
}

func (s *Service) collectEvidence(
	ctx context.Context,
	request Request,
	mapped contracts.ImplementationRequest,
	executionReport execution.ExecutionReport,
	plan ResolvedPlan,
) (VerificationEvidence, error) {
	workspace, cleanup, err := s.workspaces.Prepare(
		ctx, request.VerificationID, request.CandidateCommit,
	)
	if err != nil {
		return VerificationEvidence{}, err
	}
	defer func() { _ = cleanup() }()
	imageID, err := s.executor.ImageID(ctx)
	if err != nil {
		return VerificationEvidence{}, fmt.Errorf("resolve verification image: %w", err)
	}
	results := make([]CheckResult, 0, len(plan.Checks))
	for _, check := range plan.Checks {
		measurement, err := s.executor.Run(ctx, workspace, imageID, check)
		if err != nil {
			return VerificationEvidence{}, fmt.Errorf("run check %s: %w", check.ID, err)
		}
		output, err := s.storeContent(ctx, measurement.Output)
		if err != nil {
			return VerificationEvidence{}, fmt.Errorf("store check output %s: %w", check.ID, err)
		}
		passed := measurement.ExitCode == 0 && !measurement.TimedOut &&
			!measurement.OutputTruncated
		results = append(results, CheckResult{
			CheckID: check.ID, CommandReference: check.CommandReference,
			Argv: slices.Clone(check.Argv), ImageID: imageID,
			CandidateCommit: request.CandidateCommit,
			StartedAt:       measurement.StartedAt.UTC().Format(time.RFC3339Nano),
			FinishedAt:      measurement.FinishedAt.UTC().Format(time.RFC3339Nano),
			ExitCode:        measurement.ExitCode, TimedOut: measurement.TimedOut,
			Output: output, OutputDigest: output.Digest,
			WallTimeNanos:   measurement.WallTime.Nanoseconds(),
			UserTimeNanos:   measurement.UserTime.Nanoseconds(),
			SystemTimeNanos: measurement.SystemTime.Nanoseconds(),
			PeakRSSBytes:    measurement.PeakRSSBytes, Passed: passed,
		})
	}
	coverage, passed := CalculateCoverage(plan.Requirements, results)
	report := VerificationReport{
		SchemaVersion: "1", VerificationID: request.VerificationID,
		VerifiedAt: request.VerificationTimestamp.UTC().Format(time.RFC3339Nano),
		RunID:      mapped.Envelope.RunID, TaskID: mapped.ApprovedTaskID,
		Attempt: mapped.Envelope.Attempt, Execution: request.ExecutionArtifact,
		BaseCommit: executionReport.BaseCommit, CandidateCommit: request.CandidateCommit,
		ImageID: imageID, Checks: results, Coverage: coverage, Passed: passed,
	}
	reportBody, err := marshalReport(report)
	if err != nil {
		return VerificationEvidence{}, err
	}
	reportArtifact, err := s.storeContent(ctx, reportBody)
	if err != nil {
		return VerificationEvidence{}, fmt.Errorf("store verification report: %w", err)
	}
	return VerificationEvidence{
		ReportArtifact: reportArtifact, CandidateCommit: request.CandidateCommit,
		Coverage: coverage, Passed: passed,
	}, nil
}

func (s *Service) storeContent(
	ctx context.Context, body []byte,
) (workflow.ArtifactRef, error) {
	artifact, err := s.artifacts.Put(ctx, body)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	sum := sha256.Sum256(body)
	if err := artifact.Validate(); err != nil ||
		artifact.Digest != hex.EncodeToString(sum[:]) {
		return workflow.ArtifactRef{}, ErrArtifactIntegrity
	}
	return artifact, nil
}

func (s *Service) recordWorkflow(
	ctx context.Context,
	request Request,
	mapped contracts.ImplementationRequest,
	evidence VerificationEvidence,
	handle VerificationHandle,
) (Result, error) {
	timestamp, err := handle.FinalTransitionTime(ctx, s.now().UTC())
	if err != nil {
		return Result{}, fmt.Errorf("reserve verification transition timestamp: %w", err)
	}
	recorded, err := s.workflow.ExecuteTask(ctx, workflow.RecordTaskVerification{
		Meta: s.commandEnvelope(
			request.VerificationID, request.ExpectedTaskRevision, timestamp,
		),
		Run: mapped.Envelope.RunID, ID: mapped.ApprovedTaskID,
		CandidateCommit: request.CandidateCommit,
		Evidence:        evidence.ReportArtifact, Passed: evidence.Passed,
	})
	if err != nil {
		return Result{}, fmt.Errorf("record task verification: %w", err)
	}
	result := Result{
		VerificationID:  request.VerificationID,
		CandidateCommit: request.CandidateCommit,
		ReportArtifact:  evidence.ReportArtifact,
		Coverage:        slices.Clone(evidence.Coverage),
		Passed:          evidence.Passed, Replay: recorded.Replay,
	}
	if err := handle.Complete(ctx, result); err != nil {
		retry, beginErr := s.ledger.Begin(ctx, VerificationStart{
			VerificationID: request.VerificationID,
			RequestDigest:  mustRequestDigest(request),
			Timestamp:      request.VerificationTimestamp, ReservationTTL: s.config.ReservationTTL,
		})
		if beginErr == nil {
			if replay, ok := retry.Replay(); ok {
				replay.Replay = true
				return replay, nil
			}
		}
		return Result{}, fmt.Errorf("finalize verification ledger: %w", err)
	}
	return result, nil
}

func mustRequestDigest(request Request) string {
	digest, _ := verificationRequestDigest(request)
	return digest
}

func (s *Service) commandEnvelope(
	verificationID string, revision uint64, timestamp time.Time,
) workflow.CommandEnvelope {
	id := func(label string) string {
		return uuid.NewSHA1(
			uuid.NameSpaceURL, []byte("harness:verification:"+verificationID+":"+label),
		).String()
	}
	return workflow.CommandEnvelope{
		CommandID: id("record"), Actor: workflow.Actor{
			ID: s.config.ActorID, Kind: workflow.ActorVerificationService,
		},
		ExpectedRevision: revision, Timestamp: timestamp,
		CorrelationID: id("correlation"), CausationID: id("record:cause"),
	}
}

func (s *Service) acquireCapacity(ctx context.Context) (func(), error) {
	if err := acquire(ctx, s.executions); err != nil {
		return nil, err
	}
	if err := acquire(ctx, s.workspace); err != nil {
		release(s.executions)
		return nil, err
	}
	return func() {
		release(s.workspace)
		release(s.executions)
	}, nil
}

func acquire(ctx context.Context, semaphore chan struct{}) error {
	select {
	case semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release(semaphore chan struct{}) { <-semaphore }

func ensureJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("unexpected JSON trailer")
	}
	return nil
}

func marshalReport(report VerificationReport) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		return nil, fmt.Errorf("encode verification report: %w", err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}
