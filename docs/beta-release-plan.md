# Beta release implementation plan: live-only agent harness

## 1. Outcome

Release the harness as a controlled beta that can take an approved, bounded
task for a real repository and produce a real candidate commit and draft pull
request containing production code.

The beta path is:

```text
authenticated intake
  -> human-approved specification
  -> human-approved one-task plan
  -> live MiniMax-M2.7 implementation proposal
  -> isolated candidate commit
  -> independent clean verification
  -> live MiniMax-M2.7 independent review
  -> explicit human approval
  -> real GitHub draft pull request
```

The release remains proposal-driven and beta-scoped. It does not merge, deploy,
or promote generated code automatically.

## 2. Non-negotiable release invariants

1. No fake implementation or review adapter exists in the shipped source,
   binaries, configuration schema, process harness, or beta acceptance path.
2. No prebuilt proposal file may stand in for either model invocation.
3. Both reasoning stages use the real `MiniMax-M2.7` model through
   `https://api.minimax.io/anthropic`.
4. A provider connectivity probe, scripted HTTP response, or component test is
   not beta release evidence. Release evidence requires the complete live
   lifecycle and exactly two accepted remote invocations: implementation and
   review.
5. The candidate must differ from the approved base, contain at least one
   in-scope file change, pass kernel-selected checks in a clean verification
   workspace, and bind the same commit through review, approval, and
   publication.
6. Human approval remains mandatory. Review is advisory and cannot authorize
   publication.
7. Publication creates a draft pull request only. Automatic merge and
   deployment stay out of scope.
8. Provider and GitHub credentials are read at use time, are never written to
   JSON configuration, manifests, logs, artifacts, or PostgreSQL, and are
   removable without rebuilding an image.
9. Exact replay is side-effect-free. Changed bindings, stale leases, changed
   base refs, and partially completed external effects fail closed.
10. Test doubles may isolate consumer-owned internal ports in unit tests, but no
    provider-adapter substitute may generate implementation or review output.
    Protocol tests exercise the production MiniMax adapter. Only a credentialed
    live run can satisfy a release gate.

## 3. Current checkout evidence

This plan is based on `main` at `d6696f5`.

- `go/cmd/workflow-service/main.go` currently accepts
  `fake_implementation_proposal_path` and `fake_review_proposal_path`, branches
  on provider mode, and constructs `FakeImplementationAdapter` and
  `FakeReviewAdapter`.
- `go/internal/reasoning/gateway/minimax.go` currently accepts both
  `minimax_anthropic` and `fake`.
- `go/internal/reasoning/gateway/fake.go` and `review_fake.go` ship the fake
  provider implementations.
- `make runtime-e2e` currently selects fake adapters unless
  `MINIMAX_LIVE_E2E=1`; the integration test writes prebuilt implementation and
  review proposal files.
- `make minimax-live-e2e` already exercises the two real MiniMax stages,
  isolated execution, independent verification, restart replay, human
  approval, and publication, but publication is against an in-process loopback
  GitHub server and a disposable fixture repository.
- `api-service` and `workflow-service` are runnable host binaries, but the
  repository does not yet ship service images or a beta deployment profile.
- Run intake persists workflow state, repository context, and runtime bindings
  in separate writes. Recovery is bounded, but whole-intake atomicity is not
  established.

Existing v1 Protobuf contracts, manifest validation, proposal-only authority,
content-addressed artifacts, candidate fencing, clean verification, human
approval, and draft-only publication remain authoritative and are not widened
by this plan.

### Provider compatibility decision

The beta replaces the repository's current `MiniMax-M3` pin with
`MiniMax-M2.7`. As of 2026-07-30, MiniMax's
[Anthropic-compatible API reference](https://platform.minimax.io/docs/api-reference/text-anthropic-api)
documents `MiniMax-M2.7` on the approved Anthropic endpoint but does not list
`MiniMax-M3`. MiniMax's
[M3 product page](https://www.minimax.io/models/text/m3) advertises M3 through
the native `chatcompletion_v2` endpoint, while the corresponding
[native API reference](https://platform.minimax.io/docs/api-reference/text-post)
is marked deprecated and does not yet list M3 in its request model enum.

The beta therefore uses the fully documented model/transport pairing instead of
shipping against an ambiguous M3 contract. Moving to M3 requires a later
provider slice with an authoritative API contract, adapter conformance tests,
and a credentialed full-lifecycle run; it is not a configuration-only change.

## 4. Release shape

Implement the beta as one dependency-ordered stack. Each branch is a thin
vertical slice, is independently buildable, keeps all lower-layer gates green,
and adds one observable capability.

```text
main
  <- beta/live-only-provider
  <- beta/live-process-gate
  <- beta/atomic-run-intake
  <- beta/repository-preflight
  <- beta/github-canary
  <- beta/service-packaging
  <- beta/release-evidence
```

Do not create all branches before their owning slice is ready to begin. Commit
the plan separately; then create each implementation branch from the accepted
lower branch with `gh stack add <branch>`.

## 5. Slice 1: live-only provider

**Branch:** `beta/live-only-provider`

**User-visible capability:** every workflow process can start only with the
approved live provider configuration; there is no runtime switch back to fake
output.

### Changes

- Delete `go/internal/reasoning/gateway/fake.go` and
  `go/internal/reasoning/gateway/review_fake.go`.
- Remove `FakeProvider`, `FakeProviderMode`, both fake proposal path fields,
  fake-path validation, `readProtoJSON`, and the fake branch in
  `providerAdapters`.
- Make the provider configuration a single closed MiniMax profile. Preserve
  exact endpoint, model, capability-to-model mapping, compatibility options,
  bounded non-streaming calls, one complete text block, strict JSON decoding,
  and kernel-injected identities.
- Fail startup when `ANTHROPIC_API_KEY` is unavailable. Do not read or cache the
  credential during config decoding; read it per invocation and perform a
  non-secret-bearing startup availability check.
- Convert adapter tests to exercise the production Anthropic/MiniMax adapter
  through a bounded local HTTP protocol server. Delete tests of fake output and
  add negative tests for a fake mode or fake proposal fields in strict process
  configuration.
- Add `scripts/no-fake-provider-adapters.sh` and run it from `make check`. The
  structural check must fail on fake provider mode declarations, fake proposal
  config fields, fake implementation/review adapter constructors, or a
  production composition branch that bypasses the MiniMax adapter.
- Update active architecture, security, development, and reasoning
  documentation to describe live-only composition. Historical phase completion
  records may retain factual history but must state that their fake path is
  removed and not a supported command.

### Acceptance

```bash
make generate-check
make check
make integration-test
git diff --check
```

Additional assertions:

- `go build ./cmd/workflow-service` contains no fake provider implementation.
- strict config rejects `"mode": "fake"` and either fake proposal path;
- missing credentials stop startup before a job can be claimed;
- protocol-test responses still pass the same production decoder and kernel
  validators used by MiniMax;
- no deterministic or scripted proposal can satisfy a process-level gate.

## 6. Slice 2: live full-process gate

**Branch:** `beta/live-process-gate`

**User-visible capability:** one command proves real production-code generation
through the complete two-process lifecycle.

### Changes

- Remove `MINIMAX_LIVE_E2E`, `fakeProposals`, the `live` branch, fake evidence
  expectations, and every prebuilt proposal path from
  `go/internal/runtime/process_e2e_test.go`.
- Make the full-process test unconditionally configure the closed MiniMax
  provider and require `ANTHROPIC_API_KEY`.
- Replace the ambiguous targets with:

  ```text
  make runtime-e2e       deterministic infrastructure/recovery integration only
  make beta-live-e2e     credentialed real-provider full lifecycle
  ```

  `runtime-e2e` must not generate reasoning output. It may exercise durable
  orchestration around already-existing immutable artifacts at a lower test
  boundary, but it cannot be reported as harness completion.
- Keep the beta live fixture disposable and retain the loopback publication
  server in this slice so the only newly live external effect is the provider.
- Assert exactly two accepted `minimax-anthropic` / `MiniMax-M2.7` invocation
  rows, non-empty provider request IDs and usage, distinct implementation and
  review identities, raw-response artifacts, one candidate commit, independent
  verification evidence, one human approval, one draft publication, restart
  replay, and secret absence.
- Make provider timeout/failure, schema-invalid output, empty changes, failed
  verification, and review `rework_required` terminate without approval or
  publication.

### Acceptance

```bash
make check
make integration-test
make beta-live-e2e
git diff --check
```

Load `ANTHROPIC_API_KEY` into the command environment from a non-logging secret
source before running the gate. The credentialed command must actually run. A
skipped test or missing credential is a failure, not an acceptable beta result.

## 7. Slice 3: atomic run intake

**Branch:** `beta/atomic-run-intake`

**User-visible capability:** an accepted run is either completely recoverable
and schedulable or absent; operators never inherit a half-created beta run.

### Changes

- Introduce one intake application transaction owned by the API composition for
  workflow run creation and runtime binding creation.
- Stage the repository-map artifact before the transaction, then atomically
  persist the run, exact artifact binding, idempotency completion, and initial
  durable intake status. If the transaction rolls back, the unbound CAS object
  is harmless and collectible.
- If current package ownership prevents a shared transaction, add an explicit
  compensating intake state and reconciler. Do not claim atomicity from a chain
  of independent repository calls.
- Ensure exact idempotent replay returns the original result, changed request
  bytes fail, and expired reservations require a new fencing generation.
- Add crash-point integration tests after every durable write and before the
  response. Restart must converge without duplicate runs, bindings, jobs, or
  provider calls.

### Acceptance

```bash
make check
make integration-test
go test -race ./internal/runtime ./internal/controlapi
git diff --check
```

The live gate from Slice 2 must remain green before this slice advances.

## 8. Slice 4: production repository preflight

**Branch:** `beta/repository-preflight`

**User-visible capability:** an operator can point the beta at an allowlisted
real repository and receive a fail-closed readiness decision before provider
tokens or mutation are used.

### Changes

- Add a `beta-preflight` command or non-mutating API operation that validates:
  exact repository identity, clean absolute roots, expected remote URL,
  approved base branch, immutable base commit reachability, no existing
  harness-owned worktree collision, Docker availability, worker image digests,
  PostgreSQL migrations, artifact-root permissions, provider credential
  availability, and publication credential availability.
- Pin execution and verification worker images by digest in beta
  configuration. Record those digests with the run.
- Require an allowlist of repository owner/name, remote, base branch, writable
  path policy, trusted check IDs, maximum changed files/bytes, and concurrency.
  Do not infer these from model output.
- Reject a moving or mismatched base before reasoning. Revalidate the remote
  base immediately before publication.
- Add a dry-run fixture that proves preflight performs no model, Git push, PR,
  workflow transition, or candidate mutation side effect.
- Document the supported beta limitation of one dependency-free task per run.
  Expanding task-graph scheduling is not required to generate production code
  and is deferred.

### Acceptance

```bash
make check
make integration-test
make beta-preflight BETA_CONFIG=/absolute/path/to/beta.json
git diff --check
```

Preflight output must contain only non-secret configuration identities and
immutable digests.

## 9. Slice 5: real GitHub canary publication

**Branch:** `beta/github-canary`

**User-visible capability:** an approved live candidate is published as a real
draft pull request in a designated canary repository.

### Changes

- Add a separate, explicitly credentialed `make beta-canary-e2e` target. It
  uses the production `GitCommandPublisher`, production GitHub REST client, a
  dedicated canary repository, and a dedicated base branch.
- Keep the disposable/loopback test from Slice 2 as integration coverage, but
  it is no longer sufficient for release. The canary target must call both
  MiniMax and GitHub.
- Require a least-privilege GitHub credential from a mounted `0600` file or
  workload secret. Never accept it in JSON or on a command line.
- Use a unique run ID and harness branch. The canary task changes one
  allowlisted fixture source file and has a deterministic repository-owned
  acceptance check.
- Verify the real draft PR is open, draft, based on the approved base branch,
  headed by the authorized candidate commit, and contains exactly the verified
  diff. Record its immutable publication artifact and URL.
- Replay the approval/publication command after process restart and prove there
  is still one remote branch and one PR and no additional provider invocation.
- Add an operator cleanup command that closes only the identified canary draft
  and deletes only its identified harness branch. Cleanup is not part of
  successful publication and must never use a broad branch pattern.

### Acceptance

```bash
make check
make integration-test
make beta-canary-e2e BETA_CONFIG=/absolute/path/to/canary.json
git diff --check
```

Load the provider credential and mounted GitHub credential from non-logging
secret sources before running the gate. The gate fails unless it observes the
real draft PR. It must never merge or deploy that PR.

## 10. Slice 6: beta service packaging

**Branch:** `beta/service-packaging`

**User-visible capability:** operators can run the exact reviewed API and
workflow binaries as pinned, non-root service images.

### Changes

- Add minimal multi-stage Dockerfiles for `api-service` and
  `workflow-service`; copy only the binary and required certificates. Run as a
  fixed non-root UID/GID with a read-only root filesystem.
- Add a beta Compose profile for the two services and PostgreSQL. Mount
  repository, artifacts, worktrees, verification workspaces, manifests,
  prompts, and credential files with the minimum required access.
- Expose API liveness/readiness and add workflow-service liveness/readiness
  that verifies configuration, database connectivity, migrations, artifact
  storage, worker image availability, and credential presence without making a
  provider call.
- Add bounded shutdown, claim draining, structured run/task/stage identifiers,
  and operator-visible terminal failure reasons that reference artifacts
  without logging proposal bodies or secrets.
- Generate a release bill of materials and image digests. Pin the exact source
  commit, manifests, prompt digests, worker image digests, and service image
  digests in the deployment record.
- Document backup/restore for PostgreSQL and CAS, credential rotation, failed
  run recovery, canary cleanup, and rollback to the prior image set.

### Acceptance

```bash
make check
make integration-test
make beta-images
make beta-deploy-smoke BETA_CONFIG=/absolute/path/to/beta.json
git diff --check
```

The smoke gate starts the packaged services, passes preflight, submits no run,
and proves clean shutdown. The Slice 2 live gate must then pass against the
packaged binaries.

## 11. Slice 7: release evidence and beta cut

**Branch:** `beta/release-evidence`

**User-visible capability:** the beta has a reproducible, auditable release
candidate and an explicit operator go/no-go decision.

### Changes

- Add a release manifest containing source commit, migration digest, agent
  manifest and prompt digests, service and worker image digests, toolchain
  versions, canary run ID, candidate commit, verification artifact, review
  artifact, approval artifact, and real draft PR URL.
- Add a machine-checkable beta readiness command that verifies every referenced
  artifact and binding from durable stores. It must not trust prose completion
  documents.
- Run a credentialed canary from the packaged services, restart both services
  at the documented boundaries, replay intake and publication, and prove no
  duplicated provider, Git, or GitHub effects.
- Update the operator runbook to remove fake-baseline instructions and state
  beta limits: one task, bounded complete-file replacements, approved checks,
  draft PR only, no merge, no deployment, and manual operational supervision.
- Record an explicit human go/no-go decision. A green provider smoke,
  deterministic CI run, or loopback publication cannot substitute for this
  decision.

### Required release gates

```bash
uv sync --frozen
make generate-check
make check
make integration-test
go test -race ./internal/orchestration ./internal/runtime ./internal/controlapi
make beta-images
make beta-deploy-smoke BETA_CONFIG=/absolute/path/to/beta.json
make beta-live-e2e BETA_CONFIG=/absolute/path/to/beta.json
make beta-canary-e2e BETA_CONFIG=/absolute/path/to/canary.json
make beta-readiness RELEASE_MANIFEST=/absolute/path/to/release.json
git diff --check
```

Load required provider and publication credentials from non-logging secret
sources before the credentialed gates.

Record literal commands, exit status, commit and image digests, and durable
artifact identities. Do not record secret values or provider response bodies in
the release report.

## 12. Beta exit criteria

The beta may be announced only when all of the following are true:

- the shipped repository contains no fake provider adapter or selectable fake
  provider mode;
- both live reasoning stages ran and produced accepted, durable provider
  evidence;
- the candidate contains real, non-empty, in-scope code changes from the model;
- clean independent verification passed for the exact candidate;
- independent review bound the exact candidate and evidence;
- a human approved the exact candidate and review;
- the production publication client created exactly one real draft PR for the
  exact candidate;
- restart and exact replay produced no duplicate provider, candidate,
  verification, approval, branch, or PR effect;
- changed replay, stale fencing, verification failure, review rework, base
  drift, and missing credentials all failed closed;
- secrets were absent from configuration, logs, artifacts, database export,
  release manifest, and Git history;
- packaged services passed readiness and shutdown checks;
- the release manifest verified against durable state; and
- a human recorded the beta go decision.

Anything less is implementation progress, not a beta release.

## 13. Explicit deferrals

The beta does not add automatic merge, deployment, multi-task graph execution,
arbitrary model tools, streaming, images, autonomous approval, hosted secret
management, multi-tenant isolation, or general provider selection. These
features require separate authority and threat-model decisions after beta
evidence exists.

Slice 4 implementation uses a versioned canonical `beta_policy` shared by both
services and preflight. Migration `0020` preserves nullable historical rows
while requiring policy and image bindings for every new intake. Workers consume
only bound `sha256:` image identities. The beta remains limited to one
dependency-free task.
