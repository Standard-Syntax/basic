# Phase 6 completion

Phase 6 implements an isolated execution library without adding a network
service, provider, declared-check runner, verification, publication, merge, or
deployment.

## Delivered boundaries

- Migration `0008` persists positive attempt-bound fencing tokens. Proposal
  acceptance and execution recording require the exact current, unexpired
  lease tuple.
- `go/internal/execution.Service.Execute` revalidates the existing v1 request
  and proposal, verifies exact proposal artifact bytes, and derives repository
  and container settings only from trusted configuration.
- Detached no-checkout worktrees are materialized from raw tracked blobs.
  Targets reject traversal, `.git`, duplicates, unsupported modes, special
  files, and symlink leaves or ancestors before workflow acceptance.
- The scratch worker runs without network or privilege, applies only
  preflighted complete-file replacements through directory descriptors, and
  preserves existing executable modes while creating files as `100644`.
- A temporary Git index creates a deterministic candidate commit and internal
  reachability ref. The complete candidate diff is checked against authorized
  operation, path, mode, and before/after SHA-256 values.
- Migration `0009` reserves execution IDs, detects digest conflicts, supports
  expired-reservation recovery, stores deterministic completed results, and
  database-enforces completed-row immutability.
- Defaults are 100 changed files, 1 MiB per file, 10 MiB total content, four
  concurrent executions, eight worktrees, and a five-minute applicator timeout.

## Recovery and evidence

Execution IDs deterministically bind the worktree path, workflow command IDs,
candidate ref, commit metadata, and report. Concurrent identical calls perform
one logical execution and return a replay; conflicting content fails closed.
Failures before final workflow recording clean worktrees and candidate refs.
A stale final lease check removes the ref and leaves the task unadvanced.

The integration gate builds the real static worker image, runs it with Docker
against a disposable repository, exercises PostgreSQL reservation/finalization,
and verifies that hostile hooks and filters do not execute. Unit coverage
includes cancellation cleanup, duplicate and malformed paths, digest mismatch,
symlink and special-file rejection, and a symlink-swap adversary after
preflight.

## Deferred work

The artifact interface remains backend-neutral. Production object storage,
credentials, declared checks, secret scanning, independent verification,
branch or pull-request publication, human approval, merge, and deployment are
outside Phase 6.
