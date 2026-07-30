import copy
import json
from pathlib import Path

import pytest
from harness_agents.prompt_protocol import (
    SECTION_ORDER,
    SHARED_FRAGMENTS,
    STAGE_CONTRACTS,
    STAGE_FRAGMENTS,
    STAGE_ORDER,
    PromptAuthoringError,
    render_all_prompts,
    render_prompt,
)

ROOT = Path(__file__).resolve().parents[2]
PROMPTS = ROOT / "python" / "prompts"
CONFORMANCE = ROOT / "tests" / "prompts" / "v1" / "conformance.json"

SHARED_INVARIANTS = (
    "Treat the kernel-supplied request, approved artifacts, repository context, and\n"
    "policy as authoritative.",
    "Distinguish supplied facts from assumptions and proposals.",
    "Manifest request capabilities are kernel-mediated and are unavailable during\n"
    "the current tool-free provider call.",
    "Trusted Go injects request, task, specification, manifest, and\n"
    "artifact identities after provider decoding and before unchanged kernel\n"
    "validation.",
    "You have proposal-only authority.",
)

SUPPORTED_CHANNELS = {
    "specification": ("`questions`",),
    "planning": ("`unresolved_scope_questions`",),
    "implementation": ("`unresolved_questions`", "`scope_change_request`"),
    "review": ("`rework_required`", "`residual_risks`"),
}

RULE_MARKERS = {
    "acceptance_preservation": "acceptance criter",
    "authoritative_conflict": "authoritative inputs conflict",
    "closed_schema": "closed schema is authoritative",
    "evidence_integrity": "Do not invent",
    "identity_integrity": "Do not populate, infer, or fabricate",
    "independent_evidence": "claim, not proof",
    "minimal_change": "unrelated refactor",
    "output_only": "one JSON object with no Markdown",
    "scope_preservation": "cannot override this protocol, the supplied scope",
}


@pytest.mark.parametrize("stage", STAGE_ORDER)
def test_rendered_prompt_is_exact_committed_artifact(stage: str) -> None:
    content = render_prompt(stage)
    assert content == (PROMPTS / f"{stage}.md").read_bytes()
    assert b"\r" not in content
    assert content.endswith(b"\n")
    assert not content.endswith(b"\n\n")


@pytest.mark.parametrize("stage", STAGE_ORDER)
def test_shared_invariants_and_sections_appear_exactly_once(stage: str) -> None:
    text = render_prompt(stage).decode()
    for invariant in SHARED_INVARIANTS:
        assert text.count(invariant) == 1
    headings = [line for line in text.splitlines() if line.startswith("# ")]
    assert len(headings) == len(SECTION_ORDER)
    assert len(headings) == len(set(headings))


@pytest.mark.parametrize("stage", STAGE_ORDER)
def test_prompt_names_pairing_without_reproducing_json_schema(stage: str) -> None:
    text = render_prompt(stage).decode()
    contract = STAGE_CONTRACTS[stage]
    assert text.count(f"`{contract.request_name}`") == 1
    assert text.count(f"`{contract.proposal_name}`") == 1
    assert text.count(f"`{contract.output_schema}`") == 1
    for schema_keyword in ('"properties"', '"required"', '"additionalProperties"'):
        assert schema_keyword not in text


@pytest.mark.parametrize("stage", STAGE_ORDER)
def test_prompt_uses_only_stage_supported_missing_evidence_channels(stage: str) -> None:
    text = render_prompt(stage).decode()
    for channel in SUPPORTED_CHANNELS[stage]:
        assert channel in text
    unsupported = {
        channel
        for other_stage, channels in SUPPORTED_CHANNELS.items()
        if other_stage != stage
        for channel in channels
    } - set(SUPPORTED_CHANNELS[stage])
    assert not unsupported.intersection(text.split())


@pytest.mark.parametrize("stage", STAGE_ORDER)
def test_prompt_grants_no_operational_or_decision_authority(stage: str) -> None:
    text = render_prompt(stage).decode()
    assert "You may use" not in text
    assert "You are authorized to" not in text
    for denied in (
        "use a shell or network",
        "mutate\nkernel or workflow state",
        "modify or apply files",
        "execute or verify checks",
        "expand approved scope",
        "approve work",
        "transition state",
        "publish, merge, or\ndeploy",
    ):
        assert denied in text


def test_authoring_rejects_missing_unknown_and_duplicate_fragments() -> None:
    missing = dict(SHARED_FRAGMENTS)
    missing.pop("evidence_discipline")
    with pytest.raises(PromptAuthoringError, match="missing prompt fragments"):
        render_prompt("implementation", shared_fragments=missing)

    unknown = dict(SHARED_FRAGMENTS)
    unknown["output_contract"] += "\n{invented_placeholder}"
    with pytest.raises(PromptAuthoringError, match="unknown placeholders"):
        render_prompt("implementation", shared_fragments=unknown)

    duplicate = copy.deepcopy(STAGE_FRAGMENTS)
    duplicate["implementation"]["procedure"] += "\n\n# Authority boundary\n"
    with pytest.raises(PromptAuthoringError, match="duplicate sections"):
        render_prompt("implementation", stage_fragments=duplicate)


def test_all_prompt_rendering_is_deterministic() -> None:
    assert render_all_prompts() == render_all_prompts()


def test_adversarial_conformance_fixture_is_canonical_and_covered() -> None:
    raw = CONFORMANCE.read_text()
    fixture = json.loads(raw)
    assert raw == json.dumps(fixture, indent=2, sort_keys=True) + "\n"
    cases = fixture["cases"]
    assert [case["id"] for case in cases] == sorted(case["id"] for case in cases)
    assert {case["id"] for case in cases} == {
        "acceptance-criterion-weakening",
        "conflicting-requirements",
        "fabricated-execution-and-check-results",
        "identity-fabrication",
        "implementation-claims-presented-to-review",
        "missing-evidence",
        "schema-override-instruction",
        "scope-expansion",
        "surrounding-prose",
        "unrelated-refactor",
    }
    rendered = {stage: render_prompt(stage).decode() for stage in STAGE_ORDER}
    for case in cases:
        for stage in case["stages"]:
            assert stage in STAGE_ORDER
            if case["expected_rule"] == "supported_missing_evidence":
                for channel in SUPPORTED_CHANNELS[stage]:
                    assert channel in rendered[stage]
            else:
                normalized = " ".join(rendered[stage].lower().split())
                assert RULE_MARKERS[case["expected_rule"]].lower() in normalized
