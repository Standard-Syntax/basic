# Resource analysis

Phase 2 adds PostgreSQL metadata persistence. Phase 3 adds an offline Python
authoring SDK, one packaged JSON Schema, four small definitions and prompts, and
canonical manifest fixtures. Phase 4 adds small canonical manifest rows and
indexed identity/digest lookup. Phase 5 originally added one in-process proposal,
content-addressed artifact ports, and one small immutable invocation row per
request. Phase 6 adds bounded detached worktrees, a static applicator container,
candidate Git objects, report artifacts, and one immutable execution row.
Phase 7 adds at most four clean candidate workspaces, bounded verification logs
and reports, a dependency-seeded verification image, and one immutable
verification ledger row per verification ID.

| Area | Initial expectation |
|---|---|
| CPU | one CPU per applicator container; at most four concurrent executions by default |
| Memory | 512 MiB per applicator container; request/proposal each default to at most 1 MiB |
| Disk | up to eight active worktrees plus candidate Git objects; report bodies remain external artifacts |
| Network | applicator containers use `--network none`; dependency installation remains a development concern |
| Database | workflow, registry, reasoning, and immutable execution metadata |
| Artifacts | request, proposal, and report bodies flow through an external content-addressed port |
| Model tokens | request/manifest bounded for live-only MiniMax-M2.7 |
| Concurrency | four executions, eight worktrees, and one logical owner per execution ID by default |
| Verification CPU | one CPU per check container; at most two concurrent verifications |
| Verification memory | 2 GiB, 256 PIDs, bounded 2 GiB tmpfs, and 1 MiB combined output per check |
| Verification disk | at most four clean workspaces; content-addressed logs and reports remain external |
| Verification network | `--network none`; `go.sum`, `uv.lock`, writable runtime copy of seeded Go build/vet caches, and generation tools seed the image |
| Review | bounded MiniMax Anthropic-compatible request plus content-addressed request, raw response, proposal, and report artifacts |
| Approval | one small immutable row and one content-addressed decision artifact per approval ID |
| Publication | bounded Git/API subprocesses, one branch ref, one draft PR, one artifact, and one immutable ledger row |
| MiniMax Anthropic compatibility | at most three non-streaming Messages attempts per implementation or review request |

The integration suite uses disposable PostgreSQL 18.1 on
`127.0.0.1:55433`, backed by tmpfs and removed after every run. Production
pool sizing, retention, vacuum, partitioning, backup, and event payload limits
remain deployment concerns.

Execution defaults are 100 changed files, 1 MiB per file, 10 MiB total
replacement content, a five-minute applicator timeout, eight active worktrees,
and four concurrent executions. Deployment operators must still measure Git
object retention, artifact retention, pool sizing, and repository-specific
worktree size.

Verification permits at most 16 catalog checks, applies a ten-minute maximum
to each, runs checks sequentially within a verification, and defaults to two
concurrent verifications and four workspace slots. Operators must size the
artifact backend for retained logs and reports.

Phase 8 adds no worker container or external network load. Review requests and
proposals retain the 1 MiB gateway defaults. Approval concurrency is serialized
per deterministic approval ID; completed replay performs no artifact or
workflow work. Operators must size retention for review/approval artifacts and
vacuum the immutable reasoning and approval ledgers.

Phase 9 admits no worker pool and performs at most one Git push and one GitHub
PR creation for a publication ID. Git and HTTP calls default to a 30-second
timeout; artifact and API bodies default to 1 MiB and 64 KiB respectively.
Completed replay performs no Git or HTTP mutation. Operators must size remote
branch, PR, artifact, and immutable publication-row retention.

Phase 10 permits at most 20,000 output tokens and the lower of manifest and
request input/output budgets. Provider calls use a five-minute maximum timeout,
three attempts, bounded `Retry-After` (at most 30 seconds), and deterministic
250/500 ms fallback delays. Raw provider responses add one content-addressed
artifact per completed invocation; PostgreSQL adds only references, request ID,
model, token counters, and attempt count. Prompt caching policy, streaming,
tools, fallback models, and multi-turn continuation remain out of scope.
The pinned SDK increases offline Go compile/vet working data; the verification
image seeds those caches and the isolated worker limit is therefore 2 GiB
memory with 2 GiB tmpfs. Trusted catalog entries may select lower memory and
PID limits, which are propagated directly to the container profile.

## Phase 11 runtime

API request bodies and CAS objects default to 1 MiB. Repository implementation
context has explicit file and byte ceilings. The PostgreSQL worker holds one
short claim transaction, then performs work outside that transaction; retries
are bounded and back off to at most one minute. The first slice schedules one
task, so it introduces no cross-task leasing or unbounded queue fan-out.
External metrics and tracing backends remain deferred.

## Beta Slice 3 intake

Each fresh run intake performs one bounded Git tree walk and blob hashing pass
at the requested commit, then publishes two CAS objects: the existing bounded
intake body and one deterministic repository-map object containing path, kind,
and SHA-256 per committed blob. The acceptance transaction is short: it holds
one idempotency-row lock while writing one workflow command, initial event and
snapshot, one complete run binding, and one response. Git and CAS staging occur
before that transaction. Concurrent duplicates either replay or report the
live reservation and do not multiply repository snapshots after completion.
Operators must include repository-map objects in CAS retention sizing; unbound
objects from failed pre-transaction staging are safe garbage-collection
candidates.

Task-graph approval stages one bounded task artifact before its transaction,
then writes one workflow decision, one task snapshot and its events, one task
binding, and one deterministic start job in the same short transaction.
Rolled-back, unbound task artifacts are safe garbage-collection candidates;
replay does not multiply database rows or jobs.

Each accepted beta run additionally binds one small canonical policy object.
Execution is capped by changed-file, per-file, and total-byte ceilings;
execution and verification concurrency are independently bounded. The current
one-task scheduler prevents task fan-out from multiplying worktrees or provider
requests.
