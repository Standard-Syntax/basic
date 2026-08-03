"""Fail-fast validation for optional live Python project inputs."""

from __future__ import annotations

import os
import stat
import sys
from collections.abc import Callable
from pathlib import Path


def validate(project_spec: str, checks: str) -> None:
    if bool(project_spec) != bool(checks):
        raise ValueError("PROJECT_SPEC and CHECKS must be set together")
    if not project_spec:
        return
    _validate_path(project_spec, "PROJECT_SPEC", stat.S_ISREG)
    _validate_path(checks, "CHECKS", stat.S_ISDIR)


def validate_evidence(report_output: str, preserve_project: str) -> None:
    report = _clean_absolute(report_output, "REPORT_OUTPUT")
    _safe_parent(report, "REPORT_OUTPUT")
    try:
        report_mode = report.lstat().st_mode
    except FileNotFoundError:
        pass
    else:
        if not stat.S_ISREG(report_mode):
            raise ValueError("REPORT_OUTPUT must be nonexistent or a non-symlinked regular file")
    if not preserve_project:
        return
    preserve = _clean_absolute(preserve_project, "PRESERVE_PROJECT")
    _safe_parent(preserve, "PRESERVE_PROJECT")
    try:
        preserve.lstat()
    except FileNotFoundError:
        return
    raise ValueError("PRESERVE_PROJECT must not already exist")


def _clean_absolute(value: str, name: str) -> Path:
    path = Path(value)
    if not value or not path.is_absolute() or value != os.path.normpath(value):
        raise ValueError(f"{name} must be a clean absolute path")
    return path


def _safe_parent(path: Path, name: str) -> None:
    parent = path.parent
    try:
        mode = parent.lstat().st_mode
    except FileNotFoundError as error:
        raise ValueError(f"{name} parent must exist") from error
    if not stat.S_ISDIR(mode):
        raise ValueError(f"{name} parent must be a non-symlinked directory")
    for component in parent.parents:
        if component == component.parent:
            break
        if stat.S_ISLNK(component.lstat().st_mode):
            raise ValueError(f"{name} parent path must not contain symlinks")


def _validate_path(value: str, name: str, expected: Callable[[int], bool]) -> None:
    path = Path(value)
    if not path.is_absolute() or value != os.path.normpath(value):
        raise ValueError(f"{name} must be a clean absolute path")
    try:
        mode = path.lstat().st_mode
    except FileNotFoundError as error:
        raise ValueError(f"{name} does not exist") from error
    except OSError as error:
        raise ValueError(f"{name} could not be inspected") from error
    if not expected(mode):
        kind = "regular file" if name == "PROJECT_SPEC" else "directory"
        raise ValueError(f"{name} must be a non-symlinked {kind}")


def main() -> int:
    try:
        validate(os.environ.get("PROJECT_SPEC", ""), os.environ.get("CHECKS", ""))
        validate_evidence(
            os.environ.get("REPORT_OUTPUT", ""), os.environ.get("PRESERVE_PROJECT", "")
        )
    except ValueError as error:
        print(f"beta-python-project-e2e: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
