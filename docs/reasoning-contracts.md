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

## Compatibility

The package and schema major version are `v1`. Published field numbers and enum
values are never reused. Compatible evolution adds fields or enum values; the
kernel fails closed on unknown authority, stage, operation, recommendation, or
policy enums. Gateways preserve unknown Protobuf fields while relaying messages.
Changing an existing field's meaning or default requires a new major version.
Every evolution must add a backward-read fixture before merge.
