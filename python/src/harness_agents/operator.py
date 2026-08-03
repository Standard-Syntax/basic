"""Secret-safe operator client for the explicit harness lifecycle."""

from __future__ import annotations

import hashlib
import json
import os
import stat
import subprocess
import tempfile
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any, cast

from google.protobuf.json_format import MessageToDict, ParseDict, ParseError

from harness_agents._generated.harness.reasoning.v1 import (
    common_pb2 as _common_pb2,
)
from harness_agents._generated.harness.reasoning.v1 import (
    planning_pb2 as _planning_pb2,
)
from harness_agents._generated.harness.reasoning.v1 import (
    specification_pb2 as _specification_pb2,
)
from harness_agents.manifest import ManifestError

common_pb2: Any = _common_pb2
planning_pb2: Any = _planning_pb2
specification_pb2: Any = _specification_pb2

CONFIG_SCHEMA = "harness_operator_config.v1"
STATE_SCHEMA = "harness_operator_state.v1"
MAXIMUM_RESPONSE_BYTES = 1 << 20
_STATE_FIELDS = frozenset(
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
    }
)


class OperatorError(ManifestError):
    """A bounded operator configuration or service failure."""


@dataclass(frozen=True)
class OperatorConfig:
    endpoint: str
    token_file: Path
    project_root: Path


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
    endpoint = cast(str, root["endpoint"])
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
    token_file = Path(cast(str, root["token_file"]))
    project_root = Path(cast(str, root["project_root"]))
    for value, name in ((token_file, "token file"), (project_root, "project root")):
        if not value.is_absolute() or value != Path(os.path.normpath(value)):
            raise OperatorError(f"{name} must be clean and absolute")
    token_stat = token_file.lstat()
    if (
        not stat.S_ISREG(token_stat.st_mode)
        or token_stat.st_uid != os.getuid()
        or token_stat.st_mode & 0o077
    ):
        raise OperatorError("token file must be owner-owned, regular, and owner-only")
    if not project_root.is_dir() or project_root.is_symlink():
        raise OperatorError("project root must be a regular directory")
    return OperatorConfig(endpoint.rstrip("/"), token_file, project_root)


def _token(config: OperatorConfig) -> str:
    value = config.token_file.read_text(encoding="utf-8").strip()
    if not value or "\n" in value or "\r" in value or len(value) > 4096:
        raise OperatorError("token file has invalid content")
    return value


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
    request = urllib.request.Request(config.endpoint + target, payload, headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
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
    except (OSError, urllib.error.URLError) as error:
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
    result = subprocess.run(
        ["git", "-c", "core.hooksPath=/dev/null", *arguments],
        cwd=project,
        env={
            "PATH": os.environ.get("PATH", ""),
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull,
            "GIT_TERMINAL_PROMPT": "0",
        },
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise OperatorError("project Git inspection failed")
    return result.stdout.strip()


def _message_dict(message: Any) -> dict[str, Any]:
    return cast(
        dict[str, Any],
        MessageToDict(message, preserving_proto_field_name=True, use_integers_for_enums=False),
    )


def _envelope(run_id: str, request_id: str, stage: int, created: datetime) -> Any:
    result = common_pb2.ReasoningRequestEnvelope(
        schema_version="1",
        request_id=request_id,
        run_id=run_id,
        stage=stage,
        attempt=1,
        authority=common_pb2.AuthorityConstraints(
            mode=common_pb2.AUTHORITY_MODE_PROPOSAL_ONLY,
        ),
        budget=common_pb2.ReasoningBudget(
            maximum_input_tokens=1,
            maximum_output_tokens=1,
            maximum_provider_requests=1,
        ),
        agent_manifest_digest=hashlib.sha256(f"trusted-{stage}".encode()).hexdigest(),
    )
    result.created_at.FromDatetime(created)
    result.expires_at.FromDatetime(created + timedelta(hours=1))
    return result


def _identity(envelope: Any) -> Any:
    return common_pb2.ProposalIdentity(
        schema_version=envelope.schema_version,
        request_id=envelope.request_id,
        run_id=envelope.run_id,
        stage=envelope.stage,
        attempt=envelope.attempt,
        agent_manifest_digest=envelope.agent_manifest_digest,
    )


def _project_state(config: OperatorConfig, project: Path, root_key: uuid.UUID) -> dict[str, Any]:
    if project != config.project_root:
        raise OperatorError("project must equal the configured project root")
    if _git(project, "status", "--porcelain", "--untracked-files=all"):
        raise OperatorError("project worktree must be clean")
    base_commit = _git(project, "rev-parse", "HEAD")
    if len(base_commit) != 40 or any(value not in "0123456789abcdef" for value in base_commit):
        raise OperatorError("project HEAD is not a full Git commit")
    metadata_path = project / ".harness" / "project.json"
    metadata = _object(
        _json_file(metadata_path, "project metadata"),
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
    criteria = metadata["acceptance_criteria"]
    checks = metadata["trusted_checks"]
    if (
        metadata["schema_version"] != "harness_python_project.v1"
        or not isinstance(metadata["objective"], str)
        or not isinstance(criteria, list)
        or not criteria
        or not isinstance(checks, list)
        or checks != ["make-check-v1"]
        or metadata["maximum_tasks"] != 1
        or not all(isinstance(paths[name], list) for name in paths)
    ):
        raise OperatorError("project metadata is incompatible with lifecycle v1")
    criterion_values = cast(list[dict[str, str]], criteria)
    run_id = str(uuid.uuid5(root_key, "run"))
    task_id = str(uuid.uuid5(root_key, "task"))
    created = datetime.now(UTC).replace(microsecond=0)
    specification_envelope = _envelope(
        run_id,
        str(uuid.uuid5(root_key, "specification-request")),
        common_pb2.REASONING_STAGE_SPECIFICATION,
        created,
    )
    specification_request = specification_pb2.SpecificationRequest(
        envelope=specification_envelope,
        problem_statement=cast(str, metadata["objective"]),
        desired_outcome=cast(str, metadata["objective"]),
        known_constraints=["Preserve trusted checks and immutable project configuration."],
        known_non_goals=["Modify tests, lockfiles, or harness metadata."],
        stakeholders=["operator"],
        repository_summary="Trusted bootstrap Python project.",
    )
    specification_proposal = specification_pb2.SpecificationProposal(
        identity=_identity(specification_envelope),
        title=f"Implement {metadata['name']}",
        goal=cast(str, metadata["objective"]),
        actors=["operator", "implementation agent"],
        constraints=["Only src is writable.", "All operator checks must pass."],
        non_goals=["Modify trusted verification inputs."],
        acceptance_criteria=[
            specification_pb2.AcceptanceCriterion(
                criterion_id=item["id"],
                description=item["description"],
                verification_method="make check",
            )
            for item in criterion_values
        ],
    )
    specification_digest = hashlib.sha256(
        specification_proposal.SerializeToString(deterministic=True)
    ).hexdigest()
    planning_envelope = _envelope(
        run_id,
        str(uuid.uuid5(root_key, "planning-request")),
        common_pb2.REASONING_STAGE_PLANNING,
        created,
    )
    repository_path = "pyproject.toml"
    repository_digest = hashlib.sha256((project / repository_path).read_bytes()).hexdigest()
    criterion_ids = [item["id"] for item in criterion_values]
    planning_request = planning_pb2.TaskPlanningRequest(
        envelope=planning_envelope,
        approved_specification_id="SPEC-001",
        approved_specification_digest=specification_digest,
        repository_map=[
            planning_pb2.RepositoryEntry(
                path=repository_path, kind="file", sha256=repository_digest
            )
        ],
        readable_paths=paths["readable"],
        writable_paths=paths["writable"],
        prohibited_paths=paths["prohibited"],
        task_count_limit=1,
        parallelism_limit=1,
        acceptance_criterion_ids=criterion_ids,
    )
    planning_proposal = planning_pb2.TaskGraphProposal(
        identity=_identity(planning_envelope),
        approved_specification_id="SPEC-001",
        approved_specification_digest=specification_digest,
        tasks=[
            planning_pb2.PlannedTask(
                task_id=task_id,
                objective=cast(str, metadata["objective"]),
                acceptance_criterion_ids=criterion_ids,
                readable_paths=paths["readable"],
                writable_paths=paths["writable"],
                prohibited_paths=paths["prohibited"],
                exclusive_resources=["source-tree"],
                required_check_ids=checks,
                stop_conditions=["Stop when make check passes or the attempt budget is exhausted."],
            )
        ],
    )
    return {
        "schema_version": STATE_SCHEMA,
        "endpoint": config.endpoint,
        "project_root": str(project),
        "run_id": run_id,
        "task_id": task_id,
        "base_commit": base_commit,
        "specification_request": _message_dict(specification_request),
        "specification_proposal": _message_dict(specification_proposal),
        "planning_request": _message_dict(planning_request),
        "planning_proposal": _message_dict(planning_proposal),
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


def load_state(path: Path, config: OperatorConfig) -> dict[str, Any]:
    root = _object(_json_file(path, "operator state"), "operator state", _STATE_FIELDS)
    if (
        root["schema_version"] != STATE_SCHEMA
        or root["endpoint"] != config.endpoint
        or root["project_root"] != str(config.project_root)
    ):
        raise OperatorError("operator state does not match configuration")
    for field in ("run_id", "task_id"):
        try:
            uuid.UUID(cast(str, root[field]))
        except (TypeError, ValueError) as error:
            raise OperatorError("operator state contains an invalid identity") from error
    try:
        validate_transport_state(root)
    except (ParseError, TypeError, ValueError) as error:
        raise OperatorError("operator state contains invalid transport JSON") from error
    return root


def _key(root: uuid.UUID, operation: str) -> uuid.UUID:
    return uuid.uuid5(root, operation)


def _revision(config: OperatorConfig, run_id: str) -> int:
    response = _request(config, "GET", f"/v1/runs/{run_id}")
    run = response.get("run")
    if not isinstance(run, dict) or not isinstance(run.get("revision"), int):
        raise OperatorError("service run response is invalid")
    return cast(int, run["revision"])


def run_lifecycle(
    config: OperatorConfig, project: Path, state_path: Path, idempotency_key: uuid.UUID
) -> dict[str, Any]:
    if state_path.exists():
        state = load_state(state_path, config)
    else:
        state = _project_state(config, project, idempotency_key)
        _write_state(state_path, state)
    run_id = cast(str, state["run_id"])
    now = datetime.now(UTC).isoformat().replace("+00:00", "Z")
    created = _request(
        config,
        "POST",
        "/v1/runs",
        body={
            "run_id": run_id,
            "base_commit": state["base_commit"],
            "content": {"objective": "trusted Python project lifecycle"},
            "decision_timestamp": now,
        },
        idempotency_key=_key(idempotency_key, "create-run"),
    )
    revision = created.get("revision")
    if not isinstance(revision, int):
        revision = _revision(config, run_id)
    return _request(
        config,
        "POST",
        f"/v1/runs/{run_id}/specification",
        body={
            "request": state["specification_request"],
            "proposal": state["specification_proposal"],
            "decision_timestamp": now,
        },
        idempotency_key=_key(idempotency_key, "propose-specification"),
        revision=revision,
    )


def approve_gate(
    config: OperatorConfig, state_path: Path, gate: str, idempotency_key: uuid.UUID
) -> dict[str, Any]:
    state = load_state(state_path, config)
    run_id = cast(str, state["run_id"])
    now = datetime.now(UTC).isoformat().replace("+00:00", "Z")
    if gate == "specification":
        approved = _request(
            config,
            "POST",
            f"/v1/runs/{run_id}/specification/approve",
            body={"decision_timestamp": now},
            idempotency_key=_key(idempotency_key, "approve-specification"),
            revision=_revision(config, run_id),
        )
        _ = approved
        return _request(
            config,
            "POST",
            f"/v1/runs/{run_id}/task-graph",
            body={
                "request": state["planning_request"],
                "proposal": state["planning_proposal"],
                "decision_timestamp": now,
            },
            idempotency_key=_key(idempotency_key, "propose-task-graph"),
            revision=_revision(config, run_id),
        )
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
    return _request(
        config,
        "POST",
        f"/v1/runs/{run_id}/submit",
        body={"decision_timestamp": datetime.now(UTC).isoformat().replace("+00:00", "Z")},
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


def validate_transport_state(state: dict[str, Any]) -> None:
    """Validate stored protobuf JSON before any service request."""
    ParseDict(state["specification_request"], specification_pb2.SpecificationRequest())
    ParseDict(state["specification_proposal"], specification_pb2.SpecificationProposal())
    ParseDict(state["planning_request"], planning_pb2.TaskPlanningRequest())
    ParseDict(state["planning_proposal"], planning_pb2.TaskGraphProposal())
