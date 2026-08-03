# Beta release evidence and cut

Release readiness requires exactly one accepted live MiniMax invocation for
each of specification, planning, implementation, and review, with distinct
request/provider identities and immutable proposal and raw-response artifacts.
Automated fixture approvals are test evidence only and never human release
decisions.

Slice 7 turns one packaged real-GitHub canary into a reproducible release
decision. It does not announce a beta automatically. The operator retains the
successful canary draft, constructs the secret-free release manifest, records
an explicit human `go` or `no_go` decision, and runs the read-only verifier.

## Prepare the candidate

Use a clean checkout at the exact source commit. Provision the deployment and
canary configuration described in [Beta service deployment](beta-deployment.md)
and [Real GitHub beta canary](beta-canary.md). The deployment policy, canary
policy, repository, CAS/worktree/verification roots, publication credential
path, Git push key path, and worker image IDs must agree exactly.

Run the credential-free gates first:

```bash
uv sync --frozen
make generate-check
make check
make integration-test
go test -race ./internal/orchestration ./internal/runtime ./internal/controlapi
make beta-images
make beta-deploy-smoke BETA_CONFIG=/absolute/path/to/deployment.json
```

Retain `.tools/beta/deployment-record.json`. It is a strict packaging bill of
materials, not live lifecycle evidence.

## Produce the live evidence

Load `ANTHROPIC_API_KEY` from a non-logging secret source. Run the loopback gate
against the packaged images, then run the real canary:

```bash
make beta-live-e2e BETA_CONFIG=/absolute/path/to/deployment.json
make beta-canary-e2e BETA_CONFIG=/absolute/path/to/canary.json
```

`beta-canary-e2e` builds and uses the exact non-root API, workflow, execution,
and verification images. It restarts API before intake replay, restarts
workflow after stage completion, restarts API before approval/publication
replay, and fails if provider, approval, publication, remote-ref, or pull-request
effects multiply. Its final JSON line contains the run/task/publication IDs,
candidate commit, four release artifact references, exact image IDs, real draft
URL, and exact cleanup command. A missing credential, skipped invocation,
nonzero exit, or absent final JSON line is not release evidence.

Do not run the cleanup command until the release decision and retained-evidence
policy permit removal of the canary branch and draft.

## Record the human decision

Create an owner-only `beta_release_manifest.v1` JSON file outside the source
repository. It contains only identities, paths, digests, artifact references,
toolchain versions, and the human decision—never secret values or provider
response bodies. Populate:

- the clean source repository root;
- absolute deployment config, deployment record, and canary config paths;
- the exact `deployment` object from the retained deployment record;
- exact outputs of `git --version`, Go `runtime.Version()` (for example
  `go1.26.0`), `uv --version`, and `docker --version`;
- the canary run ID, task ID, publication ID, candidate commit, verification,
  review, approval, and publication artifact references, and draft URL;
- `decision.status` as `go` or `no_go`, the human principal UUID, a UTC RFC3339
  timestamp, and a nonempty reason.

The release decision is an operator fact. Automation must not manufacture a
`go` record from green checks.

## Verify and retain

```bash
make beta-readiness RELEASE_MANIFEST=/absolute/path/to/release.json
git diff --check
```

Exit `0` with `status:"ready"` means the verifier matched the clean source,
deployment/configuration digest, migration ledger, manifest/prompt bytes, four
installed images, one-task workflow, immutable runtime policy, exactly two
accepted MiniMax-M2.7 invocations, CAS-backed verification/review/approval/
publication artifacts, completed PostgreSQL ledgers, exact open GitHub draft,
and a human `go` decision. Exit `1` is not ready. Exit `2` is an invalid
release manifest. Exit `3` with `status:"inconclusive"` means cancellation or a
deadline prevented the verifier from reaching a positive or negative result;
its `failed_check` is `timeout`. Output is intentionally redacted to stable
check names. For exit `1` or `3`, the report adds `failed_check` with the stable
failed boundary; the report schema remains `beta_readiness_report.v1`.
The possible values are `configuration`, `source_checkout`, `toolchains`,
`migrations`, `files`, `images`, `workflow_runtime`, `reasoning`,
`verification`, `review`, `approval`, `publication`, `github_draft`, and
`human_decision`, plus `manifest_count_limit`, `manifest_size_limit`,
`prompt_count_limit`, `prompt_size_limit`, `timeout`, and `unknown`. Manifest
and prompt directories share limits of 64 regular files and 1 MiB per file.
Digest mismatches, symlinks, non-regular files, and other unsafe-file failures
remain classified as `files`; `unknown` makes an unclassified verifier failure
explicit instead of reporting a misleading configuration failure.

Retain the release manifest, its SHA-256 digest from the readiness report, the
deployment record, literal commands and exit statuses, final canary JSON, and
the current and previous deployment records. Never retain credentials,
provider response bodies, or database exports containing secrets in the
release report.

The beta remains limited to one dependency-free task, bounded complete-file
replacement, kernel-approved checks, explicit human approval, draft PR only,
and manual supervision. It has no merge, deployment, autonomous approval,
multi-task scheduling, provider selection, or cleanup-by-pattern authority.
