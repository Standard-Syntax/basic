import json
import math
from pathlib import Path

import pytest
from harness_agents import (
    AgentDefinition,
    ConfigurationMetadata,
    ContextPolicy,
    ManifestError,
    ModelPolicy,
    OutputContract,
    PromptTemplate,
    ToolRequestPolicy,
)

ROOT = Path(__file__).resolve().parents[2]
SCHEMA = json.loads((ROOT / "schemas/agent-manifest-v1.schema.json").read_text())


def definition(**changes: object) -> AgentDefinition:
    values: dict[str, object] = {
        "name": "bounded-implementation",
        "version": "1.0.0",
        "stage": "implementation",
        "prompt": PromptTemplate.from_text("Propose a bounded implementation.\n"),
        "model": ModelPolicy("strong_coding", 0.1, 20_000),
        "context": ContextPolicy(True, True, "kernel_selected", 100_000),
        "tools": ToolRequestPolicy(
            frozenset(
                {
                    "read_repository_file",
                    "search_repository",
                    "request_declared_check",
                    "report_blocker",
                }
            )
        ),
        "output": OutputContract("implementation_proposal.v1"),
        "metadata": ConfigurationMetadata("Golden implementation agent", frozenset({"golden"})),
    }
    values.update(changes)
    return AgentDefinition(**values)  # type: ignore[arg-type]


def test_compilation_is_byte_identical_for_equivalent_input_order() -> None:
    first = definition().compile()
    reordered = definition(
        tools=ToolRequestPolicy(
            frozenset(
                [
                    "report_blocker",
                    "request_declared_check",
                    "search_repository",
                    "read_repository_file",
                ]
            )
        )
    ).compile(SCHEMA)
    assert first.canonical_bytes == reordered.canonical_bytes
    assert first.digest == reordered.digest
    assert first.value["prompt"]["artifact_uri"].endswith(first.value["prompt"]["sha256"])


def test_schema_override_cannot_bypass_packaged_validation() -> None:
    permissive_schema = {"type": "object"}
    with pytest.raises(ManifestError, match="does not match"):
        definition(name="Invalid Name").compile(permissive_schema)


@pytest.mark.parametrize(
    ("stage", "output_schema"),
    [
        ("specification", "task_graph_proposal.v1"),
        ("planning", "implementation_proposal.v1"),
        ("implementation", "review_proposal.v1"),
        ("review", "specification_proposal.v1"),
    ],
)
def test_stage_requires_corresponding_output_schema(stage: str, output_schema: str) -> None:
    with pytest.raises(ManifestError, match="requires output schema"):
        definition(stage=stage, output=OutputContract(output_schema)).compile()


@pytest.mark.parametrize(
    ("field", "value", "message"),
    [
        ("stage", "deployment", "unsupported stage"),
        (
            "tools",
            ToolRequestPolicy(frozenset(), arbitrary_shell=True),
            "shell, network, and direct file write",
        ),
        (
            "tools",
            ToolRequestPolicy(frozenset(), arbitrary_network=True),
            "shell, network, and direct file write",
        ),
        (
            "tools",
            ToolRequestPolicy(frozenset(), direct_file_write=True),
            "shell, network, and direct file write",
        ),
        (
            "model",
            ModelPolicy("strong_coding", math.inf, 100),
            "temperature must be finite",
        ),
        (
            "context",
            ContextPolicy(True, True, "agent_selected", 100),
            "repository_selection must be kernel_selected",
        ),
    ],
)
def test_unsafe_or_unsupported_definitions_fail(field: str, value: object, message: str) -> None:
    with pytest.raises(ManifestError, match=message):
        definition(**{field: value}).compile(SCHEMA)


def test_closed_schema_rejects_authority_and_secret_fields() -> None:
    compiled = definition().compile(SCHEMA)
    for forbidden in ("shell_command", "database_credentials", "writable_paths", "approval_rules"):
        value = dict(compiled.value)
        value[forbidden] = "unsafe"
        from jsonschema import Draft202012Validator

        assert list(Draft202012Validator(SCHEMA).iter_errors(value))


def test_prompt_hashes_exact_utf8_bytes(tmp_path: Path) -> None:
    prompt = tmp_path / "prompt.md"
    prompt.write_bytes("café\r\n".encode())
    compiled = definition(prompt=PromptTemplate.from_file(prompt)).compile(SCHEMA)
    import hashlib

    assert compiled.value["prompt"]["sha256"] == hashlib.sha256(prompt.read_bytes()).hexdigest()


def test_invalid_utf8_prompt_is_rejected(tmp_path: Path) -> None:
    prompt = tmp_path / "prompt.md"
    prompt.write_bytes(b"\xff")
    with pytest.raises(ManifestError, match="valid UTF-8"):
        definition(prompt=PromptTemplate.from_file(prompt)).compile()
