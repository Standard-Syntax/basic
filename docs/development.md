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

`make integration-test` builds the static scratch worker, starts
`postgres:18.1-alpine` on
`127.0.0.1:55433`, waits for health, applies embedded migrations, runs the
tagged workflow, registry, reasoning-gateway, and execution suites against one
shared database, exercises the live isolated worker, and removes the disposable
resources even on test failure. The
packages may migrate in any order. Success includes:

```text
ok github.com/Standard-Syntax/basic/go/internal/workflow
ok github.com/Standard-Syntax/basic/go/internal/registry
ok github.com/Standard-Syntax/basic/go/internal/reasoning/gateway
ok github.com/Standard-Syntax/basic/go/internal/execution
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
```

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

## In-process fake reasoning gateway

`go/internal/reasoning/gateway` is a library boundary, not a daemon. Construct
it with an exact manifest resolver, deterministic fake adapter,
content-addressed artifact store, invocation repository, and clock, then call
`Service.ProposeImplementation`. `NewService` applies 1 MiB request/proposal
defaults; pass one `ByteLimits` value to configure smaller or larger positive
limits.

There is intentionally no production artifact-store implementation in Phase 5.
The integration tests use an integrity-checking in-memory store while
PostgreSQL persists only artifact references and invocation metadata. The
`go/cmd/reasoning-gateway` package builds but starts no listener.

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

`make generate` also compiles the four example agent manifests. The generation
check compares their canonical JSON and digest sidecars with the committed
fixtures, while Go and Python tests verify cross-language digest equality.
