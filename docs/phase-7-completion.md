# Phase 7 completion

Date: 2026-07-29

## Outcome

Phase 7 implements an in-process independent verification boundary. It
materializes the exact Phase 6 candidate from raw Git objects, resolves only
kernel-approved checks, executes them offline in a dedicated locked-down
container, stores bounded content-addressed evidence, calculates complete
acceptance coverage, and records the existing verification transition.

No Protobuf or workflow-state schema changed. Review/approval, publication,
providers, production artifact storage, secret scanning, merge, deployment,
and network services remain out of scope.

## Stack

| Branch | Scope |
|---|---|
| `phase07/check-resolution` | trusted catalog, request mappings, coverage and report types |
| `phase07/clean-runner` | shared raw-object materializer, worker image, Docker runner |
| `phase07/evidence-workflow` | artifact validation, evidence collection, workflow transition |
| `phase07/verification-ledger` | PostgreSQL reservation, checkpoint, replay and immutability |
| `phase07/hardening-docs` | adversarial/live tests, canonical integration gate and documentation |

## Security and recovery properties

- Unknown or unavailable checks fail before container or workflow activity.
- The initial `make-check-v1` resolves only to `["make", "check"]`.
- Check failures, timeouts, overflow, missing coverage, corrupt artifacts,
  cancellation, malformed worker results, and cleanup failures cannot advance
  the task.
- Logs, reports, coverage, image identity, and workflow evidence bind the exact
  candidate commit.
- Concurrent exact replay runs checks once. Evidence-ready recovery retries
  only the idempotent workflow command, and conflicting ID reuse fails.
- Migration `0010` rejects protected evidence/identity updates and all
  completed-row updates or deletions.

## Verification

The final gate is:

```bash
uv sync --frozen
make generate-check
make check
make integration-test
git diff --check
gh stack view --json
```

The integration target builds both worker images, uses disposable PostgreSQL,
and runs the repository aggregate check in the verification image against a
new raw-object materialization of the exact current commit.
