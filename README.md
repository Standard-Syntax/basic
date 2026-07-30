# Agent Harness

This repository contains the Phase 0–10 kernel and the Phase 11 durable-runtime
foundation for a kernel-driven agent harness.
Go is the trusted control-plane language; Python is limited to declarative agent
configuration. Reasoning outputs are untrusted, provider-neutral proposals.
Phase 11 adds a loopback authenticated HTTP API, a durable PostgreSQL
reconciler, and a filesystem SHA-256 content-addressed store. Full in-process
stage composition and the full lifecycle process E2E remain pending.

The trusted Go kernel now makes transactional run and task lifecycle decisions
with append-only PostgreSQL events. Leases and artifact, execution,
verification, review, approval, and merge values are immutable references.
Phase 6 performs proposal-authorized complete-file application and
candidate-commit creation. Phase 7 independently materializes that exact
candidate, executes only kernel-approved checks, records bounded evidence, and
calculates acceptance coverage. Phase 9 publishes the approved candidate to
one immutable branch and creates one recoverable GitHub draft pull request.
Merge and deployment remain external.

The installed Python SDK compiles offline definitions for all four reasoning
stages into schema-validated RFC 8785 manifests and SHA-256 sidecars. The Go
reader independently enforces the same v1 stage/output and safety policy.

The in-process Go reasoning gateway executes implementation and review stages
through deterministic fake adapters. It resolves exact registered manifest
digests, validates proposals into five stable rejection codes, stores payloads
through a content-addressed artifact interface, and records immutable
PostgreSQL invocation metadata. It starts no listener and calls no model.

The Go execution library revalidates accepted proposals, fences them to the
active lease, applies them in a network-disabled unprivileged container, creates
a verified candidate commit with hook/filter-free Git plumbing, and records a
content-addressed execution report through an immutable PostgreSQL ledger.

The in-process verification library validates the execution report and exact
candidate binding, resolves `make-check-v1` from an immutable Go catalog, runs
`make check` offline in a separate locked-down image, and records a
content-addressed verification report through a recoverable immutable ledger.

The trusted review library reconstructs that diff and evidence, records
blocking rework or advisory acceptance, and never approves work. The separate
human approval library enforces standard/elevated roles, writes an immutable
approval artifact, and alone may move a reviewed task to `ACCEPTED`.

The in-process publication library revalidates the approved Phase 5–8 evidence,
requires the reviewed base to equal the configured remote base, publishes the
exact candidate to `harness/<run-id>` with an empty expected-value lease, and
creates one marked draft PR through a bounded REST client. Migration `0013`
checkpoints branch and PR identity so replay performs no Git or network write.

## Quick start

Prerequisites: Go 1.26, Python 3.14, `uv` 0.11 or newer, GNU Make, Git, and
Docker for PostgreSQL and isolated-worker integration tests.

```bash
uv sync --frozen
make check
make integration-test
```

See [docs/development.md](docs/development.md) for exact commands and
[docs/architecture.md](docs/architecture.md) for the trust boundaries.
