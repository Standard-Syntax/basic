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

## DEC-012: Task attempts own monotonically increasing lease fences

### Decision
Persist a positive fencing token equal to the incremented task attempt and
require the complete lease tuple on proposal acceptance and execution
recording.
### Options considered
Revision-only checks; lease ID checks; attempt-bound full-tuple fencing.
### Pros
Expired and superseded workers cannot publish a candidate after a newer lease.
### Cons
Every execution transition and persisted lease carries one additional value.
### Why this option
Revision checks alone do not prove that a long-running worker still owns the
attempt.
### Consequences
Migration `0008` backfills active leases from `current_attempt`; stale lease
commands fail without snapshot, event, or command-result mutation.
### Date
2026-07-28

## DEC-013: Complete-file application runs in a locked-down private container

### Decision
Run a static Go applicator as the host UID/GID with no network, capabilities,
or privilege escalation and with fixed CPU, memory, and PID limits.
### Options considered
Host filesystem writes; shell-generated patches; a private static applicator.
### Pros
No model shell surface, bounded resources, and descriptor-relative path
handling.
### Cons
Linux and Docker are runtime requirements for execution.
### Why this option
Proposal content is untrusted even after schema and scope validation.
### Consequences
Binary replacements and new executable files remain unsupported in v1.
### Date
2026-07-28

## DEC-014: Candidate commits use hook/filter-free Git plumbing

### Decision
Materialize raw blobs and build candidates through a temporary index using
`read-tree`, `hash-object`, `update-index`, `write-tree`, and `commit-tree`.
### Options considered
Normal checkout and commit; patch application; fixed plumbing.
### Pros
Exact deterministic trees without hooks, clean/smudge filters, textconv, or
external diff drivers.
### Cons
The service must implement explicit mode and diff verification.
### Why this option
Repository-controlled Git configuration is not trusted execution policy.
### Consequences
Candidates remain reachable only under `refs/harness/candidates/...` until a
later approved publication phase.
### Date
2026-07-28

## DEC-015: Independent checks resolve from a fixed offline catalog

### Decision
Resolve verification commands only from an ordered immutable Go catalog and
run the selected checks against a newly raw-object-materialized candidate in a
dedicated network-disabled image.
### Options considered
Model-supplied commands; repository-defined commands; kernel catalog entries
executed by a static verification worker.
### Pros
Model completion claims cannot become evidence, argv is reviewable, and every
log and coverage decision binds an exact commit and immutable image.
### Cons
New checks require a kernel release and the dependency-seeded image is larger
than the execution-worker scratch image.
### Why this option
Independent verification must not delegate command authority to the proposal
or repository it is evaluating.
### Consequences
The first entry is `make-check-v1`; checks run offline, in catalog order, and
all mapped checks must pass for a criterion.
### Date
2026-07-29

## DEC-016: Verification evidence is checkpointed before workflow mutation

### Decision
Persist verification IDs through `reserved`, `evidence_ready`, and `completed`
states, with database-protected identity and evidence fields.
### Options considered
Rerun checks after transition failure; one-step completion; evidence checkpoint
followed by an idempotent workflow command.
### Pros
Concurrent replay runs checks once, recovery cannot replace evidence, and an
ambiguous workflow response does not require repeating expensive checks.
### Cons
The ledger requires a recovery state and a deterministic transition command.
### Why this option
Evidence production and workflow mutation cross separate durability
boundaries.
### Consequences
Only expired pre-evidence reservations are taken over; evidence-ready recovery
retries the workflow command and completed replay returns the original result.
### Date
2026-07-29

## DEC-017: Elevated approval risk is deterministic

### Decision
Classify elevated work from both actual changed paths and kernel-supplied
exclusive-resource labels using a closed policy in trusted Go.
### Options considered
Reviewer-assigned risk; caller boolean; deterministic trusted classification.
### Pros
The same evidence always requires the same role and model output cannot lower
the required authority.
### Cons
Policy changes require a trusted release.
### Why this option
Approval authorization must not depend on advisory prose.
### Consequences
Protected dependencies, schemas, APIs, auth policy, deployment configuration,
Dockerfiles, and workflows require `elevated_approver`.
### Date
2026-07-29

## DEC-018: Human approvals are checkpointed before workflow mutation

### Decision
Persist approval IDs through `reserved`, `decision_ready`, and `completed`,
checkpointing the immutable `task_approval.v1` artifact before the command.
### Options considered
Workflow first; one transaction across external stores; checkpoint then
idempotent transition.
### Pros
Recovery cannot replace identity or evidence and ambiguous completion does not
create a second logical decision.
### Cons
The ledger and workflow command both require deterministic identity.
### Why this option
Artifact storage, PostgreSQL, and workflow mutation are separate durability
boundaries.
### Consequences
Exact replay returns the original decision, conflicts fail closed, and
`decision_ready` recovery retries only the human workflow transition.
### Date
2026-07-29

## DEC-019: Publication checkpoints external identity before workflow mutation

### Decision
Publish only to deterministic `harness/<run-id>`, recover the exact remote ref
and marked draft PR, and persist `reserved`, `branch_ready`, `pr_ready`, then
`completed` before and after each durability boundary.
### Options considered
Uncheckpointed push/PR creation; mutable branch updates; immutable staged
checkpointing with exact recovery.
### Pros
Ambiguous Git or API outcomes do not create a second logical branch or PR, and
replay cannot replace reviewed evidence or human-edited completed PR content.
### Cons
Publication requires a ledger, deterministic identifiers, remote inspection,
and an idempotent workflow command.
### Why this option
Git, GitHub, artifact storage, PostgreSQL, and workflow state cannot share one
transaction.
### Consequences
Base drift and mismatched branches or PRs fail closed. Phase 9 creates drafts
only and grants no ready, merge, branch-delete, deployment, or secret-storage
authority.
### Date
2026-07-29
