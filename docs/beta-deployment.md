# Beta service deployment

The beta deployment runs one exact `api-service`, `workflow-service`,
execution-worker, and verification-worker image set. `beta_deployment.v1` is a
strict, secret-free envelope: it contains immutable `sha256:` image IDs,
absolute mount and credential-file paths, service config paths, the common beta
policy, source commit, and Docker socket group. Unknown fields, tags, relative
paths, and policy/image disagreement fail closed.

## Provisioning

1. Provision PostgreSQL storage, CAS, worktree, verification, manifest, prompt,
   configuration, and credential directories on one trusted Docker host. The
   repository path must be identical on the host and in both service
   containers. Grant repository read-only access to API and the reviewed Git
   write boundary to workflow.
2. Create the MiniMax file as UID/GID `65532:65532`, mode `0600`, under a
   directory mounted read-only into workflow. Create publication credentials
   with the same ownership and mode when publication is enabled. Provision the
   PostgreSQL initialization password separately for the PostgreSQL container.
3. Put exact service configuration files at the envelope paths. Packaged
   workflow configuration selects `provider.api_key_file` only; do not also set
   `api_key_env`. Set API and workflow listeners to `0.0.0.0:8080` and
   `0.0.0.0:8081`; Compose publishes only API on host loopback.
4. Build and inspect the images, then replace the envelope's image IDs with the
   reported IDs. Resolve the Docker socket group with
   `stat -c '%g' /var/run/docker.sock` and record it in the envelope. The value
   must be positive; group `0` is rejected and has no root-group fallback.

```bash
make beta-images
docker image inspect --format '{{.Id}}' \
  basic-api-service:beta basic-workflow-service:beta \
  basic-execution-worker:beta basic-verification-worker:beta
make beta-deploy-smoke BETA_CONFIG=/absolute/path/to/beta.json
```

The ordinary PostgreSQL development service remains in `compose.yaml`. Beta
services are in `compose.beta.yaml`; invoke them with both files. Every beta
image, service configuration, durable root, database secret, and Docker group
substitution is required. Only explicitly optional publication credential
mounts retain the `/dev/null` fallback.

The smoke uses an isolated Compose project, waits for dependency-aware
readiness, submits no run, makes no provider or GitHub request, verifies that no
job remains claimed, shuts down cleanly, and writes the secret-free deployment
record to `.tools/beta/deployment-record.json`. Retain that record with the
reviewed source commit; it is packaging evidence, not live-run evidence.

## Health and shutdown

`/healthz` reports process liveness. `/readyz` returns only `ready` or
`unavailable`. API readiness verifies PostgreSQL, applied migration digests,
CAS, and repository access. Workflow additionally verifies all migrations,
manifests/prompts, the owner-only MiniMax file, and both exact worker images
through the Docker Engine API without calling MiniMax or changing Git state.

On SIGTERM workflow stops taking new claims immediately. Its current fenced
claim continues heartbeat renewal for the configured drain deadline (60
seconds by default). At expiry the handler is canceled; the lease remains
recoverable under the existing fencing rules. `GET /v1/runs/{run_id}` exposes
stage state and terminal failure artifact URI/digest, never failure contents or
provider bodies.

## Backup and restore

Quiesce intake, allow workflow to drain, and record the exact deployment record
before backup. Take a PostgreSQL logical backup and a CAS filesystem snapshot
from the same quiesced point. Preserve CAS names, bytes, ownership, and modes;
worktrees and verification workspaces are disposable and are not authoritative.

Restore PostgreSQL and CAS together into empty locations, restore the exact
configuration/manifest/prompt set, load the prior exact images, and start the
services. Do not apply an older binary to a newer migration ledger. Readiness
must verify every stored migration digest before intake is reopened.

## Rotation, recovery, and rollback

Rotate MiniMax by atomically replacing the file inside its mounted credential
directory with a new UID/GID `65532:65532`, mode `0600` file. The source is
reread for every invocation; readiness observes missing or unsafe replacements
without sending a request. Rotate database or publication credentials under
their own procedures and restart only the consumers that cannot reread them.

For a failed stage, retrieve its failure reference from the run endpoint,
inspect the immutable artifact through the operator-authorized CAS path, fix
the external condition, and let the normal retry/recovery path reclaim only an
expired fenced claim. Never edit runtime job rows or reuse a stale fencing
token. Canary publication cleanup remains the separate, exact-SHA command in
`docs/beta-canary.md`.

Rollback means restoring the previous deployment record's complete four-image
set, service configs, manifests, prompts, and configuration digest together.
Restore the matching PostgreSQL/CAS backup when schema or durable data changed.
Never roll back by retagging an image or mixing service and worker IDs from two
records. Keep the current and previous records for the full beta retention
period, plus any records referenced by retained run evidence.

The final beta cut procedure and read-only durable evidence verifier are in
`docs/beta-release.md`. A deployment smoke record alone is never release
readiness or a human go decision.
