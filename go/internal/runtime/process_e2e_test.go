//go:build integration

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBetaLiveProcessesCompleteDisposableFixture(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Fatal("ANTHROPIC_API_KEY is required for the beta live E2E")
	}
	apiBinary := requireEnvironment(t, "RUNTIME_API_BINARY")
	workflowBinary := requireEnvironment(t, "RUNTIME_WORKFLOW_BINARY")
	databaseURL := requireEnvironment(t, "TEST_DATABASE_URL")
	root := t.TempDir()
	repository, remote, baseCommit := fixtureRepository(t, root)
	artifactRoot := filepath.Join(root, "artifacts")
	worktrees := filepath.Join(root, "worktrees")
	verificationRoot := filepath.Join(root, "verification")
	for _, path := range []string{artifactRoot, worktrees, verificationRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	promptPaths, manifestPaths := runtimeManifests(t, root)
	token := "runtime-operator-token"
	tokenDigest := sha256.Sum256([]byte(token))
	prServer, prState := loopbackPullRequests(t)
	defer prServer.Close()
	tokenFile := filepath.Join(root, "github-token")
	if err := os.WriteFile(tokenFile, []byte("loopback-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	apiAddress := freeAddress(t)
	actorIDs := make([]string, 7)
	for index := range actorIDs {
		actorIDs[index] = uuid.NewString()
	}
	apiConfig := filepath.Join(root, "api.json")
	writeJSONFile(t, apiConfig, map[string]any{
		"listen": apiAddress, "database_url": databaseURL, "artifact_root": artifactRoot,
		"repository_root":  repository,
		"service_actor_id": actorIDs[0], "max_artifact_bytes": 4 << 20,
		"max_body_bytes": 4 << 20, "trusted_checks": []string{"make-check-v1"},
		"principals": []map[string]any{{
			"id": actorIDs[1], "token_sha256": hex.EncodeToString(tokenDigest[:]),
			"roles": []string{"operator", "approver", "elevated_approver"},
		}},
		"publication": map[string]any{
			"repository_root": repository, "repository_owner": "local",
			"repository_name": "fixture", "remote": remote, "base_branch": "main",
			"branch_prefix": "harness/", "actor_id": actorIDs[6],
			"api_endpoint": prServer.URL, "api_version": "2022-11-28",
			"token_file": tokenFile,
		},
	})
	workflowConfig := filepath.Join(root, "workflow.json")
	workflowValues := map[string]any{
		"database_url": databaseURL, "artifact_root": artifactRoot,
		"owner_id": uuid.NewString(), "max_artifact_bytes": 4 << 20,
		"repository_root": repository, "worktree_root": worktrees,
		"verification_workspace_root": verificationRoot,
		"execution_worker_image":      "basic-execution-worker:runtime",
		"verification_worker_image":   "basic-verification-worker:runtime",
		"worker_uid":                  os.Getuid(), "worker_gid": os.Getgid(),
		"service_actor_id": actorIDs[0], "reasoning_actor_id": actorIDs[2],
		"execution_actor_id": actorIDs[3], "verification_actor_id": actorIDs[4],
		"review_actor_id":              actorIDs[5],
		"implementation_manifest_path": manifestPaths[0],
		"implementation_prompt_path":   promptPaths[0],
		"review_manifest_path":         manifestPaths[1], "review_prompt_path": promptPaths[1],
		"context_max_files": 8, "context_max_bytes": 1 << 20,
		"task_lease_duration_nanoseconds": int64(30 * time.Minute),
		"claim_ttl_nanoseconds":           int64(3 * time.Second),
		"provider": map[string]any{
			"mode": "minimax_anthropic", "base_url": "https://api.minimax.io/anthropic",
			"model": "MiniMax-M2.7", "api_key_env": "ANTHROPIC_API_KEY",
		},
	}
	writeJSONFile(t, workflowConfig, workflowValues)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var processes []*process
	api := startProcess(t, ctx, apiBinary, apiConfig)
	processes = append(processes, api)
	waitHTTP(t, "http://"+apiAddress+"/healthz", "", http.StatusOK)
	client := &runtimeClient{base: "http://" + apiAddress, token: token}
	runID, taskID := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	client.mutate(t, "/v1/runs", "", map[string]any{
		"run_id": runID, "base_commit": baseCommit,
		"content":            map[string]any{"objective": "correct Add without changing tests"},
		"decision_timestamp": now,
	}, http.StatusCreated)
	specRequest, specProposal := specificationPair(runID, now)
	client.mutate(t, "/v1/runs/"+runID+"/specification", client.revision(t, runID),
		protoPair(t, specRequest, specProposal, now.Add(time.Second)), http.StatusOK)
	client.mutate(t, "/v1/runs/"+runID+"/specification/approve", client.revision(t, runID),
		map[string]any{"decision_timestamp": now.Add(2 * time.Second)}, http.StatusOK)
	specBody, _ := proto.MarshalOptions{Deterministic: true}.Marshal(specProposal)
	planningRequest, graph := planningPair(
		t, runID, taskID, now, repository, baseCommit, Digest(specBody),
	)
	client.mutate(t, "/v1/runs/"+runID+"/task-graph", client.revision(t, runID),
		protoPair(t, planningRequest, graph, now.Add(3*time.Second)), http.StatusOK)
	client.mutate(t, "/v1/runs/"+runID+"/task-graph/approve", client.revision(t, runID),
		map[string]any{"decision_timestamp": now.Add(4 * time.Second)}, http.StatusOK)
	api.stop(t)
	api = startProcess(t, ctx, apiBinary, apiConfig)
	processes = append(processes, api)
	waitHTTP(t, "http://"+apiAddress+"/healthz", "", http.StatusOK)
	workflowProcess := startProcess(t, ctx, workflowBinary, workflowConfig)
	processes = append(processes, workflowProcess)
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	waitFor(t, 12*time.Minute, func() bool {
		var state string
		err := pool.QueryRow(t.Context(), `SELECT state FROM workflow_tasks
			WHERE run_id=$1 AND task_id=$2`, runID, taskID).Scan(&state)
		var failedStage, failureDigest string
		if failureErr := pool.QueryRow(t.Context(), `SELECT stage,failure_digest
			FROM runtime_stage_jobs
			WHERE run_id=$1 AND task_id=$2 AND state='FAILED'
			ORDER BY updated_at DESC LIMIT 1`, runID, taskID).
			Scan(&failedStage, &failureDigest); failureErr == nil {
			t.Fatalf("runtime stage %s failed: artifact digest %s", failedStage, failureDigest)
		}
		if state == string(workflow.TaskStateProposalRejected) {
			var code int
			var summary, details string
			if rejectionErr := pool.QueryRow(t.Context(), `SELECT rejection_code,
				rejection_summary,rejection_details::text
				FROM reasoning_invocations WHERE run_id=$1 AND task_id=$2
				AND final_status='rejected' ORDER BY completed_at DESC LIMIT 1`,
				runID, taskID).Scan(&code, &summary, &details); rejectionErr != nil {
				t.Fatalf("implementation proposal rejected; evidence unavailable: %v", rejectionErr)
			}
			t.Fatalf("implementation proposal rejected: code=%d summary=%s details=%s",
				code, summary, details)
		}
		if state == string(workflow.TaskStateReworkRequired) {
			var stageName string
			_ = pool.QueryRow(t.Context(), `SELECT stage FROM runtime_stage_jobs
				WHERE run_id=$1 AND task_id=$2 AND state='COMPLETED'
				ORDER BY updated_at DESC LIMIT 1`, runID, taskID).Scan(&stageName)
			t.Fatalf("task required rework after stage %s", stageName)
		}
		return err == nil && state == string(workflow.TaskStateAwaitingApproval)
	})
	var completedBeforeRestart int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM runtime_stage_jobs
		WHERE run_id=$1 AND state='COMPLETED'`, runID).Scan(&completedBeforeRestart); err != nil {
		t.Fatal(err)
	}
	workflowProcess.stop(t)
	workflowProcess = startProcess(t, ctx, workflowBinary, workflowConfig)
	processes = append(processes, workflowProcess)
	waitFor(t, 10*time.Second, func() bool {
		var state string
		err := pool.QueryRow(t.Context(), `SELECT state FROM workflow_tasks
			WHERE run_id=$1 AND task_id=$2`, runID, taskID).Scan(&state)
		return err == nil && state == string(workflow.TaskStateAwaitingApproval)
	})
	var completedAfterRestart int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM runtime_stage_jobs
		WHERE run_id=$1 AND state='COMPLETED'`, runID).Scan(&completedAfterRestart); err != nil {
		t.Fatal(err)
	}
	if completedAfterRestart != completedBeforeRestart {
		t.Fatalf("completed effects repeated across restart: before=%d after=%d",
			completedBeforeRestart, completedAfterRestart)
	}
	workflowProcess.stop(t)
	approvalPath := "/v1/runs/" + runID + "/approval"
	approvalRevision := client.revision(t, runID)
	approvalKey := uuid.NewString()
	approvalRequest := map[string]any{
		"decision_timestamp": now.Add(10 * time.Minute),
	}
	approvalResponse := client.mutateWithKey(
		t, approvalPath, approvalRevision, approvalKey, approvalRequest, http.StatusOK,
	)
	publicationValue, ok := approvalResponse["publication"].(map[string]any)
	if !ok || publicationValue["candidate_commit"] == "" ||
		publicationValue["pull_request_number"] != float64(1) {
		t.Fatalf("publication = %#v", publicationValue)
	}
	candidate := publicationValue["candidate_commit"].(string)
	if candidate == baseCommit {
		t.Fatal("candidate equals base")
	}
	branch, ok := publicationValue["branch"].(string)
	if !ok || branch != "harness/"+runID {
		t.Fatalf("publication branch = %#v", publicationValue["branch"])
	}
	changed := strings.Fields(runGitOutput(t, repository, "diff", "--name-only", baseCommit, candidate))
	if len(changed) != 1 || changed[0] != "add.go" {
		t.Fatalf("changed paths = %v", changed)
	}
	runGitE2E(t, repository, "worktree", "add", "--detach", filepath.Join(root, "candidate"), candidate)
	command := exec.Command("make", "check")
	command.Dir = filepath.Join(root, "candidate")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("candidate verification: %v: %s", err, output)
	}
	assertPublishedFixture(t, repository, remote, baseCommit, candidate, branch, prState)
	assertReasoningEvidence(t, pool, artifactRoot, runID)
	assertDurableOutcome(t, pool, runID, taskID, actorIDs[1], candidate)
	beforeReplay := snapshotSideEffects(
		t, pool, repository, remote, runID, taskID, prState,
	)

	api.stop(t)
	api = startProcess(t, ctx, apiBinary, apiConfig)
	processes = append(processes, api)
	waitHTTP(t, "http://"+apiAddress+"/healthz", "", http.StatusOK)
	replayedApproval := client.mutateWithKey(
		t, approvalPath, approvalRevision, approvalKey, approvalRequest, http.StatusOK,
	)
	if !reflect.DeepEqual(replayedApproval, approvalResponse) {
		t.Fatalf("approval replay changed response: first=%#v replay=%#v",
			approvalResponse, replayedApproval)
	}
	afterReplay := snapshotSideEffects(
		t, pool, repository, remote, runID, taskID, prState,
	)
	if afterReplay != beforeReplay {
		t.Fatalf("approval replay repeated a side effect: before=%#v after=%#v",
			beforeReplay, afterReplay)
	}
	var events int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workflow_events
		WHERE aggregate_id IN ($1,$2)`, runID, taskID).Scan(&events); err != nil || events < 10 {
		t.Fatalf("events = %d, %v", events, err)
	}
	assertSecretAbsent(t, apiKey, pool, artifactRoot, repository,
		[]string{
			apiConfig, workflowConfig, promptPaths[0], promptPaths[1],
			manifestPaths[0], manifestPaths[1],
		}, processes)
}

type process struct {
	command *exec.Cmd
	logs    *bytes.Buffer
}

func startProcess(t *testing.T, ctx context.Context, binary, config string) *process {
	t.Helper()
	command := exec.CommandContext(ctx, binary, "-config", config)
	logs := &bytes.Buffer{}
	command.Stdout, command.Stderr = logs, logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	result := &process{command: command, logs: logs}
	t.Cleanup(func() {
		if t.Failed() && logs.Len() > 0 {
			sanitized := logs.String()
			if secret := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); secret != "" {
				sanitized = strings.ReplaceAll(sanitized, secret, "[REDACTED]")
			}
			t.Logf("%s logs:\n%s", filepath.Base(binary), sanitized)
		}
	})
	return result
}

func (p *process) stop(t *testing.T) {
	t.Helper()
	if p.command.ProcessState != nil {
		return
	}
	signalErr := p.command.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- p.command.Wait() }()
	select {
	case <-time.After(10 * time.Second):
		_ = p.command.Process.Kill()
		t.Fatalf("process did not stop: %s", p.logs)
	case err := <-done:
		if signalErr != nil && err != nil {
			t.Fatalf("process stopped before interrupt: signal=%v wait=%v: %s",
				signalErr, err, p.logs)
		}
	}
}

type runtimeClient struct {
	base, token string
}

func (c runtimeClient) mutate(
	t *testing.T, path, revision string, body any, status int,
) map[string]any {
	t.Helper()
	return c.mutateWithKey(t, path, revision, uuid.NewString(), body, status)
}

func (c runtimeClient) mutateWithKey(
	t *testing.T, path, revision, idempotencyKey string, body any, status int,
) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	if revision != "" {
		request.Header.Set("If-Match", revision)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		t.Fatalf("%s status=%d body=%s", path, response.StatusCode, responseBody)
	}
	var value map[string]any
	if err := json.Unmarshal(responseBody, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (c runtimeClient) run(t *testing.T, runID string) map[string]any {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, c.base+"/v1/runs/"+runID, http.NoBody)
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	var value map[string]any
	_ = json.NewDecoder(response.Body).Decode(&value)
	return value
}

func (c runtimeClient) revision(t *testing.T, runID string) string {
	t.Helper()
	value := c.run(t, runID)
	run := value["run"].(map[string]any)
	return fmt.Sprintf("\"%.0f\"", run["revision"].(float64))
}

func fixtureRepository(t *testing.T, root string) (string, string, string) {
	t.Helper()
	repository := filepath.Join(root, "repository")
	remote := filepath.Join(root, "remote.git")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitE2E(t, repository, "init", "-q", "-b", "main")
	runGitE2E(t, repository, "config", "user.name", "Fixture")
	runGitE2E(t, repository, "config", "user.email", "fixture@example.invalid")
	writeFile(t, filepath.Join(repository, "go.mod"), "module fixture\n\ngo 1.23\n")
	writeFile(t, filepath.Join(repository, "add.go"), "package fixture\n\nfunc Add(a, b int) int { return a - b }\n")
	writeFile(t, filepath.Join(repository, "add_test.go"), "package fixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(2, 3) != 5 { t.Fatal(\"wrong\") } }\n")
	writeFile(t, filepath.Join(repository, "Makefile"), ".PHONY: check\ncheck:\n\tgo test ./...\n")
	runGitE2E(t, repository, "add", ".")
	runGitE2E(t, repository, "commit", "-qm", "fixture")
	runGitE2E(t, root, "init", "--bare", "-q", remote)
	runGitE2E(t, repository, "remote", "add", "origin", remote)
	runGitE2E(t, repository, "push", "-q", "origin", "main:main")
	base := strings.TrimSpace(runGitOutput(t, repository, "rev-parse", "HEAD"))
	return repository, "origin", base
}

func runtimeManifests(t *testing.T, root string) ([2]string, [2]string) {
	t.Helper()
	var prompts [2]string
	var manifests [2]string
	for index, stageName := range []string{"implementation", "review"} {
		prompt := "Return only a valid implementation_proposal.v1 JSON object for the " +
			"approved task. Set a non-empty summary and exactly one changes item: path add.go, " +
			"operation update, expected_original_sha256 equal to the supplied add.go digest, " +
			"replacement_content equal to the complete add.go file with Add returning a + b, " +
			"a non-empty rationale, and acceptance_criterion_ids exactly [\"AC-001\"]. " +
			"Set requested_declared_check_ids exactly [\"make-check-v1\"]. Do not request a " +
			"scope change and do not change tests or any other file."
		if stageName == "review" {
			prompt = "Independently review the exact candidate diff and verification evidence. " +
				"Return advisory accept only when the candidate is in scope and all assigned " +
				"acceptance criteria are proven by the trusted check."
		}
		prompts[index] = filepath.Join(root, stageName+".md")
		writeFile(t, prompts[index], prompt)
		digest := Digest([]byte(prompt))
		capability, output := "strong_coding", "implementation_proposal.v1"
		if stageName == "review" {
			capability, output = "independent_review", "review_proposal.v1"
		}
		value := manifest.Manifest{
			SchemaVersion: "1", Agent: manifest.Agent{Name: "runtime-" + stageName, Version: "1.0.0"},
			Stage: stageName, Prompt: manifest.Prompt{
				ArtifactURI: "artifact://sha256/" + digest, SHA256: digest,
			},
			Model: manifest.Model{
				CapabilityClass: capability, Temperature: 0, MaximumOutputTokens: 20000,
			},
			Context: manifest.Context{
				IncludeSpecification: true, IncludeTask: true,
				RepositorySelection: "kernel_selected", MaximumContextTokens: 200000,
			},
			Tools: manifest.Tools{AllowedRequests: []string{
				"read_repository_file", "report_blocker", "request_declared_check", "search_repository",
			}},
			Output:   manifest.Output{Schema: output},
			Metadata: manifest.Metadata{Description: "runtime fixture", Labels: []string{"test"}},
		}
		manifests[index] = filepath.Join(root, stageName+".manifest.json")
		writeJSONFile(t, manifests[index], value)
	}
	return prompts, manifests
}

func specificationPair(
	runID string, now time.Time,
) (*reasoningv1.SpecificationRequest, *reasoningv1.SpecificationProposal) {
	envelope := reasoningEnvelope(runID, nil,
		reasoningv1.ReasoningStage_REASONING_STAGE_SPECIFICATION, now)
	request := &reasoningv1.SpecificationRequest{
		Envelope: envelope, ProblemStatement: "Add is incorrect",
		DesiredOutcome: "Add returns a plus b", Stakeholders: []string{"maintainer"},
	}
	proposal := &reasoningv1.SpecificationProposal{
		Identity: proposalIdentity(envelope), Title: "Correct Add", Goal: "Return sums",
		Actors: []string{"maintainer"}, AcceptanceCriteria: []*reasoningv1.AcceptanceCriterion{{
			CriterionId: "AC-001", Description: "Add returns the sum",
			VerificationMethod: "make check",
		}},
	}
	return request, proposal
}

func planningPair(
	t *testing.T, runID, taskID string, now time.Time,
	repository, baseCommit, specificationDigest string,
) (*reasoningv1.TaskPlanningRequest, *reasoningv1.TaskGraphProposal) {
	t.Helper()
	snapshot, err := SnapshotRepository(t.Context(), repository, baseCommit)
	if err != nil {
		t.Fatal(err)
	}
	envelope := reasoningEnvelope(runID, nil,
		reasoningv1.ReasoningStage_REASONING_STAGE_PLANNING, now.Add(time.Second))
	request := &reasoningv1.TaskPlanningRequest{
		Envelope: envelope, ApprovedSpecificationId: "SPEC-001",
		ApprovedSpecificationDigest: specificationDigest, RepositoryMap: snapshot.Entries,
		ReadablePaths:   []string{"add.go", "add_test.go"},
		WritablePaths:   []string{"add.go"},
		ProhibitedPaths: []string{"add_test.go", "go.mod", "Makefile"},
		TaskCountLimit:  1, ParallelismLimit: 1,
		AcceptanceCriterionIds: []string{"AC-001"},
	}
	graph := &reasoningv1.TaskGraphProposal{
		Identity: envelopeIdentity(envelope), ApprovedSpecificationId: "SPEC-001",
		ApprovedSpecificationDigest: specificationDigest,
		Tasks: []*reasoningv1.PlannedTask{{
			TaskId: taskID, Objective: "Correct Add",
			AcceptanceCriterionIds: []string{"AC-001"},
			ReadablePaths:          []string{"add.go", "add_test.go"},
			WritablePaths:          []string{"add.go"},
			ProhibitedPaths:        []string{"add_test.go", "go.mod", "Makefile"},
			RequiredCheckIds:       []string{"make-check-v1"},
			StopConditions:         []string{"make check passes"},
		}},
	}
	return request, graph
}

func reasoningEnvelope(
	runID string, taskID *string, stage reasoningv1.ReasoningStage, now time.Time,
) *reasoningv1.ReasoningRequestEnvelope {
	return &reasoningv1.ReasoningRequestEnvelope{
		SchemaVersion: "1", RequestId: uuid.NewString(), RunId: runID, TaskId: taskID,
		Stage: stage, Attempt: 1, CreatedAt: timestamppb.New(now),
		ExpiresAt: timestamppb.New(now.Add(time.Hour)),
		Authority: &reasoningv1.AuthorityConstraints{
			Mode: reasoningv1.AuthorityMode_AUTHORITY_MODE_PROPOSAL_ONLY,
		},
		Budget: &reasoningv1.ReasoningBudget{
			MaximumInputTokens: 1000, MaximumOutputTokens: 1000, MaximumProviderRequests: 1,
		},
		AgentManifestDigest: strings.Repeat("a", 64),
	}
}

func proposalIdentity(envelope *reasoningv1.ReasoningRequestEnvelope) *reasoningv1.ProposalIdentity {
	return envelopeIdentity(envelope)
}

func envelopeIdentity(envelope *reasoningv1.ReasoningRequestEnvelope) *reasoningv1.ProposalIdentity {
	return &reasoningv1.ProposalIdentity{
		SchemaVersion: envelope.SchemaVersion, RequestId: envelope.RequestId,
		RunId: envelope.RunId, TaskId: envelope.TaskId, Stage: envelope.Stage,
		Attempt: envelope.Attempt, AgentManifestDigest: envelope.AgentManifestDigest,
	}
}

func protoPair(t *testing.T, request, proposal proto.Message, decision time.Time) map[string]any {
	t.Helper()
	options := protojson.MarshalOptions{UseProtoNames: true}
	requestBody, err := options.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	proposalBody, err := options.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"request": json.RawMessage(requestBody), "proposal": json.RawMessage(proposalBody),
		"decision_timestamp": decision,
	}
}

type pullRequestState struct {
	mu      sync.Mutex
	creates int
	created map[string]any
}

func loopbackPullRequests(t *testing.T) (*httptest.Server, *pullRequestState) {
	t.Helper()
	state := &pullRequestState{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if state.created == nil {
				_, _ = w.Write([]byte("[]"))
			} else {
				_ = json.NewEncoder(w).Encode([]any{state.created})
			}
			return
		}
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		state.creates++
		state.created = map[string]any{
			"number": 1, "html_url": "http://127.0.0.1/draft/1", "state": "open",
			"draft": true, "body": input["body"],
			"head": map[string]any{"ref": input["head"]},
			"base": map[string]any{"ref": input["base"]},
		}
		_ = json.NewEncoder(w).Encode(state.created)
	}))
	return server, state
}

func waitHTTP(t *testing.T, target, token string, status int) {
	t.Helper()
	waitFor(t, 20*time.Second, func() bool {
		request, _ := http.NewRequest(http.MethodGet, target, http.NoBody)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == status
	})
}

func waitFor(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGitE2E(t *testing.T, root string, arguments ...string) {
	t.Helper()
	_ = runGitOutput(t, root, arguments...)
}

func runGitOutput(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(output)
}

func requireEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Skip(name + " is required")
	}
	return value
}

func assertReasoningEvidence(
	t *testing.T, pool *pgxpool.Pool, artifactRoot, runID string,
) {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT request_id,stage,provider,model,input_tokens,
		output_tokens,provider_requests,COALESCE(provider_request_id,''),
		proposal_artifact_uri,provider_response_artifact_uri,final_status
		FROM reasoning_invocations WHERE run_id=$1 ORDER BY stage`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type evidence struct {
		requestIdentity, stage, provider, model, providerRequestID string
		proposalURI, responseURI, status                           string
		input, output                                              int64
		requests                                                   int
	}
	var values []evidence
	for rows.Next() {
		var value evidence
		if err := rows.Scan(
			&value.requestIdentity, &value.stage, &value.provider, &value.model,
			&value.input, &value.output, &value.requests, &value.providerRequestID,
			&value.proposalURI, &value.responseURI, &value.status,
		); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 {
		t.Fatalf("reasoning invocations = %d; want exactly implementation and review", len(values))
	}
	if values[0].stage != "implementation" || values[1].stage != "review" ||
		values[0].requestIdentity == values[1].requestIdentity {
		t.Fatalf("reasoning stage identities = %#v", values)
	}
	for _, value := range values {
		if value.provider != "minimax-anthropic" || value.model != "MiniMax-M2.7" ||
			value.requests != 1 || value.proposalURI == "" || value.responseURI == "" ||
			value.status != "accepted" || value.providerRequestID == "" ||
			value.input <= 0 || value.output <= 0 {
			t.Fatalf("live reasoning evidence for %s = %#v", value.stage, value)
		}
		assertArtifactIntegrity(t, artifactRoot, value.proposalURI)
		assertArtifactIntegrity(t, artifactRoot, value.responseURI)
	}
}

func assertDurableOutcome(
	t *testing.T, pool *pgxpool.Pool, runID, taskID, approverID, candidate string,
) {
	t.Helper()
	queries := []struct {
		name, query string
		arguments   []any
	}{
		{"verification", `SELECT count(*) FROM verification_ledger
			WHERE state='completed' AND result_json->>'CandidateCommit'=$1`, []any{candidate}},
		{"review", `SELECT count(*) FROM workflow_tasks WHERE run_id=$1 AND task_id=$2
			AND review_uri IS NOT NULL AND review_digest IS NOT NULL`, []any{runID, taskID}},
		{"approval", `SELECT count(*) FROM task_approvals WHERE run_id=$1 AND task_id=$2
			AND principal_id=$3 AND candidate_commit=$4 AND decision='approve'
			AND state='completed'`, []any{runID, taskID, approverID, candidate}},
		{"publication", `SELECT count(*) FROM draft_pull_request_publications
			WHERE published_candidate_commit=$1 AND state='completed'`, []any{candidate}},
	}
	for _, check := range queries {
		var count int
		if err := pool.QueryRow(t.Context(), check.query, check.arguments...).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s durable evidence count = %d, %v", check.name, count, err)
		}
	}
}

func assertArtifactIntegrity(t *testing.T, artifactRoot, uri string) {
	t.Helper()
	const prefix = "artifact://sha256/"
	digest := strings.TrimPrefix(uri, prefix)
	if len(digest) != 64 || prefix+digest != uri {
		t.Fatalf("invalid artifact URI %q", uri)
	}
	body, err := os.ReadFile(filepath.Join(artifactRoot, digest[:2], digest))
	if err != nil {
		t.Fatalf("read artifact %s: %v", uri, err)
	}
	if Digest(body) != digest {
		t.Fatalf("artifact integrity mismatch for %s", uri)
	}
}

func assertPublishedFixture(
	t *testing.T, repository, remote, base, candidate, branch string,
	state *pullRequestState,
) {
	t.Helper()
	remoteHead := strings.Fields(runGitOutput(
		t, repository, "ls-remote", "--heads", remote, "refs/heads/"+branch,
	))
	if len(remoteHead) != 2 || remoteHead[0] != candidate ||
		remoteHead[1] != "refs/heads/"+branch {
		t.Fatalf("remote harness branch = %v", remoteHead)
	}
	harnessHeads := strings.Fields(runGitOutput(
		t, repository, "ls-remote", "--heads", remote, "refs/heads/harness/*",
	))
	if len(harnessHeads) != 2 || harnessHeads[0] != candidate ||
		harnessHeads[1] != "refs/heads/"+branch {
		t.Fatalf("remote harness branches = %v", harnessHeads)
	}
	remoteBase := strings.Fields(runGitOutput(
		t, repository, "ls-remote", "--heads", remote, "refs/heads/main",
	))
	if len(remoteBase) != 2 || remoteBase[0] != base ||
		remoteBase[1] != "refs/heads/main" {
		t.Fatalf("remote base branch = %v", remoteBase)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.creates != 1 || state.created == nil ||
		state.created["state"] != "open" || state.created["draft"] != true {
		t.Fatalf("draft PR state = creates:%d value:%#v", state.creates, state.created)
	}
	head, _ := state.created["head"].(map[string]any)
	baseValue, _ := state.created["base"].(map[string]any)
	if head["ref"] != branch || baseValue["ref"] != "main" {
		t.Fatalf("draft PR bindings = head:%#v base:%#v candidate:%s expected-base:%s",
			head, baseValue, candidate, base)
	}
}

type sideEffects struct {
	reasoning, approvals, publications, pullRequests int
	remoteRefs                                       string
}

func snapshotSideEffects(
	t *testing.T, pool *pgxpool.Pool, repository, remote, runID, taskID string,
	state *pullRequestState,
) sideEffects {
	t.Helper()
	var value sideEffects
	queries := []struct {
		target *int
		query  string
		args   []any
	}{
		{&value.reasoning, `SELECT count(*) FROM reasoning_invocations WHERE run_id=$1`,
			[]any{runID}},
		{&value.approvals, `SELECT count(*) FROM task_approvals
			WHERE run_id=$1 AND task_id=$2`, []any{runID, taskID}},
		{&value.publications, `SELECT count(*) FROM draft_pull_request_publications
			WHERE published_branch=$1`, []any{"harness/" + runID}},
	}
	for _, query := range queries {
		if err := pool.QueryRow(t.Context(), query.query, query.args...).Scan(query.target); err != nil {
			t.Fatal(err)
		}
	}
	state.mu.Lock()
	value.pullRequests = state.creates
	state.mu.Unlock()
	value.remoteRefs = runGitOutput(t, repository, "ls-remote", "--heads", remote)
	return value
}

func assertSecretAbsent(
	t *testing.T,
	secret string,
	pool *pgxpool.Pool,
	artifactRoot, repository string,
	configPaths []string,
	processes []*process,
) {
	t.Helper()
	check := func(name string, body []byte) {
		t.Helper()
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("ANTHROPIC_API_KEY persisted or logged in %s", name)
		}
	}
	for _, path := range configPaths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		check(path, body)
	}
	for _, process := range processes {
		check(filepath.Base(process.command.Path)+" logs", process.logs.Bytes())
	}
	if err := filepath.WalkDir(artifactRoot, func(
		path string, entry os.DirEntry, err error,
	) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		check(path, body)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(t.Context(), `SELECT schemaname, tablename
		FROM pg_catalog.pg_tables
		WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY schemaname, tablename`)
	if err != nil {
		t.Fatalf("list PostgreSQL evidence tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Schema string
		Table  string
	}])
	if err != nil {
		t.Fatalf("collect PostgreSQL evidence tables: %v", err)
	}
	connection, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire PostgreSQL evidence connection: %v", err)
	}
	defer connection.Release()
	for _, table := range tables {
		var dump bytes.Buffer
		name := pgx.Identifier{table.Schema, table.Table}.Sanitize()
		if _, err := connection.Conn().PgConn().CopyTo(
			t.Context(), &dump, "COPY "+name+" TO STDOUT WITH (FORMAT csv, HEADER true)",
		); err != nil {
			t.Fatalf("export PostgreSQL evidence table %s: %v", name, err)
		}
		check("PostgreSQL evidence table "+name, dump.Bytes())
	}
	assertSecretAbsentFromGit(t, secret, repository)
}

func assertSecretAbsentFromGit(t *testing.T, secret, repository string) {
	t.Helper()
	objectLines := strings.Split(
		runGitOutput(t, repository, "rev-list", "--objects", "--all"), "\n",
	)
	seen := make(map[string]struct{}, len(objectLines))
	for _, line := range objectLines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		objectID := fields[0]
		if _, ok := seen[objectID]; ok {
			continue
		}
		seen[objectID] = struct{}{}
		command := exec.Command("git", "-C", repository, "cat-file", "-p", objectID)
		body, err := command.Output()
		if err != nil {
			t.Fatalf("inspect reachable fixture Git object %s: %v", objectID, err)
		}
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("ANTHROPIC_API_KEY persisted in reachable fixture Git object %s", objectID)
		}
	}
}
