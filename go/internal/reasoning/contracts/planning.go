package contracts

import (
	"context"
	"errors"
	"path"
	"regexp"
	"slices"
	"strings"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
)

var taskIDPattern = regexp.MustCompile(`^TASK-[0-9]{3}$`)
var checkIDPattern = regexp.MustCompile(`^CHECK-[A-Z0-9][A-Z0-9-]*$`)
var resourcePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type TaskPlanningRequest struct {
	Envelope                    Envelope
	ApprovedSpecificationID     string
	ApprovedSpecificationDigest string
	ReadablePaths               []string
	WritablePaths               []string
	ProhibitedPaths             []string
	TaskCountLimit              uint32
	ParallelismLimit            uint32
	AcceptanceCriterionIDs      []string
}

type PlannedTask struct {
	ID                     string
	Objective              string
	Dependencies           []string
	AcceptanceCriterionIDs []string
	ReadablePaths          []string
	WritablePaths          []string
	ProhibitedPaths        []string
	ExclusiveResources     []string
	RequiredCheckIDs       []string
	StopConditions         []string
}

type TaskGraphProposal struct {
	Tasks                    []PlannedTask
	Assumptions              []string
	UnresolvedScopeQuestions []string
}

type TaskPlanningReasoner interface {
	ProposeTaskGraph(context.Context, TaskPlanningRequest) (TaskGraphProposal, error)
}

func MapTaskPlanningRequest(value *reasoningv1.TaskPlanningRequest) (TaskPlanningRequest, error) {
	if value == nil {
		return TaskPlanningRequest{}, errors.New("task planning request is required")
	}
	envelope, err := MapEnvelope(value.GetEnvelope(), StagePlanning)
	if err != nil {
		return TaskPlanningRequest{}, err
	}
	if value.GetApprovedSpecificationId() == "" ||
		!digestPattern.MatchString(value.GetApprovedSpecificationDigest()) ||
		value.GetTaskCountLimit() == 0 || value.GetParallelismLimit() == 0 ||
		len(value.GetAcceptanceCriterionIds()) == 0 {
		return TaskPlanningRequest{}, errors.New("incomplete task planning request")
	}
	if err := validatePathScopes(
		value.GetReadablePaths(), value.GetWritablePaths(), value.GetProhibitedPaths(),
	); err != nil {
		return TaskPlanningRequest{}, err
	}
	if !validCriteria(value.GetAcceptanceCriterionIds()) {
		return TaskPlanningRequest{}, errors.New("invalid acceptance criteria")
	}
	return TaskPlanningRequest{
		Envelope:                    envelope,
		ApprovedSpecificationID:     value.GetApprovedSpecificationId(),
		ApprovedSpecificationDigest: value.GetApprovedSpecificationDigest(),
		ReadablePaths:               value.GetReadablePaths(),
		WritablePaths:               value.GetWritablePaths(),
		ProhibitedPaths:             value.GetProhibitedPaths(),
		TaskCountLimit:              value.GetTaskCountLimit(),
		ParallelismLimit:            value.GetParallelismLimit(),
		AcceptanceCriterionIDs:      value.GetAcceptanceCriterionIds(),
	}, nil
}

func MapTaskGraphProposal(
	value *reasoningv1.TaskGraphProposal, request TaskPlanningRequest,
) (TaskGraphProposal, error) {
	if value == nil || value.GetIdentity() == nil {
		return TaskGraphProposal{}, errors.New("task graph proposal identity is required")
	}
	if err := validateProposalIdentity(value.GetIdentity(), request.Envelope, StagePlanning); err != nil {
		return TaskGraphProposal{}, err
	}
	if value.GetApprovedSpecificationId() != request.ApprovedSpecificationID ||
		value.GetApprovedSpecificationDigest() != request.ApprovedSpecificationDigest {
		return TaskGraphProposal{}, errors.New("approved specification mismatch")
	}
	if len(value.GetTasks()) == 0 || uint32(len(value.GetTasks())) > request.TaskCountLimit {
		return TaskGraphProposal{}, errors.New("task count is outside approved limit")
	}
	known := make(map[string]struct{}, len(value.GetTasks()))
	for _, task := range value.GetTasks() {
		if !taskIDPattern.MatchString(task.GetTaskId()) {
			return TaskGraphProposal{}, errors.New("invalid task ID")
		}
		if _, exists := known[task.GetTaskId()]; exists {
			return TaskGraphProposal{}, errors.New("duplicate task ID")
		}
		known[task.GetTaskId()] = struct{}{}
	}
	assigned := make(map[string]int)
	tasks := make([]PlannedTask, 0, len(value.GetTasks()))
	for _, task := range value.GetTasks() {
		if task.GetObjective() == "" || len(task.GetAcceptanceCriterionIds()) == 0 ||
			len(task.GetRequiredCheckIds()) == 0 || len(task.GetStopConditions()) == 0 {
			return TaskGraphProposal{}, errors.New("incomplete planned task")
		}
		if err := validatePathScopes(
			task.GetReadablePaths(), task.GetWritablePaths(), task.GetProhibitedPaths(),
		); err != nil {
			return TaskGraphProposal{}, err
		}
		if !pathsWithin(task.GetReadablePaths(), request.ReadablePaths) ||
			!pathsWithin(task.GetWritablePaths(), request.WritablePaths) {
			return TaskGraphProposal{}, errors.New("task scope exceeds planning request")
		}
		dependencies := make([]string, 0, len(task.GetDependencies()))
		for _, dependency := range task.GetDependencies() {
			if dependency.GetTaskId() == task.GetTaskId() {
				return TaskGraphProposal{}, errors.New("task cannot depend on itself")
			}
			if _, exists := known[dependency.GetTaskId()]; !exists {
				return TaskGraphProposal{}, errors.New("unknown task dependency")
			}
			dependencies = append(dependencies, dependency.GetTaskId())
		}
		for _, criterion := range task.GetAcceptanceCriterionIds() {
			if !slices.Contains(request.AcceptanceCriterionIDs, criterion) {
				return TaskGraphProposal{}, errors.New("unknown acceptance criterion assignment")
			}
			assigned[criterion]++
		}
		for _, resource := range task.GetExclusiveResources() {
			if !resourcePattern.MatchString(resource) {
				return TaskGraphProposal{}, errors.New("invalid exclusive resource")
			}
		}
		for _, check := range task.GetRequiredCheckIds() {
			if !checkIDPattern.MatchString(check) {
				return TaskGraphProposal{}, errors.New("invalid required check ID")
			}
		}
		tasks = append(tasks, PlannedTask{
			ID: task.GetTaskId(), Objective: task.GetObjective(),
			Dependencies:           dependencies,
			AcceptanceCriterionIDs: task.GetAcceptanceCriterionIds(),
			ReadablePaths:          task.GetReadablePaths(), WritablePaths: task.GetWritablePaths(),
			ProhibitedPaths:    task.GetProhibitedPaths(),
			ExclusiveResources: task.GetExclusiveResources(),
			RequiredCheckIDs:   task.GetRequiredCheckIds(), StopConditions: task.GetStopConditions(),
		})
	}
	for _, criterion := range request.AcceptanceCriterionIDs {
		if assigned[criterion] != 1 {
			return TaskGraphProposal{}, errors.New("each acceptance criterion must be assigned exactly once")
		}
	}
	if graphHasCycle(tasks) {
		return TaskGraphProposal{}, errors.New("task graph contains a cycle")
	}
	return TaskGraphProposal{
		Tasks: tasks, Assumptions: value.GetAssumptions(),
		UnresolvedScopeQuestions: value.GetUnresolvedScopeQuestions(),
	}, nil
}

func validateProposalIdentity(
	value *reasoningv1.ProposalIdentity, request Envelope, expected Stage,
) error {
	stage, err := mapStage(value.GetStage())
	if err != nil || stage != expected || value.GetSchemaVersion() != request.SchemaVersion ||
		value.GetRequestId() != request.RequestID || value.GetRunId() != request.RunID ||
		value.GetAttempt() != request.Attempt ||
		value.GetAgentManifestDigest() != request.AgentManifestDigest {
		return errors.New("proposal request identity mismatch")
	}
	return nil
}

func validCriteria(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !criterionID.MatchString(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validRepoPath(value string) bool {
	return value != "" && value != "." && !strings.HasPrefix(value, "/") &&
		!strings.Contains(value, "\\") && path.Clean(value) == value &&
		!strings.HasPrefix(value, "../")
}

func pathWithin(value string, roots []string) bool {
	for _, root := range roots {
		if value == root || strings.HasPrefix(value, root+"/") {
			return true
		}
	}
	return false
}

func pathsWithin(values, roots []string) bool {
	for _, value := range values {
		if !pathWithin(value, roots) {
			return false
		}
	}
	return true
}

func validatePathScopes(readable, writable, prohibited []string) error {
	if len(readable) == 0 {
		return errors.New("readable paths are required")
	}
	for _, group := range [][]string{readable, writable, prohibited} {
		for _, value := range group {
			if !validRepoPath(value) {
				return errors.New("invalid repository-relative path")
			}
		}
	}
	if !pathsWithin(writable, readable) {
		return errors.New("writable paths must be readable")
	}
	for _, writablePath := range writable {
		for _, prohibitedPath := range prohibited {
			if pathWithin(writablePath, []string{prohibitedPath}) ||
				pathWithin(prohibitedPath, []string{writablePath}) {
				return errors.New("writable and prohibited paths overlap")
			}
		}
	}
	return nil
}

func graphHasCycle(tasks []PlannedTask) bool {
	dependencies := make(map[string][]string, len(tasks))
	for _, task := range tasks {
		dependencies[task.ID] = task.Dependencies
	}
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, dependency := range dependencies[id] {
			if visit(dependency) {
				return true
			}
		}
		visiting[id] = false
		visited[id] = true
		return false
	}
	for id := range dependencies {
		if visit(id) {
			return true
		}
	}
	return false
}
