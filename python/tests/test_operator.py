import json
import stat
import subprocess
import threading
import uuid
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest
from harness_agents.bootstrap import bootstrap_project
from harness_agents.operator import (
    OperatorError,
    approve_gate,
    export_bundle,
    load_config,
    run_lifecycle,
    status,
    submit_candidate,
)


class LifecycleHandler(BaseHTTPRequestHandler):
    calls: list[tuple[str, str, dict[str, str], dict[str, Any]]] = []
    revision = 1

    def log_message(self, format: str, *args: object) -> None:  # skipcq: PYL-W0622
        """Keep the loopback test server silent."""

    def _respond(self, value: dict[str, Any], status_code: int = 200) -> None:
        body = json.dumps(value).encode()
        self.send_response(status_code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        self.calls.append(("GET", self.path, dict(self.headers), {}))
        if self.path.endswith("/support-bundle"):
            self._respond({"schema_version": "support_bundle.v1", "reasoning_diagnostics": []})
        else:
            self._respond({"run": {"revision": self.revision, "state": "DRAFT"}})

    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        self.calls.append(("POST", self.path, dict(self.headers), body))
        if self.path == "/v1/runs":
            self.revision = 1
            self._respond({"revision": self.revision}, 201)
            return
        self.revision += 1
        self._respond({"revision": self.revision})


@pytest.fixture
def operator_fixture(tmp_path: Path) -> Iterator[tuple[Any, Path, Path, ThreadingHTTPServer]]:
    spec = tmp_path / "spec.json"
    checks = tmp_path / "checks"
    project = tmp_path / "project"
    checks.mkdir()
    spec.write_text(
        json.dumps(
            {
                "schema_version": "harness_python_project.v1",
                "name": "operator-demo",
                "package_name": "operator_demo",
                "objective": "Implement the operator demo.",
                "acceptance_criteria": [{"id": "AC-001", "description": "the demo exports main"}],
            }
        )
    )
    (checks / "test_acceptance.py").write_text("def test_placeholder() -> None:\n    pass\n")
    bootstrap_project(project, spec.resolve(), checks.resolve())
    server = ThreadingHTTPServer(("127.0.0.1", 0), LifecycleHandler)
    LifecycleHandler.calls = []
    LifecycleHandler.revision = 1
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    token = tmp_path / "token"
    token.write_text("operator-secret\n")
    token.chmod(0o600)
    config_path = tmp_path / "operator.json"
    config_path.write_text(
        json.dumps(
            {
                "schema_version": "harness_operator_config.v1",
                "endpoint": f"http://127.0.0.1:{server.server_port}",
                "token_file": str(token),
                "project_root": str(project),
            }
        )
    )
    try:
        yield load_config(config_path.resolve()), project, token, server
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def test_operator_runs_explicit_gates_submits_and_exports_redacted_bundle(
    operator_fixture: tuple[Any, Path, Path, ThreadingHTTPServer], tmp_path: Path
) -> None:
    config, project, _, _ = operator_fixture
    state_path = (tmp_path / "state.json").resolve()
    root_key = uuid.uuid4()
    run_lifecycle(config, project.resolve(), state_path, root_key)
    first_timestamp = json.loads(state_path.read_text())["operations"]["run"]["decision_timestamp"]
    run_lifecycle(config, project.resolve(), state_path, root_key)
    state = json.loads(state_path.read_text())
    assert stat.S_IMODE(state_path.stat().st_mode) == 0o600
    assert state["schema_version"] == "harness_operator_state.v1"
    assert state["operations"]["run"]["decision_timestamp"] == first_timestamp
    assert state["planning_proposal"]["tasks"][0]["required_check_ids"] == ["make-check-v1"]

    approve_gate(config, state_path, "specification", uuid.uuid4())
    approve_gate(config, state_path, "task-graph", uuid.uuid4())
    approve_gate(config, state_path, "candidate", uuid.uuid4())
    submit_candidate(config, state_path, uuid.uuid4())
    status(config, state["run_id"])
    output = (tmp_path / "support.json").resolve()
    export_bundle(config, state["run_id"], output)

    assert stat.S_IMODE(output.stat().st_mode) == 0o600
    assert json.loads(output.read_text())["schema_version"] == "support_bundle.v1"
    paths = [value[1] for value in LifecycleHandler.calls]
    assert paths.count(f"/v1/runs/{state['run_id']}/approval") == 1
    assert paths.count(f"/v1/runs/{state['run_id']}/submit") == 1
    for method, _, headers, _ in LifecycleHandler.calls:
        assert headers["Authorization"] == "Bearer operator-secret"
        if method == "POST":
            uuid.UUID(headers["Idempotency-Key"])
    encoded_calls = json.dumps(LifecycleHandler.calls)
    assert "operator-secret" in encoded_calls
    assert "raw_provider_response" not in output.read_text()


def test_operator_rejects_token_file_readable_by_group(
    operator_fixture: tuple[Any, Path, Path, ThreadingHTTPServer], tmp_path: Path
) -> None:
    config, project, token, server = operator_fixture
    token.chmod(0o640)
    path = tmp_path / "bad-config.json"
    path.write_text(
        json.dumps(
            {
                "schema_version": "harness_operator_config.v1",
                "endpoint": config.endpoint,
                "token_file": str(token),
                "project_root": str(project),
            }
        )
    )
    with pytest.raises(OperatorError, match="owner-only"):
        load_config(path.resolve())
    assert server


@pytest.mark.parametrize(
    "endpoint", ["file:///tmp/control", "https://127.0.0.1:8443", "http://example.com"]
)
def test_operator_rejects_non_loopback_http_endpoints(
    operator_fixture: tuple[Any, Path, Path, ThreadingHTTPServer],
    tmp_path: Path,
    endpoint: str,
) -> None:
    _, project, token, _ = operator_fixture
    path = tmp_path / "bad-endpoint.json"
    path.write_text(
        json.dumps(
            {
                "schema_version": "harness_operator_config.v1",
                "endpoint": endpoint,
                "token_file": str(token),
                "project_root": str(project),
            }
        )
    )

    with pytest.raises(OperatorError, match="loopback HTTP origin"):
        load_config(path.resolve())


def test_operator_refuses_token_replaced_by_symlink(
    operator_fixture: tuple[Any, Path, Path, ThreadingHTTPServer], tmp_path: Path
) -> None:
    config, _, token, _ = operator_fixture
    outside = tmp_path / "outside-token"
    outside.write_text("replacement-secret")
    outside.chmod(0o600)
    token.unlink()
    token.symlink_to(outside)

    with pytest.raises(OperatorError, match="token file has invalid content"):
        status(config, str(uuid.uuid4()))
    assert not LifecycleHandler.calls


def test_operator_refuses_dirty_project_before_network(
    operator_fixture: tuple[Any, Path, Path, ThreadingHTTPServer], tmp_path: Path
) -> None:
    config, project, _, _ = operator_fixture
    (project / "src" / "operator_demo" / "__init__.py").write_text("dirty = True\n")
    with pytest.raises(OperatorError, match="worktree must be clean"):
        run_lifecycle(config, project.resolve(), (tmp_path / "state.json").resolve(), uuid.uuid4())
    assert not LifecycleHandler.calls
    assert subprocess.run(
        ["git", "-C", project, "status", "--porcelain"],
        check=False,
        capture_output=True,
    ).stdout
