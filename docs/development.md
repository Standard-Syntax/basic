# Development

## Supported toolchain

- Go 1.26.x
- Python 3.14.x
- uv 0.11.x
- GNU Make
- Docker with Compose (integration tests only)

Install locked Python dependencies, then run the aggregate gate:

```bash
uv sync --frozen
make check
make integration-test
```

`make check` runs generation reproducibility, formatting, lint, Python source
type checks, Go and Python tests, all Go command builds, and a Python import
smoke test. Generated Protobuf Python modules are excluded from the source type
check because their runtime-defined attributes do not ship static type stubs.

`make integration-test` builds the static execution worker and the
dependency-seeded verification image, starts
`postgres:18.1-alpine` on
`127.0.0.1:55433`, waits for health, applies embedded migrations, runs the
tagged workflow, registry, reasoning-gateway, execution, and verification
suites against one shared database, exercises both live isolated workers,
runs the repository's `make check` offline against a newly materialized exact
commit, and removes the disposable resources even on test failure. The
packages may migrate in any order. Success includes:

```text
ok github.com/Standard-Syntax/basic/go/internal/workflow
ok github.com/Standard-Syntax/basic/go/internal/registry
ok github.com/Standard-Syntax/basic/go/internal/reasoning/gateway
ok github.com/Standard-Syntax/basic/go/internal/execution
ok github.com/Standard-Syntax/basic/go/internal/verification
```

If port 55433 is occupied, stop the conflicting process or change both the
Compose mapping and the Make target URL. Use `docker compose logs postgres` for
startup errors and `docker compose down --volumes` after an interrupted run.

Focused commands:

```bash
make generate        # regenerate committed Go and Python Protobuf bindings
make generate-check  # regenerate and compare with committed bindings
make lint
make type-check
make test
make build
make integration-test
make provider-smoke     # requires ANTHROPIC_API_KEY and ANTHROPIC_MODEL
```

The historical `make runtime-e2e` and `make minimax-live-e2e` targets still
contain the pre-beta mixed fake/live process fixture. They are not supported
Beta Slice 1 evidence: strict production configuration rejects the fake path,
and Slice 2 owns the live fixture and target redesign. See
[Run the agent harness live](live-harness.md).

## Installed manifest compiler

Build and install the SDK wheel, then invoke the CLI from any directory:

```bash
uv build --wheel
uv tool install --force dist/harness_agents-0.1.0-py3-none-any.whl
cd /tmp/agent-authoring
harness-agents compile ./implementation.json \
  --output ./implementation.manifest.json \
  --digest-output ./implementation.manifest.sha256
```

`prompt_file` in the definition is resolved relative to the definition file.
The default v1 schema comes from the installed wheel. Use
`--schema /absolute/path/to/additional.schema.json` only to add site-specific
restrictions; it cannot loosen the bundled v1 validation.

Compilation performs local reads of the definition, prompt, and optional schema
and local writes of the two requested outputs. It makes no provider, network,
database, Git, shell, registration, or workflow call. Authoring and I/O errors
print to stderr and return exit code `2`; neither output is attempted until
validation succeeds.

Generation uses `grpcio-tools==1.75.1` from `uv.lock` and installs
`protoc-gen-go@v1.36.10` into the ignored `.tools/bin` directory. It does not
use an unpinned system `protoc`.

Generated transport types live under `go/gen` and
`python/src/harness_agents/_generated`. Handwritten domain validation must not
be added to those directories.

## In-process reasoning gateway

`go/internal/reasoning/gateway` is a library boundary, not a daemon. Construct
it with an exact manifest resolver, production MiniMax-compatible adapter,
content-addressed artifact store, invocation repository, and clock, then call
`Service.ProposeImplementation`. `NewService` applies 1 MiB request, proposal,
and provider-response defaults; pass one `ByteLimits` value to configure
smaller or larger positive limits.

There is intentionally no production artifact-store implementation in Phase 5.
The integration tests use an integrity-checking in-memory store while
PostgreSQL persists only artifact references and invocation metadata. The
`go/cmd/reasoning-gateway` package builds but starts no listener.

## MiniMax Anthropic-compatible implementation and review adapters

Construct `NewAnthropicImplementationAdapter` or
`NewAnthropicReviewAdapter` with a request-local `CredentialSource`, trusted
`CapabilityModelResolver`, and the same backend-neutral `ArtifactStore` used by
the gateway. Shipped workflow composition accepts only
`minimax_anthropic`, `https://api.minimax.io/anthropic`, `MiniMax-M2.7`, and
`ANTHROPIC_API_KEY`; omitted provider configuration normalizes to that exact
profile. Gateway, kernel, workflow, Protobuf, and manifest contracts do not
change.

The official SDK is pinned at `v1.61.0`. SDK retries are disabled; the adapter
owns the bounded retry policy and five-minute timeout. Production callers must
provide a real content-addressed artifact backend and credential source. Do not
put API keys in manifests, artifacts, command arguments, logs, or database
configuration. `make check` also runs `scripts/no-fake-provider-adapters.sh`,
which rejects removed fake symbols, proposal-path loaders, and alternate
production provider branches.

The local suite uses loopback HTTP and no provider credential. Run the explicit
live smoke separately:

```bash
ANTHROPIC_API_KEY='...' ANTHROPIC_MODEL='...' make provider-smoke
```

This performs exactly one real implementation invocation and one real review
invocation and validates both against the unchanged kernel mappings. It never
silently skips: missing variables or either provider failure make the target
fail. The live smoke is not part of `make check` or `make integration-test`.

## Isolated execution service

`go/internal/execution.Service` is an in-process boundary. Configure absolute
trusted repository/worktree roots, the worker image, non-root UID/GID,
execution actor UUID, deterministic author identity, content-addressed artifact
store, workflow store, and execution ledger. Call `Execute` with the existing
v1 request/proposal transports, exact proposal artifact, active lease, expected
task revision, execution UUID, and stable execution timestamp.

Build the private worker directly with:

```bash
docker build -f Dockerfile.execution-worker \
  -t basic-execution-worker:integration .
```

The worker is not a network service. Interrupted runs may be inspected with
`git worktree list --porcelain` and `git show-ref
refs/harness/candidates/`. Normal cancellation and failure paths remove
abandoned worktrees; only successfully workflow-recorded candidates retain an
internal ref.

## Independent verification service

`go/internal/verification.Service` is an in-process boundary. Supply the
verification actor UUID, immutable catalog, content-addressed artifact store,
workflow command store, clean-workspace preparer, check executor, and
verification ledger. `Verify` accepts a stable verification UUID/timestamp,
the validated v1 implementation request, exact Phase 6 report artifact and
candidate commit, expected task revision, and kernel-selected
criterion-to-check mappings.

Build the dedicated image directly with:

```bash
docker build -f Dockerfile.verification-worker \
  -t basic-verification-worker:integration .
docker image inspect --format '{{.Id}}' \
  basic-verification-worker:integration
```

The initial catalog entry is `make-check-v1`, which always resolves to
`["make", "check"]`. The image contains dependencies locked by `go.sum` and
`uv.lock`, seeded Go build and vet caches copied into bounded runtime scratch, and
`protoc-gen-go@v1.36.10`; runtime execution is offline.
`go/cmd/verification-service` and `go/cmd/verification-worker` open no
listeners.

## Trusted review and human approval

Construct `gateway.NewReviewService` with the registered review manifest,
the production MiniMax-compatible `AnthropicReviewAdapter`,
content-addressed artifact store, invocation repository, and clock. Pass that
gateway to `review.NewService` with a review-service actor UUID and workflow
store. `Review` requires exact Phase 6 and Phase 7 report artifacts, the frozen
v1 request, kernel resource labels, and expected task revision.

Construct `approval.NewService` with the same content-addressed store, workflow
store, and optionally `PostgresApprovalRepository`. Call `ApproveTask` or
`RequireTaskRework` with a trusted principal, actual changed paths, exclusive
resource labels, exact candidate/upstream references, and expected revision.
There is no listener or authentication middleware in Phase 8.

`make integration-test` applies migrations `0011` and `0012` in disposable
PostgreSQL and includes the approval repository concurrency, replay, rollback,
and immutability suite.

## Draft pull request publication

Construct `publication.Service` with trusted repository coordinates, remote and
base branch, `harness/` prefix, publication actor UUID, content-addressed
artifact store, workflow store, `GitCommandPublisher`, `GitHubRESTClient`, and
`PostgresPublicationRepository`. Supply a stable publication UUID/timestamp,
run ID/revision, reviewed base and candidate commits, and exact specification,
implementation, execution, verification, review, and approval references.

The credential source returns an ephemeral token per request. Do not place the
token in the endpoint, environment inherited by Git, artifact store, or
database. Production endpoints must use HTTPS. Normal tests use a local bare
remote and `httptest`; they do not contact GitHub.

`make integration-test` also applies migration `0013` and runs publication
checkpoint, conflict, rollback, concurrency, migration-digest, trigger, bare
Git, and loopback REST tests.

## Phase 11 processes

Both processes accept `-config /absolute/path/config.json`. Configuration is
strict JSON: unknown fields and trailing documents are rejected. The API
configuration includes a loopback listener, PostgreSQL URL, absolute CAS root,
service actor UUID, body limits, and principals with UUID identity, SHA-256
token digest, and roles. Workflow configuration includes the PostgreSQL URL,
absolute CAS root, worker-owner UUID, artifact limit, and the closed
MiniMax-M2.7 provider profile. Unknown fields, fake proposal fields, fake mode,
alternate endpoints/models/credential names, and trailing JSON fail closed.
`ANTHROPIC_API_KEY` availability is checked before database or orchestration
setup and the environment is read again for every provider invocation.

```bash
cd go
go run ./cmd/api-service -config /absolute/path/api.json
go run ./cmd/workflow-service -config /absolute/path/workflow.json
```

Every mutation supplies `Authorization: Bearer ...`, a UUID
`Idempotency-Key`, and a `decision_timestamp`. Existing resources also require
`If-Match: "<revision>"`. The old process-E2E targets are historical until
Beta Slice 2 replaces their mixed fake/live fixture. Keep `make provider-smoke`
separate: it uses the generic Anthropic endpoint and is not MiniMax
runtime-E2E evidence.

`make generate` also compiles the four example agent manifests. The generation
check compares their canonical JSON and digest sidecars with the committed
fixtures, while Go and Python tests verify cross-language digest equality.
