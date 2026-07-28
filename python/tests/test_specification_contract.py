from pathlib import Path

from harness_agents._generated.harness.reasoning.v1 import common_pb2, specification_pb2

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "tests/contracts/v1/specification"


def test_python_request_fixture_round_trip() -> None:
    value = specification_pb2.SpecificationRequest.FromString(
        (FIXTURES / "request.bin").read_bytes()
    )
    assert value.envelope.request_id == "request-spec-001"
    assert value.envelope.authority.mode == common_pb2.AUTHORITY_MODE_PROPOSAL_ONLY
    assert value.SerializeToString(deterministic=True) == (FIXTURES / "request.bin").read_bytes()


def test_python_proposal_fixture_round_trip() -> None:
    value = specification_pb2.SpecificationProposal.FromString(
        (FIXTURES / "proposal.bin").read_bytes()
    )
    assert value.identity.request_id == "request-spec-001"
    assert value.acceptance_criteria[0].criterion_id == "AC-001"
    assert value.SerializeToString(deterministic=True) == (FIXTURES / "proposal.bin").read_bytes()
