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
