# Agent Harness

This repository is the Phase 0–5 foundation for a kernel-driven agent harness.
Go is the trusted control-plane language; Python is limited to declarative agent
configuration. Reasoning outputs are untrusted, provider-neutral proposals.

The trusted Go kernel now makes transactional run and task lifecycle decisions
with append-only PostgreSQL events. Leases and artifact, execution,
verification, review, approval, and merge values are immutable references to
externally produced facts; this phase performs none of those side effects.

The installed Python SDK compiles offline definitions for all four reasoning
stages into schema-validated RFC 8785 manifests and SHA-256 sidecars. The Go
reader independently enforces the same v1 stage/output and safety policy.

The in-process Go reasoning gateway now executes only the implementation stage
through a deterministic fake adapter. It resolves the exact registered
manifest digest, validates proposals into five stable rejection codes, stores
payloads through a content-addressed artifact interface, and records immutable
PostgreSQL invocation metadata. It starts no listener and calls no model.

## Quick start

Prerequisites: Go 1.26, Python 3.14, `uv` 0.11 or newer, GNU Make, and Docker
for PostgreSQL integration tests.

```bash
uv sync --frozen
make check
make integration-test
```

See [docs/development.md](docs/development.md) for exact commands and
[docs/architecture.md](docs/architecture.md) for the trust boundaries.
