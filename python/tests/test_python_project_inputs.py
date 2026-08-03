from pathlib import Path

import pytest
from harness_agents import project_inputs

validate = project_inputs.validate


def test_project_inputs_allow_golden_defaults_and_valid_pair(tmp_path: Path) -> None:
    validate("", "")
    specification = tmp_path / "project.json"
    checks = tmp_path / "checks"
    specification.write_text("{}")
    checks.mkdir()

    validate(str(specification), str(checks))


@pytest.mark.parametrize("missing", ["PROJECT_SPEC", "CHECKS"])
def test_project_inputs_reject_partial_pair(tmp_path: Path, missing: str) -> None:
    specification = tmp_path / "project.json"
    checks = tmp_path / "checks"
    specification.write_text("{}")
    checks.mkdir()
    values = {
        "PROJECT_SPEC": "" if missing == "PROJECT_SPEC" else str(specification),
        "CHECKS": "" if missing == "CHECKS" else str(checks),
    }

    with pytest.raises(ValueError, match="must be set together"):
        validate(values["PROJECT_SPEC"], values["CHECKS"])


@pytest.mark.parametrize("unclean", ["relative.json", "/tmp/../tmp/project.json"])
def test_project_inputs_reject_relative_or_unclean_paths(unclean: str) -> None:
    with pytest.raises(ValueError, match="clean absolute path"):
        validate(unclean, "/tmp/checks")


def test_project_inputs_reject_missing_paths(tmp_path: Path) -> None:
    with pytest.raises(ValueError, match="PROJECT_SPEC does not exist"):
        validate(str(tmp_path / "missing.json"), str(tmp_path / "checks"))


def test_project_inputs_report_stat_failures_without_tracebacks(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    specification = tmp_path / "project.json"
    checks = tmp_path / "checks"
    specification.write_text("{}")
    checks.mkdir()
    monkeypatch.setenv("PROJECT_SPEC", str(specification))
    monkeypatch.setenv("CHECKS", str(checks))

    def denied(_path: Path) -> object:
        raise PermissionError("denied")

    monkeypatch.setattr(Path, "lstat", denied)

    assert project_inputs.main() == 2
    assert capsys.readouterr().err == (
        "beta-python-project-e2e: PROJECT_SPEC could not be inspected\n"
    )


@pytest.mark.parametrize("unsafe", ["symlink", "directory"])
def test_project_inputs_reject_unsafe_spec_types(tmp_path: Path, unsafe: str) -> None:
    specification = tmp_path / "project.json"
    checks = tmp_path / "checks"
    checks.mkdir()
    if unsafe == "symlink":
        target = tmp_path / "target.json"
        target.write_text("{}")
        specification.symlink_to(target)
    else:
        specification.mkdir()

    with pytest.raises(ValueError, match="non-symlinked regular file"):
        validate(str(specification), str(checks))


@pytest.mark.parametrize("unsafe", ["symlink", "file"])
def test_project_inputs_reject_unsafe_checks_types(tmp_path: Path, unsafe: str) -> None:
    specification = tmp_path / "project.json"
    checks = tmp_path / "checks"
    specification.write_text("{}")
    if unsafe == "symlink":
        target = tmp_path / "target"
        target.mkdir()
        checks.symlink_to(target, target_is_directory=True)
    else:
        checks.write_text("not a directory")

    with pytest.raises(ValueError, match="non-symlinked directory"):
        validate(str(specification), str(checks))
