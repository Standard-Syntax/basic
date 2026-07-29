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

The deterministic fake adapter binds proposal request, run, task, attempt,
manifest, input-artifact, approved-task, and approved-specification identities
from the request. It consumes exactly one provider-request budget unit and
executes no command, repository mutation, network call, or workflow transition.
Request and proposal transports retain their published v1 Protobuf shape.

Phase 8 applies the same deterministic byte limits, artifact integrity,
provider accounting, typed rejection, immutable replay, and conflict behavior
to `ReviewRequest` and `ReviewProposal`. The registered manifest must be stage
`review` with output `review_proposal.v1`. Proposal identity is derived from
the request; neither the fake adapter nor a recommendation carries approval
authority. The trusted review service separately fixes `HIGH` and `CRITICAL`
as blocking and requires unexpected changed paths to be reported.

Phase 10 leaves every published Protobuf and manifest-v1 field unchanged.
Anthropic-specific implementation and review JSON schemas are internal, closed
projections containing only model-owned proposal fields. The adapter injects
`ProposalIdentity`, approved task/specification bindings, manifest digest, and
input-artifact digests from the trusted request. Valid projections must still
pass the same `MapImplementationProposal` or `MapReviewProposal` validators.

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
