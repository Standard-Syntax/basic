"""Typed, declarative agent definitions and immutable manifest compilation."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass, field
from importlib import resources
from pathlib import Path
from typing import Any, ClassVar

import rfc8785
from jsonschema import Draft202012Validator

SCHEMA_ID = "urn:agent-harness:schema:agent-manifest:v1"
SCHEMA_VERSION = "1"
STAGES = frozenset({"specification", "planning", "implementation", "review"})
CAPABILITY_CLASSES = frozenset({"general_reasoning", "strong_coding", "independent_review"})
TOOL_REQUESTS = frozenset(
    {
        "read_repository_file",
        "search_repository",
        "request_declared_check",
        "report_blocker",
    }
)
OUTPUT_SCHEMAS = frozenset(
    {
        "specification_proposal.v1",
        "task_graph_proposal.v1",
        "implementation_proposal.v1",
        "review_proposal.v1",
    }
)
STAGE_OUTPUT_SCHEMAS = {
    "specification": "specification_proposal.v1",
    "planning": "task_graph_proposal.v1",
    "implementation": "implementation_proposal.v1",
    "review": "review_proposal.v1",
}


class ManifestError(ValueError):
    """A deterministic manifest-definition or compilation failure."""


@dataclass(frozen=True)
class PromptTemplate:
    content: bytes

    @classmethod
    def from_file(cls, path: str | Path) -> PromptTemplate:
        return cls(Path(path).read_bytes())

    @classmethod
    def from_text(cls, text: str) -> PromptTemplate:
        return cls(text.encode("utf-8"))

    def digest(self) -> str:
        return hashlib.sha256(self.content).hexdigest()

    def validate(self) -> None:
        try:
            self.content.decode("utf-8")
        except UnicodeDecodeError as error:
            raise ManifestError("prompt content must be valid UTF-8") from error


@dataclass(frozen=True)
class ModelPolicy:
    capability_class: str
    temperature: float
    maximum_output_tokens: int

    def validate(self) -> None:
        if self.capability_class not in CAPABILITY_CLASSES:
            raise ManifestError(f"unsupported model capability class: {self.capability_class}")
        if not math.isfinite(self.temperature) or not 0 <= self.temperature <= 2:
            raise ManifestError("temperature must be finite and between 0 and 2")
        if isinstance(self.maximum_output_tokens, bool) or not (
            1 <= self.maximum_output_tokens <= 200_000
        ):
            raise ManifestError("maximum_output_tokens must be between 1 and 200000")


@dataclass(frozen=True)
class ContextPolicy:
    include_specification: bool
    include_task: bool
    repository_selection: str
    maximum_context_tokens: int

    def validate(self) -> None:
        if self.repository_selection != "kernel_selected":
            raise ManifestError("repository_selection must be kernel_selected")
        if isinstance(self.maximum_context_tokens, bool) or not (
            1 <= self.maximum_context_tokens <= 1_000_000
        ):
            raise ManifestError("maximum_context_tokens must be between 1 and 1000000")


@dataclass(frozen=True)
class ToolRequestPolicy:
    allowed_requests: frozenset[str]
    arbitrary_shell: bool = False
    arbitrary_network: bool = False
    direct_file_write: bool = False

    def validate(self) -> None:
        unsafe = self.arbitrary_shell or self.arbitrary_network or self.direct_file_write
        if unsafe:
            raise ManifestError("shell, network, and direct file write must all be false")
        unsupported = self.allowed_requests - TOOL_REQUESTS
        if unsupported:
            raise ManifestError(f"unsupported tool requests: {sorted(unsupported)!r}")


@dataclass(frozen=True)
class OutputContract:
    schema: str

    def validate(self) -> None:
        if self.schema not in OUTPUT_SCHEMAS:
            raise ManifestError(f"unsupported output schema: {self.schema}")


@dataclass(frozen=True)
class ConfigurationMetadata:
    description: str = ""
    labels: frozenset[str] = field(default_factory=frozenset)


@dataclass(frozen=True)
class CompiledManifest:
    canonical_bytes: bytes
    digest: str
    value: dict[str, Any]


@dataclass(frozen=True)
class AgentDefinition:
    name: str
    version: str
    stage: str
    prompt: PromptTemplate
    model: ModelPolicy
    context: ContextPolicy
    tools: ToolRequestPolicy
    output: OutputContract
    metadata: ConfigurationMetadata = field(default_factory=ConfigurationMetadata)

    _schema: ClassVar[dict[str, Any] | None] = None

    def compile(self, schema: dict[str, Any] | None = None) -> CompiledManifest:
        if self.stage not in STAGES:
            raise ManifestError(f"unsupported stage: {self.stage}")
        self.prompt.validate()
        self.model.validate()
        self.context.validate()
        self.tools.validate()
        self.output.validate()
        expected_output = STAGE_OUTPUT_SCHEMAS[self.stage]
        if self.output.schema != expected_output:
            raise ManifestError(f"stage {self.stage!r} requires output schema {expected_output!r}")
        prompt_digest = self.prompt.digest()
        value: dict[str, Any] = {
            "schema_version": SCHEMA_VERSION,
            "agent": {"name": self.name, "version": self.version},
            "stage": self.stage,
            "prompt": {
                "artifact_uri": f"artifact://sha256/{prompt_digest}",
                "sha256": prompt_digest,
            },
            "model": {
                "capability_class": self.model.capability_class,
                "temperature": self.model.temperature,
                "maximum_output_tokens": self.model.maximum_output_tokens,
            },
            "context": {
                "include_specification": self.context.include_specification,
                "include_task": self.context.include_task,
                "repository_selection": self.context.repository_selection,
                "maximum_context_tokens": self.context.maximum_context_tokens,
            },
            "tools": {
                "allowed_requests": sorted(self.tools.allowed_requests),
                "arbitrary_shell": self.tools.arbitrary_shell,
                "arbitrary_network": self.tools.arbitrary_network,
                "direct_file_write": self.tools.direct_file_write,
            },
            "output": {"schema": self.output.schema},
            "metadata": {
                "description": self.metadata.description,
                "labels": sorted(self.metadata.labels),
            },
        }
        schemas = [self._packaged_schema()]
        if schema is not None:
            schemas.append(schema)
        for validation_schema in schemas:
            errors = sorted(
                Draft202012Validator(validation_schema).iter_errors(value),
                key=lambda e: e.json_path,
            )
            if errors:
                first = errors[0]
                raise ManifestError(f"{first.json_path}: {first.message}")
        try:
            canonical_bytes = rfc8785.dumps(value)
        except (rfc8785.CanonicalizationError, UnicodeError, TypeError) as error:
            raise ManifestError(f"canonicalization failed: {error}") from error
        return CompiledManifest(
            canonical_bytes=canonical_bytes,
            digest=hashlib.sha256(canonical_bytes).hexdigest(),
            value=value,
        )

    @classmethod
    def _packaged_schema(cls) -> dict[str, Any]:
        if cls._schema is None:
            schema_resource = resources.files("harness_agents").joinpath(
                "schemas", "agent-manifest-v1.schema.json"
            )
            try:
                loaded = json.loads(schema_resource.read_text(encoding="utf-8"))
            except (json.JSONDecodeError, OSError, UnicodeError) as error:
                raise ManifestError(f"cannot load packaged manifest schema: {error}") from error
            if not isinstance(loaded, dict):
                raise ManifestError("packaged manifest schema must be a JSON object")
            Draft202012Validator.check_schema(loaded)
            cls._schema = loaded
        return cls._schema
