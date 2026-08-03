"""Trusted bootstrap for a bounded Python target repository."""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
from dataclasses import dataclass
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
    root = _object(
        value,
        "project spec",
        frozenset({"schema_version", "name", "package_name", "objective", "acceptance_criteria"}),
    )
    if root["schema_version"] != PROJECT_SCHEMA:
        raise ManifestError("unsupported project spec schema")
    if not isinstance(root["name"], str) or not _DISTRIBUTION.fullmatch(root["name"]):
        raise ManifestError("invalid distribution name")
    if not isinstance(root["package_name"], str) or not _MODULE.fullmatch(root["package_name"]):
        raise ManifestError("invalid package name")
    if (
        not isinstance(root["objective"], str)
        or not root["objective"].strip()
        or len(root["objective"]) > 4096
    ):
        raise ManifestError("objective must be a non-empty string of at most 4096 characters")
    criteria = root["acceptance_criteria"]
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
        if not isinstance(description, str) or not description.strip() or len(description) > 2048:
            raise ManifestError("acceptance criterion descriptions must be non-empty and bounded")
        seen.add(identifier)
        normalized.append({"id": identifier, "description": description})
    return ProjectSpec(
        name=root["name"],
        package_name=root["package_name"],
        objective=root["objective"],
        acceptance_criteria=tuple(normalized),
    )


def _trusted_checks(source: Path) -> list[tuple[Path, bytes]]:
    if not source.is_absolute() or source != Path(os.path.normpath(source)) or not source.is_dir():
        raise ManifestError("checks path must be a clean absolute directory")
    result: list[tuple[Path, bytes]] = []
    total = 0
    for root, directories, files in os.walk(source, followlinks=False):
        directories.sort()
        files.sort()
        root_path = Path(root)
        for directory in directories:
            if (root_path / directory).is_symlink():
                raise ManifestError("trusted checks cannot contain symlinks")
        for filename in files:
            path = root_path / filename
            if path.is_symlink() or not path.is_file() or path.suffix != ".py":
                raise ManifestError("trusted checks must contain regular Python files only")
            relative = path.relative_to(source)
            if any(
                part.startswith(".") or part in {"__pycache__", ".."} for part in relative.parts
            ):
                raise ManifestError("trusted check paths must be visible and normalized")
            body = path.read_bytes()
            try:
                body.decode("utf-8")
            except UnicodeDecodeError as error:
                raise ManifestError("trusted checks must be UTF-8") from error
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
{spec.name} = "{spec.package_name}:main"

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


def _run(arguments: list[str], root: Path) -> None:
    environment = {
        "PATH": os.environ.get("PATH", ""),
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_CONFIG_SYSTEM": os.devnull,
        "GIT_TERMINAL_PROMPT": "0",
    }
    result = subprocess.run(
        arguments,
        cwd=root,
        env=environment,
        check=False,
        capture_output=True,
        text=True,
    )
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
            "\tuv run --frozen ty check src\n\tuv run --frozen pytest\n\tuv build\n",
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
        for relative, body in checks:
            target = temporary / "tests" / "acceptance" / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(body)
        (temporary / ".harness" / "project.json").write_bytes(_metadata(spec))
        _run(["uv", "lock"], temporary)
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
