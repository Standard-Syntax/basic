# Resource analysis

Phase 0–1 runs only local builds, compilation, and tests. The service commands
perform no work and consume no persistent resources.

| Area | Initial expectation |
|---|---|
| CPU | one Go/Python build process per developer or CI job |
| Memory | under 2 GiB for the aggregate check |
| Disk | source, dependency caches, and generated bindings; no runtime data |
| Network | dependency bootstrap only; no runtime network |
| Database | none; later metadata growth must be measured per run/task/event |
| Artifacts | committed fixtures only; later logs/responses stored out of rows |
| Model tokens | zero; no provider exists |
| Concurrency | one local validation job; runtime tasks deferred |

Before runtime implementation, configuration must bound maximum proposal bytes,
changed files, single-file size, total replacement content, context and output
tokens, task and test duration, retries, active worktrees, and concurrent tasks.
Recommended conservative starting values require measurement before adoption:
1 MiB proposals, 100 files, 1 MiB per file, 10 MiB total replacement content,
100,000 context tokens, 20,000 output tokens, 30-minute tasks, 15-minute tests,
3 retries, 8 worktrees, and 4 concurrent tasks.
