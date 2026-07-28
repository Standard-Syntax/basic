# Agent manifest

Agent authors use typed Python declarations. Compilation reads the exact UTF-8
prompt bytes, stores their SHA-256 digest and
`artifact://sha256/<lowercase-hex>` URI, validates the closed v1 JSON Schema,
canonicalizes with RFC 8785, and hashes those canonical bytes.

The compiler writes a JSON manifest and a lowercase hexadecimal digest sidecar.
No field may grant shell, network, file-write, credential, workflow, approval,
publication, or task-specific scope authority.
