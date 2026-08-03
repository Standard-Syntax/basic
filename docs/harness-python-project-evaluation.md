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

## Hardening acceptance

The five-layer hardening stack was exercised at source commit `08f4a92` on
2026-08-03. The first credentialed attempt exposed an operator configuration
defect before any provider call: the CLI had been given the publication token
file and received `401 unauthenticated`. The operator layer was corrected to
use its dedicated API credential file, the dependent commits were rebased, and
both required credentialed profiles then passed:

```bash
make beta-python-project-e2e \
  REPORT_OUTPUT=/home/dscv/Repo/basic/.tools/evidence/python-project-golden-report.json

make beta-python-project-e2e \
  PROJECT_SPEC=/home/dscv/Repo/basic/.tools/evidence/operator-alt-input/project-spec.json \
  CHECKS=/home/dscv/Repo/basic/.tools/evidence/operator-alt-input/checks \
  REPORT_OUTPUT=/home/dscv/Repo/basic/.tools/evidence/python-project-operator-alt-report.json
```

The golden profile passed in 21.44 seconds and the different operator-provided
`operator_alt` project passed in 28.28 seconds. Both reports have mode `0600`,
status `passed`, installed harness version `0.2.0`, complete base/candidate/run
and task identifiers, successful trusted checks, and one replay each for run,
candidate approval, and publication. The golden report also records a
successful console check; the custom profile intentionally has no golden-only
console assertion.

`make integration-test` and `make beta-images` also passed through Buildx
v0.36.0. These results do not provide live specification/planning, a real human
approval decision, or a real GitHub canary.

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

The findings below originated in the `b22b888` evaluation. Their status after
the hardening stack is recorded inline; live claims remain pending until the
corresponding credentialed command has actually run.

### Resolved P0 - Generated console command returned a failure status

The template emits:

```toml
[project.scripts]
harness-generated-demo = "harness_generated_demo:main"
```

The accepted objective requires `main()` to return `"ready"`. In the evaluated
revision, all trusted checks passed while the installed command exited `1`.
The generator now emits a separate `cli() -> None` wrapper, points
`[project.scripts]` at that wrapper, installs the generated wheel into an
isolated environment, and asserts stdout `ready` with exit status `0`.

### Resolved P1 - Durable redacted evidence

The passing target prints only the Go test name and duration. Its temporary
repository, run ID, candidate commit, artifact digests, stage timings, and
redacted lifecycle summary disappear with the test directory. Emit a
The gate now atomically writes a mode-`0600`
`harness_python_project_e2e_report.v1` report on pass or in-process failure and
can preserve only a fully scanned generated repository at an explicitly safe,
nonexistent destination. Deterministic shape, redaction, replacement, failure,
and preservation tests pass. Both credentialed profiles produced passing,
mode-`0600` reports.

### Resolved P1 - Operator project inputs

The target hard-codes the `live-demo` specification, acceptance test, objective,
and expected changed file. It proves the supported generated-project profile,
but it does not run the preserved project or accept an operator-provided
specification/check directory. The target now accepts a validated paired
`PROJECT_SPEC` and `CHECKS`, commits them through the installed package, and
derives the bounded lifecycle from `.harness/project.json`; the fixed profile
remains the reproducible golden case. Both the golden profile and the distinct
`operator_alt` profile passed the credentialed gate.

### Resolved P1 - Installed operator CLI

The test drives the control API from Go. It does not prove that an operator can
The process test now builds and installs the current wheel, then routes run,
three approvals, submit, status, and export through that exact executable and
its mode-`0600` state/configuration files across an API restart. PostgreSQL is
read only for waits and immutable-evidence assertions. Both credentialed
profiles completed through this installed-CLI path.

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

### Resolved P2 - Docker used the deprecated legacy builder

Every repository image build now requires Docker Buildx exactly `v0.36.0` and
uses `docker buildx build --load --iidfile`. The wrapper compares the emitted
IID with the post-load inspected image ID. Missing/mismatched version tests and
`make beta-images` passed with the pinned builder.

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
| Run generated console command successfully | Pass | Installed generated wheel prints `ready` and exits `0` |
| Accept operator-provided project inputs | Pass | Golden and distinct `operator_alt` credentialed profiles passed |
| Persist redacted lifecycle evidence | Pass | Both credentialed runs emitted mode-`0600` passing reports; failure/redaction/preservation tests pass |
| Exercise installed operator CLI end to end | Pass, automated approvals | Both credentialed profiles used installed wheel across restart and replay |
| Build repository images with pinned Buildx | Pass | Missing/version/IID tests and `make beta-images` passed with v0.36.0 |
| Publish a real GitHub draft | Not run | Requires the separate credentialed canary profile |
