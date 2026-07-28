# Resource analysis

Phase 2 adds PostgreSQL metadata persistence. The committed runtime still starts
no service and performs no external work.

| Area | Initial expectation |
|---|---|
| CPU | one Go/Python build process per developer or CI job |
| Memory | under 2 GiB for the aggregate check |
| Disk | source, dependency caches, and generated bindings; no runtime data |
| Network | dependency bootstrap only; no runtime network |
| Database | one snapshot plus one command and event row per accepted transition; dependency edges scale with the approved DAG |
| Artifacts | URIs and SHA-256 digests only; bodies, logs, and responses remain external |
| Model tokens | zero; no provider exists |
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
