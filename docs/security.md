# Security

## Phase 0–7 boundaries

- Reasoning authority is proposal-only and fails closed.
- Python configuration has no database, Git, shell, network, write, credential,
  workflow-transition, approval, publication, or task-scope capability.
- Manifests are schema-validated, canonicalized, immutable, and SHA-256
  addressed.
- Registry versions and manifest digests are unique. Exact replay is
  idempotent, conflicting replacement fails closed, and PostgreSQL triggers
  reject both update and deletion.
- Registry reads revalidate stored manifest bytes and verify their canonical
  form, digest, and embedded identity against the indexed columns. Corrupt
  persisted data is never returned as a valid record.
- The reasoning gateway accepts only the implementation stage and exact
  `implementation_proposal.v1` manifest output. Registry lookup, adapter,
  artifact, cancellation, and database failures cannot be recast as policy
  rejections.
- Request IDs bind to deterministic request bytes. A committed in-progress
  reservation makes concurrent identical calls converge on one immutable
  result without holding a database connection during adapter work; different
  bytes under the same ID fail closed.
- Request and proposal bodies are content-addressed and verified on write and
  replay. PostgreSQL stores only URIs, digests, identity, fake-adapter metadata,
  usage, status, and rejection metadata. Completed rows cannot be updated or
  deleted.
- Request and proposal transports default to a 1 MiB limit. The fake adapter
  consumes exactly one permitted provider request and has no shell, network,
  filesystem-write, workflow, approval, or execution capability.
- The installed compiler always applies its packaged v1 schema; an override can
  only add restrictions. Python and Go enforce the same closed stage/output
  mapping.
- Prompt files must be UTF-8, but their exact bytes are hashed without newline
  or encoding normalization. Definition parsing rejects duplicate and unknown
  fields before any output write.
- The CLI performs local authoring I/O only. It cannot call a model, provider,
  network, database, Git, shell, registry, workflow, approval, or publication
  interface.
- Generated transports carry data but do not authorize it.
- Review recommendations are advisory and cannot encode approval.
- The registry has no HTTP/gRPC transport, authentication surface, provider,
  runtime agent, production secrets, automatic merge, or deployment.
- The reasoning gateway command remains non-networked. There is no production
  artifact backend, provider credential path, model invocation, or runtime
  listener.
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
- Execution acceptance and final recording require the exact lease ID, owner,
  expiry, and positive per-attempt fencing token. Expired and superseded leases
  produce no execution transition; a stale final check deletes the candidate
  ref.
- Execution paths reject absolute paths, traversal, control characters,
  `.git`, duplicate normalized paths, gitlinks, special files, symlink leaves,
  and symlink ancestors. The worker uses descriptor-relative operations and
  rechecks inode identity after preflight.
- The worker has no network, capabilities, or privilege escalation; its root is
  read-only and it is limited to one CPU, 512 MiB, 64 PIDs, and the configured
  non-root UID/GID. The worktree `.git` file is separately read-only.
- Candidate commits use raw blobs and a temporary index. Repository hooks,
  clean/smudge filters, textconv, and external diff drivers are not invoked.
  Candidate refs are internal reachability records, not publication.
- Verification rejects unknown, unavailable, duplicated, or criterion-mismatched
  check bindings before workspace creation, container launch, artifact writes,
  or workflow mutation. Model prose and repository content cannot introduce an
  argv.
- The verification worker runs the fixed `make check` argv as a non-root
  UID/GID with no network, capabilities, privilege escalation, inherited
  secrets, Git credentials, or writable root. It is limited to one CPU, 1 GiB,
  256 PIDs, bounded tmpfs, ten minutes, and 1 MiB combined output per check.
- Verification resolves and records the immutable Docker image ID before
  execution. Every log, report, coverage row, and workflow command binds the
  exact candidate commit and Phase 6 report.
- At most two verifications and four verification workspaces are admitted.
  Timeouts, output overflow, cancellation, malformed worker output, cleanup
  failure, corrupt artifacts, stale revisions, and candidate mismatches cannot
  advance a task.
- Verification IDs bind deterministic request digests. Exact replay never
  reruns checks, expired pre-evidence reservations may be recovered, and
  evidence-ready retries perform only the workflow transition. PostgreSQL
  protects evidence-ready identity/evidence and completed rows from mutation
  or deletion.

Phase 7 does not add credentials, secret scanning, review/approval,
publication, merge, deployment, a listener, or network access. Those remain
later authorization boundaries.
