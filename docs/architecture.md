# Architecture

The harness follows a proposal/kernel split. Models and Python configuration
may describe requested work; only a Go kernel may authorize, execute, verify,
approve, or advance state.

Phase 0–1 contains six empty Go command boundaries: API, workflow, agent
registry, reasoning gateway, execution, and verification. They compile but
start no server. Shared business rules live in handwritten packages, while
generated Protobuf packages remain transport-only.

Python is an offline authoring tool. It compiles declarative definitions into
immutable canonical JSON and has no workflow, database, Git, command execution,
credential, approval, publication, or task-scope authority.

PostgreSQL, artifact storage, gRPC servers, workflow runtime, leases, worktrees,
providers, file mutation, verification execution, and pull-request publication
are architectural commitments or later-phase candidates, not implemented here.
