# Agent Harness

This repository is the Phase 0–1 foundation for a kernel-driven agent harness.
Go is the trusted control-plane language; Python is limited to declarative agent
configuration. Reasoning outputs are untrusted, provider-neutral proposals.

The implemented scope is intentionally limited to build infrastructure, agent
manifest compilation, and shared reasoning contracts. Workflow execution,
databases, worktrees, providers, services, publication, merge, and deployment
are deferred.

## Quick start

Prerequisites: Go 1.26, Python 3.14, `uv` 0.11 or newer, and GNU Make.

```bash
uv sync --frozen
make check
```

See [docs/development.md](docs/development.md) for exact commands and
[docs/architecture.md](docs/architecture.md) for the trust boundaries.
