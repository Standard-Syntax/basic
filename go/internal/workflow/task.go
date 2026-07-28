package workflow

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

type TaskDefinition struct {
	ID          string `json:"task_id"`
	MaxAttempts uint32 `json:"max_attempts"`
}

type TaskDependency struct {
	TaskID      string `json:"task_id"`
	DependsOnID string `json:"depends_on_id"`
}

type Task struct {
	ID             string    `json:"id"`
	RunID          string    `json:"run_id"`
	State          TaskState `json:"state"`
	Revision       uint64    `json:"revision"`
	MaxAttempts    uint32    `json:"max_attempts"`
	CurrentAttempt uint32    `json:"current_attempt"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (t Task) Validate() error {
	if err := validateID("task", t.ID); err != nil {
		return err
	}
	if err := validateID("run", t.RunID); err != nil {
		return err
	}
	if err := t.State.Validate(); err != nil {
		return err
	}
	if t.Revision == 0 || t.MaxAttempts == 0 || t.CurrentAttempt > t.MaxAttempts ||
		t.CreatedAt.IsZero() || t.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: task snapshot", ErrInvalid)
	}
	return nil
}

func NewPlannedTasks(
	runID string,
	definitions []TaskDefinition,
	dependencies []TaskDependency,
	meta CommandEnvelope,
) ([]Task, []TaskDependency, []Event, error) {
	if err := validateID("run", runID); err != nil {
		return nil, nil, nil, err
	}
	taskIDs, err := plannedTaskIDs(definitions)
	if err != nil {
		return nil, nil, nil, err
	}
	incoming, adjacency, err := plannedTaskGraph(taskIDs, dependencies)
	if err != nil {
		return nil, nil, nil, err
	}
	if !taskGraphAcyclic(taskIDs, incoming, adjacency) {
		return nil, nil, nil, fmt.Errorf("%w: cyclic task graph", ErrInvalid)
	}
	tasks, events := buildPlannedTasks(runID, definitions, incoming, meta)
	return tasks, append([]TaskDependency(nil), dependencies...), events, nil
}

func plannedTaskIDs(definitions []TaskDefinition) (map[string]struct{}, error) {
	if len(definitions) == 0 {
		return nil, fmt.Errorf("%w: empty task graph", ErrInvalid)
	}
	taskIDs := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		if err := validateID("task", definition.ID); err != nil {
			return nil, err
		}
		if definition.MaxAttempts == 0 {
			return nil, fmt.Errorf("%w: max attempts", ErrInvalid)
		}
		if _, exists := taskIDs[definition.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate task", ErrInvalid)
		}
		taskIDs[definition.ID] = struct{}{}
	}
	return taskIDs, nil
}

func plannedTaskGraph(
	taskIDs map[string]struct{}, dependencies []TaskDependency,
) (map[string]int, map[string][]string, error) {
	incoming := make(map[string]int, len(taskIDs))
	adjacency := make(map[string][]string, len(taskIDs))
	edges := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		if _, exists := taskIDs[dependency.TaskID]; !exists {
			return nil, nil, fmt.Errorf("%w: dependency task", ErrInvalid)
		}
		if _, exists := taskIDs[dependency.DependsOnID]; !exists {
			return nil, nil, fmt.Errorf("%w: dependency target", ErrInvalid)
		}
		if dependency.TaskID == dependency.DependsOnID {
			return nil, nil, fmt.Errorf("%w: self dependency", ErrInvalid)
		}
		key := dependency.TaskID + ":" + dependency.DependsOnID
		if _, exists := edges[key]; exists {
			return nil, nil, fmt.Errorf("%w: duplicate dependency", ErrInvalid)
		}
		edges[key] = struct{}{}
		incoming[dependency.TaskID]++
		adjacency[dependency.DependsOnID] = append(adjacency[dependency.DependsOnID], dependency.TaskID)
	}
	return incoming, adjacency, nil
}

func taskGraphAcyclic(
	taskIDs map[string]struct{}, incoming map[string]int, adjacency map[string][]string,
) bool {
	queue := make([]string, 0, len(taskIDs))
	degrees := make(map[string]int, len(incoming))
	for id := range taskIDs {
		degrees[id] = incoming[id]
		if incoming[id] == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, dependent := range adjacency[id] {
			degrees[dependent]--
			if degrees[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}
	return visited == len(taskIDs)
}

func buildPlannedTasks(
	runID string, definitions []TaskDefinition, incoming map[string]int, meta CommandEnvelope,
) ([]Task, []Event) {
	tasks := make([]Task, 0, len(definitions))
	events := make([]Event, 0, len(definitions))
	for _, definition := range definitions {
		state := TaskStatePending
		eventType := "TASK_CREATED"
		if incoming[definition.ID] == 0 {
			state = TaskStateReady
			eventType = "TASK_READY"
		}
		task := Task{
			ID: definition.ID, RunID: runID, State: state, Revision: 1,
			MaxAttempts: definition.MaxAttempts, CreatedAt: meta.Timestamp.UTC(),
			UpdatedAt: meta.Timestamp.UTC(),
		}
		tasks = append(tasks, task)
		eventID := uuid.NewSHA1(
			uuid.MustParse(meta.CommandID),
			[]byte(definition.ID+":"+eventType),
		).String()
		events = append(events, Event{
			ID: eventID, AggregateType: "TASK", AggregateID: task.ID,
			Revision: 1, Type: eventType, Timestamp: meta.Timestamp.UTC(),
			Actor: meta.Actor, CorrelationID: meta.CorrelationID,
			CausationID: meta.CausationID,
			Payload:     map[string]any{"run_id": runID, "max_attempts": definition.MaxAttempts},
		})
	}
	return tasks, events
}
