# Real GitHub beta canary

The Slice 5 canary runs the complete MiniMax-M2.7 lifecycle and leaves one real
open draft pull request in `Standard-Syntax/basic-beta-canary`. It never merges,
deploys, creates repositories, provisions credentials, or cleans up a successful
publication automatically.

## Provisioning

Create the private or public canary repository out of band with `main` containing
only the canonical fixture contract required by the policy: `add.go`,
`add_test.go`, and `Makefile`. `add.go` must initially contain the deliberately
incorrect `Add` implementation; `add_test.go` and the repository-owned `check`
target must prove the corrected implementation. Protect `main` from direct
automation writes.

Provision two separate least-privilege credentials:

- a fine-grained GitHub token with Metadata read and Pull requests read/write
  for only `Standard-Syntax/basic-beta-canary`; store it in a regular,
  operator-owned `0600` file for REST draft inspection, creation, and closure;
- a write-enabled deploy key limited to only the canary repository; store the
  SSH private key in a different operator-owned `0600` file. Git uses it with
  `IdentitiesOnly=yes` and non-interactive authentication only for the exact
  branch push or exact-SHA deletion.

Export `ANTHROPIC_API_KEY` only in the invoking environment. Do not put any
credential value in JSON, command arguments, Git configuration, or a repository.
Prepare PostgreSQL with all checked-in migrations, and create empty,
non-overlapping, owner-controlled artifact, execution-worktree, and verification
roots. Preload the exact execution and verification images by immutable Docker
`sha256:` IDs.

The strict `BETA_CONFIG` uses the same envelope as production preflight and adds
only the path to the separate Git push key:

```json
{
  "database_url": "postgres://...",
  "artifact_root": "/srv/basic-canary/artifacts",
  "worktree_root": "/srv/basic-canary/worktrees",
  "verification_workspace_root": "/srv/basic-canary/verification",
  "provider_credential_environment": "ANTHROPIC_API_KEY",
  "publication_credential_file": "/run/secrets/basic-canary-github-token",
  "git_push_credential_file": "/run/secrets/basic-canary-deploy-key",
  "beta_policy": {
    "version": "1.0",
    "repository": {
      "owner": "Standard-Syntax",
      "name": "basic-beta-canary",
      "root": "/srv/basic-canary/repository",
      "remote": "origin",
      "remote_url": "git@github.com:Standard-Syntax/basic-beta-canary.git",
      "base_branch": "main",
      "base_commit": "<40 lowercase hex characters>"
    },
    "paths": {
      "readable": ["Makefile", "add.go", "add_test.go"],
      "writable": ["add.go"],
      "prohibited": ["Makefile", "add_test.go"]
    },
    "trusted_checks": ["make-check-v1"],
    "limits": {
      "maximum_tasks": 1,
      "maximum_changed_files": 1,
      "maximum_file_bytes": 1048576,
      "maximum_total_bytes": 4194304,
      "execution_concurrency": 1,
      "verification_concurrency": 1
    },
    "images": {
      "execution": "sha256:<64 lowercase hex characters>",
      "verification": "sha256:<64 lowercase hex characters>"
    }
  }
}
```

Unknown fields, broader fixture paths, another repository or base, a mutable
image, a missing credential, unsafe file ownership/mode, a dirty checkout, base
movement, or a nonempty worktree root fails before provider or publication work.

## Run and interpret evidence

```bash
export ANTHROPIC_API_KEY='...'
make beta-canary-e2e BETA_CONFIG=/absolute/path/to/canary.json
```

The target builds and starts the exact pinned, non-root beta service and worker
images rather than host service binaries. Operator credentials are copied into
disposable owner-only service mounts; the provisioned source files are not
reowned or modified.

Success requires production preflight, exactly one accepted implementation
invocation and one accepted independent-review invocation, a one-file `add.go`
candidate that passes the repository-owned check, an exact remote branch named
`harness/canary/<run-id>`, and one real open draft whose base/head refs and
commits match the immutable publication. The API is restarted and the same
approval is replayed byte-for-byte; provider ledger counts, branch state, and
the unique draft must remain unchanged.

The final JSON line contains `run_id`, `task_id`, `publication_id`,
`candidate_commit`, verification/review/approval/publication artifact
references, all four image IDs, `pull_request_url`, and the exact
`cleanup_command`. Retain it for `beta_release_manifest.v1`. Treat the gate as
pending—not passing—unless this credentialed command exits 0 and that real URL
is inspected. Process logs, generated configs, CAS, PostgreSQL evidence, and
reachable Git objects are scanned for the provider token, REST token, and Git
push key.

## Cleanup and partial recovery

Successful canary acceptance intentionally leaves the draft and branch for
operator inspection. When inspection is complete, copy the exact command from
the evidence JSON:

```bash
make beta-canary-cleanup \
  BETA_CONFIG=/absolute/path/to/canary.json \
  CANARY_PUBLICATION_ID=<publication-id>
```

Cleanup loads the completed immutable publication row and CAS artifact, checks
them against the dedicated policy, deletes only its exact branch when the remote
head still equals the recorded candidate, and closes only its exact matching
open draft. A missing matching branch or already-closed matching draft is replay
success. If interruption happens after either operation, rerun the same command.
Any marker, URL, base, head, commit, repository, draft-state, or remote-head
mismatch fails closed; investigate manually and do not broaden the command.

Neither publication nor cleanup has merge or deployment authority. Merging,
marking ready, deploying, deleting a repository, or deleting branches by prefix
remains prohibited.
