# Agent Harness

This repository is the Phase 0–2 foundation for a kernel-driven agent harness.
Go is the trusted control-plane language; Python is limited to declarative agent
configuration. Reasoning outputs are untrusted, provider-neutral proposals.

The trusted Go kernel now makes transactional run and task lifecycle decisions
with append-only PostgreSQL events. Leases and artifact, execution,
verification, review, approval, and merge values are immutable references to
externally produced facts; this phase performs none of those side effects.

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
