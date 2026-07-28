import json
import subprocess
import sys
from pathlib import Path

import pytest
from harness_agents.cli import main

ROOT = Path(__file__).resolve().parents[2]


def test_cli_compiles_golden_manifest(tmp_path: Path) -> None:
    output = tmp_path / "manifest.json"
    digest = tmp_path / "manifest.sha256"
    result = subprocess.run(
        [
            sys.executable,
            "-m",
            "harness_agents.cli",
            "compile",
            str(ROOT / "python/agents/implementation.json"),
            "--output",
            str(output),
            "--digest-output",
            str(digest),
        ],
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    assert json.loads(output.read_bytes())
    assert len(digest.read_text().strip()) == 64
    assert (
        output.read_bytes()
        == (ROOT / "tests/contracts/v1/manifest/implementation.json").read_bytes()
    )


@pytest.mark.parametrize(
    "definition",
    [
        '{"name":"first","name":"second"}',
        (
            '{"name":"agent","version":"1.0.0","stage":"implementation",'
            '"prompt_file":"prompt.md","model":{"capability_class":"strong_coding",'
            '"temperature":0,"maximum_output_tokens":1,"unknown":true},'
            '"context":{},"tools":{},"output":{}}'
        ),
    ],
)
def test_cli_rejects_non_closed_definitions_without_outputs(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, definition: str
) -> None:
    definition_path = tmp_path / "agent.json"
    definition_path.write_text(definition)
    output = tmp_path / "manifest.json"
    digest = tmp_path / "manifest.sha256"
    monkeypatch.setattr(
        sys,
        "argv",
        [
            "harness-agents",
            "compile",
            str(definition_path),
            "--output",
            str(output),
            "--digest-output",
            str(digest),
        ],
    )
    assert main() == 2
    assert not output.exists()
    assert not digest.exists()


def test_cli_cleans_temporary_files_after_write_failure(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    output = tmp_path / "manifest.json"
    digest = tmp_path / "missing" / "manifest.sha256"
    monkeypatch.setattr(
        sys,
        "argv",
        [
            "harness-agents",
            "compile",
            str(ROOT / "python/agents/implementation.json"),
            "--output",
            str(output),
            "--digest-output",
            str(digest),
        ],
    )
    assert main() == 2
    assert not output.exists()
    assert not digest.exists()
    assert not list(tmp_path.glob(".*.tmp"))


def test_cli_cleans_temporary_files_after_replace_failure(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    output = tmp_path / "manifest.json"
    output.mkdir()
    digest = tmp_path / "manifest.sha256"
    monkeypatch.setattr(
        sys,
        "argv",
        [
            "harness-agents",
            "compile",
            str(ROOT / "python/agents/implementation.json"),
            "--output",
            str(output),
            "--digest-output",
            str(digest),
        ],
    )
    assert main() == 2
    assert output.is_dir()
    assert not digest.exists()
    assert not list(tmp_path.glob(".*.tmp"))


def test_cli_rejects_unresolvable_schema_reference(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    schema = tmp_path / "schema.json"
    schema.write_text('{"$ref":"urn:missing-schema"}')
    output = tmp_path / "manifest.json"
    digest = tmp_path / "manifest.sha256"
    monkeypatch.setattr(
        sys,
        "argv",
        [
            "harness-agents",
            "compile",
            str(ROOT / "python/agents/implementation.json"),
            "--schema",
            str(schema),
            "--output",
            str(output),
            "--digest-output",
            str(digest),
        ],
    )
    assert main() == 2
    assert "cannot resolve manifest schema reference" in capsys.readouterr().err
    assert not output.exists()
    assert not digest.exists()


def test_installed_wheel_compiles_outside_repository(tmp_path: Path) -> None:
    wheel_dir = tmp_path / "wheel"
    environment = tmp_path / "venv"
    outside = tmp_path / "outside"
    wheel_dir.mkdir()
    outside.mkdir()
    subprocess.run(
        ["uv", "build", "--wheel", "--out-dir", str(wheel_dir)],
        cwd=ROOT,
        check=True,
        capture_output=True,
        text=True,
    )
    subprocess.run(
        ["uv", "venv", "--python", sys.executable, str(environment)],
        check=True,
        capture_output=True,
        text=True,
    )
    wheels = list(wheel_dir.glob("*.whl"))
    assert len(wheels) == 1
    wheel = wheels[0]
    python = environment / "bin/python"
    subprocess.run(
        ["uv", "pip", "install", "--python", str(python), str(wheel)],
        check=True,
        capture_output=True,
        text=True,
    )
    definition = outside / "implementation.json"
    prompt = outside / "prompt.md"
    value = json.loads((ROOT / "python/agents/implementation.json").read_text())
    value["prompt_file"] = "prompt.md"
    definition.write_text(json.dumps(value))
    prompt.write_bytes((ROOT / "python/prompts/implementation.md").read_bytes())
    output = outside / "manifest.json"
    digest = outside / "manifest.sha256"
    result = subprocess.run(
        [
            str(environment / "bin/harness-agents"),
            "compile",
            str(definition),
            "--output",
            str(output),
            "--digest-output",
            str(digest),
        ],
        cwd=outside,
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    assert output.is_file()
    assert len(digest.read_text().strip()) == 64
