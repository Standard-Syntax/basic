# Live MiniMax beta profile

Beta Slice 2 ships the credentialed full-process gate for the live-only
implementation and independent-review composition.

## Closed production profile

`workflow-service` accepts only this profile:

```text
mode:        omitted or minimax_anthropic
base URL:    https://api.minimax.io/anthropic
model:       MiniMax-M2.7
credential:  ANTHROPIC_API_KEY
```

Omitted values normalize to those exact values. Fake mode, another endpoint,
another model, another environment-variable name, removed fake proposal
fields, unknown fields, and trailing JSON fail closed.

The service performs a redacted credential-availability check before
migrations, database connections, manifest bootstrap, job claiming, or
reconciliation. That value is discarded. The same uncached environment source
is read again for every implementation and review invocation. Credentials are
not written to configuration, artifacts, PostgreSQL, provider context, errors,
or logs.

## Adapter protocol

Both stages use non-streaming, tool-free Anthropic Messages requests through
MiniMax's compatibility endpoint. Requests contain no tools, thinking
configuration, or `output_config`; the closed stage schema is embedded in the
system prompt. MiniMax's documented signed `thinking` response block is kept
only in the immutable raw-response artifact. Exactly one complete JSON text
block is parsed, with no unknown fields or trailing JSON; tools, extra text,
and any other content type fail closed.

Trusted Go injects proposal identity and immutable request bindings after
decoding. The unchanged kernel validators still decide whether the result is
accepted or mapped to one of the five stable rejection codes. Raw provider
responses, provider request ID, model, token usage, and request count remain
immutable replay evidence. Exact replay performs no credential lookup or
network call.

## Local verification

The credential-free provider and gateway suites use bounded loopback HTTP
servers while exercising the production MiniMax adapter path:

```bash
make generate-check
make check
make integration-test
make runtime-e2e
go build ./cmd/workflow-service
```

`make check` includes `scripts/no-fake-provider-adapters.sh`, which scans the
production gateway and workflow composition for removed fake symbols,
proposal loaders, and alternate provider branches.

The generic `make provider-smoke` target remains an explicit Anthropic adapter
connectivity check. It is not a MiniMax process-E2E acceptance run.

## Credentialed beta gate

Load `ANTHROPIC_API_KEY` from a non-logging secret source into the command
environment, then run:

```bash
make beta-live-e2e
```

The disposable API configuration supplies its fixture repository as the
required top-level `repository_root`. `POST /v1/runs` resolves and binds the
fixture base commit and repository-map CAS artifact before any worker starts.
After the API restart, replay of the original run request must return the stored
`201` bytes with `Idempotent-Replay: true`; a replay must not read Git, publish
CAS objects, create another workflow run or binding, enqueue work, or invoke
MiniMax.

The gate builds both host services and both isolated worker images, starts
disposable PostgreSQL, and runs one task against MiniMax-M2.7 through
implementation, execution, independent verification, review, process restart,
human approval, a separate operator submission, and draft publication. The Git repository and publication
remote are disposable; GitHub REST remains loopback-only in Slice 2.

Acceptance requires one implementation request and one distinct review
request, one provider request per stage, immutable proposal/raw-response
evidence with request IDs and token usage, an independently passing `add.go`
candidate, one approval and one submission, and exact replay after API restart
without another provider request, push, approval, submission, or PR. The fixture scans
configuration, prompts, process logs, CAS, PostgreSQL, and reachable Git
objects for the provider secret.

`make runtime-e2e` remains credential-free and intentionally narrower. It
proves PostgreSQL claim takeover, fencing, CAS integrity, and reconciler
recovery using an already-existing artifact; it does not generate a proposal
or prove beta lifecycle completion. A missing credential, skipped live test,
provider failure, evidence mismatch, replayed side effect, or secret leak
fails `make beta-live-e2e`.

Before targeting a production repository, run
`make beta-preflight BETA_CONFIG=/absolute/path/to/beta-preflight.json` and
require exit 0 with `status:"ready"`. Exit 1 exposes only redacted readiness
codes; exit 2 means invalid configuration. This non-mutating check is not a
substitute for the credentialed live lifecycle gate.
