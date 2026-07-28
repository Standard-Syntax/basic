# Security

## Phase 0–2 boundaries

- Reasoning authority is proposal-only and fails closed.
- Python configuration has no database, Git, shell, network, write, credential,
  workflow-transition, approval, publication, or task-scope capability.
- Manifests are schema-validated, canonicalized, immutable, and SHA-256
  addressed.
- Generated transports carry data but do not authorize it.
- Review recommendations are advisory and cannot encode approval.
- No production secrets, runtime side effects, automatic merge, or deployment
  exist in this phase.
- Humans alone approve or reject specifications, task graphs, reviewed tasks,
  and runs, and humans alone cancel runs.
- Service actors record only stage-specific operational facts. Python, model,
  and unknown actor kinds have no transition authority.
- Revisions and row locks prevent lost updates. A command ID is bound to its
  deterministic request digest; exact replay returns the recorded result and
  conflicting reuse fails closed.
- Events are append-only by database trigger. State, events, command result,
  task creation, dependency readiness, and cancellation cascade share one
  serializable transaction.

Future side-effect execution must normalize paths, reject traversal and symlink escapes,
deny network by default, use short-lived scoped credentials and fencing tokens,
scan inputs and outputs for secrets, and bind independent evidence to exact
candidate commits. Those controls are documented contracts, not current runtime
claims.
