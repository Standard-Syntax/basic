"""Generate committed Go and Python Protobuf transport bindings."""

import shutil
import subprocess
import sys
from pathlib import Path

from grpc_tools import protoc

ROOT = Path(__file__).resolve().parents[1]
PROTO_ROOT = ROOT / "proto"
PROTO_FILES = sorted(PROTO_ROOT.rglob("*.proto"))
GO_OUT = ROOT / "go" / "gen"
PYTHON_OUT = ROOT / "python" / "src" / "harness_agents" / "_generated"
GO_PLUGIN = ROOT / ".tools" / "bin" / "protoc-gen-go"


def main() -> int:
    if not PROTO_FILES:
        raise SystemExit("no .proto files found")
    if not GO_PLUGIN.is_file():
        raise SystemExit(f"missing pinned Go plugin: {GO_PLUGIN}")

    GO_OUT.mkdir(parents=True, exist_ok=True)
    PYTHON_OUT.mkdir(parents=True, exist_ok=True)
    for cache in PYTHON_OUT.rglob("__pycache__"):
        shutil.rmtree(cache)
    relative = [str(path.relative_to(PROTO_ROOT)) for path in PROTO_FILES]

    result = protoc.main(
        [
            "grpc_tools.protoc",
            f"-I{PROTO_ROOT}",
            f"--plugin=protoc-gen-go={GO_PLUGIN}",
            f"--go_out={GO_OUT}",
            "--go_opt=paths=source_relative",
            f"--python_out={PYTHON_OUT}",
            *relative,
        ]
    )
    if result != 0:
        return result

    subprocess.run(
        [sys.executable, str(ROOT / "scripts" / "write_generated_inits.py")],
        check=True,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
