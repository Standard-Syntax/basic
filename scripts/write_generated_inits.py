"""Ensure generated Python package directories have deterministic markers."""

from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
GENERATED = ROOT / "python" / "src" / "harness_agents" / "_generated"
MARKER = '"""Generated Protobuf transport package. Do not edit."""\n'

for directory in [GENERATED, *sorted(path for path in GENERATED.rglob("*") if path.is_dir())]:
    (directory / "__init__.py").write_text(MARKER, encoding="utf-8", newline="\n")
