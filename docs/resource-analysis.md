# Resource analysis

Phase 2 adds PostgreSQL metadata persistence. Phase 3 adds an offline Python
authoring SDK, one packaged JSON Schema, four small definitions and prompts, and
canonical manifest fixtures. The committed runtime still starts no service and
performs no external work.

| Area | Initial expectation |
|---|---|
| CPU | one Go/Python build or local manifest compilation process per developer or CI job |
| Memory | under 2 GiB for the aggregate check; one manifest and schema in memory per compile |
| Disk | source, dependency caches, generated bindings, wheel, and small manifest fixtures; no runtime artifact bodies |
| Network | dependency or wheel installation only; manifest compilation has no runtime network path |
| Database | one snapshot plus one command and event row per accepted transition; dependency edges scale with the approved DAG |
| Artifacts | URIs and SHA-256 digests only; bodies, logs, and responses remain external |
| Model tokens | zero; the SDK defines policies but no provider or runtime agent exists |
| Concurrency | serializable transactions and aggregate row locks; task execution remains deferred |

The integration suite uses disposable PostgreSQL 18.1 on
`127.0.0.1:55433`, backed by tmpfs and removed after every run. Production
pool sizing, retention, vacuum, partitioning, backup, and event payload limits
remain deployment concerns.

Before runtime implementation, configuration must bound maximum proposal bytes,
changed files, single-file size, total replacement content, context and output
tokens, task and test duration, retries, active worktrees, and concurrent tasks.
Recommended conservative starting values require measurement before adoption:
1 MiB proposals, 100 files, 1 MiB per file, 10 MiB total replacement content,
100,000 context tokens, 20,000 output tokens, 30-minute tasks, 15-minute tests,
3 retries, 8 worktrees, and 4 concurrent tasks.
