import os
import subprocess
from pathlib import Path


def fake_docker(tmp_path: Path) -> tuple[Path, Path]:
    executable = tmp_path / "docker"
    log = tmp_path / "docker.log"
    executable.write_text(
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        'printf "%s\\n" "$*" >>"$DOCKER_LOG"\n'
        "if [[ $1 == buildx && $2 == version ]]; then\n"
        '  printf "%s\\n" "$BUILDX_VERSION"\n'
        "  exit 0\n"
        "fi\n"
        "if [[ $1 == buildx && $2 == build ]]; then\n"
        "  while (($#)); do\n"
        '    if [[ $1 == --iidfile ]]; then shift; printf "%s\\n" "$BUILD_IID" >"$1"; fi\n'
        "    shift || true\n"
        "  done\n"
        "  exit 0\n"
        "fi\n"
        'if [[ $1 == image && $2 == inspect ]]; then printf "%s\\n" "$INSPECT_IID"; exit 0; fi\n'
        "exit 1\n"
    )
    executable.chmod(0o700)
    return executable, log


def build_environment(tmp_path: Path) -> tuple[dict[str, str], Path]:
    docker, log = fake_docker(tmp_path)
    image_id = "sha256:" + "a" * 64
    environment = os.environ | {
        "DOCKER_CLI": str(docker),
        "DOCKER_LOG": str(log),
        "BUILDX_VERSION": "github.com/docker/buildx v0.36.0 commit",
        "BUILD_IID": image_id,
        "INSPECT_IID": image_id,
    }
    return environment, log


def test_buildx_prerequisite_rejects_missing_or_wrong_version(tmp_path: Path) -> None:
    missing = subprocess.run(
        ["./scripts/require-buildx.sh"],
        env=os.environ | {"DOCKER_CLI": str(tmp_path / "missing")},
        check=False,
        capture_output=True,
        text=True,
    )
    assert missing.returncode == 2
    assert "Buildx v0.36.0 is required" in missing.stderr

    environment, _ = build_environment(tmp_path)
    environment["BUILDX_VERSION"] = "github.com/docker/buildx v0.35.0 commit"
    wrong = subprocess.run(
        ["./scripts/require-buildx.sh"],
        env=environment,
        check=False,
        capture_output=True,
        text=True,
    )
    assert wrong.returncode == 2
    assert "found v0.35.0" in wrong.stderr


def test_build_image_loads_and_verifies_iid(tmp_path: Path) -> None:
    environment, log = build_environment(tmp_path)
    result = subprocess.run(
        ["./scripts/build-image.sh", "example:test", "-f", "Dockerfile.execution-worker"],
        env=environment,
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    invocation = log.read_text()
    assert "buildx build --load --iidfile" in invocation
    assert "-t example:test -f Dockerfile.execution-worker ." in invocation
    assert "image inspect --format {{.Id}} example:test" in invocation


def test_build_image_rejects_loaded_iid_mismatch(tmp_path: Path) -> None:
    environment, _ = build_environment(tmp_path)
    environment["INSPECT_IID"] = "sha256:" + "b" * 64
    result = subprocess.run(
        ["./scripts/build-image.sh", "example:test", "-f", "Dockerfile.execution-worker"],
        env=environment,
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 1
    assert "loaded image ID mismatch" in result.stderr
