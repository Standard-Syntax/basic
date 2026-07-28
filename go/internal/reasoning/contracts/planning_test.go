package contracts

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"google.golang.org/protobuf/proto"
)

func planningFixture(t *testing.T, name string) []byte {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	path := filepath.Join(
		filepath.Dir(source), "..", "..", "..", "..", "tests", "contracts", "v1", "planning", name,
	)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func validPlanning(t *testing.T) (TaskPlanningRequest, *reasoningv1.TaskGraphProposal) {
	t.Helper()
	var requestPB reasoningv1.TaskPlanningRequest
	if err := proto.Unmarshal(planningFixture(t, "request.bin"), &requestPB); err != nil {
		t.Fatal(err)
	}
	request, err := MapTaskPlanningRequest(&requestPB)
	if err != nil {
		t.Fatal(err)
	}
	var proposal reasoningv1.TaskGraphProposal
	if err := proto.Unmarshal(planningFixture(t, "proposal.bin"), &proposal); err != nil {
		t.Fatal(err)
	}
	return request, &proposal
}

func TestPlanningFixturesMapAndRoundTripInGo(t *testing.T) {
	request, proposal := validPlanning(t)
	mapped, err := MapTaskGraphProposal(proposal, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Tasks) != 2 {
		t.Fatalf("tasks = %d", len(mapped.Tasks))
	}
	if len(request.RepositoryMap) != 2 {
		t.Fatal("repository map was discarded")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, planningFixture(t, "proposal.bin")) {
		t.Fatal("Go deterministic serialization differs from Python fixture")
	}
}

func TestOneTaskGraphIsValid(t *testing.T) {
	request, proposal := validPlanning(t)
	request.AcceptanceCriterionIDs = []string{"AC-001"}
	proposal.Tasks = proposal.Tasks[:1]
	if _, err := MapTaskGraphProposal(proposal, request); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidTaskGraphsFailWithoutPartialResult(t *testing.T) {
	tests := map[string]func(*reasoningv1.TaskGraphProposal){
		"cycle": func(value *reasoningv1.TaskGraphProposal) {
			value.Tasks[0].Dependencies = []*reasoningv1.TaskDependency{{TaskId: "TASK-002"}}
		},
		"unknown dependency": func(value *reasoningv1.TaskGraphProposal) {
			value.Tasks[1].Dependencies[0].TaskId = "TASK-999"
		},
		"write not readable": func(value *reasoningv1.TaskGraphProposal) {
			value.Tasks[0].WritablePaths = []string{"go/internal/reasoning"}
		},
		"missing criterion assignment": func(value *reasoningv1.TaskGraphProposal) {
			value.Tasks[1].AcceptanceCriterionIds = nil
		},
		"duplicate task": func(value *reasoningv1.TaskGraphProposal) {
			value.Tasks[1].TaskId = "TASK-001"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request, proposal := validPlanning(t)
			mutate(proposal)
			mapped, err := MapTaskGraphProposal(proposal, request)
			if err == nil {
				t.Fatal("invalid graph accepted")
			}
			if len(mapped.Tasks) != 0 {
				t.Fatal("partial result returned")
			}
		})
	}
}

func TestPlanningRequestRejectsInvalidScopeAndLimits(t *testing.T) {
	var request reasoningv1.TaskPlanningRequest
	if err := proto.Unmarshal(planningFixture(t, "request.bin"), &request); err != nil {
		t.Fatal(err)
	}
	request.TaskCountLimit = 0
	if _, err := MapTaskPlanningRequest(&request); err == nil {
		t.Fatal("zero task limit accepted")
	}
	request.TaskCountLimit = 2
	request.WritablePaths = []string{"../escape"}
	if _, err := MapTaskPlanningRequest(&request); err == nil {
		t.Fatal("path traversal accepted")
	}
}
