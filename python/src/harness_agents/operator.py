"""Secret-safe operator client for the explicit harness lifecycle."""

from __future__ import annotations

import json
import os
import shutil
import stat
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, cast

from harness_agents.manifest import ManifestError

CONFIG_SCHEMA = "harness_operator_config.v1"
STATE_SCHEMA = "harness_operator_state.v2"
LEGACY_STATE_SCHEMA = "harness_operator_state.v1"
MAXIMUM_RESPONSE_BYTES = 1 << 20
MAXIMUM_TOKEN_BYTES = 4096
_STATE_FIELDS = frozenset(
    {
        "schema_version",
        "endpoint",
        "project_root",
        "run_id",
        "base_commit",
        "specification_input",
        "operations",
    }
)
_LEGACY_STATE_FIELDS = frozenset(
    {
        "schema_version",
        "endpoint",
        "project_root",
        "run_id",
        "task_id",
        "base_commit",
        "specification_request",
        "specification_proposal",
        "planning_request",
        "planning_proposal",
        "operations",
    }
)


class OperatorError(ManifestError):
    """A bounded operator configuration or service failure."""


@dataclass(frozen=True)
class OperatorConfig:
    endpoint: str
    token_file: Path
    project_root: Path


@dataclass(frozen=True)
class ProjectMetadata:
    name: str
    objective: str
    criteria: tuple[dict[str, str], ...]
    readable_paths: tuple[str, ...]
    writable_paths: tuple[str, ...]
    prohibited_paths: tuple[str, ...]
    trusted_checks: tuple[str, ...]


def _pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise OperatorError(f"duplicate JSON field: {key!r}")
        result[key] = value
    return result


def _json_file(path: Path, name: str) -> object:
    if not path.is_absolute() or path != Path(os.path.normpath(path)):
        raise OperatorError(f"{name} path must be clean and absolute")
    try:
        return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=_pairs)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise OperatorError(f"{name} must be valid UTF-8 JSON") from error


def _object(value: object, name: str, fields: frozenset[str]) -> dict[str, Any]:
    if not isinstance(value, dict) or not all(isinstance(key, str) for key in value):
        raise OperatorError(f"{name} must be an object")
    result = cast(dict[str, Any], value)
    if set(result) != fields:
        raise OperatorError(f"{name} fields must be exactly {sorted(fields)!r}")
    return result


def _loopback_origin(endpoint: str) -> str:
    parsed = urllib.parse.urlsplit(endpoint)
    if (
        parsed.scheme != "http"
        or parsed.hostname not in {"127.0.0.1", "::1", "localhost"}
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or parsed.path not in {"", "/"}
    ):
        raise OperatorError("endpoint must be a loopback HTTP origin")
    return endpoint.rstrip("/")


def _clean_absolute_path(value: str, name: str) -> Path:
    path = Path(value)
    if not path.is_absolute() or path != Path(os.path.normpath(path)):
        raise OperatorError(f"{name} must be clean and absolute")
    return path


def _private_token_file(token_stat: os.stat_result) -> None:
    if (
        not stat.S_ISREG(token_stat.st_mode)
        or token_stat.st_uid != os.getuid()
        or token_stat.st_mode & 0o077
    ):
        raise OperatorError("token file must be owner-owned, regular, and owner-only")


def load_config(path: Path) -> OperatorConfig:
    root = _object(
        _json_file(path, "operator config"),
        "operator config",
        frozenset({"schema_version", "endpoint", "token_file", "project_root"}),
    )
    if root["schema_version"] != CONFIG_SCHEMA:
        raise OperatorError("unsupported operator config schema")
    if not all(
        isinstance(root[field], str) for field in ("endpoint", "token_file", "project_root")
    ):
        raise OperatorError("operator config paths and endpoint must be strings")
    endpoint = _loopback_origin(cast(str, root["endpoint"]))
    token_file = _clean_absolute_path(cast(str, root["token_file"]), "token file")
    project_root = _clean_absolute_path(cast(str, root["project_root"]), "project root")
    _private_token_file(token_file.lstat())
    if not project_root.is_dir() or project_root.is_symlink():
        raise OperatorError("project root must be a regular directory")
    return OperatorConfig(endpoint, token_file, project_root)


def _token(config: OperatorConfig) -> str:
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(config.token_file, flags)
        with os.fdopen(descriptor, "rb") as token_stream:
            _private_token_file(os.fstat(token_stream.fileno()))
            raw = token_stream.read(MAXIMUM_TOKEN_BYTES + 1)
        value = raw.decode("utf-8").strip()
    except (OSError, UnicodeDecodeError) as error:
        raise OperatorError("token file has invalid content") from error
    if not value or "\n" in value or "\r" in value or len(raw) > MAXIMUM_TOKEN_BYTES:
        raise OperatorError("token file has invalid content")
    return value


def _service_url(config: OperatorConfig, target: str) -> str:
    target_parts = urllib.parse.urlsplit(target)
    if (
        not target.startswith("/v1/")
        or target.startswith("//")
        or target_parts.scheme
        or target_parts.netloc
        or target_parts.query
        or target_parts.fragment
    ):
        raise OperatorError("service target must be an absolute v1 API path")
    url = config.endpoint + target
    parsed = urllib.parse.urlsplit(url)
    origin = urllib.parse.urlsplit(config.endpoint)
    if (
        parsed.scheme != "http"
        or parsed.hostname not in {"127.0.0.1", "::1", "localhost"}
        or parsed.netloc != origin.netloc
    ):
        raise OperatorError("service URL must remain on the configured loopback origin")
    return url


def _request(
    config: OperatorConfig,
    method: str,
    target: str,
    *,
    body: dict[str, Any] | None = None,
    idempotency_key: uuid.UUID | None = None,
    revision: int | None = None,
) -> dict[str, Any]:
    payload = None
    headers = {"Authorization": "Bearer " + _token(config), "Accept": "application/json"}
    if body is not None:
        payload = json.dumps(body, sort_keys=True, separators=(",", ":")).encode()
        headers["Content-Type"] = "application/json"
    if idempotency_key is not None:
        headers["Idempotency-Key"] = str(idempotency_key)
    if revision is not None:
        headers["If-Match"] = f'"{revision}"'
    request = urllib.request.Request(_service_url(config, target), payload, headers, method=method)
    try:
        # _service_url rejects non-HTTP and non-loopback targets immediately above.
        with urllib.request.urlopen(request, timeout=30) as response:  # skipcq: BAN-B310
            raw = response.read(MAXIMUM_RESPONSE_BYTES + 1)
    except urllib.error.HTTPError as error:
        raw = error.read(MAXIMUM_RESPONSE_BYTES + 1)
        code = "service_error"
        try:
            value = json.loads(raw)
            if isinstance(value, dict) and isinstance(value.get("error"), dict):
                candidate = value["error"].get("code")
                if isinstance(candidate, str) and len(candidate) <= 64:
                    code = candidate
        except (UnicodeDecodeError, json.JSONDecodeError):
            pass
        raise OperatorError(f"service returned HTTP {error.code}: {code}") from error
    except OSError as error:
        raise OperatorError("service request failed") from error
    if len(raw) > MAXIMUM_RESPONSE_BYTES:
        raise OperatorError("service response exceeds byte limit")
    try:
        value = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise OperatorError("service returned invalid JSON") from error
    if not isinstance(value, dict):
        raise OperatorError("service returned a non-object response")
    return cast(dict[str, Any], value)


def _git(project: Path, *arguments: str) -> str:
    environment = {
        "PATH": os.environ.get("PATH", ""),
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_CONFIG_SYSTEM": os.devnull,
        "GIT_TERMINAL_PROMPT": "0",
    }
    executable = shutil.which("git", path=environment["PATH"])
    if executable is None or not Path(executable).is_absolute():
        raise OperatorError("absolute Git executable not found")
    try:
        result = subprocess.run(
            [executable, "-c", "core.hooksPath=/dev/null", *arguments],
            cwd=project,
            env=environment,
            check=False,
            capture_output=True,
            text=True,
            timeout=30,
        )
    except subprocess.TimeoutExpired as error:
        raise OperatorError("project Git inspection timed out") from error
    if result.returncode != 0:
        raise OperatorError("project Git inspection failed")
    return result.stdout.strip()


def _project_head(config: OperatorConfig, project: Path) -> str:
    if project != config.project_root:
        raise OperatorError("project must equal the configured project root")
    if _git(project, "status", "--porcelain", "--untracked-files=all"):
        raise OperatorError("project worktree must be clean")
    base_commit = _git(project, "rev-parse", "HEAD")
    if len(base_commit) != 40 or any(value not in "0123456789abcdef" for value in base_commit):
        raise OperatorError("project HEAD is not a full Git commit")
    return base_commit


def _string_tuple(value: object, name: str) -> tuple[str, ...]:
    if not isinstance(value, list) or not value or not all(isinstance(item, str) for item in value):
        raise OperatorError(f"{name} must be a non-empty string array")
    return tuple(cast(list[str], value))


def _criteria(value: object) -> tuple[dict[str, str], ...]:
    if not isinstance(value, list) or not value:
        raise OperatorError("project acceptance criteria must be a non-empty array")
    result: list[dict[str, str]] = []
    for index, item in enumerate(value):
        criterion = _object(
            item,
            f"project acceptance_criteria[{index}]",
            frozenset({"id", "description"}),
        )
        if not isinstance(criterion["id"], str) or not isinstance(criterion["description"], str):
            raise OperatorError("project acceptance criteria must contain string values")
        result.append(cast(dict[str, str], criterion))
    return tuple(result)


def _project_metadata(project: Path) -> ProjectMetadata:
    metadata = _object(
        _json_file(project / ".harness" / "project.json", "project metadata"),
        "project metadata",
        frozenset(
            {
                "schema_version",
                "name",
                "package_name",
                "objective",
                "acceptance_criteria",
                "paths",
                "trusted_checks",
                "maximum_tasks",
            }
        ),
    )
    paths = _object(
        metadata["paths"], "project paths", frozenset({"readable", "writable", "prohibited"})
    )
    if metadata["schema_version"] != "harness_python_project.v1":
        raise OperatorError("project metadata is incompatible with lifecycle v1")
    if not isinstance(metadata["name"], str) or not isinstance(metadata["objective"], str):
        raise OperatorError("project metadata is incompatible with lifecycle v1")
    if metadata["maximum_tasks"] != 1:
        raise OperatorError("project metadata is incompatible with lifecycle v1")
    checks = _string_tuple(metadata["trusted_checks"], "trusted_checks")
    if checks != ("make-check-v1",):
        raise OperatorError("project metadata is incompatible with lifecycle v1")
    return ProjectMetadata(
        name=cast(str, metadata["name"]),
        objective=cast(str, metadata["objective"]),
        criteria=_criteria(metadata["acceptance_criteria"]),
        readable_paths=_string_tuple(paths["readable"], "readable paths"),
        writable_paths=_string_tuple(paths["writable"], "writable paths"),
        prohibited_paths=_string_tuple(paths["prohibited"], "prohibited paths"),
        trusted_checks=checks,
    )


def _project_state(config: OperatorConfig, project: Path, root_key: uuid.UUID) -> dict[str, Any]:
    base_commit = _project_head(config, project)
    metadata = _project_metadata(project)
    run_id = str(uuid.uuid5(root_key, "run"))
    return {
        "schema_version": STATE_SCHEMA,
        "endpoint": config.endpoint,
        "project_root": str(project),
        "run_id": run_id,
        "base_commit": base_commit,
        "specification_input": {
            "schema_version": "run_intake_specification.v1",
            "problem_statement": metadata.objective,
            "desired_outcome": metadata.objective,
            "known_constraints": [
                "Preserve trusted checks and immutable project configuration.",
                "Acceptance criteria: "
                + "; ".join(f"{item['id']}: {item['description']}" for item in metadata.criteria),
                "Readable paths: " + ", ".join(metadata.readable_paths),
                "Writable paths: " + ", ".join(metadata.writable_paths),
                "Prohibited paths: " + ", ".join(metadata.prohibited_paths),
                "Trusted checks: " + ", ".join(metadata.trusted_checks),
            ],
            "known_non_goals": ["Modify tests, lockfiles, or harness metadata."],
            "stakeholders": ["operator"],
            "repository_summary": f"Trusted bootstrap Python project {metadata.name}.",
        },
        "operations": {},
    }


def _write_state(path: Path, value: dict[str, Any], *, replace: bool = False) -> None:
    if not path.is_absolute() or path != Path(os.path.normpath(path)):
        raise OperatorError("state path must be clean and absolute")
    if path.exists() and not replace:
        raise OperatorError("state file already exists")
    path.parent.mkdir(parents=False, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(value, stream, indent=2, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def _upgrade_legacy_state(path: Path, raw: dict[str, Any]) -> dict[str, Any]:
    legacy = _object(raw, "operator state", _LEGACY_STATE_FIELDS)
    request = legacy["specification_request"]
    if not isinstance(request, dict):
        raise OperatorError("legacy operator state contains invalid specification input")
    specification_input = {
        "schema_version": "run_intake_specification.v1",
        "problem_statement": request.get("problem_statement"),
        "desired_outcome": request.get("desired_outcome"),
        "known_constraints": request.get("known_constraints", []),
        "known_non_goals": request.get("known_non_goals", []),
        "stakeholders": request.get("stakeholders", []),
    }
    if "repository_summary" in request:
        specification_input["repository_summary"] = request["repository_summary"]
    root = {
        "schema_version": STATE_SCHEMA,
        "endpoint": legacy["endpoint"],
        "project_root": legacy["project_root"],
        "run_id": legacy["run_id"],
        "base_commit": legacy["base_commit"],
        "specification_input": specification_input,
        "operations": legacy["operations"],
    }
    _write_state(path, root, replace=True)
    return root


def _validate_state(root: dict[str, Any], config: OperatorConfig) -> None:
    if (
        root["schema_version"] != STATE_SCHEMA
        or root["endpoint"] != config.endpoint
        or root["project_root"] != str(config.project_root)
    ):
        raise OperatorError("operator state does not match configuration")
    try:
        uuid.UUID(cast(str, root["run_id"]))
    except (TypeError, ValueError) as error:
        raise OperatorError("operator state contains an invalid identity") from error
    operations = root["operations"]
    if not isinstance(operations, dict) or not all(
        isinstance(name, str)
        and isinstance(value, dict)
        and set(value) == {"root_key", "decision_timestamp"}
        and isinstance(value["root_key"], str)
        and isinstance(value["decision_timestamp"], str)
        for name, value in operations.items()
    ):
        raise OperatorError("operator state contains invalid operation recovery data")
    specification_input = root["specification_input"]
    if (
        not isinstance(specification_input, dict)
        or specification_input.get("schema_version") != "run_intake_specification.v1"
    ):
        raise OperatorError("operator state contains invalid specification input")


def load_state(path: Path, config: OperatorConfig) -> dict[str, Any]:
    raw = _json_file(path, "operator state")
    if isinstance(raw, dict) and raw.get("schema_version") == LEGACY_STATE_SCHEMA:
        root = _upgrade_legacy_state(path, cast(dict[str, Any], raw))
    else:
        root = _object(raw, "operator state", _STATE_FIELDS)
    _validate_state(root, config)
    return root


def _key(root: uuid.UUID, operation: str) -> uuid.UUID:
    return uuid.uuid5(root, operation)


def _operation_timestamp(
    state_path: Path, state: dict[str, Any], operation: str, root_key: uuid.UUID
) -> str:
    operations = cast(dict[str, dict[str, str]], state["operations"])
    existing = operations.get(operation)
    if existing is not None:
        if existing["root_key"] != str(root_key):
            raise OperatorError(f"operation {operation!r} already has a different idempotency root")
        return existing["decision_timestamp"]
    timestamp = datetime.now(UTC).isoformat().replace("+00:00", "Z")
    operations[operation] = {"root_key": str(root_key), "decision_timestamp": timestamp}
    _write_state(state_path, state, replace=True)
    return timestamp


def _revision(config: OperatorConfig, run_id: str) -> int:
    response = _request(config, "GET", f"/v1/runs/{run_id}")
    run = response.get("run")
    if not isinstance(run, dict) or not isinstance(run.get("revision"), int):
        raise OperatorError("service run response is invalid")
    return cast(int, run["revision"])


def _wait_for_run_state(
    config: OperatorConfig, run_id: str, expected: str, *, timeout: float = 120.0
) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    terminal = {"FAILED", "CANCELLED", "REJECTED"}
    while True:
        response = _request(config, "GET", f"/v1/runs/{run_id}")
        run = response.get("run")
        state = run.get("state") if isinstance(run, dict) else None
        if state == expected:
            return response
        if state in terminal:
            raise OperatorError(f"run reached terminal state before {expected}")
        if time.monotonic() >= deadline:
            raise OperatorError(f"timed out waiting for run state {expected}")
        time.sleep(0.25)


def run_lifecycle(
    config: OperatorConfig, project: Path, state_path: Path, idempotency_key: uuid.UUID
) -> dict[str, Any]:
    if state_path.exists():
        state = load_state(state_path, config)
    else:
        state = _project_state(config, project, idempotency_key)
        _write_state(state_path, state)
    run_id = cast(str, state["run_id"])
    now = _operation_timestamp(state_path, state, "run", idempotency_key)
    _request(
        config,
        "POST",
        "/v1/runs",
        body={
            "run_id": run_id,
            "base_commit": state["base_commit"],
            "content": state["specification_input"],
            "decision_timestamp": now,
        },
        idempotency_key=_key(idempotency_key, "create-run"),
    )
    return _wait_for_run_state(config, run_id, "SPECIFICATION_REVIEW")


def approve_gate(
    config: OperatorConfig, state_path: Path, gate: str, idempotency_key: uuid.UUID
) -> dict[str, Any]:
    state = load_state(state_path, config)
    run_id = cast(str, state["run_id"])
    now = _operation_timestamp(state_path, state, "approve-" + gate, idempotency_key)
    if gate == "specification":
        _request(
            config,
            "POST",
            f"/v1/runs/{run_id}/specification/approve",
            body={"decision_timestamp": now},
            idempotency_key=_key(idempotency_key, "approve-specification"),
            revision=_revision(config, run_id),
        )
        return _wait_for_run_state(config, run_id, "TASK_PLAN_REVIEW")
    suffix = "task-graph/approve" if gate == "task-graph" else "approval"
    return _request(
        config,
        "POST",
        f"/v1/runs/{run_id}/{suffix}",
        body={"decision_timestamp": now},
        idempotency_key=_key(idempotency_key, f"approve-{gate}"),
        revision=_revision(config, run_id),
    )


def submit_candidate(
    config: OperatorConfig, state_path: Path, idempotency_key: uuid.UUID
) -> dict[str, Any]:
    state = load_state(state_path, config)
    run_id = cast(str, state["run_id"])
    decision_timestamp = _operation_timestamp(state_path, state, "submit", idempotency_key)
    return _request(
        config,
        "POST",
        f"/v1/runs/{run_id}/submit",
        body={"decision_timestamp": decision_timestamp},
        idempotency_key=_key(idempotency_key, "submit"),
        revision=_revision(config, run_id),
    )


def status(config: OperatorConfig, run_id: str) -> dict[str, Any]:
    try:
        uuid.UUID(run_id)
    except ValueError as error:
        raise OperatorError("run ID must be a UUID") from error
    return _request(config, "GET", f"/v1/runs/{run_id}")


def export_bundle(config: OperatorConfig, run_id: str, output: Path) -> dict[str, Any]:
    bundle = _request(config, "GET", f"/v1/runs/{run_id}/support-bundle")
    _write_state(output, bundle, replace=False)
    return bundle
