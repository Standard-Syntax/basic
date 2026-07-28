## Summary

<!-- What outcome does this PR deliver, and why is it needed? -->

## Stack

<!-- Complete this section even for a standalone PR. Use "none" where applicable. -->

- Base PR / dependency:
- This PR:
- Next PR:
- Review only:

## Scope

<!-- List the intentional changes in this layer of the stack. -->

-

## Out of scope

<!-- Name nearby work intentionally deferred to another PR or task. -->

-

## Validation

<!-- Paste the exact commands run and their results. Do not write only "CI passes." -->

```text
uv sync --frozen
make check
```

## Risk and security

<!-- Describe compatibility, security, data-integrity, and rollback concerns. -->

- Risk:
- Mitigation / rollback:

## Checklist

- [ ] The PR targets the correct base branch for its stack layer.
- [ ] The diff contains only this layer's intended changes.
- [ ] Tests cover changed behavior and important failure paths.
- [ ] Generated files were regenerated with `make generate` when contracts changed.
- [ ] `make check` passes locally, or failures are documented above.
- [ ] No secrets, credentials, or sensitive data are included.
- [ ] Documentation or compatibility notes are updated where behavior changed.
