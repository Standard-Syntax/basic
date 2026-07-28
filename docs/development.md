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

`make integration-test` starts `postgres:18.1-alpine` on
`127.0.0.1:55433`, waits for health, applies embedded migrations, runs the
tagged transactional suite, and removes the disposable resources even on test
failure. Success ends with:

```text
ok github.com/Standard-Syntax/basic/go/internal/workflow
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

Generation uses `grpcio-tools==1.75.1` from `uv.lock` and installs
`protoc-gen-go@v1.36.10` into the ignored `.tools/bin` directory. It does not
use an unpinned system `protoc`.

Generated transport types live under `go/gen` and
`python/src/harness_agents/_generated`. Handwritten domain validation must not
be added to those directories.
