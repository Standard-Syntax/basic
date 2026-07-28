"""Generate committed Go and Python Protobuf transport bindings."""

import os
import shutil
import subprocess
import sys
from pathlib import Path

import grpc_tools
from grpc_tools import protoc

ROOT = Path(__file__).resolve().parents[1]
PROTO_ROOT = ROOT / "proto"
PROTO_FILES = sorted(PROTO_ROOT.rglob("*.proto"))
GO_OUT = ROOT / "go" / "gen"
PYTHON_OUT = ROOT / "python" / "src" / "harness_agents" / "_generated"
GO_PLUGIN = ROOT / ".tools" / "bin" / "protoc-gen-go"
WELL_KNOWN_TYPES = Path(grpc_tools.__file__).resolve().parent / "_proto"


def main() -> int:
    if not PROTO_FILES:
        raise SystemExit("no .proto files found")
    if not GO_PLUGIN.is_file():
        raise SystemExit(f"missing pinned Go plugin: {GO_PLUGIN}")
    go_executable = shutil.which("go")
    if go_executable is None:
        raise SystemExit("go executable not found")

    GO_OUT.mkdir(parents=True, exist_ok=True)
    PYTHON_OUT.mkdir(parents=True, exist_ok=True)
    for cache in PYTHON_OUT.rglob("__pycache__"):
        shutil.rmtree(cache)
    relative = [str(path.relative_to(PROTO_ROOT)) for path in PROTO_FILES]

    result = protoc.main(
        [
            "grpc_tools.protoc",
            f"-I{PROTO_ROOT}",
            f"-I{WELL_KNOWN_TYPES}",
            f"--plugin=protoc-gen-go={GO_PLUGIN}",
            f"--go_out={GO_OUT}",
            "--go_opt=paths=source_relative",
            f"--python_out={PYTHON_OUT}",
            *relative,
        ]
    )
    if result != 0:
        return result

    for generated in PYTHON_OUT.rglob("*_pb2.py"):
        content = generated.read_text(encoding="utf-8")
        content = content.replace(
            "from harness.reasoning.v1 import ",
            "from harness_agents._generated.harness.reasoning.v1 import ",
        )
        generated.write_text(content, encoding="utf-8", newline="\n")

    subprocess.run(
        [sys.executable, str(ROOT / "scripts" / "write_generated_inits.py")],
        check=True,
    )
    subprocess.run(
        [sys.executable, str(ROOT / "scripts" / "generate_contract_fixtures.py")],
        check=True,
        env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"},
    )
    subprocess.run(
        [go_executable, "run", "./internal/testfixtures"],
        check=True,
        cwd=ROOT / "go",
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
