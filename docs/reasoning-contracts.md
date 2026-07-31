# Reasoning contracts

`harness.reasoning.v1` is a provider-neutral Protobuf package. It contains
shared request identity, immutable artifact and manifest bindings, proposal-only
authority, budgets, timestamps, stage and attempt metadata, and typed
specification, planning, implementation, and review messages.

Generated Go and Python bindings are transport-only. Handwritten domain
mappings validate authority, identity, normalized scope, graph, coverage, and
stage invariants before a proposal can be considered. Validation returns the
first deterministic rejection in this order: schema, request identity,
artifact/manifest binding, authority, scope, coverage, stage policy.

The five stable v1 rejection codes are `SCHEMA_INVALID`, `REQUEST_MISMATCH`,
`AUTHORITY_VIOLATION`, `SCOPE_VIOLATION`, and
`REQUIRED_COVERAGE_MISSING`.

The Phase 5 Go gateway applies that ordering to implementation requests and
proposals. `Service.ProposeImplementation` returns exactly one validated
`ImplementationProposal` or one `ProposalRejection`. Schema, policy, scope,
coverage, and expiry failures are policy outcomes; cancellation, registry,
adapter, artifact, and PostgreSQL failures remain Go errors. Scope-change
requests remain advisory proposal data and never alter kernel-selected scope.

The shipped beta adapter binds proposal request, run, task, attempt, manifest,
input-artifact, approved-task, and approved-specification identities from the
request. Provider output cannot supply those kernel-owned fields, execute
tools, or mutate workflow state.
Request and proposal transports retain their published v1 Protobuf shape.

Phase 8 applies the same deterministic byte limits, artifact integrity,
provider accounting, typed rejection, immutable replay, and conflict behavior
to `ReviewRequest` and `ReviewProposal`. The registered manifest must be stage
`review` with output `review_proposal.v1`. Proposal identity is derived from
the request; neither a provider adapter nor a recommendation carries approval
authority. The trusted review service separately fixes `HIGH` and `CRITICAL`
as blocking and requires unexpected changed paths to be reported.

Phase 10 leaves every published Protobuf and manifest-v1 field unchanged.
MiniMax Anthropic-compatible implementation and review JSON schemas are internal, closed
projections containing only model-owned proposal fields. The adapter injects
`ProposalIdentity`, approved task/specification bindings, manifest digest, and
input-artifact digests from the trusted request. Valid projections must still
pass the same `MapImplementationProposal` or `MapReviewProposal` validators.
Provider prompts therefore instruct models to populate only fields exposed by
the provider-supplied closed schema. Trusted Go, not the model, injects request,
task, specification, manifest, candidate, and artifact identities before
kernel validation.

Matched prompt protocol `1.1.0` applies one proposal-only authority and evidence
discipline across all four v1 stages while using only fields each stage
actually supports. Specification uses blocking `questions`; planning uses
`unresolved_scope_questions`; implementation uses `unresolved_questions` and
an advisory `scope_change_request`; review uses `rework_required`, assumptions,
and residual risks when evidence cannot support evaluation. Review findings may
reference only supplied independent evidence IDs, and blocking recommendations
remain derived from the trusted review policy.

V1 has no universal blocker or evidence-request result structure. In
particular, a scope-change-only or otherwise incomplete implementation
proposal can fail required-coverage validation and leave workflow state
unchanged. Universal blocker handling, generic tool loops, and live provider
adapters for specification and planning remain deferred.

The closed beta profile is `minimax_anthropic` at
`https://api.minimax.io/anthropic`, model `MiniMax-M2.7`, with
`ANTHROPIC_API_KEY` read for every invocation. Requests are non-streaming and
tool-free, omit thinking and `output_config`, and embed the closed schema in
the system prompt.

Malformed complete provider responses are typed adapter results that only the
gateway translates to `SCHEMA_INVALID` and persists with the exact raw
response. Credential, transport, provider, cancellation, timeout, and refusal
failures remain Go errors and release the unfinished reservation. Exact replay
returns the immutable outcome without resolving a manifest, credential, or
model and without another network attempt.

## Compatibility

The package and schema major version are `v1`. Published field numbers and enum
values are never reused. Compatible evolution adds fields or enum values; the
kernel fails closed on unknown authority, stage, operation, recommendation, or
policy enums. Gateways preserve unknown Protobuf fields while relaying messages.
Changing an existing field's meaning or default requires a new major version.
Every evolution must add a backward-read fixture before merge.
