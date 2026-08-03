"""Trusted bootstrap for a bounded Python target repository."""

from __future__ import annotations

import json
import os
import re
import shutil
import stat
import subprocess
import tempfile
from dataclasses import dataclass
from importlib.resources import files as resource_files
from pathlib import Path
from typing import Any, cast

from harness_agents.manifest import ManifestError

PROJECT_SCHEMA = "harness_python_project.v1"
MAX_CHECK_FILES = 100
MAX_CHECK_BYTES = 1 << 20
_DISTRIBUTION = re.compile(r"^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$")
_MODULE = re.compile(r"^[a-z][a-z0-9_]*$")
_CRITERION = re.compile(r"^AC-[0-9]{3,6}$")


@dataclass(frozen=True)
class ProjectSpec:
    name: str
    package_name: str
    objective: str
    acceptance_criteria: tuple[dict[str, str], ...]


def _object(value: object, name: str, fields: frozenset[str]) -> dict[str, Any]:
    if not isinstance(value, dict) or not all(isinstance(key, str) for key in value):
        raise ManifestError(f"{name} must be an object")
    normalized = cast(dict[str, Any], value)
    if set(normalized) != fields:
        raise ManifestError(f"{name} fields must be exactly {sorted(fields)!r}")
    return normalized


def _pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ManifestError(f"duplicate JSON field: {key!r}")
        result[key] = value
    return result


def load_project_spec(path: Path) -> ProjectSpec:
    if not path.is_absolute() or path != Path(os.path.normpath(path)):
        raise ManifestError("project spec path must be clean and absolute")
    try:
        value = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=_pairs)
    except UnicodeDecodeError as error:
        raise ManifestError("project spec must be UTF-8") from error
    except json.JSONDecodeError as error:
        raise ManifestError("project spec must be valid JSON") from error
    root = _object(
        value,
        "project spec",
        frozenset({"schema_version", "name", "package_name", "objective", "acceptance_criteria"}),
    )
    if root["schema_version"] != PROJECT_SCHEMA:
        raise ManifestError("unsupported project spec schema")
    name = root["name"]
    package_name = root["package_name"]
    if not isinstance(name, str) or not _DISTRIBUTION.fullmatch(name):
        raise ManifestError("invalid distribution name")
    if not isinstance(package_name, str) or not _MODULE.fullmatch(package_name):
        raise ManifestError("invalid package name")
    objective = _bounded_text(root["objective"], "objective", 4096)
    normalized = _acceptance_criteria(root["acceptance_criteria"])
    return ProjectSpec(
        name=name,
        package_name=package_name,
        objective=objective,
        acceptance_criteria=tuple(normalized),
    )


def _bounded_text(value: object, name: str, maximum: int) -> str:
    if not isinstance(value, str) or not value.strip() or len(value) > maximum:
        raise ManifestError(f"{name} must be a non-empty string of at most {maximum} characters")
    return value


def _acceptance_criteria(value: object) -> list[dict[str, str]]:
    criteria = value
    if not isinstance(criteria, list) or not criteria or len(criteria) > 20:
        raise ManifestError("acceptance_criteria must contain between 1 and 20 entries")
    normalized: list[dict[str, str]] = []
    seen: set[str] = set()
    for index, item in enumerate(criteria):
        criterion = _object(item, f"acceptance_criteria[{index}]", frozenset({"id", "description"}))
        identifier, description = criterion["id"], criterion["description"]
        if (
            not isinstance(identifier, str)
            or not _CRITERION.fullmatch(identifier)
            or identifier in seen
        ):
            raise ManifestError("acceptance criterion IDs must be unique AC-NNN identifiers")
        description = _bounded_text(description, "acceptance criterion description", 2048)
        seen.add(identifier)
        normalized.append({"id": identifier, "description": description})
    return normalized


def _check_body(source: Path, path: Path) -> tuple[Path, bytes]:
    if path.suffix != ".py":
        raise ManifestError("trusted checks must contain regular Python files only")
    relative = path.relative_to(source)
    if any(part.startswith(".") or part in {"__pycache__", ".."} for part in relative.parts):
        raise ManifestError("trusted check paths must be visible and normalized")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as error:
        raise ManifestError("trusted checks must contain regular Python files only") from error
    with os.fdopen(descriptor, "rb") as check_file:
        if not stat.S_ISREG(os.fstat(check_file.fileno()).st_mode):
            raise ManifestError("trusted checks must contain regular Python files only")
        body = check_file.read(MAX_CHECK_BYTES + 1)
    try:
        body.decode("utf-8")
    except UnicodeDecodeError as error:
        raise ManifestError("trusted checks must be UTF-8") from error
    return relative, body


def _trusted_checks(source: Path) -> list[tuple[Path, bytes]]:
    if not source.is_absolute() or source != Path(os.path.normpath(source)) or not source.is_dir():
        raise ManifestError("checks path must be a clean absolute directory")
    result: list[tuple[Path, bytes]] = []
    total = 0
    for root, directories, files in os.walk(source, followlinks=False):
        directories.sort()
        files.sort()
        root_path = Path(root)
        if any((root_path / directory).is_symlink() for directory in directories):
            raise ManifestError("trusted checks cannot contain symlinks")
        for filename in files:
            relative, body = _check_body(source, root_path / filename)
            total += len(body)
            result.append((relative, body))
    if not result or len(result) > MAX_CHECK_FILES or total > MAX_CHECK_BYTES:
        raise ManifestError("trusted checks exceed file or byte limits")
    return result


def _pyproject(spec: ProjectSpec) -> str:
    return f'''[build-system]
requires = ["hatchling==1.27.0"]
build-backend = "hatchling.build"

[project]
name = "{spec.name}"
version = "0.1.0"
description = "Trusted harness bootstrap project"
requires-python = ">=3.13,<3.15"
dependencies = []

[project.scripts]
{spec.name} = "{spec.package_name}._cli:cli"

[dependency-groups]
dev = [
  "pytest==8.4.2",
  "ruff==0.14.2",
  "ty==0.0.64",
]

[tool.hatch.build.targets.wheel]
packages = ["src/{spec.package_name}"]

[tool.pytest.ini_options]
testpaths = ["tests"]

[tool.ruff]
line-length = 100
target-version = "py313"

[tool.ruff.lint]
select = ["E", "F", "I", "UP", "B", "SIM"]
'''


def _metadata(spec: ProjectSpec) -> bytes:
    value = {
        "schema_version": PROJECT_SCHEMA,
        "name": spec.name,
        "package_name": spec.package_name,
        "objective": spec.objective,
        "acceptance_criteria": list(spec.acceptance_criteria),
        "paths": {
            "readable": ["Makefile", "pyproject.toml", "src", "tests", "uv.lock"],
            "writable": ["src"],
            "prohibited": [".harness", "Makefile", "pyproject.toml", "tests", "uv.lock"],
        },
        "trusted_checks": ["make-check-v1"],
        "maximum_tasks": 1,
    }
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True).encode() + b"\n"


def _lockfile(spec: ProjectSpec) -> str:
    template = (
        resource_files("harness_agents.templates").joinpath("uv.lock").read_text(encoding="utf-8")
    )
    marker = 'name = "harness-bootstrap-template"'
    if template.count(marker) != 1:
        raise ManifestError("packaged bootstrap lockfile is invalid")
    return template.replace(marker, f'name = "{spec.name}"', 1)


def _run(arguments: list[str], root: Path) -> None:
    environment = {
        "PATH": os.environ.get("PATH", ""),
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_CONFIG_SYSTEM": os.devnull,
        "GIT_TERMINAL_PROMPT": "0",
    }
    try:
        result = subprocess.run(
            arguments,
            cwd=root,
            env=environment,
            check=False,
            capture_output=True,
            text=True,
            timeout=30,
        )
    except subprocess.TimeoutExpired as error:
        raise ManifestError(f"bootstrap command timed out: {arguments[0]}") from error
    if result.returncode != 0:
        diagnostic = result.stderr.strip() or result.stdout.strip() or arguments[0]
        raise ManifestError(f"bootstrap command failed: {diagnostic}")


def bootstrap_project(destination: Path, spec_path: Path, checks_path: Path) -> None:
    destination = destination.resolve()
    if destination.exists():
        raise ManifestError("destination must not exist")
    parent = destination.parent
    if not parent.is_dir():
        raise ManifestError("destination parent must exist")
    spec = load_project_spec(spec_path)
    checks = _trusted_checks(checks_path)
    temporary = Path(tempfile.mkdtemp(prefix=f".{destination.name}.", dir=parent))
    try:
        (temporary / "src" / spec.package_name).mkdir(parents=True)
        (temporary / "tests" / "acceptance").mkdir(parents=True)
        (temporary / ".harness").mkdir()
        (temporary / "pyproject.toml").write_text(_pyproject(spec), encoding="utf-8")
        (temporary / "Makefile").write_text(
            ".PHONY: check\ncheck:\n\tuv run --frozen ruff check .\n"
            "\tuv run --frozen ty check src\n\tPYTHONPATH=src uv run --frozen pytest\n\tuv build\n",
            encoding="utf-8",
        )
        (temporary / ".gitignore").write_text(
            ".venv/\n.pytest_cache/\n.ruff_cache/\n__pycache__/\ndist/\n",
            encoding="utf-8",
        )
        (temporary / "src" / spec.package_name / "__init__.py").write_text(
            '"""Application entry point to be implemented by the harness."""\n\n\n'
            'def main() -> None:\n    raise NotImplementedError("implementation pending")\n',
            encoding="utf-8",
        )
        (temporary / "src" / spec.package_name / "_cli.py").write_text(
            '"""Stable console entry point for the generated application."""\n\n'
            "from . import main\n\n\n"
            "def cli() -> None:\n"
            "    result = main()\n"
            "    if result is not None:\n"
            "        print(result)\n",
            encoding="utf-8",
        )
        for relative, body in checks:
            target = temporary / "tests" / "acceptance" / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(body)
        (temporary / ".harness" / "project.json").write_bytes(_metadata(spec))
        (temporary / "uv.lock").write_text(_lockfile(spec), encoding="utf-8")
        _run(["git", "init", "--quiet", "--initial-branch=main"], temporary)
        _run(["git", "-c", "core.hooksPath=/dev/null", "add", "--all"], temporary)
        _run(
            [
                "git",
                "-c",
                "core.hooksPath=/dev/null",
                "-c",
                "user.name=Harness Bootstrap",
                "-c",
                "user.email=bootstrap@harness.invalid",
                "commit",
                "--quiet",
                "--message",
                "chore: establish trusted project base",
            ],
            temporary,
        )
        temporary.rename(destination)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)
