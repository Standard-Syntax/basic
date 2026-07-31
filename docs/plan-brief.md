# High-Level Implementation Plan: Kernel-Driven Agent Harness

> Historical roadmap: fake-provider steps below describe the original
> dependency order and are not supported runtime configuration. DEC-023 and
> the live-only MiniMax-M2.7 beta composition supersede those production
> provider choices without rewriting this roadmap.

## 1. Objective

Build a production-oriented agent harness with:

- a trusted control plane implemented as Go microservices;
- Python used only for declarative agent configuration and prompt composition;
- provider-neutral reasoning contracts;
- deterministic workflow control;
- strict separation between model proposals and kernel authority;
- isolated execution and independent verification;
- immutable, auditable artifacts and workflow events.

The harness must enforce the following invariant:

> Models may propose actions and changes. Only the Go kernel may authorize, execute, verify, approve, or advance workflow state.

---

# 2. Core Workflow

Implement the following workflow:

```text
User request
    ↓
Specification proposal
    ↓
Human specification approval
    ↓
Task graph proposal
    ↓
Kernel validation
    ↓
Human task-plan approval
    ↓
Bounded implementation proposal
    ↓
Controlled execution in isolated workspace
    ↓
Independent verification
    ↓
Independent review
    ↓
Human approval
    ↓
Draft pull request
```

The first release must not automatically merge pull requests or deploy changes.

---

# 3. System Boundaries

## 3.1 Go control plane

Go services own all trusted behavior:

- workflow and task state;
- state transitions;
- authorization;
- policy enforcement;
- proposal validation;
- task scheduling;
- resource leases;
- Git worktree management;
- controlled file mutation;
- command execution;
- verification;
- evidence collection;
- approval enforcement;
- audit events;
- artifact metadata;
- pull-request creation.

## 3.2 Python agent configuration

Python defines:

- agent roles;
- prompt templates;
- context-selection preferences;
- model capability requirements;
- model parameter preferences;
- allowed tool-request types;
- output contract references;
- stage-specific agent configuration.

Python must not:

- update workflow state;
- access kernel databases;
- invoke Git directly;
- execute task commands;
- approve work;
- widen task scope;
- create resource leases;
- publish branches;
- open pull requests;
- provide authoritative test evidence.

Python configuration must compile into a canonical, immutable JSON manifest before being registered with the Go control plane.

---

# 4. Initial Architectural Style

Use a monorepo with separately deployable Go services.

```text
agent-harness/
├── go/
│   ├── cmd/
│   │   ├── api-service/
│   │   ├── workflow-service/
│   │   ├── agent-registry/
│   │   ├── reasoning-gateway/
│   │   ├── execution-service/
│   │   └── verification-service/
│   ├── internal/
│   │   ├── workflow/
│   │   ├── reasoning/
│   │   ├── policy/
│   │   ├── execution/
│   │   ├── verification/
│   │   ├── approval/
│   │   ├── artifacts/
│   │   └── events/
│   └── pkg/
│
├── python/
│   ├── src/harness_agents/
│   ├── agents/
│   ├── prompts/
│   └── tests/
│
├── proto/
│   └── harness/
│       ├── reasoning/v1/
│       ├── workflow/v1/
│       ├── execution/v1/
│       └── events/v1/
│
├── schemas/
│   └── agent-manifest-v1.schema.json
│
├── migrations/
├── deploy/
├── docs/
└── tests/
```

Keep business logic in Go packages that can be tested without starting network services.

Do not create additional microservices unless a clear ownership, scaling, security, or deployment boundary requires one.

---

# 5. Initial Go Services

## 5.1 API Service

Responsibilities:

- expose CLI- and UI-facing HTTP APIs;
- authenticate users;
- accept commands;
- expose workflow and task status;
- expose pending approvals;
- forward commands to the workflow service.

Initial endpoints:

```text
POST /runs
GET  /runs/{run_id}
POST /runs/{run_id}/specification
POST /runs/{run_id}/specification/approve
POST /runs/{run_id}/task-graph
POST /runs/{run_id}/task-graph/approve
POST /tasks/{task_id}/retry
POST /tasks/{task_id}/approve
POST /tasks/{task_id}/reject
POST /runs/{run_id}/cancel
```

## 5.2 Workflow Service

Responsibilities:

- own run and task lifecycle state;
- enforce legal transitions;
- evaluate task readiness;
- coordinate stage progression;
- enforce retry limits;
- persist commands and resulting events;
- dispatch long-running activities.

The workflow service must not call model providers directly.

## 5.3 Agent Registry

Responsibilities:

- accept compiled agent manifests;
- validate manifests against JSON Schema;
- canonicalize JSON;
- calculate SHA-256 digests;
- register immutable agent versions;
- return manifests by name, version, or digest;
- reject mutable version replacement.

## 5.4 Reasoning Gateway

Responsibilities:

- receive typed reasoning requests;
- load an immutable agent manifest;
- assemble model input;
- route to a provider adapter;
- decode structured model output;
- return an untrusted proposal;
- record model usage and invocation metadata.

The reasoning gateway must not:

- mutate workflow state;
- execute proposed file changes;
- mark evidence as verified;
- approve proposals;
- widen task scope.

## 5.5 Execution Service

Responsibilities:

- create isolated Git worktrees;
- apply accepted file-change proposals;
- enforce writable-path policies;
- prevent path traversal and symlink escapes;
- run approved commands;
- create candidate commits;
- report actual changed files;
- enforce CPU, memory, storage, network, and timeout limits.

## 5.6 Verification Service

Responsibilities:

- check out the exact candidate commit in a clean environment;
- run kernel-selected checks;
- record command, exit code, timing, and output digest;
- map evidence to acceptance criteria;
- produce scope and verification reports;
- store logs and evidence artifacts.

Verification must be independent from the implementation model and implementation workspace.

---

# 6. Deferred Services

Do not initially create separate services for:

- policy evaluation;
- approval management;
- artifact metadata;
- event storage;
- scheduling.

Implement these as internal Go modules first.

Extract them into separate services only after operational evidence demonstrates a need.

---

# 7. Workflow State Machine

Implement explicit run and task states.

## Run states

```text
DRAFT
SPECIFICATION_REVIEW
TASK_PLANNING
TASK_PLAN_REVIEW
READY
EXECUTING
VERIFYING
REVIEWING
AWAITING_APPROVAL
MERGE_READY
MERGED
REJECTED
FAILED
CANCELLED
```

## Task states

```text
PENDING
READY
LEASED
REASONING
PROPOSAL_REJECTED
EXECUTING
VERIFYING
REVIEWING
REWORK_REQUIRED
AWAITING_APPROVAL
ACCEPTED
FAILED
CANCELLED
```

Every transition must:

1. validate the current state;
2. validate actor authority;
3. validate required artifacts;
4. produce an append-only event;
5. update current state atomically.

Invalid transitions must leave state unchanged.

---

# 8. Reasoning Contracts

Define provider-neutral Protobuf contracts.

Create:

```text
proto/harness/reasoning/v1/common.proto
proto/harness/reasoning/v1/specification.proto
proto/harness/reasoning/v1/planning.proto
proto/harness/reasoning/v1/implementation.proto
proto/harness/reasoning/v1/review.proto
```

## Common request envelope

Include:

- schema version;
- request ID;
- run ID;
- optional task ID;
- stage;
- attempt number;
- creation time;
- expiration time;
- authority constraints;
- token and request budgets;
- input artifact digests;
- agent manifest digest.

## Common authority contract

Reasoning authority must always specify:

```text
mode: proposal_only
may_mutate_kernel_state: false
may_execute_commands: false
may_modify_files: false
may_expand_scope: false
may_approve_work: false
```

The kernel must reject any request or response that violates this authority model.

---

# 9. Stage-Specific Reasoning Operations

Implement four initial reasoning operations.

## 9.1 Specification

Input:

- problem statement;
- desired outcome;
- known constraints;
- known non-goals;
- stakeholders;
- optional repository summary.

Output proposal:

- title;
- goal;
- actors;
- constraints;
- non-goals;
- acceptance criteria;
- verification method for each criterion;
- assumptions;
- risks;
- blocking and non-blocking questions.

## 9.2 Task Planning

Input:

- approved specification;
- specification digest;
- repository map;
- permitted roots;
- prohibited paths;
- task-count limit;
- parallelism limit.

Output proposal:

- task graph;
- task dependencies;
- task objectives;
- acceptance-criterion assignments;
- readable paths;
- writable paths;
- exclusive resources;
- required checks;
- stop conditions;
- assumptions;
- unresolved scope questions.

## 9.3 Implementation

Input:

- approved task;
- approved specification;
- task and specification digests;
- base commit;
- readable paths;
- writable paths;
- prohibited paths;
- acceptance criteria;
- available check IDs;
- kernel-selected repository context.

Output proposal:

- summary;
- structured file changes;
- expected original file digests;
- acceptance-criterion mapping;
- requested declared checks;
- assumptions;
- unresolved questions;
- optional scope-change request.

## 9.4 Review

Input:

- approved specification;
- approved task;
- base and candidate commits;
- actual diff;
- scope report;
- independently collected evidence;
- acceptance coverage;
- review policy.

Output proposal:

- recommendation;
- findings;
- severity;
- category;
- evidence references;
- required actions;
- unrequested changes;
- residual risks;
- assumptions.

A review recommendation is advisory. It is not an approval.

---

# 10. Go Reasoning Interfaces

Define small consumer-owned Go interfaces.

```go
type SpecificationReasoner interface {
    DraftSpecification(
        context.Context,
        SpecificationRequest,
    ) (SpecificationProposal, error)
}

type TaskPlanningReasoner interface {
    ProposeTaskGraph(
        context.Context,
        TaskPlanningRequest,
    ) (TaskGraphProposal, error)
}

type ImplementationReasoner interface {
    ProposeImplementation(
        context.Context,
        ImplementationRequest,
    ) (ImplementationProposal, error)
}

type ReviewReasoner interface {
    ReviewCandidate(
        context.Context,
        ReviewRequest,
    ) (ReviewProposal, error)
}
```

Do not expose a generic interface returning `any`.

Do not include workflow or execution methods in reasoning interfaces.

---

# 11. Initial Rejection Codes

Implement these five stable rejection codes first:

```text
SCHEMA_INVALID
REQUEST_MISMATCH
AUTHORITY_VIOLATION
SCOPE_VIOLATION
REQUIRED_COVERAGE_MISSING
```

## Definitions

### `SCHEMA_INVALID`

The request or proposal cannot be decoded or violates structural invariants.

### `REQUEST_MISMATCH`

The proposal references the wrong request, task, attempt, agent manifest, or artifact digest.

### `AUTHORITY_VIOLATION`

The reasoning output attempts to approve, execute, mutate state, widen scope, or claim authoritative evidence.

### `SCOPE_VIOLATION`

The proposal accesses or modifies resources outside its authorized scope.

### `REQUIRED_COVERAGE_MISSING`

The proposal does not address all assigned acceptance criteria or required outputs.

Each rejection must contain:

```text
code
summary
details
retryable
request_id
run_id
task_id
attempt
timestamp
```

---

# 12. Python Agent SDK

Create a small Python package that allows authors to define agents declaratively.

Example API:

```python
implementation_agent = AgentDefinition(
    name="bounded-implementation",
    version="1.0.0",
    stage="implementation",
    prompt=PromptTemplate.from_file(
        "prompts/implementation.md"
    ),
    model=ModelPolicy(
        capability_class="strong_coding",
        temperature=0.1,
        maximum_output_tokens=20_000,
    ),
    context=ContextPolicy(
        include_specification=True,
        include_task=True,
        repository_selection="kernel_selected",
        maximum_context_tokens=100_000,
    ),
    tools=ToolRequestPolicy(
        allowed_requests={
            "read_repository_file",
            "search_repository",
            "request_declared_check",
            "report_blocker",
        },
        arbitrary_shell=False,
        arbitrary_network=False,
        direct_file_write=False,
    ),
    output=OutputContract(
        schema="implementation_proposal.v1"
    ),
)
```

The SDK must compile the configuration into canonical JSON.

The compiler must:

- validate required fields;
- reject unsupported stages;
- reject direct shell, network, or write permissions;
- sort object keys;
- normalize values;
- produce deterministic output;
- calculate a SHA-256 digest;
- emit the manifest and digest.

---

# 13. Agent Manifest Schema

Create:

```text
schemas/agent-manifest-v1.schema.json
```

The manifest must include:

```text
schema_version
agent name
agent version
stage
prompt artifact URI
prompt digest
model capability class
model parameters
context policy
tool-request policy
output schema
configuration metadata
```

Do not allow manifests to contain:

- database credentials;
- model-provider secrets;
- arbitrary executable code;
- shell commands;
- unrestricted URLs;
- workflow transition instructions;
- approval rules;
- writable paths;
- task-specific scope.

Task-specific authority always comes from the Go kernel.

---

# 14. Proposal Validation Pipeline

For each proposal, execute validators in this order:

```text
1. Decode and schema validation
2. Request and attempt identity validation
3. Artifact and manifest digest validation
4. Authority validation
5. Path and resource scope validation
6. Acceptance-criterion coverage validation
7. Stage-specific policy validation
```

No execution-side effect may occur before every required validator passes.

Return the first deterministic rejection initially.

Add multi-error collection only if operational experience shows it improves repair behavior.

---

# 15. Implementation Proposal Format

For the first implementation, use structured complete-file replacement.

Each change includes:

```text
path
operation: create | update | delete
expected_original_digest
replacement_content
rationale
```

The execution service must:

1. normalize the repository-relative path;
2. reject absolute paths;
3. reject `..`;
4. reject symlink escapes;
5. verify the path is writable;
6. verify the expected original digest;
7. apply the change atomically;
8. inspect the actual Git diff;
9. reject unexpected file changes.

Do not initially support arbitrary model-generated shell commands or scripts.

---

# 16. Task Scope Model

Every task must define:

```text
readable paths
writable paths
prohibited paths
exclusive resources
required checks
acceptance criteria
stop conditions
attempt limit
time limit
model budget
```

Writable paths must be a subset of readable paths.

Parallel tasks may execute only when:

```text
write_scope(A) ∩ write_scope(B) = ∅
```

They must also not share an exclusive semantic resource such as:

```text
database-schema
dependency-manifest
public-api-contract
authentication-policy
deployment-configuration
```

---

# 17. Resource Leasing

Implement leases for:

- tasks;
- worktrees;
- branches;
- file scopes;
- exclusive semantic resources.

Each lease must include:

```text
resource ID
holder task
attempt
acquired time
expiration time
fencing token
```

A worker result must be rejected if its lease expired or its fencing token is stale.

---

# 18. Execution Isolation

The execution service must run each task in an isolated workspace.

Initial implementation:

- dedicated Git worktree;
- unprivileged operating-system user or container;
- read-only mounts where possible;
- no production secrets;
- network disabled by default;
- explicit CPU and memory limits;
- task timeout;
- disk quota;
- sanitized environment variables.

The implementation model must not receive direct Git credentials.

Branch publication must be performed by the execution or repository adapter after kernel authorization.

---

# 19. Independent Verification

Verification must run in a clean environment against the exact candidate commit.

For each check, record:

```text
check ID
approved command reference
resolved command
candidate commit
start time
end time
exit code
output digest
artifact URI
resource usage
```

The verifier must not trust model claims such as:

```text
Tests passed.
Type checking succeeded.
The task is complete.
```

The kernel must calculate acceptance coverage from independently collected evidence.

---

# 20. Event and Audit Model

Persist an append-only event for every material change.

Initial events:

```text
RunCreated
SpecificationProposed
SpecificationRejected
SpecificationApproved
TaskGraphProposed
TaskGraphRejected
TaskGraphApproved
TaskReady
TaskLeaseAcquired
ReasoningRequested
ReasoningProposalReceived
ProposalRejected
ProposalAccepted
ExecutionStarted
CandidateCommitCreated
VerificationStarted
VerificationCompleted
ReviewRequested
ReviewSubmitted
TaskReworkRequested
TaskApproved
RunMergeReady
DraftPullRequestCreated
RunCancelled
RunFailed
```

Each event must include:

```text
event ID
event type
aggregate ID
run ID
optional task ID
actor
timestamp
correlation ID
causation ID
payload schema version
payload
```

Maintain current-state tables for efficient queries while keeping events append-only.

---

# 21. Persistence

Use PostgreSQL for:

- runs;
- tasks;
- task dependencies;
- current workflow state;
- approvals;
- resource leases;
- agent registrations;
- event metadata;
- proposal metadata;
- evidence metadata.

Use object or filesystem-backed artifact storage for:

- complete prompts;
- model responses;
- patches;
- test logs;
- review reports;
- large evidence files.

Store artifact URIs and SHA-256 digests in PostgreSQL.

Do not store large logs directly in workflow-state rows.

---

# 22. Service Communication

Use:

- Protobuf and gRPC for synchronous Go service calls;
- durable workflow activities or queue-backed commands for long-running work;
- append-only domain events for completed facts;
- canonical JSON for Python agent manifests.

Commands must be named as imperatives:

```text
ExecuteTask
VerifyCandidate
RequestReview
CreateDraftPullRequest
```

Events must describe completed facts:

```text
TaskExecutionStarted
CandidateVerified
ReviewSubmitted
DraftPullRequestCreated
```

Do not use event names as disguised commands.

---

# 23. Workflow Runtime

Place the workflow engine behind a Go interface.

```go
type WorkflowRuntime interface {
    StartRun(context.Context, StartRunCommand) error
    SignalApproval(context.Context, ApprovalSignal) error
    CancelRun(context.Context, string) error
    QueryRun(context.Context, string) (RunView, error)
}
```

The domain and policy packages must not import the workflow-engine SDK.

Use workflow orchestration for:

- durable waits;
- retries;
- approval signals;
- cancellation;
- timeouts;
- long-running execution;
- recovery after worker failure.

Keep nondeterministic operations outside deterministic workflow code.

---

# 24. Observability

Implement structured logging and tracing from the beginning.

Every request must propagate:

```text
trace ID
correlation ID
run ID
task ID
request ID
attempt
agent manifest digest
candidate commit
```

Initial metrics:

```text
runs created
runs completed
tasks completed
tasks rejected
rejections by code
reasoning latency
reasoning token usage
execution duration
verification duration
review findings by severity
retry count
scope violations
stale worker rejections
human approval latency
```

Do not log:

- secrets;
- full environment variables;
- private model credentials;
- hidden model reasoning;
- unredacted sensitive repository content.

---

# 25. Security Requirements

Implement the following minimum protections:

1. No model or Python configuration has direct database access.
2. No model receives production credentials.
3. Python compilation runs without network access by default.
4. Manifests are immutable and digest-addressed.
5. All repository paths are normalized and scope checked.
6. Symlink traversal is rejected.
7. Dependency-file changes require elevated approval.
8. Schema, authentication, authorization, and deployment changes require elevated approval.
9. Worker operations use short-lived scoped credentials.
10. Network access is deny-by-default.
11. Model inputs and outputs pass secret scanning.
12. Every external side effect uses an idempotency key.
13. Worker results require a valid lease and fencing token.
14. Proposals cannot serve as execution evidence.
15. No automatic merge or deployment in the first release.

---

# 26. Testing Strategy

## Unit tests

Test:

- state transitions;
- rejection codes;
- path normalization;
- scope matching;
- acceptance coverage;
- task graph validation;
- lease behavior;
- agent-manifest validation;
- canonical JSON generation;
- digest calculation;
- proposal identity validation.

## Contract tests

Test compatibility among:

- Protobuf contracts;
- Go domain mappings;
- JSON Schema;
- Python manifest output;
- reasoning gateway serialization.

## Integration tests

Build tests proving:

1. A valid proposal can be accepted.
2. A malformed proposal returns `SCHEMA_INVALID`.
3. A stale request returns `REQUEST_MISMATCH`.
4. A self-approval attempt returns `AUTHORITY_VIOLATION`.
5. An unauthorized file returns `SCOPE_VIOLATION`.
6. An omitted acceptance criterion returns `REQUIRED_COVERAGE_MISSING`.
7. Rejected proposals cause no filesystem mutations.
8. Expired worker results are rejected.
9. Failed verification prevents approval.
10. A review recommendation cannot directly approve a task.

## End-to-end test

Implement one complete vertical scenario:

```text
Create run
Approve specification
Approve one-task graph
Return fake implementation proposal
Apply proposal in worktree
Run deterministic test
Return fake review approval
Record human approval
Create draft pull request
```

Use fake reasoning adapters until all boundaries are proven.

---

# 27. Implementation Phases

## Phase 0: Repository foundation

Deliver:

- monorepo structure;
- Go workspace;
- Python project with `uv`;
- linting;
- formatting;
- test commands;
- Protobuf generation;
- CI;
- local development instructions.

Acceptance:

- all projects build;
- tests run in one command;
- generated files are reproducible;
- CI is green.

## Phase 1: Shared contracts

Deliver:

- Protobuf reasoning contracts;
- workflow identifiers;
- proposal types;
- rejection types;
- agent manifest JSON Schema;
- canonical artifact digest rules.

Acceptance:

- Go contract tests pass;
- Python manifests validate;
- identical input produces identical digest.

## Phase 2: Kernel state machine

Deliver:

- run and task aggregates;
- legal transitions;
- commands;
- events;
- current-state persistence;
- invalid-transition tests.

Acceptance:

- all allowed transitions succeed;
- all invalid transitions fail without changing state.

## Phase 3: Python agent SDK

Deliver:

- `AgentDefinition`;
- prompt configuration;
- model policy;
- context policy;
- tool-request policy;
- output contract;
- canonical JSON compiler;
- manifest CLI.

Acceptance:

- example agents compile;
- unsafe permissions are rejected;
- output is deterministic.

## Phase 4: Agent registry

Deliver:

- registration API;
- JSON Schema validation;
- canonicalization;
- immutable version storage;
- digest lookup.

Acceptance:

- duplicate identical registration is idempotent;
- conflicting version replacement is rejected.

## Phase 5: Fake reasoning gateway

Deliver:

- Go reasoning interfaces;
- fake reasoning adapter;
- request and proposal persistence;
- proposal validator pipeline;
- five rejection codes.

Acceptance:

- tests demonstrate all five rejection paths;
- rejected proposals cause no state or filesystem mutation.

## Phase 6: Execution service

Deliver:

- worktree lifecycle;
- path validation;
- structured file application;
- digest checks;
- candidate commit creation;
- actual-diff reporting;
- resource limits.

Acceptance:

- unauthorized and symlinked writes fail;
- valid changes produce a candidate commit;
- stale leases cannot publish results.

## Phase 7: Verification service

Deliver:

- clean candidate checkout;
- approved command resolution;
- command execution;
- evidence collection;
- artifact digests;
- acceptance-coverage calculation.

Acceptance:

- a false model completion claim cannot pass verification;
- evidence is bound to the exact candidate commit.

## Phase 8: Review and approvals

Deliver:

- review request and proposal contracts;
- fake reviewer;
- review policy;
- human approval APIs;
- risk-based approval requirements.

Acceptance:

- reviewer recommendation alone cannot approve;
- blocking findings require rework;
- approvals bind to artifact digests.

## Phase 9: Draft pull request

Deliver:

- branch publication;
- draft pull-request creation;
- linked specification, evidence, and review summary;
- idempotent PR creation.

Acceptance:

- repeated publication does not create duplicate PRs;
- no merge occurs automatically.

## Phase 10: First real model provider

Deliver:

- one provider adapter;
- structured output decoding;
- timeout and retry translation;
- usage accounting;
- provider-specific integration tests.

Acceptance:

- provider failures do not corrupt workflow state;
- malformed outputs return deterministic rejections;
- replacing the provider does not require kernel changes.

---

# 28. Initial Non-Goals

Do not implement initially:

- automatic pull-request merge;
- production deployment;
- self-modifying agents;
- dynamic agent generation;
- long-lived conversational agent memory;
- autonomous scope expansion;
- cross-repository tasks;
- model-driven scheduling;
- model-generated arbitrary shell commands;
- unrestricted network access;
- policy authored by agents;
- generalized multi-agent chat;
- complex web UI;
- provider fallback and routing;
- task prioritization by a model.

---

# 29. Definition of Done for the First Vertical Slice

The first vertical slice is complete when:

1. A user can create a run.
2. A specification can be proposed and human-approved.
3. A one-task graph can be proposed, validated, and approved.
4. The workflow service creates a bounded implementation request.
5. A fake reasoning gateway returns a structured proposal.
6. The Go policy layer validates the proposal.
7. The execution service applies the proposal in an isolated worktree.
8. The actual Git diff matches authorized scope.
9. The verification service runs an approved check in a clean environment.
10. Evidence is tied to the candidate commit.
11. A fake independent reviewer returns a structured review.
12. Human approval is recorded against immutable artifact digests.
13. A draft pull request is created idempotently.
14. Every material action is represented by an append-only event.
15. All five initial rejection paths have integration tests.
16. No Python component can mutate kernel state or execute repository changes.

---

# 30. Required Engineering Principles

Apply:

- Single Responsibility Principle;
- Interface Segregation;
- Dependency Inversion;
- Separation of Concerns;
- low coupling and high cohesion;
- KISS;
- YAGNI;
- DRY;
- fail-closed authorization;
- immutable contracts;
- idempotent side effects;
- explicit state transitions;
- consumer-owned Go interfaces.

Prefer KISS and YAGNI over premature extensibility.

---

# 31. Decision Log

Maintain:

```text
docs/decision-log.md
```

For each material architecture or implementation choice, append:

```markdown
## DEC-XXX: Decision title

### Decision

### Options considered

### Pros

### Cons

### Why this option

### Consequences

### Date
```

At minimum, record decisions for:

- Go microservices as the trusted kernel;
- Python compiling to canonical JSON;
- Protobuf for internal service contracts;
- complete-file replacement for the first patch format;
- PostgreSQL for authoritative metadata;
- clean independent verification;
- no automatic merge;
- fake reasoning provider before real model integration.

---

# 32. Resource Analysis Requirements

Document expected resource usage for each service.

At minimum analyze:

- CPU;
- memory;
- disk;
- network;
- database growth;
- artifact-storage growth;
- model token consumption;
- build and test workload;
- maximum concurrent task count.

Add configurable limits for:

```text
maximum proposal bytes
maximum changed files
maximum single-file size
maximum total replacement content
maximum context tokens
maximum output tokens
maximum task duration
maximum test duration
maximum retries
maximum active worktrees
maximum concurrent tasks
```

---

# 33. Required Deliverables

Codex must produce:

```text
README.md
docs/architecture.md
docs/security.md
docs/resource-analysis.md
docs/decision-log.md
docs/workflow-state-machine.md
docs/reasoning-contracts.md
docs/agent-manifest.md
docs/development.md
proto definitions
JSON Schema
Go service skeletons
Go domain packages
Python agent SDK
Python manifest compiler
database migrations
fake reasoning adapter
unit tests
contract tests
integration tests
local development configuration
CI workflow
```

---

# 34. Implementation Instructions for Codex

1. Begin by inspecting the repository and documenting the existing structure.
2. Do not introduce a model provider in the first implementation.
3. Create or update `docs/decision-log.md` for every material choice.
4. Implement contracts before services.
5. Implement domain packages before transport handlers.
6. Keep generated Protobuf code out of domain logic.
7. Use dependency injection through small Go interfaces.
8. Keep Python outside the production request path.
9. Make all external side effects idempotent.
10. Add tests before advancing to the next phase.
11. Do not silently widen requirements.
12. Stop and document a blocker when implementation requires:

    - an additional service;
    - a new infrastructure dependency;
    - broader file access;
    - a schema migration beyond the approved design;
    - production credentials;
    - an automatic merge or deployment capability.

13. After each phase, update:

    - architecture documentation;
    - decision log;
    - resource analysis;
    - security analysis;
    - test coverage summary.

14. Do not mark a phase complete unless its acceptance criteria pass.
15. Produce a final completion report listing:

    - files changed;
    - commands executed;
    - tests and results;
    - assumptions;
    - unresolved issues;
    - deviations from this plan;
    - recommended next phase.

---

# 35. First Codex Task

Implement only Phase 0 and Phase 1 initially:

```text
Phase 0: Repository foundation
Phase 1: Shared contracts
```

Do not implement workflow execution, worktrees, a database runtime, or a model provider yet.

The first task must establish:

- repository structure;
- Go and Python build systems;
- Protobuf generation;
- reasoning request and proposal contracts;
- rejection contracts;
- agent manifest JSON Schema;
- deterministic Python manifest compilation;
- cross-language contract tests;
- CI.

The implementation should leave clear interfaces and task boundaries for the subsequent kernel state-machine phase.
