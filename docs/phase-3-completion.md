# Phase 3 completion report

Date: 2026-07-28

## Outcome

Phase 3 completes and hardens the existing Python agent SDK without changing
the agent-manifest v1 wire format, enums, canonicalization, artifact URI, or
digest rules. Manifest validation is unavoidable, the schema ships in the
wheel, the installed CLI works outside the repository, and the four reasoning
stages have reproducible example definitions, prompts, manifests, and digest
sidecars.

Python remains an offline-only authoring boundary. This phase adds no provider,
runtime Python agent, database access, Git/shell/network execution,
task-specific scope, registration API, workflow authority, or manifest v2
field.

## Stack

| Branch | Scope |
|---|---|
| `phase03/sdk-validation` | packaged schema, mandatory validation, stage/output pairing, Go parity |
| `phase03/manifest-cli` | closed parsing, portable default schema, safe output workflow, wheel smoke |
| `phase03/stage-catalog` | four definitions/prompts, generated fixtures, cross-language equality |
| `phase03/compatibility-docs` | negative compatibility coverage, boundary and workflow documentation |

The independent stack is rooted at the current `main`. It has not been pushed
and no pull requests were created.

## Files changed

- SDK and CLI: `python/src/harness_agents/manifest.py`,
  `python/src/harness_agents/cli.py`, and the packaged v1 schema.
- Compatibility reader: `go/internal/manifest/reader.go`.
- Catalog: `python/agents`, `python/prompts`, and
  `tests/contracts/v1/manifest`.
- Generation and tests: `scripts/generate_contract_fixtures.py`,
  Python SDK/CLI/catalog tests, and Go manifest reader tests.
- Integration documentation: agent manifest, development, architecture,
  security, resource analysis, README, and this report.

## Verification

Each implementation branch passed `make check` before its commit. The final
acceptance gate passed:

```text
make generate-check
make check
git diff --check
```

The final aggregate run completed all Go tests and builds and reported
`42 passed` for Python.

The wheel smoke builds the wheel, installs it into a fresh environment, changes
outside the repository, and invokes `harness-agents compile` using the bundled
default schema. Catalog tests repeat compilation with reordered set-valued
inputs. Go reads every Python-generated fixture and returns the exact committed
canonical bytes and lowercase digest.

## Assumptions and deviations

- The existing agent-manifest v1 schema and Phase 1 dataclass/CLI interfaces
  remain authoritative.
- An optional schema override is additive: the packaged v1 schema always runs
  first.
- Prompt files must decode as UTF-8, while hashing preserves their exact bytes.
- The stack was initialized after `main` fast-forwarded to include the merged
  Phase 2 work; no rebase conflict occurred.
- No scope deviation was required. Publication and the Phase 2 disposable
  PostgreSQL integration suite remain outside the Phase 3 acceptance gate.

## Unresolved issues and Phase 4 recommendation

No unresolved Phase 3 implementation issue remains. Phase 4 should introduce a
provider adapter and runtime reasoning boundary only after defining request
timeouts, retry/idempotency behavior, response-size limits, secret handling,
and immutable binding of provider responses to the exact request and agent
manifest digest. It must preserve proposal-only authority and keep all
authorization, mutation, verification, approval, and promotion decisions in
the Go kernel.
