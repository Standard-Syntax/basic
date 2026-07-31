# Phase 10 completion

> Historical phase report: DEC-023 supersedes the constructor-selected fake
> production path for the shipped beta. Current workflow composition is
> live-only MiniMax-M2.7.

Date: 2026-07-29

## Outcome

Phase 10 adds official Anthropic Messages adapters for implementation and
review without changing Protobuf, manifest v1, workflow authority, execution,
approval, or publication. Fake and Anthropic adapters remain constructor
choices behind the same consumer-owned gateway seams.

## Stack

| Branch | Scope |
|---|---|
| `phase10/provider-outcomes` | generalized budgets, raw response metadata, malformed-output result, migration `0016` |
| `phase10/anthropic-implementation` | SDK pin, trusted credential/model/artifact/context boundary, closed implementation schema |
| `phase10/provider-reliability` | bounded retries, typed provider errors, response/usage classification |
| `phase10/anthropic-review` | shared runtime and closed review projection |
| `phase10/hardening-docs` | loopback/PostgreSQL/adversarial tests, live smoke target, documentation |

## Boundary properties

- The SDK is pinned at `v1.61.0` and its retries are disabled.
- Model IDs are capability configuration, never manifest or model output.
- Prompt and input artifacts are SHA-256 verified and secret guarded.
- Requests are non-streaming, tool-free, single-turn Messages calls.
- Golden `1.1.0` prompt request capabilities remain kernel-mediated and are
  unavailable inside those tool-free calls.
- Provider schemas are internal, closed, and exclude trusted identity.
- At most three network attempts fit within request count, lifetime, caller,
  and configured timeout.
- Exact raw responses, provider request ID/model, aggregate token usage, and
  actual attempts are immutable replay evidence.
- Provider failures release unfinished reservations; deterministic malformed
  responses persist as `SCHEMA_INVALID`.
- Exact replay performs no manifest resolution, credential lookup, or network
  request.

## Verification

The canonical final local gate is:

```bash
make generate-check
make check
make integration-test
git diff --check
gh stack view --json
```

`make check` includes loopback HTTP coverage for exact Messages headers, stage
schemas, no-tools request shape, retries, usage, secret rejection, malformed
outputs, and both adapters. `make integration-test` applies and replays
migration `0016` and verifies concurrent immutable PostgreSQL replay.

The real-provider smoke is separate and explicit:

```bash
ANTHROPIC_API_KEY='...' ANTHROPIC_MODEL='...' make provider-smoke
```

It loads the committed implementation and review prompts, performs one live
request for each stage, and fails instead of skipping if credentials, model
configuration, closed-schema decoding, or unchanged kernel validation fail.
The implementation input includes adversarial scope/schema instructions. The
review input conflicts an implementation narrative with independent evidence
and requires the evidence hierarchy to win.
