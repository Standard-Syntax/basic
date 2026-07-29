# Phase 8 completion

Date: 2026-07-29

## Outcome

Phase 8 implements deterministic advisory review, trusted evidence
reconstruction and policy, authenticated task approval, and a recoverable
immutable PostgreSQL approval ledger. The published v1 Protobuf field numbers,
enums, fixtures, manifest mappings, and `review_proposal.v1` behavior are
unchanged.

Review recommendations cannot approve tasks. `HIGH`, `CRITICAL`, or explicit
rework moves a task to `REWORK_REQUIRED`; advisory acceptance stops at
`AWAITING_APPROVAL`. Only a distinct authenticated human approval can reach
`ACCEPTED`, and elevated work requires `elevated_approver`.

## Stack

| Branch | Scope |
|---|---|
| `phase08/fake-review` | deterministic review gateway and migration `0011` |
| `phase08/review-workflow` | evidence reconstruction, fixed policy, trusted report |
| `phase08/approval-policy` | roles, risk classification, human application API |
| `phase08/approval-ledger` | checkpoint/replay repository and migration `0012` |
| `phase08/hardening-docs` | cross-phase/adversarial tests and documentation |

## Security and recovery properties

- Execution diff, verification evidence, coverage, candidate, run/task/attempt,
  approved digests, and content-addressed artifacts are cross-checked before
  reviewer invocation.
- Review reports bind the request/proposal plus execution and verification
  references. Approval artifacts bind the approver, roles, decision, timestamp,
  risk, candidate, approved digests, and every upstream digest.
- Missing roles, forged evidence, candidate mismatch, corrupt artifacts,
  blocking findings, stale revisions, and conflicting replay cause no
  unauthorized transition.
- Exact concurrent replay returns one immutable logical decision.
  `decision_ready` recovery retries only the idempotent workflow command.

## Verification

The final gate is:

```bash
make generate-check
make check
make integration-test
git diff --check
gh stack view --json
```

The integration target builds both isolated worker images, uses disposable
PostgreSQL, and runs workflow, registry, reasoning, execution, verification,
and approval repository suites.
