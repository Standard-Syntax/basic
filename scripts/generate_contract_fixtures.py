"""Generate deterministic shared Protobuf contract fixtures."""

from pathlib import Path

from google.protobuf.timestamp_pb2 import Timestamp
from harness_agents._generated.harness.reasoning.v1 import common_pb2, specification_pb2

ROOT = Path(__file__).resolve().parents[1]
OUTPUT = ROOT / "tests" / "contracts" / "v1" / "specification"


def timestamp(seconds: int) -> Timestamp:
    value = Timestamp()
    value.seconds = seconds
    return value


def request() -> specification_pb2.SpecificationRequest:
    return specification_pb2.SpecificationRequest(
        envelope=common_pb2.ReasoningRequestEnvelope(
            schema_version="1",
            request_id="request-spec-001",
            run_id="run-001",
            stage=common_pb2.REASONING_STAGE_SPECIFICATION,
            attempt=1,
            created_at=timestamp(1_785_139_200),
            expires_at=timestamp(1_785_142_800),
            authority=common_pb2.AuthorityConstraints(mode=common_pb2.AUTHORITY_MODE_PROPOSAL_ONLY),
            budget=common_pb2.ReasoningBudget(
                maximum_input_tokens=10_000,
                maximum_output_tokens=5_000,
                maximum_provider_requests=1,
            ),
            input_artifacts=[
                common_pb2.ArtifactDigest(
                    artifact_uri=f"artifact://sha256/{'a' * 64}",
                    sha256="a" * 64,
                )
            ],
            agent_manifest_digest="b" * 64,
        ),
        problem_statement="Establish a shared specification contract.",
        desired_outcome="Provider-neutral specification proposals.",
        known_constraints=["proposal-only authority"],
        known_non_goals=["workflow execution"],
        stakeholders=["kernel operator"],
        repository_summary="Phase 0 foundation",
    )


def proposal() -> specification_pb2.SpecificationProposal:
    return specification_pb2.SpecificationProposal(
        identity=common_pb2.ProposalIdentity(
            schema_version="1",
            request_id="request-spec-001",
            run_id="run-001",
            stage=common_pb2.REASONING_STAGE_SPECIFICATION,
            attempt=1,
            agent_manifest_digest="b" * 64,
            input_artifact_digests=["a" * 64],
        ),
        title="Shared specification contract",
        goal="Exchange bounded specification proposals.",
        actors=["kernel operator"],
        constraints=["proposal-only authority"],
        non_goals=["workflow execution"],
        acceptance_criteria=[
            specification_pb2.AcceptanceCriterion(
                criterion_id="AC-001",
                description="Contracts round-trip across languages.",
                verification_method="Go and Python fixture tests",
            )
        ],
        assumptions=["UTF-8 transport"],
        risks=[
            specification_pb2.SpecificationRisk(
                risk_id="RISK-001",
                description="Authority may be misread.",
                mitigation="Fail closed in domain mapping.",
            )
        ],
        questions=[
            specification_pb2.SpecificationQuestion(
                question_id="Q-001",
                question="Is any runtime included?",
                blocking=False,
            )
        ],
    )


def main() -> None:
    OUTPUT.mkdir(parents=True, exist_ok=True)
    (OUTPUT / "request.bin").write_bytes(request().SerializeToString(deterministic=True))
    (OUTPUT / "proposal.bin").write_bytes(proposal().SerializeToString(deterministic=True))


if __name__ == "__main__":
    main()
