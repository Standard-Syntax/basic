# Run the agent harness live

This is the supported operator procedure for a real MiniMax-backed harness run
in this repository. It starts the `api-service` and `workflow-service`
processes, calls MiniMax-M3 for implementation and independent review, executes
and verifies the proposed change in isolated Docker workers, records a human
approval, and exercises draft-pull-request publication against a loopback
server.

The live target is a disposable end-to-end acceptance run. It creates its own
fixture Git repository, PostgreSQL database, API configuration, workflow
configuration, bearer principal, artifact store, worktrees, and loopback GitHub
server. It does not modify the checkout under test, publish to GitHub, merge,
deploy, or provide a persistent production installation.

## 1. Install the supported toolchain

Run this procedure on Linux from a fresh clone. The required host tools are:

- Go 1.26.x;
- Python 3.14.x;
- uv 0.11.x;
- GNU Make;
- Git;
- a running Docker Engine with the Compose plugin; and
- PostgreSQL client tools containing `pg_dump`.

The Docker daemon must be usable by the current user without adding `sudo` to
the Make commands. TCP port `127.0.0.1:55433` must be free. The first run also
needs network access to download locked Go and Python dependencies, pull
`postgres:18.1-alpine`, build the worker images, and call
`https://api.minimax.io/anthropic`.

Clone the repository if necessary, enter its root, and verify the installed
tools:

```bash
git clone git@github.com:Standard-Syntax/basic.git
cd basic

go version
python3.14 --version
uv --version
make --version
git --version
docker --version
docker compose version
pg_dump --version
docker info >/dev/null
```

The version output must match the supported versions above. `docker info` must
exit successfully. Install the locked Python environment:

```bash
uv sync --frozen
```

Do not substitute an unlocked `uv sync`, and do not manually install a system
`protoc`. Generation uses the locked `grpcio-tools` package and installs the
pinned Go plugin into `.tools/bin`.

## 2. Establish a deterministic baseline

Run the repository gate before supplying a provider credential:

```bash
make check
make runtime-e2e
```

Both commands must exit with status 0. `make runtime-e2e` uses deterministic
fake implementation and review adapters while exercising the same two service
processes, PostgreSQL migrations, isolated execution and verification workers,
restart recovery, approval, and loopback publication used by the live run.

`make runtime-e2e` begins and ends with:

```bash
docker compose down --volumes
```

It therefore deletes this Compose project's disposable PostgreSQL data. Do not
point this checkout's Compose project at a database that must be retained.

## 3. Configure the MiniMax credential

Obtain a MiniMax API key authorized to call MiniMax-M3 through the Anthropic
compatible endpoint. Read it without putting it in shell history, reject an
empty value, and export it for child processes:

```bash
read -rsp 'MiniMax API key: ' ANTHROPIC_API_KEY
printf '\n'
test -n "$ANTHROPIC_API_KEY"
export ANTHROPIC_API_KEY
```

The live runtime configuration is intentionally closed to these values:

```text
provider mode:  minimax_anthropic
base URL:       https://api.minimax.io/anthropic
model:          MiniMax-M3
credential:     ANTHROPIC_API_KEY
```

The Make target and test generate the strict JSON configuration files in a
private temporary directory. Do not create an `.env` file, add the key to JSON,
set `ANTHROPIC_MODEL`, or pass a key on a command line. The workflow service
reads `ANTHROPIC_API_KEY` for each provider invocation, and the test verifies
that the key is absent from generated configuration, process logs, artifacts,
and a PostgreSQL dump.

`make provider-smoke` is not a substitute for this procedure. That target
exercises the generic Anthropic endpoint and requires an independently selected
`ANTHROPIC_MODEL`; it does not configure the MiniMax runtime.

## 4. Run the live harness

From the repository root, run exactly:

```bash
make minimax-live-e2e
```

Do not invoke `make runtime-e2e` for live evidence: without
`MINIMAX_LIVE_E2E=1` it deliberately uses fake adapters. The
`minimax-live-e2e` target checks for the credential and sets that mode itself.

A successful run makes exactly two accepted reasoning invocations: one
implementation and one independent review. Each invocation is limited to one
provider request. The surrounding workflow:

1. builds `basic-execution-worker:runtime` and
   `basic-verification-worker:runtime`;
2. builds `.tools/runtime/api-service` and
   `.tools/runtime/workflow-service`;
3. starts disposable PostgreSQL on `127.0.0.1:55433`;
4. creates and approves specification and one-task planning inputs;
5. obtains a MiniMax-M3 implementation proposal;
6. applies the proposal in the isolated execution worker;
7. runs `make check` offline in the independent verification worker;
8. obtains a MiniMax-M3 review proposal;
9. restarts the workflow service and proves completed effects do not repeat;
10. records the test's explicit human approval and one loopback draft PR;
11. checks the exact candidate changes only `add.go` and passes `make check`;
12. verifies durable implementation, verification, review, approval,
    publication, usage, request-ID, artifact, event, and secret-absence
    evidence; and
13. stops PostgreSQL and removes its volume.

The command is successful only if it exits with status 0 and its final test
result includes:

```text
ok   github.com/Standard-Syntax/basic/go/internal/runtime
```

The exact elapsed time printed by `go test` varies. A provider call can take up
to five minutes; the harness does not silently skip a missing credential or a
provider failure.

After the command returns, remove the credential from the current shell:

```bash
unset ANTHROPIC_API_KEY
```

## 5. Recover from an interrupted or failed run

If the command is interrupted before its cleanup block runs, remove only this
repository's disposable Compose resources:

```bash
docker compose down --volumes
```

Then use the first matching diagnostic:

- `ANTHROPIC_API_KEY is required`: repeat the credential setup in the same
  shell used to invoke Make.
- Docker permission or connection error: start Docker and make
  `docker info` succeed as the current user.
- PostgreSQL health or bind error: make `127.0.0.1:55433` available, then run
  `docker compose logs postgres`.
- `Anthropic authentication`, `permission`, or `billing` error: correct the
  MiniMax account or key; the adapter intentionally omits response bodies and
  credentials from the error.
- `rate_limit`, `timeout`, `transport`, or provider error: preserve the failing
  output as evidence and retry only after the provider condition is understood.
- `runtime stage ... failed`: the provider output, proposal validation,
  execution, verification, or review did not satisfy the closed workflow. Do
  not report the run as live success.
- `pg_dump` not found or PostgreSQL secret-evidence failure: install a
  compatible PostgreSQL client and rerun; secret-absence verification is part
  of the live acceptance gate.

Do not call the run successful based only on image builds, service readiness,
provider connectivity, or a completed implementation call. Status 0 from
`make minimax-live-e2e` is the acceptance boundary.
