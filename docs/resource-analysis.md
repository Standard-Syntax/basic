# Resource analysis

Phase 2 adds PostgreSQL metadata persistence. Phase 3 adds an offline Python
authoring SDK, one packaged JSON Schema, four small definitions and prompts, and
canonical manifest fixtures. Phase 4 adds small canonical manifest rows and
indexed identity/digest lookup. Phase 5 adds one in-process fake proposal,
content-addressed artifact ports, and one small immutable invocation row per
request. Phase 6 adds bounded detached worktrees, a static applicator container,
candidate Git objects, report artifacts, and one immutable execution row.

| Area | Initial expectation |
|---|---|
| CPU | one CPU per applicator container; at most four concurrent executions by default |
| Memory | 512 MiB per applicator container; request/proposal each default to at most 1 MiB |
| Disk | up to eight active worktrees plus candidate Git objects; report bodies remain external artifacts |
| Network | applicator containers use `--network none`; dependency installation remains a development concern |
| Database | workflow, registry, reasoning, and immutable execution metadata |
| Artifacts | request, proposal, and report bodies flow through an external content-addressed port |
| Model tokens | zero real-provider tokens; fake usage counters are deterministic metadata |
| Concurrency | four executions, eight worktrees, and one logical owner per execution ID by default |

The integration suite uses disposable PostgreSQL 18.1 on
`127.0.0.1:55433`, backed by tmpfs and removed after every run. Production
pool sizing, retention, vacuum, partitioning, backup, and event payload limits
remain deployment concerns.

Execution defaults are 100 changed files, 1 MiB per file, 10 MiB total
replacement content, a five-minute applicator timeout, eight active worktrees,
and four concurrent executions. Deployment operators must still measure Git
object retention, artifact retention, pool sizing, and repository-specific
worktree size.
