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

## DEC-005: PostgreSQL is planned for authoritative metadata

### Decision
Use PostgreSQL for future authoritative workflow metadata and external artifact
storage for large immutable content.
### Options considered
Files only; embedded database; PostgreSQL plus artifact storage.
### Pros
Transactional state/event updates and operational maturity.
### Cons
Future infrastructure and migrations.
### Why this option
Atomic state plus append-only event requirements need transactional storage.
### Consequences
No database runtime or migration is added in Phase 0–1.
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
Prove kernel boundaries with a future fake adapter before adding a provider.
### Options considered
Provider first; fake adapter first.
### Pros
Deterministic boundary tests without credentials.
### Cons
Provider behavior is not exercised initially.
### Why this option
Authority and validation must be provider-independent.
### Consequences
Phase 0–1 contains no fake adapter or real provider runtime.
### Date
2026-07-27
