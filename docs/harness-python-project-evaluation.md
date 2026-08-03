# Harness installation and real Python project evaluation

Date: 2026-08-03
Source: `b22b888` (`harness/python-live-gate`)
Harness package: `harness-agents==0.2.0`

## Outcome

The requested end state was achieved. The current harness wheel installed, the
installed `harness-agents init` command created a fresh committed Python
project from an operator-owned specification and acceptance test, and the
credentialed `make beta-python-project-e2e` gate completed successfully.

This is materially stronger than the previous 0.1.0 evaluation. The dedicated
gate proves a generated Python target can pass live implementation and
independent review, isolated offline verification, explicit approval and
submission, idempotent replay, and disposable draft publication. It does not
prove a real GitHub canary, a human-made approval decision, or generic
operator-CLI execution against the preserved project.

## Artifacts

- Installed wheel: `dist/harness_agents-0.2.0-py3-none-any.whl`
- Wheel SHA-256:
  `0f34d028fc8963d3356b80d3d1c7863920855ed19d031d2d590b812577e1f03a`
- Preserved generated project:
  `.tools/harness-evaluation/generated-python-project-v0.2`
- Generated trusted-base commit:
  `d9f05f2ed72b10bfdb591ff2eedc4a82c62478bc`
- Preserved input specification and checks:
  `.tools/harness-evaluation/v0.2-input`

The `.tools` tree is intentionally ignored by Git. The preserved project is
back at its clean trusted-base commit after the evaluation. The live gate owns
a separate disposable generated repository and removes it on completion.

## Commands and observed results

### Install the current harness

```bash
uv sync --frozen
uv build --wheel
uv tool install --force dist/harness_agents-0.2.0-py3-none-any.whl
harness-agents --help
uv tool list
```

Result: success. The installed package is `harness-agents v0.2.0`, and help
lists the `compile`, `init`, and `operator` command groups.

### Create a real Python project with the installed harness

```bash
harness-agents init \
  /home/dscv/Repo/basic/.tools/harness-evaluation/generated-python-project-v0.2 \
  --project-spec \
  /home/dscv/Repo/basic/.tools/harness-evaluation/v0.2-input/project-spec.json \
  --checks \
  /home/dscv/Repo/basic/.tools/harness-evaluation/v0.2-input/checks
```

Result: success. The harness created a fresh `main` Git repository and one
trusted-base commit containing:

- a `src/`-layout Python package;
- exact development dependency pins and a packaged `uv.lock`;
- operator-owned acceptance tests;
- a fixed `make check` command covering Ruff, ty, pytest, and package builds;
- `.harness/project.json`, which permits writes only under `src` and prohibits
  model changes to checks, configuration, the lockfile, and harness metadata.

The bootstrap itself does not resolve dependencies or implement the objective.
Its initial `make check` therefore reached the acceptance test and failed as
expected on:

```text
NotImplementedError: implementation pending
```

This is expected scaffold state, not a bootstrap error.

### Run the credentialed generated-project lifecycle

```bash
make beta-python-project-e2e
```

Result: success. The live integration test reported:

```text
--- PASS: TestBetaLiveProcessesCompleteGeneratedPythonProject (32.88s)
PASS
ok github.com/Standard-Syntax/basic/go/internal/runtime 32.886s
```

The target rebuilt the execution and verification workers, started disposable
PostgreSQL, invoked MiniMax-M2.7 for implementation and independent review, and
then removed PostgreSQL and its Compose network. No Compose service remained.

The passing test asserts all of the following:

1. `harness-agents init` creates the disposable target before any model call.
2. Run intake, specification approval, and task-graph approval are durable.
3. Live implementation changes exactly `src/live_demo/__init__.py`.
4. Isolated offline `make check` passes on the exact candidate commit.
5. Independent live review reaches `AWAITING_APPROVAL`.
6. Approval and publication are separate mutations.
7. The exact candidate is published to one disposable branch and draft endpoint.
8. API/workflow restarts and repeated idempotency keys repeat no side effects.
9. Durable reasoning, execution, verification, review, approval, publication,
   and workflow evidence exists.
10. The provider credential is absent from reachable logs, artifacts, database
    evidence, and Git objects.

### Validate the generated console entry point

For this focused check only, the preserved scaffold implementation was changed
to the same accepted behavior (`main() -> "ready"`), tested, and then restored
to its clean trusted-base state.

```bash
make check
uv run --frozen harness-generated-demo
```

`make check` passed and built both distributions. The console command printed
`ready` but exited with status `1` because the generated project maps the
console script directly to a function returning a string. Python console-script
wrappers pass that return value to `sys.exit`, where a string is treated as an
error message. This is a real template defect not covered by the live gate.

## Errors, gaps, and improvements

### P0 - A passing project can still have a failing generated console command

The template emits:

```toml
[project.scripts]
harness-generated-demo = "harness_generated_demo:main"
```

The accepted objective requires `main()` to return `"ready"`. Consequently,
all trusted checks pass while the installed command exits `1`. Generate a
separate `cli() -> None` wrapper that prints or otherwise consumes the domain
return value, and point `[project.scripts]` at that wrapper. Add an acceptance
test that installs the wheel and asserts the console command's stdout and exit
status.

### P1 - Successful live evidence is ephemeral and minimally reported

The passing target prints only the Go test name and duration. Its temporary
repository, run ID, candidate commit, artifact digests, stage timings, and
redacted lifecycle summary disappear with the test directory. Emit a
machine-readable redacted report to a caller-selected output path on both pass
and failure. Optionally preserve the generated target when explicitly
requested for diagnosis.

### P1 - The dedicated gate is objective-specific, not project-generic

The target hard-codes the `live-demo` specification, acceptance test, objective,
and expected changed file. It proves the supported generated-project profile,
but it does not run the preserved project or accept an operator-provided
specification/check directory. Add bounded `PROJECT_SPEC`, `CHECKS`, and report
output parameters while retaining the fixed profile as a reproducible golden
case.

### P1 - The installed operator CLI is not the path exercised by this gate

The test drives the control API from Go. It does not prove that an operator can
use the installed `harness-agents operator` commands to run, approve, submit,
inspect, and export this project. Add a process-level acceptance profile that
uses only the installed CLI and its documented state file across an API restart.

### P1 - Some lifecycle authority remains test-fixture supplied

Specification and task-graph proposals are constructed directly by the test,
and test actors perform the approval calls. The gate provides real model-backed
implementation and review evidence and proves the approval boundary, but it
does not provide live specification/planning or a real human decision. Keep
that distinction explicit in release claims.

### P2 - Publication is disposable, not a real GitHub canary

This gate uses a local bare remote and loopback pull-request endpoint. That is
appropriate for repeatable project acceptance, but it cannot establish GitHub
credential, permission, draft-PR, or cleanup behavior. Only the separate
operator-provisioned `beta-canary-e2e` can provide that evidence.

### P2 - Docker uses the deprecated legacy builder

Both worker builds succeeded, but Docker warned that the legacy builder is
deprecated. Migrate the targets to BuildKit/buildx and keep immutable image-ID
recording so this warning does not become a future hard failure.

## Acceptance status

| Requirement | Status | Evidence |
| --- | --- | --- |
| Install current harness | Pass | Wheel built and installed as `harness-agents v0.2.0` |
| Have harness create a Python project | Pass | Fresh committed `src/` package generated by installed `init` |
| Establish operator-owned checks and policy | Pass | Trusted test, `make check`, lockfile, and protected paths committed before model use |
| Execute live implementation and review | Pass | Credentialed generated-project test passed in 32.88 seconds |
| Verify and build exact candidate | Pass | Gate reran offline `make check`; exactly one source file changed |
| Approve and publish exact candidate | Pass, disposable | Separate approval/submission and loopback draft publication asserted |
| Preserve replay and credential safety | Pass | Restart, idempotency, side-effect, and secret-absence assertions passed |
| Run generated console command successfully | Fail | Command prints `ready` but exits `1` |
| Exercise installed operator CLI end to end | Not run | Gate uses its Go runtime client directly |
| Publish a real GitHub draft | Not run | Requires the separate credentialed canary profile |
