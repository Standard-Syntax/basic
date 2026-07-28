import json
import subprocess
import sys
from pathlib import Path


def test_cli_compiles_golden_manifest(tmp_path: Path) -> None:
    root = Path(__file__).resolve().parents[2]
    output = tmp_path / "manifest.json"
    digest = tmp_path / "manifest.sha256"
    result = subprocess.run(
        [
            sys.executable,
            "-m",
            "harness_agents.cli",
            "compile",
            str(root / "python/agents/implementation.json"),
            "--output",
            str(output),
            "--digest-output",
            str(digest),
        ],
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    assert json.loads(output.read_bytes())
    assert len(digest.read_text().strip()) == 64
    assert (
        output.read_bytes()
        == (root / "tests/contracts/v1/manifest/implementation.json").read_bytes()
    )
