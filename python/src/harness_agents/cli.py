"""Command-line manifest compiler."""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
from pathlib import Path
from typing import Any, cast

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


def _closed_object(
    value: object,
    name: str,
    *,
    required: frozenset[str],
    optional: frozenset[str] = frozenset(),
) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ManifestError(f"{name} must be an object")
    if not all(isinstance(key, str) for key in value):
        raise ManifestError(f"{name} fields must be strings")
    normalized = cast(dict[str, Any], value)
    fields = set(normalized)
    missing = sorted(required - fields)
    if missing:
        raise ManifestError(f"{name} missing required fields: {missing!r}")
    extra = sorted(fields - required - optional)
    if extra:
        raise ManifestError(f"unsupported {name} fields: {extra!r}")
    return normalized


def _reject_duplicate_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            raise ManifestError(f"duplicate JSON field: {key!r}")
        value[key] = item
    return value


def _load_json(path: Path, name: str) -> object:
    try:
        return json.loads(
            path.read_text(encoding="utf-8"), object_pairs_hook=_reject_duplicate_fields
        )
    except UnicodeDecodeError as error:
        raise ManifestError(f"{name} must be valid UTF-8") from error


def _definition(value: object, base: Path) -> AgentDefinition:
    root = _closed_object(
        value,
        "definition",
        required=frozenset(
            {"name", "version", "stage", "prompt_file", "model", "context", "tools", "output"}
        ),
        optional=frozenset({"metadata"}),
    )
    try:
        model = _closed_object(
            root["model"],
            "model",
            required=frozenset({"capability_class", "temperature", "maximum_output_tokens"}),
        )
        context = _closed_object(
            root["context"],
            "context",
            required=frozenset(
                {
                    "include_specification",
                    "include_task",
                    "repository_selection",
                    "maximum_context_tokens",
                }
            ),
        )
        tools = _closed_object(
            root["tools"],
            "tools",
            required=frozenset({"allowed_requests"}),
            optional=frozenset({"arbitrary_shell", "arbitrary_network", "direct_file_write"}),
        )
        output = _closed_object(root["output"], "output", required=frozenset({"schema"}))
        metadata = _closed_object(
            root.get("metadata", {}),
            "metadata",
            required=frozenset(),
            optional=frozenset({"description", "labels"}),
        )
        prompt_path = base / root["prompt_file"]
        return AgentDefinition(
            name=root["name"],
            version=root["version"],
            stage=root["stage"],
            prompt=PromptTemplate.from_file(prompt_path),
            model=ModelPolicy(**model),
            context=ContextPolicy(**context),
            tools=ToolRequestPolicy(
                allowed_requests=frozenset(tools["allowed_requests"]),
                arbitrary_shell=tools.get("arbitrary_shell", False),
                arbitrary_network=tools.get("arbitrary_network", False),
                direct_file_write=tools.get("direct_file_write", False),
            ),
            output=OutputContract(**output),
            metadata=ConfigurationMetadata(
                description=metadata.get("description", ""),
                labels=frozenset(metadata.get("labels", [])),
            ),
        )
    except (KeyError, TypeError, OSError) as error:
        raise ManifestError(f"invalid definition: {error}") from error


def _write_outputs(output: Path, manifest: bytes, digest_output: Path, digest: str) -> None:
    if output.resolve() == digest_output.resolve():
        raise ManifestError("manifest and digest output paths must be different")
    temporary_paths: list[Path] = []
    try:
        payloads = ((output, manifest + b"\n"), (digest_output, (digest + "\n").encode("ascii")))
        for destination, payload in payloads:
            with tempfile.NamedTemporaryFile(
                mode="wb",
                dir=destination.parent,
                prefix=f".{destination.name}.",
                suffix=".tmp",
                delete=False,
            ) as temporary:
                temporary.write(payload)
                temporary.flush()
                os.fsync(temporary.fileno())
                temporary_paths.append(Path(temporary.name))
        manifest_temporary, digest_temporary = temporary_paths
        os.replace(manifest_temporary, output)
        temporary_paths.remove(manifest_temporary)
        os.replace(digest_temporary, digest_output)
        temporary_paths.remove(digest_temporary)
    finally:
        for path in temporary_paths:
            path.unlink(missing_ok=True)


def compile_command(args: argparse.Namespace) -> int:
    definition_path = args.definition.resolve()
    try:
        value = _load_json(definition_path, "definition")
        schema = None
        if args.schema is not None:
            schema_value = _load_json(args.schema, "schema")
            if not isinstance(schema_value, dict):
                raise ManifestError("schema must be a JSON object")
            schema = cast(dict[str, Any], schema_value)
        compiled = _definition(value, definition_path.parent).compile(schema)
        _write_outputs(args.output, compiled.canonical_bytes, args.digest_output, compiled.digest)
    except (json.JSONDecodeError, OSError, ManifestError) as error:
        print(f"harness-agents: {error}", file=sys.stderr)
        return 2
    return 0


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(prog="harness-agents")
    commands = root.add_subparsers(required=True)
    compile_parser = commands.add_parser("compile")
    compile_parser.add_argument("definition", type=Path)
    compile_parser.add_argument("--schema", type=Path)
    compile_parser.add_argument("--output", type=Path, required=True)
    compile_parser.add_argument("--digest-output", type=Path, required=True)
    compile_parser.set_defaults(handler=compile_command)
    return root


def main() -> int:
    args = parser().parse_args()
    return args.handler(args)


if __name__ == "__main__":
    raise SystemExit(main())
