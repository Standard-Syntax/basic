# Harness installation and real Python project evaluation

Date: 2026-08-02
Source: `3f8b066` (`beta/publication-client-hardening`)
Harness package: `harness-agents==0.1.0`

## Outcome

The harness installed successfully, and the installed SDK compiled an
implementation agent definition from outside this repository. A standalone
Python package was initialized, synced, imported, executed, and built into an
sdist and wheel.

The requested end state was **not achieved**: the harness did not create the
Python project. `harness-agents` is an offline manifest compiler with only a
`compile` subcommand. The repository's only turnkey live command is a Go
integration test with a hard-coded Go `add.go` fixture. That live lifecycle
reached implementation, execution, and verification, but MiniMax returned an
invalid review JSON response on every retry, so the task never reached human
approval or publication.

## Artifacts

- Installed harness wheel:
  `dist/harness_agents-0.1.0-py3-none-any.whl`
  (`sha256:55d7354abd008b5fb00b14aa2d85ab22746d93ce12c80ec912ea91ef051a3687`)
- Preserved evaluation project:
  `.tools/harness-evaluation/python-project`
- Evaluation project commit:
  `6aa5b81ef9aafd5ef96686dffd1224245895ba5d`
- Built evaluation wheel:
  `.tools/harness-evaluation/python-project/dist/harness_created_demo-0.1.0-py3-none-any.whl`
  (`sha256:414a3302b67dc808175f16e43f09ad7939e76cabe1506e1dd53a961586ffafb3`)
- Compiled implementation manifest:
  `.tools/harness-evaluation/python-project/harness/implementation.manifest.json`
- Manifest digest:
  `2c83c87f278c7370ec1a6e46b093f70654013595723deaa6c366792ac63e03e1`

The `.tools` tree is intentionally ignored by Git. The Python package is a
real, runnable package, but it was initialized with `uv`; only its agent
manifest was produced by the installed harness.

## Commands and observed results

### Install

```bash
uv sync --frozen
uv build --wheel
uv tool install --force dist/harness_agents-0.1.0-py3-none-any.whl
harness-agents --help
```

Result: success. `uv tool list` reports `harness-agents v0.1.0`. Help output
lists only `{compile}`.

### Exercise the installed SDK in a standalone project

```bash
uv init --package --name harness-created-demo /tmp/harness-python-project.RSOuhK
harness-agents compile harness/agents/implementation.json \
  --output harness/implementation.manifest.json \
  --digest-output harness/implementation.manifest.sha256
uv sync
uv build
uv run python -c 'import harness_created_demo; harness_created_demo.main()'
```

Result: success. The program printed `Hello from harness-created-demo!`, and
both package distributions were built. `uv` selected Python 3.13.14 for the
target package; the separately installed harness remains isolated on its own
Python version.

### Attempt project execution through the installed CLI

```bash
harness-agents run .
```

Result: exit 2.

```text
harness-agents: error: argument {compile}: invalid choice: 'run' (choose from 'compile')
```

### Run the credentialed live lifecycle

```bash
make beta-live-e2e
```

Result: Make exited 2 after the Go test failed. Worker images and service
binaries built successfully; disposable PostgreSQL became healthy. The
built-in Go fixture then completed these stages:

1. start
2. implementation request
3. live implementation reasoning
4. isolated execution
5. independent verification (`make check`)

Independent review failed closed. Five provider attempts were made with
backoffs of 1, 2, 4, and 8 seconds. Each was rejected as:

```text
REJECTION_CODE_SCHEMA_INVALID: provider response is not valid review JSON
```

The terminal failure evidence digest was
`541709f2c2ed4f564115bfade642d9f7471ab5493bd1cabd9385abc5b55bc355`.
No approval, candidate publication, or Python-project mutation occurred.
Compose cleanup completed and no service remained running.

## Errors and gaps

### P0 - No operator path from install to project creation

The package name and runbook imply an executable harness, but the installed
CLI only compiles declarative agent manifests. There is no `init`, `run`,
`submit`, `status`, `approve`, or artifact-inspection workflow. Running the
actual lifecycle requires knowing and invoking a Go integration-test target.

### P0 - The live gate is not project-generic

`make beta-live-e2e` creates `go.mod`, `add.go`, `add_test.go`, and a Makefile in
test code. Its policy, prompts, specification, task graph, path assertions, and
final diff assertion all name `add.go`. There is no supported argument for a
local repository, objective, readable/writable paths, or expected checks.
Consequently it cannot be pointed at the preserved Python project without
editing the test implementation.

### P0 - Live review is not reliable enough to complete the golden path

Implementation and verification succeeded, but review produced invalid JSON
on all five attempts. The review prompt asks for an advisory accept but does
not show a concrete minimal response example in the fixture. The failure is
terminal after retry exhaustion and prevents approval/publication even when
trusted verification has passed.

### P1 - The documented full lifecycle is partially preconstructed

The live test injects and approves a specification and task graph directly in
Go before starting the workflow service. It proves live implementation and
review, not live specification/planning or natural-language project creation.
This distinction should be explicit wherever the command is described as a
"full lifecycle."

### P1 - Failure evidence is difficult to inspect after the test exits

The terminal message exposes only a CAS digest. The integration test owns a
temporary artifact root that is removed during cleanup, leaving no supported
command to export a redacted failure artifact. The stage logs identify schema
invalidity but not enough response structure to diagnose why MiniMax violated
the review contract.

### P1 - A repository must already contain its verification entry point

The only trusted check is `make-check-v1`, which resolves to `make check`.
Creating a project from an empty repository is therefore not turnkey: a
Makefile and language-specific test/build setup must already exist and be
protected before the harness can verify generated files.

### P2 - Target-runtime compatibility is implicit

The SDK requires Python 3.14, while a newly initialized Python target used
Python 3.13. This works because `uv tool install` isolates the SDK, but the
documentation does not clearly separate the authoring CLI's Python requirement
from supported target-project runtimes.

## Recommended improvements

1. Add an operator CLI that accepts an existing clean repository, objective,
   policy/config file, and idempotency key, then exposes run status, approval,
   and redacted artifact export. Keep approval explicit rather than automatic.
2. Make the disposable live runner data-driven. Add a checked-in Python fixture
   with `pyproject.toml`, locked dependencies, source, tests, and `make check`,
   and run it in CI or as a separate credentialed acceptance profile.
3. Add a project-bootstrap mode or clearly rename the current behavior to
   "modify an existing prepared repository." If bootstrap is supported, define
   how verification files and the initial trusted base are established without
   granting model authority over checks.
4. Harden review structured output: include the exact closed response shape in
   the stage prompt, add a minimal valid example, record a safe structural
   diagnostic (content-block types, parse location, unknown field names), and
   test malformed live responses without retaining provider text in logs.
5. On live-gate failure, optionally copy redacted run metadata and failure
   artifacts to a caller-selected durable directory, while continuing to scan
   them for credential leakage.
6. Update the runbook to distinguish SDK installation/manifest compilation,
   deterministic runtime recovery, the hard-coded live fixture, a generic
   repository run, and a real GitHub canary. Today these are separate
   capabilities with materially different evidence.

## Acceptance status

| Requirement | Status | Evidence |
| --- | --- | --- |
| Install harness | Pass | Wheel built and installed as `harness-agents v0.1.0` |
| Use installed SDK outside repository | Pass | Manifest and SHA-256 sidecar generated |
| Produce runnable Python package | Pass, via `uv` | Import/run and sdist/wheel build succeeded |
| Have harness create the Python project | Fail | No CLI/runtime entry point; project initialized by `uv` |
| Execute live implementation | Pass on built-in Go fixture | Implementation, execution, and verification completed |
| Complete live lifecycle | Fail | Review JSON invalid after retry exhaustion |
| Mutate/approve/publish Python project | Not run | Current live gate cannot target it |
