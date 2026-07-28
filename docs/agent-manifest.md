# Agent manifest

Agent authors use the typed Python SDK or its JSON authoring format. Every
`AgentDefinition.compile()` validates the complete value against the v1 schema
bundled in the installed wheel. Supplying `schema=` or CLI `--schema` adds
validation and cannot bypass the bundled contract.

Compilation reads exact prompt bytes, rejects non-UTF-8 content, stores their
lowercase SHA-256 digest and `artifact://sha256/<digest>` URI, canonicalizes the
manifest with RFC 8785, and hashes the canonical bytes. Set-valued tool requests
and labels are sorted before canonicalization.

The v1 stage/output pairs are closed:

| Stage | Output schema |
|---|---|
| `specification` | `specification_proposal.v1` |
| `planning` | `task_graph_proposal.v1` |
| `implementation` | `implementation_proposal.v1` |
| `review` | `review_proposal.v1` |

The Python compiler and Go reader reject mismatches, unknown fields, invalid
identity or metadata, unsupported policy values, unsafe permissions, and
non-canonical tool or label sets. The CLI writes canonical JSON and a lowercase
digest sidecar only after the definition and prompt validate completely.

Definitions and prompts for all four stages live in `python/agents` and
`python/prompts`. Their generated fixtures live in
`tests/contracts/v1/manifest` and are covered by `make generate-check`.

## Registry boundary

`registry.Register` accepts compiled bytes, repeats the Go reader's closed
validation and canonicalization, and persists only exact canonical JSON.
`(agent name, semantic version)` is an immutable identity. Re-registering the
same canonical content returns the original timestamp with `Created=false`;
different content for that identity returns `ErrVersionConflict`.

`registry.Get` and `registry.GetByDigest` return the same validated record,
including a defensive copy of its canonical bytes. Lookup rejects malformed
keys before querying and fails with `ErrCorruptData` if persisted canonical
bytes, digest, or embedded identity disagree. The application API has no
network transport or runtime provider behavior.

No manifest field grants shell, network, file-write, credential, workflow,
approval, publication, registration, or task-specific scope authority. A
manifest is immutable configuration evidence, not permission to perform work.
