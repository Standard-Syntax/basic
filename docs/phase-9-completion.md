# Phase 9 completion

Date: 2026-07-29

## Outcome

Phase 9 publishes one approved run candidate to deterministic
`harness/<run-id>`, creates one GitHub draft pull request, stores
`draft_pull_request.v1`, emits `DRAFT_PULL_REQUEST_CREATED`, increments the run
revision, and leaves the run `MERGE_READY`. It never marks a PR ready, merges,
deletes a branch, deploys, or changes the public transport and reasoning
contracts.

## Stack

| Branch | Scope |
|---|---|
| `phase09/publication-workflow` | publication actor, run artifact, command, event, accepted-task gate, migration `0014` |
| `phase09/branch-publication` | consumer-owned ports, upstream validation, immutable leased Git push |
| `phase09/draft-pull-request` | deterministic renderer and bounded marked-draft REST client |
| `phase09/publication-ledger` | service recovery, migration `0013`, immutable four-state checkpoints |
| `phase09/hardening-docs` | bare-remote/`httptest` scenario, adversarial tests, documentation |

## Security and recovery properties

- All six upstream artifacts are integrity checked and their run, base,
  candidate, evidence, recommendation, decision, and digest bindings agree
  before an external mutation.
- Base drift fails before push and is rechecked before PR creation.
- The exact empty lease permits only initial branch creation. An equal branch
  recovers; any other branch value conflicts.
- Exact head/base lookup and the hidden publication-ID marker recover ambiguous
  PR creation. Closed, non-draft, mismatched, or duplicate PRs fail closed.
- The token is request-local. HTTPS, redirect blocking, explicit API version,
  timeouts, and bounded bodies constrain the REST boundary.
- `reserved → branch_ready → pr_ready → completed` preserves every external
  fact. Exact completed replay performs no Git or network mutation.

## Verification

The canonical final gate is:

```bash
make generate-check
make check
make integration-test
git diff --check
gh stack view --json
```

Normal tests use a local bare Git remote and loopback `httptest` API. No live
GitHub repository is mutated. A disposable live-repository smoke remains
optional and is not represented by these gates.
