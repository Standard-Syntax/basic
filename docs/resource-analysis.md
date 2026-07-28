# Resource analysis

Phase 2 adds PostgreSQL metadata persistence. Phase 3 adds an offline Python
authoring SDK, one packaged JSON Schema, four small definitions and prompts, and
canonical manifest fixtures. Phase 4 adds small canonical manifest rows and
indexed identity/digest lookup. Phase 5 adds one in-process fake proposal,
content-addressed artifact ports, and one small immutable invocation row per
request. The committed runtime still starts no service and performs no
external work.

| Area | Initial expectation |
|---|---|
| CPU | one Go/Python build, local manifest compilation, or deterministic in-process fake proposal per caller |
| Memory | under 2 GiB for the aggregate check; request and proposal each default to at most 1 MiB |
| Disk | source, dependency caches, generated bindings, wheel, and small manifest fixtures; no runtime artifact bodies |
| Network | dependency or wheel installation only; manifest compilation has no runtime network path |
| Database | workflow metadata, one immutable row per registered agent version, and one immutable reasoning row per request ID |
| Artifacts | request and proposal bodies flow through an external content-addressed port; PostgreSQL stores only URIs and SHA-256 digests |
| Model tokens | zero real-provider tokens; fake usage counters are deterministic metadata |
| Concurrency | workflow row locks plus advisory locks per agent identity and reasoning request ID; task execution remains deferred |

The integration suite uses disposable PostgreSQL 18.1 on
`127.0.0.1:55433`, backed by tmpfs and removed after every run. Production
pool sizing, retention, vacuum, partitioning, backup, and event payload limits
remain deployment concerns.

Phase 5 adopts 1 MiB defaults for request and proposal transports. Before
side-effect execution, configuration must also bound
changed files, single-file size, total replacement content, context and output
tokens, task and test duration, retries, active worktrees, and concurrent tasks.
Recommended conservative starting values require measurement before adoption:
100 files, 1 MiB per file, 10 MiB total replacement content,
100,000 context tokens, 20,000 output tokens, 30-minute tasks, 15-minute tests,
3 retries, 8 worktrees, and 4 concurrent tasks.
