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
make runtime-e2e      # credential-free PostgreSQL/CAS/reconciler recovery
make beta-live-e2e    # requires ANTHROPIC_API_KEY; complete live process gate
make beta-python-project-e2e # live generated-Python-project acceptance gate
make provider-smoke     # requires ANTHROPIC_API_KEY and ANTHROPIC_MODEL
```

`make runtime-e2e` is deterministic infrastructure evidence only. It recovers
an expired PostgreSQL stage claim through the reconciler, consumes a
pre-existing integrity-checked CAS artifact, and makes no reasoning-provider
request. It is not beta lifecycle completion. See
[Run the agent harness live](live-harness.md) for the credentialed gate.
The objective-specific `beta-python-project-e2e` target first bootstraps a new
repository from operator-owned checks, then requires live implementation and
review, offline verification, separate approval/submission, replay safety, and
disposable draft publication.

## Installed manifest compiler

Build and install the SDK wheel, then invoke the CLI from any directory:

```bash
uv build --wheel
uv tool install --force dist/harness_agents-0.2.0-py3-none-any.whl
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

The same installed wheel can establish a new trusted Python base:

```bash
harness-agents init /absolute/path/to/new-project \
  --project-spec /absolute/path/to/project-spec.json \
  --checks /absolute/path/to/operator-checks
```

The strict `harness_python_project.v1` specification contains `name`,
`package_name`, `objective`, and one or more unique `AC-NNN` acceptance
criteria. The checks directory contains only bounded regular UTF-8 `.py`
files. Bootstrap writes a fixed Python 3.13/3.14 package, exact dependency
pins, `make check`, immutable path metadata, and a packaged lockfile; creates a
fresh `main` repository with hooks and global Git configuration disabled; and
commits the trusted base. The destination must not already exist.

The operator client uses a strict absolute configuration:

```json
{
  "schema_version": "harness_operator_config.v1",
  "endpoint": "http://127.0.0.1:8080",
  "token_file": "/absolute/path/operator.token",
  "project_root": "/absolute/path/to/new-project"
}
```

The token file must be a regular owner-owned `0600` file. The endpoint must be
a loopback HTTP origin. Start the lifecycle from a clean trusted base and keep
the generated `0600` state file for idempotent recovery:

```bash
harness-agents operator run --config /absolute/path/operator.json \
  --project /absolute/path/to/new-project \
  --state-file /absolute/path/run-state.json \
  --idempotency-key 11111111-1111-4111-8111-111111111111
harness-agents operator approve --gate specification \
  --config /absolute/path/operator.json --state-file /absolute/path/run-state.json \
  --idempotency-key 22222222-2222-4222-8222-222222222222
harness-agents operator approve --gate task-graph \
  --config /absolute/path/operator.json --state-file /absolute/path/run-state.json \
  --idempotency-key 33333333-3333-4333-8333-333333333333
harness-agents operator approve --gate candidate \
  --config /absolute/path/operator.json --state-file /absolute/path/run-state.json \
  --idempotency-key 44444444-4444-4444-8444-444444444444
harness-agents operator submit --config /absolute/path/operator.json \
  --state-file /absolute/path/run-state.json \
  --idempotency-key 55555555-5555-4555-8555-555555555555
```

Specification approval submits the already stored, operator-derived one-task
graph but does not approve it. Each later command performs exactly one approval
or publication action. Use `operator status RUN_ID --config ...` for current
state and `operator export RUN_ID --config ... --output /absolute/support.json`
for an atomic redacted bundle.

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

The review `1.2.0` prompt contains one exact minimal advisory-accept object.
Malformed responses expose only structural diagnostics; inspect their class,
safe field name, or byte offset without copying raw provider text into logs.

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

Packaged beta operation, readiness, backup/restore, credential rotation, and
exact-image rollback are documented in `docs/beta-deployment.md`.

Both processes accept `-config /absolute/path/config.json`. Configuration is
strict JSON: unknown fields and trailing documents are rejected. The API
configuration includes a loopback listener, PostgreSQL URL, absolute CAS root,
a required clean absolute `repository_root`, service actor UUID, body limits,
`task_max_attempts` (omitted defaults to `2`, accepted range `2` through `10`),
and principals with UUID identity, SHA-256 token digest, and deduplicated roles
from `operator`, `approver`, and `elevated_approver`. Model output cannot alter
the attempt budget. The single-tenant beta read policy permits every
authenticated principal holding any one of those known roles to read runs,
events, stages, and pending approvals; mutation routes retain their narrower
role checks. The API
repository root is the source of the committed repository map bound by
`POST /v1/runs`; it must address the same repository used by the workflow
service. Workflow configuration includes the PostgreSQL URL,
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
`If-Match: "<revision>"`. Mutation routes accept only `POST`; read routes accept
only `GET`, and method failures return `405` with `Allow`. Final human approval
is `POST /v1/runs/{run_id}/approval` and records no Git or GitHub effect.
An operator separately invokes `POST /v1/runs/{run_id}/submit` with the new
`MERGE_READY` revision to publish the exact approval-bound candidate as a draft.
Both operations have independent idempotency keys and exact replay responses.
`GET /v1/runs/{run_id}/support-bundle` returns `support_bundle.v1` for an
authenticated operator. It is safe to retain with an incident because it
contains workflow evidence references and structural reasoning diagnostics,
never raw provider request or response content. Treat referenced CAS artifacts
as separately controlled evidence; the bundle does not retrieve them.
`make beta-live-e2e` is the full two-process MiniMax
gate: it builds both service binaries and worker images, starts disposable
PostgreSQL, uses a disposable Git repository and loopback GitHub publication,
and exercises implementation, execution, verification, review, restart,
human approval, separate draft submission, and exact replay of both operations. Keep
`make provider-smoke` separate: it uses the generic Anthropic endpoint and is
not MiniMax runtime-E2E evidence.

Run intake is the exception to the generic mutation composition: its workflow
`CreateRun`, initial event and snapshot, complete runtime binding, and exact
`201` response are one serializable transaction. Retry the same request bytes
with the same key after a lost response; `Idempotent-Replay: true` means no Git,
CAS, workflow, job, or provider work was repeated. Reusing the key with changed
bytes returns an idempotency conflict.

`make generate` also compiles the four example agent manifests. The generation
check compares their canonical JSON and digest sidecars with the committed
fixtures, while Go and Python tests verify cross-language digest equality.

## Production repository preflight

Run `make beta-preflight BETA_CONFIG=/absolute/path/to/beta-preflight.json`.
Exit 0 means ready, 1 means a readiness check failed, and 2 means strict JSON
configuration is invalid. Repository, artifact, execution-worktree, and
verification-workspace roots must already exist, be non-symlinked,
owner-controlled, and non-overlapping. The command verifies Git/remote-base
identity, empty harness worktree roots, exact Docker image IDs, already-applied
migrations, the MiniMax environment credential, and a regular `0600`
publication credential file. It never applies migrations or creates resources.

For the separately credentialed real GitHub gate and its exact cleanup command,
follow [Real GitHub beta canary](beta-canary.md). `make beta-live-e2e` remains the
loopback lifecycle gate; only a successful `make beta-canary-e2e
BETA_CONFIG=/absolute/path/to/canary.json` proves real GitHub publication.
The canary now runs the exact packaged service and worker images. After retaining
its final JSON evidence, follow [Beta release evidence and cut](beta-release.md)
and run `make beta-readiness RELEASE_MANIFEST=/absolute/path/to/release.json`.
