"""Generate deterministic shared Protobuf contract fixtures."""

import hashlib
from pathlib import Path

from google.protobuf.timestamp_pb2 import Timestamp
from harness_agents._generated.harness.reasoning.v1 import (
    common_pb2,
    implementation_pb2,
    planning_pb2,
    specification_pb2,
)

ROOT = Path(__file__).resolve().parents[1]
OUTPUT = ROOT / "tests" / "contracts" / "v1" / "specification"
PLANNING_OUTPUT = ROOT / "tests" / "contracts" / "v1" / "planning"
IMPLEMENTATION_OUTPUT = ROOT / "tests" / "contracts" / "v1" / "implementation"


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


def planning_request() -> planning_pb2.TaskPlanningRequest:
    value = request().envelope
    value.request_id = "request-plan-001"
    value.stage = common_pb2.REASONING_STAGE_PLANNING
    return planning_pb2.TaskPlanningRequest(
        envelope=value,
        approved_specification_id="spec-001",
        approved_specification_digest="c" * 64,
        repository_map=[
            planning_pb2.RepositoryEntry(path="go", kind="directory", sha256="d" * 64),
            planning_pb2.RepositoryEntry(path="docs", kind="directory", sha256="e" * 64),
        ],
        readable_paths=["docs", "go"],
        writable_paths=["docs/reasoning-contracts.md", "go/internal/reasoning"],
        prohibited_paths=["go/gen"],
        task_count_limit=4,
        parallelism_limit=2,
        acceptance_criterion_ids=["AC-001", "AC-002"],
    )


def planning_proposal() -> planning_pb2.TaskGraphProposal:
    return planning_pb2.TaskGraphProposal(
        identity=common_pb2.ProposalIdentity(
            schema_version="1",
            request_id="request-plan-001",
            run_id="run-001",
            stage=common_pb2.REASONING_STAGE_PLANNING,
            attempt=1,
            agent_manifest_digest="b" * 64,
            input_artifact_digests=["a" * 64],
        ),
        approved_specification_id="spec-001",
        approved_specification_digest="c" * 64,
        tasks=[
            planning_pb2.PlannedTask(
                task_id="TASK-001",
                objective="Document the planning contract.",
                acceptance_criterion_ids=["AC-001"],
                readable_paths=["docs"],
                writable_paths=["docs/reasoning-contracts.md"],
                prohibited_paths=[],
                exclusive_resources=["public-api-contract"],
                required_check_ids=["CHECK-DOCS"],
                stop_conditions=["contract authority changes"],
            ),
            planning_pb2.PlannedTask(
                task_id="TASK-002",
                objective="Validate bounded task graphs.",
                dependencies=[planning_pb2.TaskDependency(task_id="TASK-001")],
                acceptance_criterion_ids=["AC-002"],
                readable_paths=["go"],
                writable_paths=["go/internal/reasoning"],
                prohibited_paths=["go/gen"],
                exclusive_resources=["public-api-contract"],
                required_check_ids=["CHECK-GO-TEST"],
                stop_conditions=["scope cannot be represented"],
            ),
        ],
        assumptions=["repository map is kernel-selected"],
        unresolved_scope_questions=[],
    )


def implementation_request() -> implementation_pb2.ImplementationRequest:
    value = request().envelope
    value.request_id = "request-impl-001"
    value.task_id = "TASK-001"
    value.stage = common_pb2.REASONING_STAGE_IMPLEMENTATION
    context_digest = hashlib.sha256(b"package reasoning\n").hexdigest()
    value.input_artifacts.append(
        common_pb2.ArtifactDigest(
            artifact_uri=f"artifact://sha256/{context_digest}",
            sha256=context_digest,
        )
    )
    return implementation_pb2.ImplementationRequest(
        envelope=value,
        approved_task_id="TASK-001",
        approved_task_digest="c" * 64,
        approved_specification_id="spec-001",
        approved_specification_digest="d" * 64,
        base_commit="e" * 40,
        readable_paths=["go"],
        writable_paths=["go/internal/reasoning"],
        prohibited_paths=["go/gen"],
        acceptance_criterion_ids=["AC-001", "AC-002", "AC-003"],
        available_check_ids=["CHECK-GO-TEST"],
        repository_context=[
            implementation_pb2.RepositoryContextFile(
                path="go/internal/reasoning/existing.go",
                sha256=context_digest,
                content="package reasoning\n",
            )
        ],
    )


def implementation_proposal() -> implementation_pb2.ImplementationProposal:
    context_digest = hashlib.sha256(b"package reasoning\n").hexdigest()
    return implementation_pb2.ImplementationProposal(
        identity=common_pb2.ProposalIdentity(
            schema_version="1",
            request_id="request-impl-001",
            run_id="run-001",
            task_id="TASK-001",
            stage=common_pb2.REASONING_STAGE_IMPLEMENTATION,
            attempt=1,
            agent_manifest_digest="b" * 64,
            input_artifact_digests=["a" * 64, context_digest],
        ),
        approved_task_id="TASK-001",
        approved_task_digest="c" * 64,
        approved_specification_digest="d" * 64,
        summary="Propose three complete-file operations.",
        changes=[
            implementation_pb2.FileChange(
                path="go/internal/reasoning/create.go",
                operation=implementation_pb2.FILE_OPERATION_CREATE,
                expected_original_sha256=(
                    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
                ),
                replacement_content="package reasoning\n",
                rationale="Add the bounded contract.",
                acceptance_criterion_ids=["AC-001"],
            ),
            implementation_pb2.FileChange(
                path="go/internal/reasoning/existing.go",
                operation=implementation_pb2.FILE_OPERATION_UPDATE,
                expected_original_sha256=context_digest,
                replacement_content="package reasoning\n\n// Updated.\n",
                rationale="Update the bounded contract.",
                acceptance_criterion_ids=["AC-002"],
            ),
            implementation_pb2.FileChange(
                path="go/internal/reasoning/obsolete.go",
                operation=implementation_pb2.FILE_OPERATION_DELETE,
                expected_original_sha256="9" * 64,
                rationale="Remove obsolete transport logic.",
                acceptance_criterion_ids=["AC-003"],
            ),
        ],
        requested_declared_check_ids=["CHECK-GO-TEST"],
        assumptions=["kernel verifies original digests"],
        unresolved_questions=[],
    )


def main() -> None:
    OUTPUT.mkdir(parents=True, exist_ok=True)
    (OUTPUT / "request.bin").write_bytes(request().SerializeToString(deterministic=True))
    (OUTPUT / "proposal.bin").write_bytes(proposal().SerializeToString(deterministic=True))
    PLANNING_OUTPUT.mkdir(parents=True, exist_ok=True)
    (PLANNING_OUTPUT / "request.bin").write_bytes(
        planning_request().SerializeToString(deterministic=True)
    )
    (PLANNING_OUTPUT / "proposal.bin").write_bytes(
        planning_proposal().SerializeToString(deterministic=True)
    )
    IMPLEMENTATION_OUTPUT.mkdir(parents=True, exist_ok=True)
    (IMPLEMENTATION_OUTPUT / "request.bin").write_bytes(
        implementation_request().SerializeToString(deterministic=True)
    )
    (IMPLEMENTATION_OUTPUT / "proposal.bin").write_bytes(
        implementation_proposal().SerializeToString(deterministic=True)
    )


if __name__ == "__main__":
    main()
