# Phase 5 completion report

## Delivered

Phase 5 implements the fake implementation-reasoning gateway as a five-branch
stack rooted at refreshed `main`.

| Branch | Scope |
|---|---|
| `phase05/implementation-validation` | typed five-code validation and deterministic first-failure ordering |
| `phase05/fake-implementation` | exact manifest resolution, in-process service, and deterministic fake adapter |
| `phase05/invocation-ledger` | content-addressed artifact ports, migration `0007`, immutable metadata, and durable replay |
| `phase05/gateway-hardening` | limits, concurrency, rollback, corruption, migration, rejection, and side-effect tests |
| `phase05/completion-docs` | architecture, contracts, security, resources, runbook, decisions, and completion evidence |

`Service.ProposeImplementation` accepts the published v1 Protobuf request and
returns exactly one validated proposal or one structured rejection. Validation
uses schema, identity/expiry, artifact/manifest binding, authority, scope,
coverage, and stage-policy order. Scope-change requests are retained only as
advisory data.

The fake adapter copies proposal content from a configured template while
binding all request-owned identities. It uses one deterministic provider
request counter and performs no execution. The gateway requires the exact
registered manifest digest and the closed
`implementation`/`implementation_proposal.v1` pairing.

Request and proposal bodies pass through `ArtifactStore.Put/Get`. PostgreSQL
stores their URI/digest references, invocation identity, stage, attempt,
manifest digest, fake-adapter metadata, timestamps, usage, final status, and
rejection metadata. It never stores complete payload bodies. Exact request
replay returns the original immutable outcome without another adapter call;
different bytes under the same request ID return `ErrInvocationConflict`.
The repository commits a narrow in-progress reservation before adapter work
and finalizes it in a separate transaction, so model or artifact latency does
not pin a pooled connection. Failed infrastructure work removes only the
unfinished reservation; completed outcomes remain immutable.

## Boundaries preserved

Phase 5 adds no Protobuf or manifest-v1 change, gRPC/HTTP server, real provider,
credential path, workflow transition, repository mutation, command execution,
verification runner, Python runtime behavior, filesystem/object-store backend,
pull-request operation, merge, or deployment. The
`go/cmd/reasoning-gateway` binary remains buildable and non-networked.

The only permitted durable effects are immutable request/proposal artifacts in
the supplied store and one immutable reasoning audit row. PostgreSQL acceptance
tests prove all five rejection paths leave workflow rows absent, and a
repository digest snapshot proves rejected proposals do not modify files.

## Verification

Every functional branch passed:

```text
make check
git diff --check
```

The persistence, hardening, and completion branches also passed:

```text
make integration-test
```

Final acceptance:

```text
make generate-check
make check
make integration-test
git diff --check
gh stack view --json
```

The tests cover deterministic valid proposals; all five exact rejection codes
and multi-fault ordering; durable replay without a second adapter call;
concurrent identical and conflicting IDs; immutable invocation rows; 1 MiB
defaults; artifact, adapter, and database failures; rollback; corrupt and
missing artifacts; expiry; migration replay and digest protection; and absence
of workflow or repository side effects.

## Assumptions and deviations

The artifact interface is intentionally backend-neutral and has no production
implementation. Tests use an integrity-checking in-memory store. Payload-size
rejections that occur before a safe request identity exists cannot be recorded
in PostgreSQL; they still return the typed policy outcome without invoking a
dependency. There are no deviations from the published Protobuf, enum,
field-number, JSON Schema, canonicalization, or manifest-digest contracts.

## Phase 6 recommendation

Phase 6 should consume the validated proposal through a separate execution
boundary that stages complete-file operations in an isolated worktree. It
should normalize paths, reject traversal and symlink escapes, verify original
digests, enforce file/count/content limits, inspect the actual diff, and bind
execution evidence to an exact candidate commit. It must not grant the
reasoning gateway workflow-transition or approval authority.
