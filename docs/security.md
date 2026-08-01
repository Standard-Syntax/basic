# Security

## Phase 0–11 boundaries

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
  replay. PostgreSQL stores only URIs, digests, identity, provider metadata,
  usage, status, and rejection metadata. Completed rows cannot be updated or
  deleted.
- Request and proposal transports default to a 1 MiB limit. Provider adapters
  have no shell, filesystem-write, workflow, approval, execution, or
  publication authority.
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
  secrets, Git credentials, or writable root. It is limited to one CPU, 2 GiB,
  256 PIDs, 2 GiB tmpfs, ten minutes, and 1 MiB combined output per check.
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

Phase 8 adds no credential store, secret scanning, publication, merge,
deployment, listener, or network access. Trusted callers supply authenticated
principal claims. Review remains proposal-only: high/critical findings force
rework and advisory acceptance stops at `AWAITING_APPROVAL`.

Approval protects dependency manifests, Protobuf/schema and SQL migration
paths, deployment configuration, Dockerfiles, GitHub workflows, and the six
documented exclusive-resource labels. Elevated approval requires
`elevated_approver`; rework accepts either approval role. Approval IDs bind
exact deterministic request bytes. The artifact is checkpointed before the
idempotent workflow command, and PostgreSQL rejects identity/evidence mutation
plus updates or deletion after checkpoint.

Phase 9 credentials are read only for an individual REST request and are never
stored in artifacts, PostgreSQL, Git configuration, or logs. Production API
endpoints require HTTPS; test HTTP is limited to loopback. The client blocks
redirects, sets an explicit API version, bounds request/response bodies, and
rejects malformed, closed, non-draft, mismarked, or identity-mismatched PRs.

Publication cannot force-update reviewed work. The remote base must still equal
the reviewed base before branch publication and again before PR creation.
The deterministic branch uses `--force-with-lease=<ref>:` only to assert that
the ref is absent. A successful-but-uncheckpointed push is recovered only when
the remote ref exactly equals the approved candidate. Checkpointed branch/PR
facts and completed rows are protected by PostgreSQL triggers.

## Phase 10 provider boundary

- The shipped beta accepts only MiniMax's Anthropic-compatible HTTPS endpoint,
  `MiniMax-M2.7`, and `ANTHROPIC_API_KEY`; omitted mode selects that profile.
  Fake and alternate provider modes fail closed.
- Startup checks credential availability before migrations, database
  connections, manifest bootstrap, job claiming, or reconciliation.
- API keys are fetched per invocation and passed only as SDK request options.
  Credential-source errors are replaced with a redacted sentinel; keys are
  never written to artifacts, PostgreSQL, model context, errors, or logs.
- Model IDs come only from trusted capability-class configuration. Manifests
  and model output cannot choose a provider destination, fallback, or model.
- Prompt and input bodies are SHA-256 verified before use. Inline repository
  files must equal their bound artifacts and appear once in rendered context.
- A conservative content guard rejects API-key, authorization, password,
  private-key, secret, and token assignments before network access.
- Messages requests are non-streaming and tool-free. They grant no shell,
  filesystem write, arbitrary destination, workflow, approval, execution,
  publication, or model-selection authority.
- SDK retries are disabled. Trusted Go retries only connection failures and
  HTTP `408`, `409`, `429`, and `5xx`, at most three attempts and within
  request budget, expiry, cancellation, and the five-minute timeout.
- Authentication, permission, billing, timeout, exhausted rate limit, refusal,
  transport, and other provider failures are typed and redacted. They cannot
  advance workflow state.
- MiniMax's documented signed `thinking` response blocks remain only in the
  immutable raw-response artifact and are never parsed as proposal content.
  Truncation, context exhaustion, empty/multiple text, tool or other unexpected
  content, malformed JSON, unknown projection fields, and over-budget usage
  persist as exact, deterministic `SCHEMA_INVALID` outcomes.
- Exact replay verifies the stored provider response and performs no credential
  lookup or provider request.

## Phase 11 runtime boundary

- API bearer tokens are represented only by configured SHA-256 digests and are
  compared in constant time. Raw tokens are neither persisted nor logged.
- Mutations require a UUID idempotency key bound to method, target, principal,
  and request digest, plus an exact revision precondition.
- The API binds to loopback and rejects non-loopback listeners.
- The filesystem CAS verifies every digest, bounds access, rejects symlinks and
  non-regular objects, and publishes durable objects atomically.
- Runtime claims use expiry and monotonically increasing fencing tokens.
  Terminal jobs are immutable and stale owners cannot checkpoint.
- GitHub credentials are loaded for each REST request from a clean absolute,
  regular, owner-only `0600` file.
- Phase 11 adds no automatic merge, deployment, arbitrary shell, unrestricted
  network, provider fallback, or cross-repository authority.

## Beta Slice 3 intake boundary

- `api-service` requires one clean absolute `repository_root`. Intake resolves
  the supplied 40-character commit to committed Git objects before acceptance;
  mutable worktree contents never define the repository map.
- The idempotency row is locked with `FOR UPDATE` inside the same serializable
  transaction as workflow creation, the complete immutable run binding, and
  the stored HTTP response. Method, target, principal, request digest,
  reservation expiry, and fencing generation are rechecked under that lock.
- Expired reservations increment their generation. A stale generation cannot
  commit, complete, or abandon a newer intake reservation.
- Run bindings accepted by the new path always contain intake and repository-map
  CAS references plus the exact base commit. Missing, malformed, digest-mismatched,
  or base-mismatched repository maps stop stage start before leases or external
  work.
- Pre-commit failures leave no workflow run, binding, completed response, job,
  or provider invocation. A crash after commit replays the stored bytes and
  performs no Git, CAS, workflow, job, or provider operation.
- New beta bindings also contain the canonical policy artifact and immutable
  execution and verification image IDs. Missing, mutable, or configuration-
  mismatched bindings fail before a lease, provider call, candidate, or
  publication.
- `beta-preflight` does not create directories, apply migrations, write CAS,
  update Git refs, call a provider, push, or contact GitHub. Its JSON contains
  only allowlisted identities/digests and stable redacted failure codes.

## Beta Slice 7 release boundary

- The release manifest is strict, bounded, secret-free JSON. It contains
  immutable identities and credential-file/config paths, never credential
  values, provider bodies, or database exports.
- Readiness opens an existing CAS without creation and uses a read-only
  PostgreSQL connection. It cannot mutate workflow, evidence, Git, GitHub, or
  Docker state; GitHub access is one exact pull-request read.
- Readiness requires one accepted task, exactly two accepted live MiniMax
  invocations, exact candidate/artifact cross-bindings, immutable image IDs,
  one completed draft publication, an open matching real draft, and an explicit
  human `go`. A `no_go` decision is valid evidence but cannot pass readiness.
- The packaged canary copies operator credentials into disposable owner-only
  mounts for the fixed service UID. It never changes the provisioned credential
  files and retains the existing exact-identity cleanup boundary.
