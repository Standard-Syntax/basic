import json
import subprocess
from collections.abc import Callable
from pathlib import Path

import pytest
from harness_agents.bootstrap import _run, bootstrap_project, load_project_spec
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


def valid_spec() -> dict[str, object]:
    return {
        "schema_version": "harness_python_project.v1",
        "name": "trusted-demo",
        "package_name": "trusted_demo",
        "objective": "Implement the trusted demo entry point.",
        "acceptance_criteria": [
            {"id": "AC-001", "description": "main prints the expected greeting"}
        ],
    }


@pytest.mark.parametrize(
    ("mutate", "message"),
    [
        (lambda value: value.update(extra=True), "project spec fields must be exactly"),
        (lambda value: value.pop("objective"), "project spec fields must be exactly"),
        (lambda value: value.update(name="Trusted Demo"), "invalid distribution name"),
        (lambda value: value.update(package_name="trusted-demo"), "invalid package name"),
        (lambda value: value.update(objective="x" * 4097), "objective must be a non-empty"),
        (
            lambda value: value.update(
                acceptance_criteria=[
                    {"id": "AC-001", "description": "first"},
                    {"id": "AC-001", "description": "duplicate"},
                ]
            ),
            "acceptance criterion IDs must be unique",
        ),
        (
            lambda value: value.update(
                acceptance_criteria=[{"id": "criterion-1", "description": "invalid"}]
            ),
            "acceptance criterion IDs must be unique",
        ),
    ],
)
def test_load_project_spec_rejects_invalid_closed_values(
    tmp_path: Path, mutate: Callable[[dict[str, object]], object], message: str
) -> None:
    value = valid_spec()
    mutate(value)
    path = tmp_path / "spec.json"
    path.write_text(json.dumps(value))

    with pytest.raises(ManifestError, match=message):
        load_project_spec(path.resolve())


def test_load_project_spec_rejects_duplicate_keys(tmp_path: Path) -> None:
    path = tmp_path / "spec.json"
    path.write_text(
        '{"schema_version":"harness_python_project.v1","name":"trusted-demo",'
        '"name":"other","package_name":"trusted_demo","objective":"demo",'
        '"acceptance_criteria":[{"id":"AC-001","description":"works"}]}'
    )

    with pytest.raises(ManifestError, match="duplicate JSON field: 'name'"):
        load_project_spec(path.resolve())


def test_load_project_spec_rejects_malformed_json(tmp_path: Path) -> None:
    path = tmp_path / "spec.json"
    path.write_text("{")

    with pytest.raises(ManifestError, match="project spec must be valid JSON"):
        load_project_spec(path.resolve())


def test_bootstrap_command_timeout_is_bounded(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    def time_out(*args: object, **kwargs: object) -> None:
        raise subprocess.TimeoutExpired(cmd="git", timeout=30)

    monkeypatch.setattr(subprocess, "run", time_out)
    with pytest.raises(ManifestError, match="bootstrap command timed out: git"):
        _run(["git", "init"], tmp_path)


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


def test_bootstrap_candidate_passes_operator_checks_offline(tmp_path: Path) -> None:
    spec = tmp_path / "spec.json"
    checks = tmp_path / "checks"
    destination = tmp_path / "project"
    checks.mkdir()
    write_spec(spec)
    (checks / "test_acceptance.py").write_text(
        "from trusted_demo import main\n\n\n"
        "def test_main_returns_ready() -> None:\n"
        '    assert main() == "ready"\n'
    )
    bootstrap_project(destination, spec.resolve(), checks.resolve())
    (destination / "src/trusted_demo/__init__.py").write_text(
        'def main() -> str:\n    return "ready"\n'
    )

    result = subprocess.run(
        ["make", "check"], cwd=destination, check=False, capture_output=True, text=True
    )
    assert result.returncode == 0, result.stdout + result.stderr

    environment = tmp_path / "installed"
    subprocess.run(["uv", "venv", str(environment)], check=True, capture_output=True, text=True)
    wheels = list((destination / "dist").glob("*.whl"))
    assert len(wheels) == 1
    subprocess.run(
        ["uv", "pip", "install", "--python", str(environment / "bin/python"), str(wheels[0])],
        check=True,
        capture_output=True,
        text=True,
    )
    command = subprocess.run(
        [str(environment / "bin/trusted-demo")], check=False, capture_output=True, text=True
    )
    assert command.returncode == 0, command.stdout + command.stderr
    assert command.stdout == "ready\n"


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
