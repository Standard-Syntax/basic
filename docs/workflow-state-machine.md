# Workflow state machine

The Go kernel owns closed run and task state enums. Every command validates the
actor, aggregate identity, expected revision, current state, and required
immutable bindings before producing a copied next snapshot and event. Errors
produce neither a partial snapshot nor events.

## Run transitions

| From | Command result |
|---|---|
| new | `DRAFT` |
| `DRAFT` | `SPECIFICATION_REVIEW` |
| `SPECIFICATION_REVIEW` | `DRAFT`, `TASK_PLANNING`, or `REJECTED` |
| `TASK_PLANNING` | `TASK_PLAN_REVIEW` |
| `TASK_PLAN_REVIEW` | `TASK_PLANNING`, `READY`, or `REJECTED` |
| `READY` | `EXECUTING` |
| `EXECUTING` | `VERIFYING` after every persisted task is accepted |
| `VERIFYING` | `REVIEWING` |
| `REVIEWING` | `AWAITING_APPROVAL` |
| `AWAITING_APPROVAL` | `MERGE_READY` or `REJECTED` |
| `MERGE_READY` | `MERGED` by recording an external merge fact |

An authorized service may fail any nonterminal run. A human may cancel any
nonterminal run; cancellation atomically cancels its nonterminal tasks. Terminal
run states are `MERGED`, `REJECTED`, `FAILED`, and `CANCELLED`.

## Task transitions

| From | Command result |
|---|---|
| new | `READY` without dependencies, otherwise `PENDING` |
| `PENDING` | `READY` when all prerequisites are accepted |
| `READY` | `LEASED`, incrementing the attempt and assigning the same positive fencing token |
| `LEASED` | `REASONING`, or `READY` on release/expiry recording |
| `REASONING` | `PROPOSAL_REJECTED` or `EXECUTING` |
| `PROPOSAL_REJECTED` | `READY`, or `FAILED` when attempts are exhausted |
| `EXECUTING` | `VERIFYING` |
| `VERIFYING` | `REVIEWING` or `REWORK_REQUIRED` |
| `REVIEWING` | `AWAITING_APPROVAL` or `REWORK_REQUIRED` |
| `REWORK_REQUIRED` | `READY`, or `FAILED` when attempts are exhausted |
| `AWAITING_APPROVAL` | `ACCEPTED` or `REWORK_REQUIRED` |

An authorized operational service may record a non-retryable failure for any
nonterminal task. Run cancellation may cancel any nonterminal task. Terminal
task states are `ACCEPTED`, `FAILED`, and `CANCELLED`.

`AcceptTaskProposal` and `RecordTaskExecution` carry the exact active lease
tuple. The execution-service command timestamp must be before the stored expiry,
and the token must match the current attempt. Mismatch, expiry, or supersession
fails before snapshot, event, or command-result mutation.

`RecordTaskVerification` is emitted only by the verification-service actor
after the report artifact is checkpointed. Its evidence and candidate commit
are exact bindings. Passing complete coverage moves `VERIFYING` to
`REVIEWING`; any failed or uncovered criterion records failed evidence and
moves the task to `REWORK_REQUIRED`.

`RecordTaskReview` is emitted only by the trusted review-service actor.
`REWORK_REQUIRED` recommendations and `HIGH` or `CRITICAL` findings record the
review and move directly to `REWORK_REQUIRED`; advisory acceptance records the
review and stops at `AWAITING_APPROVAL`.

Only the approval application may translate an authenticated principal with an
`approver` role into `ActorHuman`. Elevated work additionally requires
`elevated_approver`. `ApproveTask` binds the exact candidate, trusted review,
and immutable approval artifact before reaching `ACCEPTED`;
`RequireTaskRework` binds the same candidate and review. A reviewer
recommendation never satisfies either human command.

Task-graph approval validates uniqueness, references, attempt limits, and
acyclicity, then atomically creates snapshots, dependency edges, creation or
readiness events, and the run transition. Models and Python have no authority.
