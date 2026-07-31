# Live MiniMax beta profile

Beta Slice 1 ships a live-only provider composition for implementation and
independent review. It removes the production fake proposal path but does not
yet deliver the redesigned process-level live harness; that fixture and its
final target name belong to Beta Slice 2.

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
system prompt. The response must be one complete JSON text block with no
unknown fields or trailing JSON.

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
go build ./cmd/workflow-service
```

`make check` includes `scripts/no-fake-provider-adapters.sh`, which scans the
production gateway and workflow composition for removed fake symbols,
proposal loaders, and alternate provider branches.

The generic `make provider-smoke` target remains an explicit Anthropic adapter
connectivity check. It is not a MiniMax process-E2E acceptance run.

## Process-E2E status

Do not use the current `make runtime-e2e` or `make minimax-live-e2e` targets as
Beta Slice 1 acceptance evidence. Their mixed fixture predates the live-only
composition: the default branch writes fake provider configuration and the
live branch still writes the superseded model. Strict production
configuration therefore prevents those scripted paths from starting
successfully.

Beta Slice 2 owns `MINIMAX_LIVE_E2E`, prebuilt process proposals, target
renaming, and the new `beta-live-e2e` path. Until that slice lands, a successful
unit/integration loopback run proves protocol and composition behavior but not
a credentialed full workflow run against MiniMax.
