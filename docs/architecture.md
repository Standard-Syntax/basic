# Architecture

The harness follows a proposal/kernel split. Models and Python configuration
may describe requested work; only a Go kernel may authorize, execute, verify,
approve, or advance state.

Phase 2 adds the handwritten `go/internal/workflow` domain model and
PostgreSQL-backed application store. Commands validate closed states, actor
authority, expected revision, aggregate identity, and immutable bindings.
Accepted commands lock the aggregate, append ordered events, update the
snapshot, and record an idempotent result in one serializable transaction.

Python is an offline authoring tool. It compiles declarative definitions into
immutable canonical JSON and has no workflow, database, Git, command execution,
credential, approval, publication, or task-scope authority.

Checked-in forward-only SQL migrations are embedded in Go and serialized by a
PostgreSQL advisory lock. Artifact bodies remain external; rows contain URIs,
lowercase SHA-256 digests, commit identifiers, and small event payloads.

No HTTP/gRPC server, provider, worktree, file mutation, command runner,
verification runner, pull-request operation, merge operation, or deployment
exists. `MERGED` records an already completed, approval-bound external fact.
