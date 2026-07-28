# Phase 0–1 completion report

Date: 2026-07-27

## Outcome

The local, unpublished stack implements the repository foundation, immutable
agent manifest compiler, and provider-neutral specification, planning,
implementation, rejection, and advisory review contracts. It does not implement
workflow execution, persistence, worktrees, file mutation, check execution,
providers, runtime servers, pull requests, merge, or deployment.

## Stack and files changed

| Branch | Commit | Primary files |
|---|---|---|
| `phase01/foundation` | `a0f0893`, `8336059` | `Makefile`, `go.work`, `go/`, `pyproject.toml`, `uv.lock`, `scripts/`, `.github/workflows/check.yml`, foundation documentation |
| `phase01/agent-manifest` | rebased `feat: compile immutable agent manifests` | `schemas/agent-manifest-v1.schema.json`, `python/src/harness_agents/{manifest,cli}.py`, `go/internal/manifest/`, manifest fixtures and tests |
| `phase01/specification-contract` | rebased `feat: define specification reasoning contract` | `common.proto`, `specification.proto`, generated bindings, Go mappings, cross-language fixtures and tests |
| `phase01/planning-contract` | rebased `feat: validate bounded task graph proposals` | `planning.proto`, graph/scope validation, planning fixtures and tests |
| `phase01/implementation-contract` | rebased `feat: validate structured implementation proposals` | `implementation.proto`, complete-file operation validation, rejection-code tests, fixtures |
| `phase01/review-and-integration` | rebased `feat: complete review contract matrix` | `review.proto`, advisory review/evidence mappings, Go-generated fixtures, unknown-field tests, compatibility documentation |

Generated Go bindings are under `go/gen/harness/reasoning/v1`. Generated
Python bindings are under
`python/src/harness_agents/_generated/harness/reasoning/v1`. Shared binary
fixtures are under `tests/contracts/v1`.

## Commands and observed results

The following commands were executed from `/home/dscv/Repo/basic`:

```bash
uv lock
cd go && go mod tidy
uv sync --frozen
make generate
make check
```

The final top-of-stack `make check` result:

- generated Go, Python, and cross-language fixtures reproduced with no diff;
- `gofmt`, `go vet`, `ruff format --check`, and `ruff check` passed;
- all Go packages built and tested;
- Python collected 19 tests and all 19 passed;
- all six Go service command skeletons built;
- the Python package import smoke passed.

Every branch was then checked out independently and the same aggregate gate was
run:

```text
phase01/foundation: make check passed
phase01/agent-manifest: make check passed
phase01/specification-contract: make check passed
phase01/planning-contract: make check passed
phase01/implementation-contract: make check passed
phase01/review-and-integration: make check passed
```

`git diff --check` passed throughout. The final validation is repeated after
this report is committed.

Fresh independent review and security audit were repeated after remediation.
Both returned `PASS` on the final contract changes. The security pass is limited
to the manifest and reasoning-contract layer; artifact-store provenance,
execution enforcement, and workflow authorization remain deferred.

## Acceptance coverage

- Repeated and differently ordered manifest definitions compile to identical
  RFC 8785 bytes and lowercase SHA-256 digests.
- Exact prompt UTF-8 bytes are digest-addressed by
  `artifact://sha256/<digest>`.
- Closed manifest objects reject unsupported stages, unsafe permissions,
  provider URLs, authority/scope additions, missing required fields, and
  non-finite model parameters.
- Python-produced fixtures map in Go and Go-produced fixtures deserialize in
  Python for all four reasoning stages.
- Elevated authority, malformed envelopes, stale identity, invalid paths and
  digests, graph cycles, unknown dependencies, invalid scope relationships,
  missing criterion coverage, and undeclared checks fail without partial domain
  results.
- All five stable rejection codes and all three complete-file operations
  round-trip in both transports.
- Review evidence is bound to the candidate commit and review recommendation is
  explicitly advisory; the proposal has no approval field.
- Protobuf unknown fields are preserved and generated code is reproducible.

## Assumptions

- Go 1.26.x, Python 3.14.x, uv 0.11.x, GNU Make, and dependency network access
  during initial bootstrap are available.
- The current `harness.reasoning.v1` field numbers, enum values, schema ID, and
  digest rules become compatibility-stable when merged.
- Static service command skeletons are sufficient for Phase 0; no server is
  started.

## Deviations and unresolved issues

- The plan requested local stack creation without publication, so GitHub Actions
  did not run. The committed workflow executes the same frozen `make check`
  gate used locally; remote CI status is therefore unverified.
- `protoc` is supplied by locked `grpcio-tools`, while `protoc-gen-go` is pinned
  by exact version and installed into `.tools`; no system `protoc` or remote
  Protobuf plugin is required.
- No runtime configuration limits were added because their consumers are
  deferred; conservative initial values and required future knobs are recorded
  in `docs/resource-analysis.md`.

## Recommended next phase

Do not add a provider or execution runtime next. First audit these v1 contracts,
then begin Phase 2 as a separate approved stack implementing only the Go kernel
run/task aggregates, legal transitions, append-only event intent, and
invalid-transition tests without database or external side effects.
