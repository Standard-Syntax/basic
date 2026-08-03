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


def _validate_path(value: str, name: str, expected: Callable[[int], bool]) -> None:
    path = Path(value)
    if not path.is_absolute() or value != os.path.normpath(value):
        raise ValueError(f"{name} must be a clean absolute path")
    try:
        mode = path.lstat().st_mode
    except FileNotFoundError as error:
        raise ValueError(f"{name} does not exist") from error
    if not expected(mode):
        kind = "regular file" if name == "PROJECT_SPEC" else "directory"
        raise ValueError(f"{name} must be a non-symlinked {kind}")


def main() -> int:
    try:
        validate(os.environ.get("PROJECT_SPEC", ""), os.environ.get("CHECKS", ""))
    except ValueError as error:
        print(f"beta-python-project-e2e: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
