import json
import subprocess
from pathlib import Path

import pytest
from harness_agents.bootstrap import bootstrap_project
from harness_agents.manifest import ManifestError


def write_spec(path: Path) -> None:
    path.write_text(
        json.dumps(
            {
                "schema_version": "harness_python_project.v1",
                "name": "trusted-demo",
                "package_name": "trusted_demo",
                "objective": "Implement the trusted demo entry point.",
                "acceptance_criteria": [
                    {"id": "AC-001", "description": "main prints the expected greeting"}
                ],
            }
        )
    )


def test_bootstrap_creates_committed_trusted_project(tmp_path: Path) -> None:
    spec = tmp_path / "spec.json"
    checks = tmp_path / "checks"
    destination = tmp_path / "project"
    checks.mkdir()
    write_spec(spec)
    (checks / "test_acceptance.py").write_text(
        "from trusted_demo import main\n\n\n"
        "def test_main_exists() -> None:\n"
        "    assert callable(main)\n"
    )

    bootstrap_project(destination, spec.resolve(), checks.resolve())

    metadata = json.loads((destination / ".harness/project.json").read_text())
    assert metadata["paths"]["writable"] == ["src"]
    assert metadata["paths"]["prohibited"] == [
        ".harness",
        "Makefile",
        "pyproject.toml",
        "tests",
        "uv.lock",
    ]
    assert (destination / "uv.lock").is_file()
    assert (
        subprocess.run(
            ["git", "-C", str(destination), "status", "--porcelain"],
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        == ""
    )
    assert (
        subprocess.run(
            ["git", "-C", str(destination), "branch", "--show-current"],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        == "main"
    )


def test_bootstrap_rejects_existing_destination_without_changes(tmp_path: Path) -> None:
    spec = tmp_path / "spec.json"
    checks = tmp_path / "checks"
    destination = tmp_path / "project"
    checks.mkdir()
    destination.mkdir()
    write_spec(spec)
    (checks / "test_acceptance.py").write_text("def test_placeholder() -> None:\n    pass\n")

    with pytest.raises(ManifestError, match="destination must not exist"):
        bootstrap_project(destination, spec.resolve(), checks.resolve())
    assert not list(destination.iterdir())


def test_bootstrap_rejects_symlinked_checks_before_creating_destination(tmp_path: Path) -> None:
    spec = tmp_path / "spec.json"
    checks = tmp_path / "checks"
    destination = tmp_path / "project"
    checks.mkdir()
    write_spec(spec)
    target = tmp_path / "outside.py"
    target.write_text("def test_outside() -> None:\n    pass\n")
    (checks / "test_acceptance.py").symlink_to(target)

    with pytest.raises(ManifestError, match="regular Python files only"):
        bootstrap_project(destination, spec.resolve(), checks.resolve())
    assert not destination.exists()
