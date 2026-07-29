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

Phase 5 adds `go/internal/reasoning/gateway`, an in-process application API for
implementation proposals only. A request is deterministically serialized,
stored by SHA-256 through an `ArtifactStore`, and bound to an immutable
invocation row. The gateway resolves the exact registered manifest digest,
requires `implementation` with `implementation_proposal.v1`, calls one
deterministic fake adapter, validates the proposal, stores its artifact, and
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

No HTTP/gRPC execution or verification server, real provider, runtime Python
agent, pull-request operation, branch publication, merge operation, or
deployment exists. `MERGED` records an already completed, approval-bound
external fact.
