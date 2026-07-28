from pathlib import Path

from harness_agents._generated.harness.reasoning.v1 import review_pb2

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "tests/contracts/v1/review"


def test_review_fixtures_round_trip_and_recommendation_is_advisory() -> None:
    request_data = (FIXTURES / "request.bin").read_bytes()
    request = review_pb2.ReviewRequest.FromString(request_data)
    assert request.candidate.candidate_commit == "f" * 40
    assert request.SerializeToString(deterministic=True) == request_data

    proposal_data = (FIXTURES / "proposal.bin").read_bytes()
    proposal = review_pb2.ReviewProposal.FromString(proposal_data)
    assert proposal.recommendation == review_pb2.REVIEW_RECOMMENDATION_ADVISORY_ACCEPT
    assert "approval" not in {field.name for field in proposal.DESCRIPTOR.fields}
    assert proposal.SerializeToString(deterministic=True) == proposal_data


def test_python_preserves_unknown_fields() -> None:
    original = (FIXTURES / "proposal.bin").read_bytes()
    unknown_field = b"\xf8\x07\x01"
    value = review_pb2.ReviewProposal.FromString(original + unknown_field)
    assert value.SerializeToString(deterministic=True).endswith(unknown_field)
