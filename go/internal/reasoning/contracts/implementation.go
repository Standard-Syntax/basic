package contracts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"

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
	if value == nil {
		return ImplementationRequest{}, errors.New("implementation request is required")
	}
	envelope, err := MapEnvelope(value.GetEnvelope(), StageImplementation)
	if err != nil {
		return ImplementationRequest{}, err
	}
	if !taskIDPattern.MatchString(value.GetApprovedTaskId()) ||
		!digestPattern.MatchString(value.GetApprovedTaskDigest()) ||
		value.GetApprovedSpecificationId() == "" ||
		!digestPattern.MatchString(value.GetApprovedSpecificationDigest()) ||
		!commitPattern.MatchString(value.GetBaseCommit()) ||
		!validCriteria(value.GetAcceptanceCriterionIds()) ||
		len(value.GetAvailableCheckIds()) == 0 {
		return ImplementationRequest{}, errors.New("invalid implementation request identity or limits")
	}
	if err := validatePathScopes(
		value.GetReadablePaths(), value.GetWritablePaths(), value.GetProhibitedPaths(),
	); err != nil {
		return ImplementationRequest{}, err
	}
	for _, check := range value.GetAvailableCheckIds() {
		if !checkIDPattern.MatchString(check) {
			return ImplementationRequest{}, errors.New("invalid available check ID")
		}
	}
	repositoryContext := make([]RepositoryContextFile, 0, len(value.GetRepositoryContext()))
	for _, file := range value.GetRepositoryContext() {
		if !validRepoPath(file.GetPath()) || !pathWithin(file.GetPath(), value.GetReadablePaths()) ||
			!digestPattern.MatchString(file.GetSha256()) {
			return ImplementationRequest{}, errors.New("invalid repository context")
		}
		for _, prohibited := range value.GetProhibitedPaths() {
			if pathWithin(file.GetPath(), []string{prohibited}) {
				return ImplementationRequest{}, errors.New("repository context targets prohibited path")
			}
		}
		sum := sha256.Sum256([]byte(file.GetContent()))
		if hex.EncodeToString(sum[:]) != file.GetSha256() {
			return ImplementationRequest{}, errors.New("repository context digest mismatch")
		}
		repositoryContext = append(repositoryContext, RepositoryContextFile{
			Path: file.GetPath(), SHA256: file.GetSha256(), Content: file.GetContent(),
		})
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

func MapImplementationProposal(
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
	for _, check := range value.GetRequestedDeclaredCheckIds() {
		if !slices.Contains(request.AvailableCheckIDs, check) {
			return ImplementationProposal{}, errors.New("undeclared check requested")
		}
	}
	covered := make(map[string]bool, len(request.AcceptanceCriterionIDs))
	changes := make([]FileChange, 0, len(value.GetChanges()))
	for _, change := range value.GetChanges() {
		if !validRepoPath(change.GetPath()) || !pathWithin(change.GetPath(), request.WritablePaths) ||
			change.GetRationale() == "" || len(change.GetAcceptanceCriterionIds()) == 0 {
			return ImplementationProposal{}, errors.New("invalid or out-of-scope file change")
		}
		for _, prohibited := range request.ProhibitedPaths {
			if pathWithin(change.GetPath(), []string{prohibited}) {
				return ImplementationProposal{}, errors.New("file change targets prohibited path")
			}
		}
		operation, err := validateFileOperation(change)
		if err != nil {
			return ImplementationProposal{}, err
		}
		for _, criterion := range change.GetAcceptanceCriterionIds() {
			if !slices.Contains(request.AcceptanceCriterionIDs, criterion) {
				return ImplementationProposal{}, errors.New("unknown acceptance criterion coverage")
			}
			covered[criterion] = true
		}
		changes = append(changes, FileChange{
			Path: change.GetPath(), Operation: operation,
			ExpectedOriginalSHA256: change.GetExpectedOriginalSha256(),
			ReplacementContent:     change.ReplacementContent, Rationale: change.GetRationale(),
			AcceptanceCriterionIDs: change.GetAcceptanceCriterionIds(),
		})
	}
	for _, criterion := range request.AcceptanceCriterionIDs {
		if !covered[criterion] {
			return ImplementationProposal{}, errors.New("required acceptance coverage missing")
		}
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
