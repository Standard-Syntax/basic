"""Command-line manifest compiler."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from harness_agents.manifest import (
    AgentDefinition,
    ConfigurationMetadata,
    ContextPolicy,
    ManifestError,
    ModelPolicy,
    OutputContract,
    PromptTemplate,
    ToolRequestPolicy,
)

ROOT = Path(__file__).resolve().parents[3]
DEFAULT_SCHEMA = ROOT / "schemas" / "agent-manifest-v1.schema.json"


def _definition(value: dict[str, Any], base: Path) -> AgentDefinition:
    allowed = {
        "name",
        "version",
        "stage",
        "prompt_file",
        "model",
        "context",
        "tools",
        "output",
        "metadata",
    }
    extra = sorted(set(value) - allowed)
    if extra:
        raise ManifestError(f"unsupported definition fields: {extra!r}")
    try:
        prompt_path = base / value["prompt_file"]
        tools = value["tools"]
        metadata = value.get("metadata", {})
        return AgentDefinition(
            name=value["name"],
            version=value["version"],
            stage=value["stage"],
            prompt=PromptTemplate.from_file(prompt_path),
            model=ModelPolicy(**value["model"]),
            context=ContextPolicy(**value["context"]),
            tools=ToolRequestPolicy(
                allowed_requests=frozenset(tools["allowed_requests"]),
                arbitrary_shell=tools.get("arbitrary_shell", False),
                arbitrary_network=tools.get("arbitrary_network", False),
                direct_file_write=tools.get("direct_file_write", False),
            ),
            output=OutputContract(**value["output"]),
            metadata=ConfigurationMetadata(
                description=metadata.get("description", ""),
                labels=frozenset(metadata.get("labels", [])),
            ),
        )
    except (KeyError, TypeError, OSError) as error:
        raise ManifestError(f"invalid definition: {error}") from error


def compile_command(args: argparse.Namespace) -> int:
    definition_path = args.definition.resolve()
    try:
        value = json.loads(definition_path.read_text(encoding="utf-8"))
        schema = json.loads(args.schema.read_text(encoding="utf-8"))
        compiled = _definition(value, definition_path.parent).compile(schema)
        args.output.write_bytes(compiled.canonical_bytes + b"\n")
        args.digest_output.write_text(compiled.digest + "\n", encoding="ascii")
    except (json.JSONDecodeError, OSError, ManifestError) as error:
        print(f"harness-agents: {error}", file=sys.stderr)
        return 2
    return 0


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(prog="harness-agents")
    commands = root.add_subparsers(required=True)
    compile_parser = commands.add_parser("compile")
    compile_parser.add_argument("definition", type=Path)
    compile_parser.add_argument("--schema", type=Path, default=DEFAULT_SCHEMA)
    compile_parser.add_argument("--output", type=Path, required=True)
    compile_parser.add_argument("--digest-output", type=Path, required=True)
    compile_parser.set_defaults(handler=compile_command)
    return root


def main() -> int:
    args = parser().parse_args()
    return args.handler(args)


if __name__ == "__main__":
    raise SystemExit(main())
