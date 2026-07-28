from pathlib import Path

from harness_agents._generated.harness.reasoning.v1 import (
    implementation_pb2,
    planning_pb2,
    review_pb2,
    specification_pb2,
)

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "tests/contracts/v1"


def test_go_generated_fixtures_deserialize_identically_in_python() -> None:
    types = {
        "specification": (
            specification_pb2.SpecificationRequest,
            specification_pb2.SpecificationProposal,
        ),
        "planning": (planning_pb2.TaskPlanningRequest, planning_pb2.TaskGraphProposal),
        "implementation": (
            implementation_pb2.ImplementationRequest,
            implementation_pb2.ImplementationProposal,
        ),
        "review": (review_pb2.ReviewRequest, review_pb2.ReviewProposal),
    }
    for stage, (request_type, proposal_type) in types.items():
        for name, message_type in (("request", request_type), ("proposal", proposal_type)):
            python_value = message_type.FromString((FIXTURES / stage / f"{name}.bin").read_bytes())
            go_value = message_type.FromString((FIXTURES / stage / f"{name}-go.bin").read_bytes())
            assert go_value == python_value
