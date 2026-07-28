package workflow

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	testTime = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	testSpec = ArtifactRef{
		URI:    "artifact://specifications/v1",
		Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
)

func envelope(kind ActorKind, revision uint64) CommandEnvelope {
	return CommandEnvelope{
		CommandID: uuid.NewString(), Actor: Actor{ID: uuid.NewString(), Kind: kind},
		ExpectedRevision: revision, Timestamp: testTime,
		CorrelationID: uuid.NewString(), CausationID: uuid.NewString(),
	}
}

func createTestRun(t *testing.T) Run {
	t.Helper()
	command := CreateRun{Meta: envelope(ActorHuman, 0), ID: uuid.NewString()}
	run, events, err := NewRun(command)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(events) != 1 || events[0].Type != "RUN_CREATED" {
		t.Fatalf("unexpected events: %#v", events)
	}
	return run
}

func TestSpecificationLifecycle(t *testing.T) {
	run := createTestRun(t)
	proposal := ProposeSpecification{
		Meta: envelope(ActorWorkflowService, run.Revision),
		ID:   run.ID, Specification: testSpec,
	}
	review, events, err := run.Apply(proposal)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if review.State != RunStateSpecificationReview || review.Specification == nil ||
		!review.Specification.Equal(testSpec) || len(events) != 1 {
		t.Fatalf("unexpected proposal result: %#v %#v", review, events)
	}

	approval := ApproveSpecification{
		Meta: envelope(ActorHuman, review.Revision),
		ID:   review.ID, Specification: testSpec,
	}
	planning, _, err := review.Apply(approval)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if planning.State != RunStateTaskPlanning {
		t.Fatalf("state = %s", planning.State)
	}
}

func TestRunCommandDigestIncludesConcreteType(t *testing.T) {
	meta := envelope(ActorWorkflowService, 1)
	runID := uuid.NewString()
	proposalDigest, err := commandDigest(ProposeSpecification{
		Meta: meta, ID: runID, Specification: testSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	approvalDigest, err := commandDigest(ApproveSpecification{
		Meta: meta, ID: runID, Specification: testSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposalDigest == approvalDigest {
		t.Fatal("same-shaped run commands produced the same digest")
	}
}

func TestSpecificationRejectionReturnsToDraft(t *testing.T) {
	run := createTestRun(t)
	review, _, err := run.Apply(ProposeSpecification{
		Meta: envelope(ActorWorkflowService, run.Revision),
		ID:   run.ID, Specification: testSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	next, events, err := review.Apply(RejectSpecification{
		Meta: envelope(ActorHuman, review.Revision), ID: review.ID,
		Specification: testSpec, Reason: "missing acceptance criteria",
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.State != RunStateDraft || events[0].Type != "SPECIFICATION_REJECTED" {
		t.Fatalf("unexpected rejection: %#v %#v", next, events)
	}
}

func TestInvalidSpecificationCommandsDoNotMutateOrEmit(t *testing.T) {
	run := createTestRun(t)
	review, _, err := run.Apply(ProposeSpecification{
		Meta: envelope(ActorWorkflowService, run.Revision),
		ID:   run.ID, Specification: testSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	mismatch := testSpec
	mismatch.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	tests := []struct {
		name    string
		command RunCommand
		target  error
	}{
		{"wrong state", ProposeSpecification{
			Meta: envelope(ActorWorkflowService, review.Revision),
			ID:   review.ID, Specification: testSpec,
		}, ErrInvalidTransition},
		{"unauthorized model", ApproveSpecification{
			Meta: envelope(ActorModel, review.Revision),
			ID:   review.ID, Specification: testSpec,
		}, ErrUnauthorized},
		{"mismatched artifact", ApproveSpecification{
			Meta: envelope(ActorHuman, review.Revision),
			ID:   review.ID, Specification: mismatch,
		}, ErrInvalid},
		{"stale revision", ApproveSpecification{
			Meta: envelope(ActorHuman, review.Revision-1),
			ID:   review.ID, Specification: testSpec,
		}, ErrRevisionConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := review
			next, events, err := review.Apply(test.command)
			if !errors.Is(err, test.target) {
				t.Fatalf("error = %v, want %v", err, test.target)
			}
			if !reflect.DeepEqual(review, before) {
				t.Fatal("receiver mutated")
			}
			if !reflect.DeepEqual(next, Run{}) || events != nil {
				t.Fatalf("partial output: %#v %#v", next, events)
			}
		})
	}
}

func TestRunStateRejectsUnknownValue(t *testing.T) {
	if !errors.Is(RunState("SURPRISE").Validate(), ErrInvalid) {
		t.Fatal("unknown state accepted")
	}
}
