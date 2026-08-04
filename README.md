# Agent Harness

This repository contains the Phase 0–10 kernel and the Phase 12 executable
first-slice runtime for a kernel-driven agent harness.
Go is the trusted control-plane language; Python owns declarative agent
configuration and a deterministic operator-invoked project bootstrap. Reasoning
outputs are untrusted, provider-neutral proposals.
The runtime adds a loopback authenticated HTTP API, a durable PostgreSQL
reconciler, a filesystem SHA-256 content-addressed store, and a closed
live-only MiniMax-M2.7 provider configuration.

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

`harness-agents init` creates a new Python package only from an explicit strict
project specification and operator-supplied trusted Python checks. It commits
the checks, fixed `make check` entry point, lockfile, and path policy before any
model call; an existing destination or symlinked check fails closed.

The in-process Go reasoning gateway executes specification, planning,
implementation, and review stages only through the runtime's closed
MiniMax-M2.7 Anthropic-compatible adapter.
It resolves exact registered manifest digests, validates proposals into five
stable rejection codes, stores payloads through a content-addressed artifact
interface, and records immutable PostgreSQL invocation metadata. The gateway
starts no listener of its own. No shipped fake proposal adapter or alternate
workflow-service provider branch remains.

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

Prerequisites: Go 1.26, Python 3.14, `uv` 0.11 or newer, GNU Make, Git 2.38 or newer,
Docker for PostgreSQL and isolated-worker integration tests, and Docker Buildx
exactly v0.36.0. Image targets fail before building when that pinned plugin is
missing or mismatched.

```bash
uv sync --frozen
make check
make integration-test
```

See [docs/development.md](docs/development.md) for exact commands and
[docs/architecture.md](docs/architecture.md) for the trust boundaries.
For the current beta provider profile and process-level live acceptance status,
see [docs/live-harness.md](docs/live-harness.md).

The controlled beta cut, human decision, and durable evidence verification
procedure is in [docs/beta-release.md](docs/beta-release.md).

Production repository readiness is checked without mutation with
`make beta-preflight BETA_CONFIG=/absolute/path/to/beta-preflight.json`. The
same immutable `beta_policy` is consumed by both services. The beta supports
exactly one dependency-free task per run.

After trusted Python bootstrap, `harness-agents operator` submits only trusted
specification intake, waits for live specification and one-task planning
proposals, drives the three explicit approval gates, separate draft submission, status reads,
and redacted support export against a configured loopback control API. See the
development guide for the strict configuration and command sequence.
`make beta-python-project-e2e` is the credentialed acceptance gate for a newly
generated Python target and uses only disposable Git and publication endpoints.
With no parameters it runs the reproducible golden project. Operators may instead
provide both `PROJECT_SPEC=/clean/absolute/spec.json` and
`CHECKS=/clean/absolute/checks`. The target writes a redacted mode-`0600` report
to `.tools/evidence/python-project-report.json` by default; `REPORT_OUTPUT` selects
another clean absolute destination, and `PRESERVE_PROJECT` selects a clean,
initially nonexistent destination for the scanned generated repository.
