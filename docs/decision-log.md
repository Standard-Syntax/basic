# Decision log

## DEC-001: Go microservices form the trusted kernel

### Decision
Use separately deployable Go command boundaries for trusted control-plane work.
### Options considered
One mixed-language process; Python control plane; Go service boundaries.
### Pros
Static types, explicit ownership, and a narrow authority boundary.
### Cons
More binaries and contract mapping.
### Why this option
Only the Go kernel may authorize side effects or state transitions.
### Consequences
Python and generated transports remain outside domain authority.
### Date
2026-07-27

## DEC-002: Python compiles to canonical JSON

### Decision
Use typed Python only to compile offline agent definitions to RFC 8785 JSON.
### Options considered
Runtime Python agents; ordinary sorted JSON; RFC 8785 canonical JSON.
### Pros
Deterministic bytes and digest-addressed configuration.
### Cons
Authors cannot rely on Python runtime behavior.
### Why this option
It makes configuration immutable without granting runtime authority.
### Consequences
The manifest compiler must reject non-canonical and unsafe values.
### Date
2026-07-27

## DEC-003: Protobuf defines internal contracts

### Decision
Use versioned Protobuf messages for provider-neutral reasoning transports.
### Options considered
Go-only structs; JSON transports; Protobuf.
### Pros
Stable field numbers and cross-language generation.
### Cons
Pinned code generation is required.
### Why this option
Go and Python must agree on typed contracts without sharing domain logic.
### Consequences
Generated bindings are committed and reproducibility-checked.
### Date
2026-07-27

## DEC-004: Complete-file replacement is the first patch format

### Decision
Represent changes as create, update, or delete of complete files.
### Options considered
Unified diffs; scripts; complete-file operations.
### Pros
Simple structural validation and digest preconditions.
### Cons
Large files are less efficient.
### Why this option
KISS and fail-closed scope checks outweigh patch compactness initially.
### Consequences
Arbitrary shell and model-generated scripts remain prohibited.
### Date
2026-07-27

## DEC-005: PostgreSQL stores authoritative metadata

### Decision
Use PostgreSQL for authoritative workflow metadata and external artifact
storage for large immutable content.
### Options considered
Files only; embedded database; PostgreSQL plus artifact storage.
### Pros
Transactional state/event updates and operational maturity.
### Cons
Database infrastructure and migrations are required.
### Why this option
Atomic state plus append-only event requirements need transactional storage.
### Consequences
Phase 2 uses pgx/v5, embedded forward-only migrations, an advisory migration
lock, serializable command transactions, and external artifact bodies.
### Date
2026-07-27

## DEC-006: Verification is clean and independent

### Decision
Future verification runs against the exact candidate in a clean environment.
### Options considered
Trust model claims; reuse implementation workspace; independent verification.
### Pros
Evidence is isolated and commit-bound.
### Cons
More runtime cost.
### Why this option
Proposal claims cannot be authoritative evidence.
### Consequences
Verification execution is deferred but its contract preserves independence.
### Date
2026-07-27

## DEC-007: No automatic merge or deployment

### Decision
Require explicit human authority beyond advisory review recommendations.
### Options considered
Automatic merge; risk-based auto merge; human-gated draft publication.
### Pros
Prevents model or reviewer recommendations from becoming approval.
### Cons
Human latency.
### Why this option
The first release requires a hard human approval boundary.
### Consequences
No merge or deployment field exists in reasoning contracts.
### Date
2026-07-27

## DEC-008: Fake reasoning precedes a real provider

### Decision
Prove kernel boundaries with a deterministic fake adapter before adding a
provider.
### Options considered
Provider first; fake adapter first.
### Pros
Deterministic boundary tests without credentials.
### Cons
Provider behavior is not exercised initially.
### Why this option
Authority and validation must be provider-independent.
### Consequences
Phase 5 implements one in-process fake implementation adapter with no real
provider, credentials, network transport, command execution, or repository
mutation.
### Date
2026-07-28

## DEC-009: Commands are idempotent transactional decisions

### Decision
Bind every command ID to a deterministic request digest and persist its result
with snapshot and event changes in one serializable transaction.
### Options considered
At-least-once mutation; event-only deduplication; command ledger plus result.
### Pros
Safe replay, conflict detection, ordered events, and atomic recovery.
### Cons
Command retention and deterministic request encoding are required.
### Why this option
Retries must not repeat a transition or accept different content silently.
### Consequences
Exact replay returns the recorded result; conflicting reuse fails closed, and
pre-commit errors leave no command, event, or state change.
### Date
2026-07-27

## DEC-010: Agent versions are immutable canonical records

### Decision
Store each validated RFC 8785 manifest once under its exact agent name and
semantic version, with a unique lowercase SHA-256 digest.
### Options considered
Mutable latest-version rows; application-only immutability; immutable
database-enforced versions.
### Pros
Requests can bind to stable bytes, retries converge, and conflicting
replacement fails closed.
### Cons
Corrections require a new semantic version and canonical manifests consume
database storage.
### Why this option
Agent identity is authority-bearing evidence and must not change underneath a
workflow request.
### Consequences
Registration is serialized per identity, exact replay is idempotent, update
and deletion are rejected by trigger, and every lookup revalidates persisted
bytes before returning.
### Date
2026-07-28

## DEC-011: Reasoning replay binds immutable metadata to external payloads

### Decision
Store complete reasoning request and proposal bodies through a
content-addressed artifact port while PostgreSQL stores immutable identity,
artifact references, adapter metadata, usage, final status, and rejection
metadata.
### Options considered
Payloads in PostgreSQL; application-only replay; external artifacts plus an
immutable invocation ledger.
### Pros
Exact replay, bounded database rows, integrity verification, and no duplicate
adapter call under concurrency.
### Cons
Replay depends on artifact availability and requires a request-scoped
reservation row while the adapter is running.
### Why this option
Reasoning payloads are immutable evidence but are not authoritative workflow
state and should not expand the metadata database.
### Consequences
Request IDs bind to deterministic bytes, completed rows reject mutation, and
missing or corrupt artifacts fail replay rather than silently re-invoking the
adapter.
### Date
2026-07-28
