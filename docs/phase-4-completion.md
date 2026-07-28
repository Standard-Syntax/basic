# Phase 4 completion report

## Delivered

Phase 4 implements the authoritative PostgreSQL-backed Go agent registry.
`go/internal/registry` exposes registration and exact lookup by agent identity
or manifest digest. It reuses `manifest.Read`; no second schema runtime or new
wire format was introduced.

Canonical manifest bytes, lowercase SHA-256 digest, embedded identity, and
registration timestamp are returned as one record. Exact registration replay
returns the original row with `Created=false`. A different manifest for an
existing name/version returns `ErrVersionConflict`.

The workflow and registry packages now share the advisory-lock and
digest-ledger migration mechanism. Registry migration `0006` adds unique
identity and digest constraints plus triggers that reject update and delete.

## Boundaries preserved

There is no HTTP/gRPC server, runtime service startup, provider adapter,
reasoning gateway, workflow integration, credential path, Python runtime
agent, or manifest format change. Phase 5 remains the fake reasoning gateway;
the first real provider remains Phase 10.

## Verification

Unit tests cover malformed manifest and lookup inputs, persisted-data
verification, canonical drift, and defensive byte ownership. The disposable
PostgreSQL suite covers initial registration, replay, replacement conflict,
both lookup paths, missing records, database immutability, concurrent
identical/conflicting calls, failed-registration rollback, corrupt rows,
migration replay, and migration digest protection.

The final acceptance gates are:

```text
make generate-check
make check
make integration-test
git diff --check
gh stack view --json
```
