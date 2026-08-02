# Architecture

The harness follows a proposal/kernel split. Models and Python configuration
may describe requested work; only a Go kernel may authorize, execute, verify,
approve, or advance state.

Phase 2 adds the handwritten `go/internal/workflow` domain model and
PostgreSQL-backed application store. Commands validate closed states, actor
authority, expected revision, aggregate identity, and immutable bindings.
Accepted commands lock the aggregate, append ordered events, update the
snapshot, and record an idempotent result in one serializable transaction.

Phase 3 hardens Python as an offline-only authoring tool. The installed SDK
compiles declarative definitions for specification, planning, implementation,
and independent review into immutable canonical JSON. Its packaged schema,
stage/output mapping, prompt digest, and manifest digest are checked again by
the Go reader at the language boundary.

Phase 4 adds `go/internal/registry`, a PostgreSQL-backed application API with
no network transport. Registration accepts manifest bytes only through
`manifest.Read`, stores the exact RFC 8785 canonical bytes, and returns records
by immutable `(agent_name, agent_version)` identity or lowercase SHA-256
digest. Per-identity transaction advisory locks make concurrent registration
deterministic.

Phase 5 originally added `go/internal/reasoning/gateway`, an in-process application API for
implementation proposals only. A request is deterministically serialized,
stored by SHA-256 through an `ArtifactStore`, and bound to an immutable
invocation row. The gateway resolves the exact registered manifest digest,
requires `implementation` with `implementation_proposal.v1`, calls one
proposal adapter, validates the proposal, stores its artifact, and
commits either an accepted proposal or a typed rejection. A short committed
reservation serializes each request ID; adapter and artifact work holds no
database transaction or pooled connection. Exact concurrent replay returns the
original outcome and different bytes fail with a conflict.

Phase 6 adds `go/internal/execution`. Its in-process `Service.Execute` binds a
request and proposal artifact to an active fenced lease, creates a deterministic
detached worktree, and invokes a static Go applicator in a network-disabled,
read-only-root Docker container. A temporary index and raw Git plumbing create
the candidate tree and commit without checkout filters, hooks, textconv, or
external diff drivers. The service verifies the complete candidate diff,
retains it under `refs/harness/candidates/...`, stores a deterministic report,
and only then records `TASK_EXECUTED`.

Migration `0008` persists positive per-attempt lease fencing tokens. Migration
`0009` reserves execution IDs, detects conflicting request digests, stores the
completed result, and rejects mutation or deletion of completed rows. Artifact
bodies remain behind the backend-neutral content-addressed port.

The shipped beta composition supersedes Phase 5's fake-first production
choice. `workflow-service` normalizes omitted provider configuration to the
single closed profile `minimax_anthropic`,
`https://api.minimax.io/anthropic`, `MiniMax-M2.7`, and
`ANTHROPIC_API_KEY`. Implementation and review use the same production
Anthropic-compatible adapter boundary; no fake adapter or alternate
composition branch ships. The credential is checked before database or
orchestration setup and read again, without caching, for each invocation.

Phase 7 adds `go/internal/verification`. `Service.Verify` revalidates the
implementation request and content-addressed Phase 6 report, requires their
run, task, attempt, base commit, and candidate commit to agree, and resolves
checks only from an ordered immutable Go catalog. The exact candidate tree is
materialized into a new directory from raw Git objects and never reuses the
execution worktree. A static verification worker runs the fixed `make check`
argv in a dedicated immutable image with no network, capabilities, privilege
escalation, inherited environment, Git metadata, or host credentials.

Each bounded combined log and the deterministic aggregate report is stored by
digest. Criterion coverage passes only when it has mapped evidence and every
mapped check passes; all selected checks run in catalog order after failures.
Migration `0010` checkpoints `reserved`, `evidence_ready`, and `completed`
verification states. Evidence-ready recovery retries only the idempotent
workflow transition, while database triggers protect identity, evidence, and
completed rows.

The Python package has no provider or runtime agent, and no workflow, database,
Git, shell, network, direct-file-mutation, credential, approval, publication,
registration, or task-scope authority. Its only writes are the explicitly
requested authoring outputs. A compiled manifest remains untrusted input until
the Go kernel validates and binds it.

Checked-in forward-only SQL migrations use a shared internal migrator and
digest ledger, are embedded in their owning package, and are serialized by a
PostgreSQL advisory lock. Artifact bodies remain external; rows contain URIs,
lowercase SHA-256 digests, canonical manifest metadata, commit identifiers,
and small event payloads.

Migration `0007` stores reasoning identity, request/proposal artifact
references, fake-adapter metadata, usage counters, timestamps, final status,
and rejection metadata. It never stores complete request or proposal bodies.
Completed invocation rows reject update and deletion by database trigger. No
production filesystem or object-store backend exists; tests use an
integrity-checking in-memory artifact store.

Phase 8 extends the same gateway with a deterministic `review` adapter and the
unchanged `review_proposal.v1` transport. Migration `0011` permits only
`implementation` and `review` invocation stages. `go/internal/review`
reconstructs the Phase 6 diff and Phase 7 evidence from content-addressed
reports, enforces fixed high/critical blocking policy, stores
`review_report.v1`, and emits only `RecordTaskReview`.

`go/internal/approval` is the distinct authenticated application boundary.
It classifies elevated paths and exclusive resources, validates every upstream
digest and candidate binding, stores `task_approval.v1`, and emits an
`ActorHuman` task command. Migration `0012` persists `reserved`,
`decision_ready`, and `completed` approval states. Recovery from
`decision_ready` retries only the deterministic workflow command; immutable
database fields prevent replacing the human identity, evidence, or decision.

Phase 9 adds `go/internal/publication`, an in-process application boundary.
It verifies the specification, implementation, execution, verification,
review, and approval bodies by digest and cross-binding before any Git or PR
mutation. The configured remote base must equal the reviewed base commit.
The exact candidate is pushed once to deterministic `harness/<run-id>` using
argument-only Git, disabled hooks/prompts, and an explicit empty expected-value
lease. An already-equal branch is recovery; any other branch value is a
conflict.

The narrow GitHub REST client lists exact owner/head/base PRs before creation,
embeds a publication-ID marker, and creates only `draft: true`. Migration
`0013` advances immutable rows through `reserved`, `branch_ready`, `pr_ready`,
and `completed`; the final checkpoint stores `draft_pull_request.v1` and emits
the idempotent `RecordDraftPullRequest` command. Migration `0014` extends only
the closed workflow actor constraint for `PUBLICATION_SERVICE`. The run remains
`MERGE_READY`.

Phase 10 adds constructor-selected Anthropic Messages adapters behind the
existing implementation and review gateway seams. The official Go SDK is
pinned at `v1.61.0` with SDK retries disabled. Trusted capability
configuration selects models; request-local credential sources supply API
keys. The adapter SHA-256 verifies the manifest prompt and every selected input
artifact, rejects secret-bearing content, and sends one non-streaming,
tool-free request with a closed provider projection schema. Trusted Go injects
all request, manifest, artifact, task, and specification identity fields after
strict projection decoding.

The adapter owns at most three network attempts, further bounded by request
budget, expiry, caller cancellation, and a five-minute provider timeout.
Migration `0016` adds the exact raw provider-response artifact, provider
request ID, model, aggregate tokens, and actual network-attempt count to the
immutable invocation outcome. Malformed complete responses become replayable
`SCHEMA_INVALID`; provider, transport, credential, refusal, and timeout errors
roll back the unfinished reservation.

Phase 11 adds HTTP only at `api-service`; execution, verification, review,
approval, and publication remain in-process application boundaries behind the
PostgreSQL workflow worker. No runtime Python agent, merge operation, or
deployment exists. `MERGED` still records an already completed,
approval-bound external fact.

## Phase 11 durable first-slice runtime

`api-service` and `workflow-service` are the two runnable control-plane
processes. The API authenticates configured principals and enforces roles,
idempotency, and revision preconditions. The worker claims deterministic stage
jobs with `FOR UPDATE SKIP LOCKED`; claim expiry permits takeover while fencing
tokens prevent stale completion.

Migration `0018` adds immutable runtime bindings, API idempotency, and stage
jobs. A local SHA-256 CAS stores bounded durable evidence. Repository context is
built from raw objects at the approved base commit, never mutable worktree
contents. The first slice accepts one dependency-free task and pauses at one
composite human approval. Publication remains draft-only and
configuration-gated; an unconfigured run stays `MERGE_READY`.

## Beta Slice 3 atomic run intake

`POST /v1/runs` has a dedicated application coordinator in `api-service`.
Before opening its acceptance transaction, the coordinator resolves the exact
requested commit from the required clean absolute `repository_root`, builds a
deterministically ordered map of committed blobs, and stages both the intake
body and repository map in CAS. Staged but unbound CAS objects are harmless if
acceptance fails.

One serializable PostgreSQL transaction then locks and revalidates the API
idempotency reservation and fencing generation, persists the workflow command,
initial event and `DRAFT` snapshot, inserts the complete immutable runtime
binding, and stores the exact HTTP `201` response. It commits once. Every
pre-commit error exposes no run; an ambiguous or post-commit response failure is
recovered from the stored response without repeating Git, CAS, workflow, job,
or provider work.

Stage start no longer snapshots the repository or checkpoints a repository map.
It loads the intake-bound CAS object, verifies its digest, strict shape, sorted
blob entries, and exact base-commit binding, then performs the existing run and
task lease transitions. Nullable repository-map columns remain migration-only
compatibility: an incomplete legacy binding fails closed.

Task-graph approval uses a second narrow application coordinator. The task
artifact is staged in CAS first, then one retryable serializable transaction
applies `ApproveTaskGraph`, checkpoints the immutable graph and task bindings,
and inserts the deterministic start job before committing once. Any unbound
artifact left by rollback is collectible; exact command replay converges on
the same single binding and job.

### Immutable beta repository policy

Slice 4 adds one canonical `beta_policy` shared by API intake, workflow
workers, and `beta-preflight`. Intake stores its CAS artifact and both worker
`sha256:` identities in the same serializable transaction as the run binding.
Stage start compares the complete bound policy and image identities with active
configuration before starting a run or acquiring a lease. Task-planning
authority must match the bound policy; proposals may narrow it but cannot widen
paths, checks, limits, concurrency, or the one dependency-free task boundary.

The Docker Engine boundary receives CPU, memory, and PID limits explicitly for
every worker. Execution uses its fixed one-CPU, 512 MiB, 64-PID profile;
verification derives the exact container limits from each trusted catalog
definition, bounded by the two-GiB memory and 256-PID ceilings.

## Beta Slice 7 release evidence

`beta_release_manifest.v1` embeds the exact Slice 6 deployment record, the
packaged canary identities and four terminal artifact references, complete
toolchain identity, and a human go/no-go record. `beta-readiness` is a read-only
composition boundary: it opens the existing CAS without creating it, connects
to PostgreSQL with `default_transaction_read_only`, revalidates the checked-in
migration ledger and cross-stage artifact bodies, inspects the installed image
IDs and clean source commit, and reads the exact GitHub pull request. It emits
no workflow command and has no approval, publication, cleanup, merge, or
deployment authority.
