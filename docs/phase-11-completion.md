# Phase 11 implementation status

## Status

Phase 11 is not complete. The durable runtime foundation is implemented and
validated, but the workflow process still uses deterministic checkpoint
handlers rather than composing the existing reasoning, execution,
verification, review, approval, and publication services.

## Outcome

Phase 11 adds two runnable processes: an authenticated loopback HTTP API and a
PostgreSQL-backed workflow worker. Durable content uses a local SHA-256 CAS.
Runtime jobs have
deterministic identities, expiry takeover, fencing, bounded retries, terminal
failure evidence, and cancellation checkpoints.

The API exposes run intake and queries, specification and task-graph decisions,
task retry, pending-approval discovery, composite run approval/rejection, and
cancellation. Mutations use strict bounded JSON, UUID idempotency keys, roles,
and revision preconditions.

## Remaining required work

- Replace checkpoint-only stage handlers with the real fake-by-default
  reasoning, execution-container, verification-container, review, approval,
  and optional publication composition.
- Bind API intake, approved specification, approved task, and composite
  approval artifacts through the runtime binding tables.
- Replace the package-level `runtime-e2e` target with the specified full
  process lifecycle, restart-boundary, bare-remote, and loopback GitHub API
  test.
- Wire configured draft publication after composite approval. The current API
  correctly reports publication as `configuration_blocked`.

## Verification completed

```text
uv sync --frozen
make generate-check
make check
make integration-test
make runtime-e2e
git diff --check
gh stack view --json
```

The listed gates pass for the implemented foundation. `make provider-smoke`
remains separate and credential-gated.

## Boundaries preserved

The runtime supports one dependency-free task per run. Fake reasoning remains
the default and Anthropic remains opt-in. The CAS is a one-node backend.
Publication is draft-only and configuration-gated. There is no automatic
merge, deployment, provider fallback, arbitrary shell, unrestricted network,
cross-repository operation, or live-GitHub canonical test.
