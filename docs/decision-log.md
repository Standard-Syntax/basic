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

## DEC-022: First runtime slice is durable, local, and single-task

### Decision
Run the authenticated API and workflow reconciler as separate Go processes
over PostgreSQL, with a one-node filesystem CAS and exactly one
dependency-free task per run.
### Options considered
In-memory orchestration; shared object storage; multi-task scheduling.
### Pros
Restarts are observable and recoverable without distributed scheduling.
### Cons
The filesystem CAS is one-node and multi-task scope conflicts remain deferred.
### Why this option
PostgreSQL jobs, immutable bindings, idempotency, and fencing complete the
first durable slice without broadening authority.
### Consequences
A composite approval binds one human decision to the candidate and upstream
evidence. Draft publication is optional and never implies merge.
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

## DEC-020: Provider projections are internal and identity-free

### Decision
Use closed Anthropic structured-output schemas containing only model-owned
implementation or review fields, then inject all identity and authority
bindings from the trusted request.
### Options considered
Expose Protobuf JSON directly; let the model echo identity; use an internal
closed projection with trusted injection.
### Pros
Provider output cannot replace request, task, specification, manifest, or
artifact identity, and published v1 contracts remain unchanged.
### Cons
Each supported stage needs a provider projection and explicit conversion.
### Why this option
Model output is advisory content, not an authority or identity source.
### Consequences
Valid output still passes unchanged kernel validators; unknown provider fields
become deterministic malformed-output outcomes.
### Date
2026-07-29

## DEC-021: Trusted Go owns bounded provider reliability

### Decision
Disable SDK retries and perform at most three explicit attempts, bounded by
request budget, expiry, cancellation, and a five-minute timeout.
### Options considered
SDK defaults; unbounded provider retry; trusted bounded retry with immutable
attempt accounting.
### Pros
Every actual network attempt is observable and enforceable against kernel
budget, while transient classifications remain testable.
### Cons
The adapter owns retry classification, `Retry-After`, and backoff behavior.
### Why this option
Hidden SDK attempts would make provider-request budgets and replay evidence
unreliable.
### Consequences
Only connection failures and HTTP `408`, `409`, `429`, and `5xx` retry.
Provider failures roll back; completed malformed responses persist and replay.
### Date
2026-07-29

## DEC-023: Shipped beta reasoning is live-only MiniMax-M2.7

### Decision
Compose `workflow-service` only with the MiniMax Anthropic-compatible
implementation and review adapters. Accept omitted mode or
`minimax_anthropic` only, pinned to
`https://api.minimax.io/anthropic`, `MiniMax-M2.7`, and
`ANTHROPIC_API_KEY`.

### Options considered
Retain fake-by-default composition; permit fake and live modes; ship one closed
live-only provider profile.

### Pros
Production startup cannot select synthesized proposals or an alternate
provider destination, model, or credential name. The availability check fails
before orchestration setup while per-invocation environment reads preserve
credential rotation.

### Cons
Credential-free process E2E needs a redesigned external loopback fixture, and
the historical mixed fake/live targets are not Beta Slice 1 acceptance
evidence.

### Why this option
The first beta must exercise the approved provider boundary in production
composition without weakening the unchanged proposal/kernel contracts.

### Consequences
DEC-008 remains historical rationale for boundary-first development but is
superseded for the shipped beta runtime. Fake provider constants, adapters,
proposal loaders, configuration fields, and composition branches are removed.
Tests obtain valid proposals through the production adapter over bounded
loopback HTTP; Beta Slice 2 owns the process-E2E redesign.

### Date
2026-07-30

## DEC-024: Separate recovery, provider smoke, and the credentialed beta gate

### Decision
Use three commands with non-overlapping evidence claims:
`make runtime-e2e` for credential-free PostgreSQL/CAS/reconciler recovery,
`make provider-smoke` for isolated provider protocol connectivity, and
`make beta-live-e2e` for the complete two-process MiniMax-M2.7 lifecycle.

### Options considered
Keep one environment-switched process fixture; treat provider smoke as live
acceptance; separate deterministic infrastructure evidence from the
credentialed lifecycle gate.

### Pros
No credential-free command can silently substitute generated proposals for the
live provider. Operators can identify whether a result proves recovery,
provider connectivity, or the full implementation-through-publication
lifecycle.

### Cons
The full gate is slower, requires a real provider credential, and remains
outside ordinary credential-free CI.

### Why this option
Beta acceptance must fail on a missing credential, skipped test, missing
provider evidence, replayed side effect, or secret leak. Environment-selected
fake/live behavior made those claims ambiguous.

### Consequences
The shipped composition remains live-only. The deterministic recovery gate
uses pre-existing immutable artifacts and never calls reasoning. The beta gate
uses disposable PostgreSQL and Git, real execution and verification workers,
and loopback-only GitHub publication; real GitHub publication remains a later
slice.

### Date
2026-07-30

## DEC-025: Accept runs through one fenced transaction

### Decision
Resolve and stage the exact intake-time repository map before acceptance, then
lock the API idempotency reservation and atomically persist workflow run
creation, the complete immutable runtime binding, and the exact HTTP response
in one caller-owned serializable transaction.

### Options considered
Keep the existing chain of independently committed repositories; add a partial
intake state and compensating reconciler; expose a narrow caller-owned workflow
transaction seam and commit the acceptance boundary once.

### Pros
An accepted run is complete and replayable, while every pre-commit failure
exposes no run. Repository context cannot drift between API intake and worker
start, and a post-commit response loss replays without external work.

### Cons
The API composition coordinates PostgreSQL records owned by workflow and
runtime packages, and failed staging can leave unbound CAS objects for normal
garbage collection.

### Why this option
Compensation cannot make a chain of visible commits atomic. PostgreSQL already
owns the workflow, binding, and idempotency records and can enforce one
serializable acceptance decision without a new migration or reconciler.

### Consequences
`api-service` requires a clean absolute repository root. New run bindings must
include intake and repository-map references plus the exact base commit. Stage
start verifies and consumes that bound map; incomplete legacy rows fail closed.
Reservation expiry increments a fencing generation, and stale generations
cannot commit, complete, or abandon intake.

### Date
2026-07-31

## DEC-026: Bind production readiness policy to every accepted run

### Decision
Represent repository identity, approved base, path/check authority, resource
ceilings, and both worker images as one strict canonical beta policy. Verify
readiness without mutation, store the policy in CAS during intake staging, and
atomically bind its artifact and image identities to the run.

### Options considered
Trust process-local configuration; copy individual settings into mutable
workflow state; bind a canonical policy artifact and image identities to the
immutable intake row.

### Pros
Workers can prove the exact accepted authority. Configuration drift, legacy
rows, mutable tags, scope widening, base movement, and migration drift fail
before leases or external work. Preflight emits secret-free evidence.

### Cons
Operators must maintain one strict policy across both services and preflight,
and roots, images, and migrations must exist before readiness passes.

### Why this option
Readiness that is not run-bound can change after acceptance and cannot be
audited. Canonical CAS bytes preserve the boundary without changing reasoning
wire contracts.

### Consequences
Migration `0020` keeps historical columns nullable, but new intake requires the
policy and both `sha256:` image IDs. Stage start rejects incomplete or changed
bindings. Task planning remains one dependency-free task.

### Date
2026-07-31

## DEC-027: Isolate real publication to a dedicated canary and separate cleanup authority

### Decision
Run real beta publication only against `Standard-Syntax/basic-beta-canary` and
`main`, using one REST credential and a separate repository-scoped Git push key.
Keep close and exact-SHA branch deletion on concrete clients invoked only by a
separate cleanup command; do not add them to runtime publication ports.

### Options considered
Publish canaries in the product repository; reuse one broad GitHub credential;
use a dedicated fixture repository with separate publish and cleanup authority.

### Pros
The real provider-to-draft boundary is exercised without granting merge or
deployment authority. Immutable record/artifact validation, exact refs and
commits, and replay-safe cleanup make every external mutation attributable.

### Cons
Operators must provision and rotate two narrowly scoped credentials, maintain
the canonical fixture, inspect the retained draft, and invoke cleanup explicitly.

### Why this option
Loopback publication cannot prove GitHub behavior, while production-repository
or broad-token canaries would turn a release gate into unnecessary production
authority. Cleanup needs external mutation rights but is not part of acceptance.

### Consequences
Canary success leaves exactly one draft and branch. Cleanup fails closed on any
identity or head mismatch and is idempotent only for the same immutable
publication. Merge, ready-for-review, deploy, and prefix deletion remain absent.

### Date
2026-07-31

## DEC-028: Package beta services around a narrow Docker Engine boundary

### Decision
Ship API and workflow as pinned, fixed-identity images and let only workflow
use an API-version-negotiating Moby client through a narrow worker Engine
interface. Describe one exact deployment with a strict, secret-free envelope
and retain a generated bill of materials for every image set.

### Options considered
Install the Docker CLI in workflow; move workers into the service process; use
the official Engine SDK while preserving isolated worker containers.

### Pros
The final service images contain no shell, package manager, or Docker CLI.
Worker mounts, non-root identity, network isolation, resource bounds, image
inspection, and cancellation cleanup remain explicit API requests. Readiness
and rollback bind the same immutable image identities.

### Cons
The trusted Docker socket remains powerful beta infrastructure, operators must
provision its positive supplemental group explicitly, and Engine API compatibility
is a new service dependency. Group zero is never an accepted fallback. This is
an explicit bounded beta risk; DEC-028 does not claim that the narrow client
interface reduces the Docker daemon's host-level authority.

### Why this option
Removing container isolation would weaken execution and verification. Shipping
the Docker CLI adds an unnecessary command parser and administration surface;
the consumer-owned SDK boundary expresses only the operations workers need.

### Consequences
Only workflow receives the socket. API is loopback-published and repository
read-only. Both services run as `65532:65532` with read-only roots, and workflow
drains one active fenced claim for at most 60 seconds while renewing it.
Deployment records contain no credentials or claims of live provider evidence.

### Date
2026-07-31

## DEC-029: Release readiness revalidates durable evidence read-only

### Decision
Represent one beta candidate with a strict secret-free release manifest and
verify its supply-chain, PostgreSQL, CAS, Git, Docker, and exact GitHub draft
bindings through a read-only command before accepting a human go decision.

### Options considered
Trust completion prose; copy selected command output into a report; revalidate
the immutable deployment record and complete live canary evidence.

### Pros
The release decision is reproducible, cross-bound to the exact candidate and
images, and cannot be synthesized from a provider smoke, loopback publication,
or green deterministic suite.

### Cons
Operators must retain three strict configuration/evidence files, preserve the
canary durable stores and draft until verification, and supply a read-only
GitHub credential during readiness.

### Why this option
Release authority depends on durable facts across stores that do not share one
transaction. Prose and copied status lines cannot prove their continued
identity or replay safety.

### Consequences
The canary uses packaged services and reports exact artifact/image identities.
`beta-readiness` performs no repair or mutation, emits only redacted check
names, and cannot turn successful checks into the required human `go` record.

### Date
2026-07-31

## DEC-030: Approve task graphs and schedule their first job atomically

### Decision
Stage the approved task artifact in CAS, then apply the task-graph workflow
command, checkpoint its immutable graph and task bindings, and enqueue the
deterministic start job in one retryable serializable PostgreSQL transaction.

### Options considered
Keep the three existing commits and repair partial state on replay; enqueue
after workflow approval through a reconciler; coordinate all three durable
writes in one application transaction.

### Pros
No approved task is visible without its binding and runnable start job. Exact
replay converges on one command, binding, and job, and fault injection can prove
rollback after every write boundary.

### Cons
The control API gains a narrow transaction-scoped coordinator and runtime
helpers. A failed request may leave one unbound CAS artifact for normal
collection.

### Why this option
All three records already share PostgreSQL and jointly define one approval
decision. Compensation or later reconciliation would expose states that the
worker cannot safely execute.

### Consequences
The HTTP handler performs validation and CAS staging only, then delegates the
durable mutation. Cancellation rollback is detached and bounded; serializable
and deadlock conflicts retry the complete transaction.

### Date
2026-08-02
