from pathlib import Path

from harness_agents._generated.harness.reasoning.v1 import common_pb2, implementation_pb2

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "tests/contracts/v1/implementation"


def test_implementation_fixtures_and_operations_round_trip() -> None:
    request_data = (FIXTURES / "request.bin").read_bytes()
    request = implementation_pb2.ImplementationRequest.FromString(request_data)
    assert request.approved_task_id == "TASK-001"
    assert request.SerializeToString(deterministic=True) == request_data

    proposal_data = (FIXTURES / "proposal.bin").read_bytes()
    proposal = implementation_pb2.ImplementationProposal.FromString(proposal_data)
    assert [change.operation for change in proposal.changes] == [
        implementation_pb2.FILE_OPERATION_CREATE,
        implementation_pb2.FILE_OPERATION_UPDATE,
        implementation_pb2.FILE_OPERATION_DELETE,
    ]
    assert proposal.SerializeToString(deterministic=True) == proposal_data


def test_all_rejection_codes_round_trip() -> None:
    codes = [
        common_pb2.REJECTION_CODE_SCHEMA_INVALID,
        common_pb2.REJECTION_CODE_REQUEST_MISMATCH,
        common_pb2.REJECTION_CODE_AUTHORITY_VIOLATION,
        common_pb2.REJECTION_CODE_SCOPE_VIOLATION,
        common_pb2.REJECTION_CODE_REQUIRED_COVERAGE_MISSING,
    ]
    for code in codes:
        rejection = common_pb2.ProposalRejection(
            code=code,
            summary="deterministic rejection",
            details=[common_pb2.RejectionDetail(field="proposal", message="invalid")],
            retryable=False,
            request_id="request-impl-001",
            run_id="run-001",
            task_id="TASK-001",
            attempt=1,
        )
        assert common_pb2.ProposalRejection.FromString(rejection.SerializeToString()).code == code
