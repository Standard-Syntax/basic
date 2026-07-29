package contracts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type ImplementationRequest struct {
	Envelope                    Envelope
	ApprovedTaskID              string
	ApprovedTaskDigest          string
	ApprovedSpecificationID     string
	ApprovedSpecificationDigest string
	BaseCommit                  string
	ReadablePaths               []string
	WritablePaths               []string
	ProhibitedPaths             []string
	AcceptanceCriterionIDs      []string
	AvailableCheckIDs           []string
	RepositoryContext           []RepositoryContextFile
}

type RepositoryContextFile struct {
	Path    string
	SHA256  string
	Content string
}

type FileOperation string

const (
	FileCreate FileOperation = "create"
	FileUpdate FileOperation = "update"
	FileDelete FileOperation = "delete"
)

type FileChange struct {
	Path                   string
	Operation              FileOperation
	ExpectedOriginalSHA256 string
	ReplacementContent     *string
	Rationale              string
	AcceptanceCriterionIDs []string
}

type ImplementationProposal struct {
	Summary                   string
	Changes                   []FileChange
	RequestedDeclaredCheckIDs []string
	Assumptions               []string
	UnresolvedQuestions       []string
	ScopeChangeRequest        *ScopeChangeRequest
}

type ScopeChangeRequest struct {
	Summary                         string
	RequestedReadablePaths          []string
	RequestedWritablePaths          []string
	RequestedAcceptanceCriterionIDs []string
	RequestedCheckIDs               []string
}

type ImplementationReasoner interface {
	ProposeImplementation(context.Context, ImplementationRequest) (ImplementationProposal, error)
}

func MapImplementationRequest(value *reasoningv1.ImplementationRequest) (ImplementationRequest, error) {
	return MapImplementationRequestAt(value, time.Time{})
}

// MapImplementationRequestAt validates an implementation request at now. A
// zero now preserves the transport-only mapping behavior for stored fixtures.
func MapImplementationRequestAt(
	value *reasoningv1.ImplementationRequest, now time.Time,
) (ImplementationRequest, error) {
	if err := validateImplementationRequest(value, now); err != nil {
		return ImplementationRequest{}, err
	}
	return convertImplementationRequest(value)
}

func convertImplementationRequest(
	value *reasoningv1.ImplementationRequest,
) (ImplementationRequest, error) {
	if value == nil {
		return ImplementationRequest{}, errors.New("implementation request is required")
	}
	envelope, err := MapEnvelope(value.GetEnvelope(), StageImplementation)
	if err != nil {
		return ImplementationRequest{}, err
	}
	if err := validateImplementationRequestFields(value); err != nil {
		return ImplementationRequest{}, err
	}
	if err := validatePathScopes(
		value.GetReadablePaths(), value.GetWritablePaths(), value.GetProhibitedPaths(),
	); err != nil {
		return ImplementationRequest{}, err
	}
	if err := validateAvailableChecks(value.GetAvailableCheckIds()); err != nil {
		return ImplementationRequest{}, err
	}
	repositoryContext, err := mapRepositoryContext(value, envelope.InputArtifacts)
	if err != nil {
		return ImplementationRequest{}, err
	}
	return ImplementationRequest{
		Envelope:                    envelope,
		ApprovedTaskID:              value.GetApprovedTaskId(),
		ApprovedTaskDigest:          value.GetApprovedTaskDigest(),
		ApprovedSpecificationID:     value.GetApprovedSpecificationId(),
		ApprovedSpecificationDigest: value.GetApprovedSpecificationDigest(),
		BaseCommit:                  value.GetBaseCommit(),
		ReadablePaths:               value.GetReadablePaths(), WritablePaths: value.GetWritablePaths(),
		ProhibitedPaths:        value.GetProhibitedPaths(),
		AcceptanceCriterionIDs: value.GetAcceptanceCriterionIds(),
		AvailableCheckIDs:      value.GetAvailableCheckIds(),
		RepositoryContext:      repositoryContext,
	}, nil
}

func validateImplementationRequest(
	value *reasoningv1.ImplementationRequest, now time.Time,
) error {
	if err := validateImplementationRequestSchema(value); err != nil {
		return err
	}
	if err := validateImplementationRequestIdentity(value, now); err != nil {
		return err
	}
	if err := validateImplementationRequestBindings(value); err != nil {
		return err
	}
	if err := validateImplementationRequestAuthority(value); err != nil {
		return err
	}
	if err := validateImplementationRequestScope(value); err != nil {
		return err
	}
	if err := validateImplementationRequestCoverage(value); err != nil {
		return err
	}
	return validateImplementationRequestPolicy(value)
}

func validateImplementationRequestSchema(value *reasoningv1.ImplementationRequest) error {
	if value == nil || value.GetEnvelope() == nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			"request", "implementation request and envelope are required",
		)
	}
	envelope := value.GetEnvelope()
	if envelope.GetSchemaVersion() != "1" || envelope.GetCreatedAt() == nil ||
		envelope.GetExpiresAt() == nil || envelope.GetAuthority() == nil ||
		envelope.GetBudget() == nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			"request.envelope", "required v1 envelope fields are missing",
		)
	}
	for _, file := range value.GetRepositoryContext() {
		if file == nil || file.GetContent() == "" {
			return validationFailure(
				reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
				"request.repository_context", "repository context entries require content",
			)
		}
	}
	return nil
}

func validateImplementationRequestIdentity(
	value *reasoningv1.ImplementationRequest, now time.Time,
) error {
	envelope := value.GetEnvelope()
	_, expires, err := mapEnvelopeTimes(envelope)
	if err != nil || envelope.GetRequestId() == "" || envelope.GetRunId() == "" ||
		envelope.GetTaskId() == "" || envelope.GetAttempt() == 0 ||
		!taskIDPattern.MatchString(value.GetApprovedTaskId()) ||
		envelope.GetTaskId() != value.GetApprovedTaskId() ||
		value.GetApprovedSpecificationId() == "" {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
			"request.envelope", "request identity, attempt, or lifetime is invalid",
		)
	}
	if !now.IsZero() && !expires.After(now) {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
			"request.envelope.expires_at", "request has expired",
		)
	}
	return nil
}

func validateImplementationRequestBindings(value *reasoningv1.ImplementationRequest) error {
	envelope := value.GetEnvelope()
	if !digestPattern.MatchString(envelope.GetAgentManifestDigest()) ||
		!digestPattern.MatchString(value.GetApprovedTaskDigest()) ||
		!digestPattern.MatchString(value.GetApprovedSpecificationDigest()) {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
			"request", "artifact or manifest digest binding is invalid",
		)
	}
	artifacts, err := mapArtifacts(envelope.GetInputArtifacts())
	if err != nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
			"request.envelope.input_artifacts", err.Error(),
		)
	}
	for _, file := range value.GetRepositoryContext() {
		if !digestPattern.MatchString(file.GetSha256()) {
			return validationFailure(
				reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
				"request.repository_context.sha256", "invalid repository context digest",
			)
		}
		sum := sha256.Sum256([]byte(file.GetContent()))
		if hex.EncodeToString(sum[:]) != file.GetSha256() ||
			!artifactContainsDigest(artifacts, file.GetSha256()) {
			return validationFailure(
				reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
				"request.repository_context", "repository context is not bound to its artifact",
			)
		}
	}
	return nil
}

func validateImplementationRequestAuthority(value *reasoningv1.ImplementationRequest) error {
	if err := validateAuthority(value.GetEnvelope().GetAuthority()); err != nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_AUTHORITY_VIOLATION,
			"request.envelope.authority", err.Error(),
		)
	}
	return nil
}

func validateImplementationRequestScope(value *reasoningv1.ImplementationRequest) error {
	if err := validatePathScopes(
		value.GetReadablePaths(), value.GetWritablePaths(), value.GetProhibitedPaths(),
	); err != nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCOPE_VIOLATION,
			"request.paths", err.Error(),
		)
	}
	for _, file := range value.GetRepositoryContext() {
		if !validRepoPath(file.GetPath()) ||
			!pathWithin(file.GetPath(), value.GetReadablePaths()) ||
			pathIsProhibited(file.GetPath(), value.GetProhibitedPaths()) {
			return validationFailure(
				reasoningv1.RejectionCode_REJECTION_CODE_SCOPE_VIOLATION,
				"request.repository_context.path", "repository context is outside readable scope",
			)
		}
	}
	return nil
}

func validateImplementationRequestCoverage(value *reasoningv1.ImplementationRequest) error {
	if !validCriteria(value.GetAcceptanceCriterionIds()) {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_REQUIRED_COVERAGE_MISSING,
			"request.acceptance_criterion_ids", "assigned acceptance criteria are required",
		)
	}
	return nil
}

func validateImplementationRequestPolicy(value *reasoningv1.ImplementationRequest) error {
	if value.GetEnvelope().GetStage() !=
		reasoningv1.ReasoningStage_REASONING_STAGE_IMPLEMENTATION {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			"request.envelope.stage", "implementation stage is required",
		)
	}
	if err := validateEnvelopeBudget(value.GetEnvelope()); err != nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			"request.envelope.budget", err.Error(),
		)
	}
	if !commitPattern.MatchString(value.GetBaseCommit()) {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			"request.base_commit", "invalid base commit",
		)
	}
	if err := validateAvailableChecks(value.GetAvailableCheckIds()); err != nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			"request.available_check_ids", err.Error(),
		)
	}
	return nil
}

func validateImplementationRequestFields(value *reasoningv1.ImplementationRequest) error {
	if !taskIDPattern.MatchString(value.GetApprovedTaskId()) ||
		!digestPattern.MatchString(value.GetApprovedTaskDigest()) ||
		value.GetApprovedSpecificationId() == "" ||
		!digestPattern.MatchString(value.GetApprovedSpecificationDigest()) ||
		!commitPattern.MatchString(value.GetBaseCommit()) ||
		!validCriteria(value.GetAcceptanceCriterionIds()) ||
		len(value.GetAvailableCheckIds()) == 0 {
		return errors.New("invalid implementation request identity or limits")
	}
	return nil
}

func validateAvailableChecks(values []string) error {
	for _, check := range values {
		if !checkIDPattern.MatchString(check) {
			return errors.New("invalid available check ID")
		}
	}
	return nil
}

func mapRepositoryContext(
	value *reasoningv1.ImplementationRequest, artifacts []Artifact,
) ([]RepositoryContextFile, error) {
	repositoryContext := make([]RepositoryContextFile, 0, len(value.GetRepositoryContext()))
	for _, file := range value.GetRepositoryContext() {
		mapped, err := mapRepositoryContextFile(file, value, artifacts)
		if err != nil {
			return nil, err
		}
		repositoryContext = append(repositoryContext, mapped)
	}
	return repositoryContext, nil
}

func mapRepositoryContextFile(
	file *reasoningv1.RepositoryContextFile,
	request *reasoningv1.ImplementationRequest,
	artifacts []Artifact,
) (RepositoryContextFile, error) {
	if !validRepoPath(file.GetPath()) || !pathWithin(file.GetPath(), request.GetReadablePaths()) ||
		!digestPattern.MatchString(file.GetSha256()) {
		return RepositoryContextFile{}, errors.New("invalid repository context")
	}
	if pathIsProhibited(file.GetPath(), request.GetProhibitedPaths()) {
		return RepositoryContextFile{}, errors.New("repository context targets prohibited path")
	}
	sum := sha256.Sum256([]byte(file.GetContent()))
	if hex.EncodeToString(sum[:]) != file.GetSha256() {
		return RepositoryContextFile{}, errors.New("repository context digest mismatch")
	}
	if !artifactContainsDigest(artifacts, file.GetSha256()) {
		return RepositoryContextFile{}, errors.New(
			"repository context is not bound to an input artifact",
		)
	}
	return RepositoryContextFile{
		Path: file.GetPath(), SHA256: file.GetSha256(), Content: file.GetContent(),
	}, nil
}

func pathIsProhibited(filePath string, prohibitedPaths []string) bool {
	for _, prohibited := range prohibitedPaths {
		if pathWithin(filePath, []string{prohibited}) {
			return true
		}
	}
	return false
}

func artifactContainsDigest(artifacts []Artifact, digest string) bool {
	for _, artifact := range artifacts {
		if artifact.SHA256 == digest {
			return true
		}
	}
	return false
}

func MapImplementationProposal(
	value *reasoningv1.ImplementationProposal, request ImplementationRequest,
) (ImplementationProposal, error) {
	if err := validateImplementationProposal(value, request); err != nil {
		return ImplementationProposal{}, err
	}
	return convertImplementationProposal(value, request)
}

func convertImplementationProposal(
	value *reasoningv1.ImplementationProposal, request ImplementationRequest,
) (ImplementationProposal, error) {
	if value == nil || value.GetIdentity() == nil {
		return ImplementationProposal{}, errors.New("implementation proposal identity is required")
	}
	if err := validateProposalIdentity(value.GetIdentity(), request.Envelope, StageImplementation); err != nil {
		return ImplementationProposal{}, err
	}
	if value.GetApprovedTaskId() != request.ApprovedTaskID ||
		value.GetApprovedTaskDigest() != request.ApprovedTaskDigest ||
		value.GetApprovedSpecificationDigest() != request.ApprovedSpecificationDigest {
		return ImplementationProposal{}, errors.New("implementation request identity mismatch")
	}
	if value.GetSummary() == "" || len(value.GetChanges()) == 0 {
		return ImplementationProposal{}, errors.New("incomplete implementation proposal")
	}
	if err := validateRequestedChecks(value.GetRequestedDeclaredCheckIds(), request); err != nil {
		return ImplementationProposal{}, err
	}
	changes, covered, err := mapFileChanges(value.GetChanges(), request)
	if err != nil {
		return ImplementationProposal{}, err
	}
	if err := validateImplementationCoverage(request.AcceptanceCriterionIDs, covered); err != nil {
		return ImplementationProposal{}, err
	}
	scopeChange, err := mapScopeChangeRequest(value.GetScopeChangeRequest())
	if err != nil {
		return ImplementationProposal{}, err
	}
	return ImplementationProposal{
		Summary: value.GetSummary(), Changes: changes,
		RequestedDeclaredCheckIDs: value.GetRequestedDeclaredCheckIds(),
		Assumptions:               value.GetAssumptions(), UnresolvedQuestions: value.GetUnresolvedQuestions(),
		ScopeChangeRequest: scopeChange,
	}, nil
}

func validateImplementationProposal(
	value *reasoningv1.ImplementationProposal, request ImplementationRequest,
) error {
	if err := validateImplementationProposalSchema(value); err != nil {
		return err
	}
	if err := validateImplementationProposalIdentity(value, request); err != nil {
		return err
	}
	if err := validateImplementationProposalBindings(value, request); err != nil {
		return err
	}
	if err := validateImplementationProposalScope(value, request); err != nil {
		return err
	}
	if err := validateImplementationProposalCoverage(value, request); err != nil {
		return err
	}
	return validateImplementationProposalPolicy(value)
}

func validateImplementationProposalSchema(value *reasoningv1.ImplementationProposal) error {
	if value == nil || value.GetIdentity() == nil || value.GetSummary() == "" ||
		len(value.GetChanges()) == 0 {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			"proposal", "identity, summary, and at least one change are required",
		)
	}
	if _, err := mapScopeChangeRequest(value.GetScopeChangeRequest()); err != nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			"proposal.scope_change_request", err.Error(),
		)
	}
	for _, change := range value.GetChanges() {
		if change == nil || change.GetPath() == "" || change.GetRationale() == "" ||
			len(change.GetAcceptanceCriterionIds()) == 0 {
			return validationFailure(
				reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
				"proposal.changes", "each change requires path, rationale, and coverage",
			)
		}
	}
	return nil
}

func validateImplementationProposalIdentity(
	value *reasoningv1.ImplementationProposal, request ImplementationRequest,
) error {
	identity := value.GetIdentity()
	if identity.GetSchemaVersion() != request.Envelope.SchemaVersion ||
		identity.GetRequestId() != request.Envelope.RequestID ||
		identity.GetRunId() != request.Envelope.RunID ||
		identity.GetAttempt() != request.Envelope.Attempt ||
		(identity.TaskId == nil) != (request.Envelope.TaskID == nil) ||
		(identity.TaskId != nil && *identity.TaskId != *request.Envelope.TaskID) ||
		value.GetApprovedTaskId() != request.ApprovedTaskID ||
		value.GetApprovedTaskDigest() != request.ApprovedTaskDigest ||
		value.GetApprovedSpecificationDigest() != request.ApprovedSpecificationDigest {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
			"proposal.identity", "proposal request, task, or attempt does not match",
		)
	}
	return nil
}

func validateImplementationProposalBindings(
	value *reasoningv1.ImplementationProposal, request ImplementationRequest,
) error {
	identity := value.GetIdentity()
	if identity.GetAgentManifestDigest() != request.Envelope.AgentManifestDigest ||
		len(identity.GetInputArtifactDigests()) != len(request.Envelope.InputArtifacts) {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
			"proposal.identity", "proposal artifact or manifest binding does not match",
		)
	}
	for index, artifact := range request.Envelope.InputArtifacts {
		if identity.GetInputArtifactDigests()[index] != artifact.SHA256 {
			return validationFailure(
				reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
				"proposal.identity.input_artifact_digests",
				"proposal artifact binding does not match",
			)
		}
	}
	return nil
}

func validateImplementationProposalScope(
	value *reasoningv1.ImplementationProposal, request ImplementationRequest,
) error {
	if err := validateRequestedChecks(value.GetRequestedDeclaredCheckIds(), request); err != nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCOPE_VIOLATION,
			"proposal.requested_declared_check_ids", err.Error(),
		)
	}
	for _, change := range value.GetChanges() {
		if !validRepoPath(change.GetPath()) ||
			!pathWithin(change.GetPath(), request.WritablePaths) ||
			pathIsProhibited(change.GetPath(), request.ProhibitedPaths) {
			return validationFailure(
				reasoningv1.RejectionCode_REJECTION_CODE_SCOPE_VIOLATION,
				"proposal.changes.path", "file change is outside writable scope",
			)
		}
		for _, criterion := range change.GetAcceptanceCriterionIds() {
			if !slices.Contains(request.AcceptanceCriterionIDs, criterion) {
				return validationFailure(
					reasoningv1.RejectionCode_REJECTION_CODE_SCOPE_VIOLATION,
					"proposal.changes.acceptance_criterion_ids",
					"change references an unassigned acceptance criterion",
				)
			}
		}
	}
	return nil
}

func validateImplementationProposalCoverage(
	value *reasoningv1.ImplementationProposal, request ImplementationRequest,
) error {
	covered := make(map[string]bool, len(request.AcceptanceCriterionIDs))
	for _, change := range value.GetChanges() {
		for _, criterion := range change.GetAcceptanceCriterionIds() {
			covered[criterion] = true
		}
	}
	if err := validateImplementationCoverage(request.AcceptanceCriterionIDs, covered); err != nil {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_REQUIRED_COVERAGE_MISSING,
			"proposal.changes.acceptance_criterion_ids", err.Error(),
		)
	}
	return nil
}

func validateImplementationProposalPolicy(value *reasoningv1.ImplementationProposal) error {
	if value.GetIdentity().GetStage() !=
		reasoningv1.ReasoningStage_REASONING_STAGE_IMPLEMENTATION {
		return validationFailure(
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			"proposal.identity.stage", "implementation stage is required",
		)
	}
	for _, change := range value.GetChanges() {
		if _, err := validateFileOperation(change); err != nil {
			return validationFailure(
				reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
				"proposal.changes.operation", err.Error(),
			)
		}
	}
	return nil
}

func validateRequestedChecks(values []string, request ImplementationRequest) error {
	for _, check := range values {
		if !slices.Contains(request.AvailableCheckIDs, check) {
			return errors.New("undeclared check requested")
		}
	}
	return nil
}

func mapFileChanges(
	values []*reasoningv1.FileChange, request ImplementationRequest,
) ([]FileChange, map[string]bool, error) {
	covered := make(map[string]bool, len(request.AcceptanceCriterionIDs))
	changes := make([]FileChange, 0, len(values))
	for _, change := range values {
		mapped, err := mapFileChange(change, request, covered)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, mapped)
	}
	return changes, covered, nil
}

func mapFileChange(
	change *reasoningv1.FileChange,
	request ImplementationRequest,
	covered map[string]bool,
) (FileChange, error) {
	if !validRepoPath(change.GetPath()) || !pathWithin(change.GetPath(), request.WritablePaths) ||
		change.GetRationale() == "" || len(change.GetAcceptanceCriterionIds()) == 0 {
		return FileChange{}, errors.New("invalid or out-of-scope file change")
	}
	if pathIsProhibited(change.GetPath(), request.ProhibitedPaths) {
		return FileChange{}, errors.New("file change targets prohibited path")
	}
	operation, err := validateFileOperation(change)
	if err != nil {
		return FileChange{}, err
	}
	for _, criterion := range change.GetAcceptanceCriterionIds() {
		if !slices.Contains(request.AcceptanceCriterionIDs, criterion) {
			return FileChange{}, errors.New("unknown acceptance criterion coverage")
		}
		covered[criterion] = true
	}
	return FileChange{
		Path: change.GetPath(), Operation: operation,
		ExpectedOriginalSHA256: change.GetExpectedOriginalSha256(),
		ReplacementContent:     change.ReplacementContent, Rationale: change.GetRationale(),
		AcceptanceCriterionIDs: change.GetAcceptanceCriterionIds(),
	}, nil
}

func validateImplementationCoverage(criteria []string, covered map[string]bool) error {
	for _, criterion := range criteria {
		if !covered[criterion] {
			return errors.New("required acceptance coverage missing")
		}
	}
	return nil
}

func mapScopeChangeRequest(value *reasoningv1.ScopeChangeRequest) (*ScopeChangeRequest, error) {
	if value == nil {
		return nil, nil
	}
	if value.GetSummary() == "" {
		return nil, errors.New("scope change summary is required")
	}
	for _, paths := range [][]string{
		value.GetRequestedReadablePaths(), value.GetRequestedWritablePaths(),
	} {
		for _, requestedPath := range paths {
			if !validRepoPath(requestedPath) {
				return nil, errors.New("invalid scope change path")
			}
		}
	}
	if len(value.GetRequestedAcceptanceCriterionIds()) > 0 &&
		!validCriteria(value.GetRequestedAcceptanceCriterionIds()) {
		return nil, errors.New("invalid scope change acceptance criterion")
	}
	for _, check := range value.GetRequestedCheckIds() {
		if !checkIDPattern.MatchString(check) {
			return nil, errors.New("invalid scope change check")
		}
	}
	return &ScopeChangeRequest{
		Summary:                         value.GetSummary(),
		RequestedReadablePaths:          value.GetRequestedReadablePaths(),
		RequestedWritablePaths:          value.GetRequestedWritablePaths(),
		RequestedAcceptanceCriterionIDs: value.GetRequestedAcceptanceCriterionIds(),
		RequestedCheckIDs:               value.GetRequestedCheckIds(),
	}, nil
}

func validateFileOperation(change *reasoningv1.FileChange) (FileOperation, error) {
	switch change.GetOperation() {
	case reasoningv1.FileOperation_FILE_OPERATION_CREATE:
		if change.GetExpectedOriginalSha256() != emptySHA256 || change.ReplacementContent == nil {
			return "", errors.New("create requires empty-content digest and replacement content")
		}
		return FileCreate, nil
	case reasoningv1.FileOperation_FILE_OPERATION_UPDATE:
		if !digestPattern.MatchString(change.GetExpectedOriginalSha256()) ||
			change.ReplacementContent == nil {
			return "", errors.New("update requires digest and replacement content")
		}
		return FileUpdate, nil
	case reasoningv1.FileOperation_FILE_OPERATION_DELETE:
		if !digestPattern.MatchString(change.GetExpectedOriginalSha256()) ||
			change.ReplacementContent != nil {
			return "", errors.New("delete requires digest and no replacement content")
		}
		return FileDelete, nil
	default:
		return "", errors.New("unsupported file operation")
	}
}
