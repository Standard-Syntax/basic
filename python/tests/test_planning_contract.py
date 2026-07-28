from pathlib import Path

from harness_agents._generated.harness.reasoning.v1 import planning_pb2

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "tests/contracts/v1/planning"


def test_planning_fixtures_round_trip() -> None:
    request_data = (FIXTURES / "request.bin").read_bytes()
    request = planning_pb2.TaskPlanningRequest.FromString(request_data)
    assert request.task_count_limit == 4
    assert request.SerializeToString(deterministic=True) == request_data

    proposal_data = (FIXTURES / "proposal.bin").read_bytes()
    proposal = planning_pb2.TaskGraphProposal.FromString(proposal_data)
    assert [task.task_id for task in proposal.tasks] == ["TASK-001", "TASK-002"]
    assert proposal.SerializeToString(deterministic=True) == proposal_data
