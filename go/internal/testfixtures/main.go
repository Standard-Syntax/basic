// Command testfixtures emits deterministic Go-generated contract fixtures.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"google.golang.org/protobuf/proto"
)

type fixture struct {
	stage   string
	name    string
	message proto.Message
}

func main() {
	fixtures := []fixture{
		{"specification", "request", &reasoningv1.SpecificationRequest{}},
		{"specification", "proposal", &reasoningv1.SpecificationProposal{}},
		{"planning", "request", &reasoningv1.TaskPlanningRequest{}},
		{"planning", "proposal", &reasoningv1.TaskGraphProposal{}},
		{"implementation", "request", &reasoningv1.ImplementationRequest{}},
		{"implementation", "proposal", &reasoningv1.ImplementationProposal{}},
		{"review", "request", &reasoningv1.ReviewRequest{}},
		{"review", "proposal", &reasoningv1.ReviewProposal{}},
	}
	for _, value := range fixtures {
		if err := generate(value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func generate(value fixture) error {
	directory := filepath.Join("..", "tests", "contracts", "v1", value.stage)
	input, err := os.ReadFile(filepath.Join(directory, value.name+".bin"))
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(input, value.message); err != nil {
		return err
	}
	output, err := proto.MarshalOptions{Deterministic: true}.Marshal(value.message)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, value.name+"-go.bin"), output, 0o644)
}
