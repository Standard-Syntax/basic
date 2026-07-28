import copy
from pathlib import Path

import pytest
from harness_agents.cli import _definition, _load_json

ROOT = Path(__file__).resolve().parents[2]
MANIFEST_FIXTURES = ROOT / "tests/contracts/v1/manifest"


@pytest.mark.parametrize("stage", ["specification", "planning", "implementation", "review"])
def test_agent_catalog_is_deterministic_and_matches_fixtures(stage: str) -> None:
    definition_path = ROOT / f"python/agents/{stage}.json"
    value = _load_json(definition_path, f"{stage} definition")
    assert isinstance(value, dict)

    first = _definition(value, definition_path.parent).compile()
    reordered_value = copy.deepcopy(value)
    reordered_value["tools"]["allowed_requests"].reverse()
    reordered_value["metadata"]["labels"].reverse()
    reordered = _definition(reordered_value, definition_path.parent).compile()

    assert first.canonical_bytes == reordered.canonical_bytes
    assert first.digest == reordered.digest
    assert first.canonical_bytes + b"\n" == (MANIFEST_FIXTURES / f"{stage}.json").read_bytes()
    assert first.digest + "\n" == (MANIFEST_FIXTURES / f"{stage}.sha256").read_text()
