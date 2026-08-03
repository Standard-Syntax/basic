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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const packagedHelperImage = "alpine:3.23.3@sha256:59855d3dceb3ae53991193bd03301e082b2a7faa56a514b03527ae0ec2ce3a95"

func TestBetaLiveProcessesCompleteDisposableFixture(t *testing.T) {
	if os.Getenv("BETA_LIVE_E2E") != "1" {
		t.Skip("BETA_LIVE_E2E=1 is required")
	}
	if os.Getenv("BETA_CANARY") == "1" {
		t.Skip("real canary mode uses the dedicated test entrypoint")
	}
	runBetaProcesses(t)
}

func TestBetaLiveProcessesCompleteGeneratedPythonProject(t *testing.T) {
	if os.Getenv("BETA_LIVE_E2E") != "1" || os.Getenv("BETA_PYTHON_PROJECT") != "1" {
		t.Skip("BETA_LIVE_E2E=1 and BETA_PYTHON_PROJECT=1 are required")
	}
	runBetaProcesses(t)
}

func TestBetaCanaryProcessesPublishRealDraft(t *testing.T) {
	if os.Getenv("BETA_CANARY") != "1" {
		t.Skip("BETA_CANARY=1 is required")
	}
	runBetaProcesses(t)
}

func TestPackagedProcessMountsAreExplicitAndCredentialFilesAreReadOnly(t *testing.T) {
	root := t.TempDir()
	paths := map[string]string{
		"config": filepath.Join(root, "api.json"), "artifacts": filepath.Join(root, "artifacts"),
		"repository": filepath.Join(root, "repository"), "token": filepath.Join(root, "github-token"),
		"key": filepath.Join(root, "git-key"),
	}
	values := map[string]any{
		"artifact_root": paths["artifacts"], "repository_root": paths["repository"],
		"publication": map[string]any{"token_file": paths["token"], "git_push_credential_file": paths["key"]},
	}
	arguments := strings.Join(packagedProcessMounts(t, paths["config"], values), " ")
	for _, path := range paths {
		if !strings.Contains(arguments, "src="+path+",dst="+path) {
			t.Fatalf("missing explicit mount for %s: %s", path, arguments)
		}
	}
	for _, path := range []string{paths["config"], paths["token"], paths["key"]} {
		if !strings.Contains(arguments, "src="+path+",dst="+path+",readonly") {
			t.Fatalf("sensitive mount is writable: %s", path)
		}
	}
	if strings.Contains(arguments, "src="+root+",dst="+root+" ") {
		t.Fatalf("broad test root was mounted: %s", arguments)
	}
}

func runBetaProcesses(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Fatal("ANTHROPIC_API_KEY is required for the beta live E2E")
	}
	apiBinary := requireEnvironment(t, "RUNTIME_API_BINARY")
	workflowBinary := requireEnvironment(t, "RUNTIME_WORKFLOW_BINARY")
	packaged := os.Getenv("RUNTIME_PACKAGED") == "1"
	canaryMode := os.Getenv("BETA_CANARY") == "1"
	pythonProject := os.Getenv("BETA_PYTHON_PROJECT") == "1"
	if pythonProject && (packaged || canaryMode) {
		t.Fatal("generated Python project mode requires the local disposable runtime")
	}
	var canaryConfig beta.Config
	var databaseURL string
	if canaryMode {
		var err error
		canaryConfig, err = beta.LoadConfig(requireEnvironment(t, "BETA_CONFIG"))
		if err != nil {
			t.Fatal(err)
		}
		if err := canaryConfig.ValidateCanary(); err != nil {
			t.Fatal(err)
		}
		databaseURL = canaryConfig.DatabaseURL
	} else {
		databaseURL = requireEnvironment(t, "TEST_DATABASE_URL")
	}
	root := t.TempDir()
	credentialRoot := ""
	if packaged {
		makeTreeAccessible(t, root)
		credentialRoot = t.TempDir()
		if err := os.Chmod(credentialRoot, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repository, remote, remoteURL, baseCommit := "", "", "", ""
	repositoryOwner, repositoryName, branchPrefix := "local", "fixture", "harness/"
	tokenFile, pushCredential, apiEndpoint := "", "", ""
	artifactRoot, worktrees, verificationRoot := "", "", ""
	executionImage, verificationImage := "", ""
	var policy any
	if canaryMode {
		repository, remote = canaryConfig.Policy.Repository.Root, canaryConfig.Policy.Repository.Remote
		remoteURL = canaryConfig.Policy.Repository.RemoteURL
		baseCommit = canaryConfig.Policy.Repository.BaseCommit
		repositoryOwner, repositoryName, branchPrefix = beta.CanaryOwner, beta.CanaryRepository, "harness/canary/"
		tokenFile, pushCredential = canaryConfig.PublicationCredentialFile, canaryConfig.GitPushCredentialFile
		apiEndpoint = "https://api.github.com"
		artifactRoot, worktrees = canaryConfig.ArtifactRoot, canaryConfig.WorktreeRoot
		verificationRoot = canaryConfig.VerificationWorkspaceRoot
		executionImage, verificationImage = canaryConfig.Policy.Images.Execution, canaryConfig.Policy.Images.Verification
		policy = canaryConfig.Policy
	} else {
		if pythonProject {
			repository, remote, baseCommit = fixturePythonRepository(
				t, root, requireEnvironment(t, "RUNTIME_SOURCE_ROOT"),
			)
		} else {
			repository, remote, baseCommit = fixtureRepository(t, root)
		}
		remoteURL = filepath.Join(root, "remote.git")
		if packaged {
			server := smartGitRemote(t, remoteURL)
			remoteURL = server.URL + "/remote.git"
			runGitE2E(t, repository, "remote", "set-url", remote, remoteURL)
		}
		if packaged {
			executionImage = requireEnvironment(t, "RUNTIME_EXECUTION_IMAGE")
			verificationImage = requireEnvironment(t, "RUNTIME_VERIFICATION_IMAGE")
		} else {
			executionImage = dockerImageID(t, "basic-execution-worker:runtime")
			verificationImage = dockerImageID(t, "basic-verification-worker:runtime")
		}
		paths := map[string]any{"readable": []string{"add.go", "add_test.go"}, "writable": []string{"add.go"}, "prohibited": []string{"Makefile", "add_test.go", "go.mod"}}
		if pythonProject {
			paths = map[string]any{
				"readable":   []string{"Makefile", "pyproject.toml", "src", "tests", "uv.lock"},
				"writable":   []string{"src"},
				"prohibited": []string{".harness", "Makefile", "pyproject.toml", "tests", "uv.lock"},
			}
		}
		policy = map[string]any{
			"version": "1.0",
			"repository": map[string]any{"owner": "local", "name": "fixture", "root": repository,
				"remote": remote, "remote_url": remoteURL, "base_branch": "main", "base_commit": baseCommit},
			"paths":          paths,
			"trusted_checks": []string{"make-check-v1"},
			"limits":         map[string]any{"maximum_tasks": 1, "maximum_changed_files": 4, "maximum_file_bytes": 1 << 20, "maximum_total_bytes": 4 << 20, "execution_concurrency": 1, "verification_concurrency": 1},
			"images":         map[string]any{"execution": executionImage, "verification": verificationImage},
		}
		artifactRoot, worktrees = filepath.Join(root, "artifacts"), filepath.Join(root, "worktrees")
		verificationRoot = filepath.Join(root, "verification")
		for _, path := range []string{artifactRoot, worktrees, verificationRoot} {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if packaged && os.Chmod(path, 0o777) != nil {
				t.Fatal("make packaged path accessible")
			}
		}
	}
	promptPaths, manifestPaths := runtimeManifests(t, root)
	token := "runtime-operator-token"
	tokenDigest := sha256.Sum256([]byte(token))
	var prServer *httptest.Server
	var prState *pullRequestState
	if !canaryMode {
		prServer, prState = loopbackPullRequests(t)
		defer prServer.Close()
		tokenFile = filepath.Join(root, "github-token")
		if packaged {
			tokenFile = filepath.Join(credentialRoot, "github-credential")
		}
		if err := os.WriteFile(tokenFile, []byte("loopback-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		apiEndpoint = prServer.URL
	}
	minimaxCredential := ""
	var canaryCredentialValues []string
	if packaged {
		minimaxCredential = filepath.Join(credentialRoot, "minimax-credential")
		writeFile(t, minimaxCredential, apiKey)
		if canaryMode {
			var tokenValue, pushValue string
			tokenFile, tokenValue = copyCredential(t, tokenFile, filepath.Join(credentialRoot, "github-credential"))
			pushCredential, pushValue = copyCredential(t, pushCredential, filepath.Join(credentialRoot, "git-push-credential"))
			canaryCredentialValues = []string{tokenValue, pushValue}
		}
		t.Cleanup(func() { restorePackagedCredentialRoot(t, credentialRoot) })
		chownPackagedCredentials(t, credentialRoot, minimaxCredential, tokenFile, pushCredential)
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
		"beta_policy":      policy,
		"service_actor_id": actorIDs[0], "max_artifact_bytes": 4 << 20,
		"max_body_bytes": 4 << 20, "trusted_checks": []string{"make-check-v1"},
		"principals": []map[string]any{{
			"id": actorIDs[1], "token_sha256": hex.EncodeToString(tokenDigest[:]),
			"roles": []string{"operator", "approver", "elevated_approver"},
		}},
		"publication": map[string]any{
			"repository_root": repository, "repository_owner": repositoryOwner,
			"repository_name": repositoryName, "remote": remote, "base_branch": "main",
			"branch_prefix": branchPrefix, "actor_id": actorIDs[6],
			"api_endpoint": apiEndpoint, "api_version": "2022-11-28",
			"token_file": tokenFile, "git_push_credential_file": pushCredential,
		},
	})
	workflowConfig := filepath.Join(root, "workflow.json")
	workflowValues := map[string]any{
		"database_url": databaseURL, "artifact_root": artifactRoot,
		"owner_id": uuid.NewString(), "max_artifact_bytes": 4 << 20,
		"repository_root": repository, "worktree_root": worktrees,
		"verification_workspace_root": verificationRoot,
		"execution_worker_image":      executionImage,
		"verification_worker_image":   verificationImage,
		"beta_policy":                 policy,
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
	if packaged {
		workflowValues["worker_uid"], workflowValues["worker_gid"] = 65532, 65532
		workflowValues["provider"] = map[string]any{"mode": "minimax_anthropic",
			"base_url": "https://api.minimax.io/anthropic", "model": "MiniMax-M2.7",
			"api_key_file": minimaxCredential}
	}
	writeJSONFile(t, workflowConfig, workflowValues)
	planningSnapshot, err := SnapshotRepository(t.Context(), repository, baseCommit)
	if err != nil {
		t.Fatal(err)
	}
	if packaged {
		if err := os.Chmod(apiConfig, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(workflowConfig, 0o644); err != nil {
			t.Fatal(err)
		}
		makePackagedTreeAccessible(t, root)
		chownPackagedTree(t, root)
		t.Cleanup(func() { restorePackagedTreeOwnership(t, root) })
		if canaryMode {
			paths := []string{repository, artifactRoot, worktrees, verificationRoot}
			t.Cleanup(func() { restorePackagedCanaryRoots(t, paths) })
			provisionPackagedCanaryRoots(t, paths)
		}
		if !canaryMode {
			command := exec.Command("docker", "run", "--rm", "--network", "host",
				"--entrypoint", "/usr/bin/git", apiBinary,
				"ls-remote", "--heads", remoteURL, "refs/heads/main")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("packaged Git smart-HTTP probe: %v: %s", err, output)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var processes []*process
	api := startProcess(t, ctx, apiBinary, apiConfig)
	processes = append(processes, api)
	waitHTTP(t, "http://"+apiAddress+"/healthz", "", http.StatusOK)
	client := &runtimeClient{base: "http://" + apiAddress, token: token}
	runID, taskID := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	runKey := uuid.NewString()
	objective := "correct Add without changing tests"
	if pythonProject {
		objective = "implement the trusted Python project without changing tests or configuration"
	}
	runRequest := map[string]any{
		"run_id": runID, "base_commit": baseCommit,
		"content":            map[string]any{"objective": objective},
		"decision_timestamp": now,
	}
	firstRun := client.mutateResponseWithKey(
		t, "/v1/runs", "", runKey, runRequest, http.StatusCreated,
	)
	if firstRun.replay {
		t.Fatal("initial run intake was marked as a replay")
	}
	specRequest, specProposal := specificationPair(runID, now)
	client.mutate(t, "/v1/runs/"+runID+"/specification", client.revision(t, runID),
		protoPair(t, specRequest, specProposal, now.Add(time.Second)), http.StatusOK)
	client.mutate(t, "/v1/runs/"+runID+"/specification/approve", client.revision(t, runID),
		map[string]any{"decision_timestamp": now.Add(2 * time.Second)}, http.StatusOK)
	specBody, _ := proto.MarshalOptions{Deterministic: true}.Marshal(specProposal)
	planningRequest, graph := planningPair(
		runID, taskID, now, repository, planningSnapshot, Digest(specBody),
	)
	client.mutate(t, "/v1/runs/"+runID+"/task-graph", client.revision(t, runID),
		protoPair(t, planningRequest, graph, now.Add(3*time.Second)), http.StatusOK)
	client.mutate(t, "/v1/runs/"+runID+"/task-graph/approve", client.revision(t, runID),
		map[string]any{"decision_timestamp": now.Add(4 * time.Second)}, http.StatusOK)
	api.stop(t)
	api = startProcess(t, ctx, apiBinary, apiConfig)
	processes = append(processes, api)
	waitHTTP(t, "http://"+apiAddress+"/healthz", "", http.StatusOK)
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if packaged {
		makePackagedPathAccessible(t, artifactRoot)
	}
	beforeRunReplay := snapshotRunIntakeSideEffects(t, pool, artifactRoot, runID)
	var replayedRun mutationResponse
	func() {
		gitPath := filepath.Join(repository, ".git")
		hiddenGitPath := filepath.Join(repository, ".git-hidden")
		if err := os.Rename(gitPath, hiddenGitPath); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Rename(hiddenGitPath, gitPath); err != nil {
				t.Fatal(err)
			}
		}()
		replayedRun = client.mutateResponseWithKey(
			t, "/v1/runs", "", runKey, runRequest, http.StatusCreated,
		)
	}()
	if !replayedRun.replay || !bytes.Equal(replayedRun.body, firstRun.body) {
		t.Fatalf("run replay header=%v first=%s replay=%s",
			replayedRun.replay, firstRun.body, replayedRun.body)
	}
	afterRunReplay := snapshotRunIntakeSideEffects(t, pool, artifactRoot, runID)
	if afterRunReplay != beforeRunReplay {
		t.Fatalf("run replay repeated a side effect: before=%#v after=%#v",
			beforeRunReplay, afterRunReplay)
	}
	workflowProcess := startProcess(t, ctx, workflowBinary, workflowConfig)
	processes = append(processes, workflowProcess)
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
			if stageName == "verification" {
				var resultDigest string
				if reportErr := pool.QueryRow(t.Context(), `SELECT result_digest
					FROM runtime_stage_jobs WHERE run_id=$1 AND task_id=$2
					AND stage='verification' AND state='COMPLETED'`, runID, taskID).
					Scan(&resultDigest); reportErr == nil {
					t.Fatalf("task required rework after verification: %s",
						verificationFailureOutput(t, artifactRoot, resultDigest))
				}
			}
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
	if approvalResponse["publication"] != nil || approvalResponse["result"] == nil {
		t.Fatalf("approval response = %#v", approvalResponse)
	}
	submitPath := "/v1/runs/" + runID + "/submit"
	submitRevision := client.revision(t, runID)
	submitKey := uuid.NewString()
	submitRequest := map[string]any{"decision_timestamp": now.Add(11 * time.Minute)}
	publicationValue := client.mutateWithKey(
		t, submitPath, submitRevision, submitKey, submitRequest, http.StatusOK,
	)
	if publicationValue["candidate_commit"] == "" ||
		publicationValue["pull_request_number"].(float64) <= 0 {
		t.Fatalf("publication = %#v", publicationValue)
	}
	candidate := publicationValue["candidate_commit"].(string)
	if candidate == baseCommit {
		t.Fatal("candidate equals base")
	}
	branch, ok := publicationValue["branch"].(string)
	if !ok || branch != branchPrefix+runID {
		t.Fatalf("publication branch = %#v", publicationValue["branch"])
	}
	changed := strings.Fields(runGitOutput(t, repository, "diff", "--name-only", baseCommit, candidate))
	expectedChanged := "add.go"
	if pythonProject {
		expectedChanged = "src/live_demo/__init__.py"
	}
	if len(changed) != 1 || changed[0] != expectedChanged {
		t.Fatalf("changed paths = %v", changed)
	}
	candidatePath := filepath.Join(root, "candidate")
	runGitE2E(t, repository, "worktree", "add", "--detach", candidatePath, candidate)
	t.Cleanup(func() {
		command := exec.Command("git", "worktree", "remove", "--force", candidatePath)
		command.Dir = repository
		_ = command.Run()
	})
	command := exec.Command("make", "check")
	command.Dir = candidatePath
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("candidate verification: %v: %s", err, output)
	}
	runGitE2E(t, repository, "worktree", "remove", candidatePath)
	if canaryMode {
		assertPublishedCanary(t, tokenFile, artifactRoot, repository, remote, baseCommit,
			candidate, branch, publicationValue)
	} else {
		assertPublishedFixture(t, repository, remote, baseCommit, candidate, branch, prState)
	}
	if packaged {
		makePackagedPathAccessible(t, artifactRoot)
	}
	assertReasoningEvidence(t, pool, artifactRoot, runID)
	assertDurableOutcome(t, pool, runID, taskID, actorIDs[1], candidate)
	beforeReplay := snapshotSideEffects(
		t, pool, repository, remote, branchPrefix, runID, taskID, prState,
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
	replayedPublication := client.mutateWithKey(
		t, submitPath, submitRevision, submitKey, submitRequest, http.StatusOK,
	)
	if !reflect.DeepEqual(replayedPublication, publicationValue) {
		t.Fatalf("publication replay changed response: first=%#v replay=%#v",
			publicationValue, replayedPublication)
	}
	afterReplay := snapshotSideEffects(
		t, pool, repository, remote, branchPrefix, runID, taskID, prState,
	)
	if afterReplay != beforeReplay {
		t.Fatalf("approval replay repeated a side effect: before=%#v after=%#v",
			beforeReplay, afterReplay)
	}
	if canaryMode {
		assertPublishedCanary(t, tokenFile, artifactRoot, repository, remote, baseCommit,
			candidate, branch, publicationValue)
	}
	var events int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workflow_events
		WHERE aggregate_id IN ($1,$2)`, runID, taskID).Scan(&events); err != nil || events < 10 {
		t.Fatalf("events = %d, %v", events, err)
	}
	if packaged {
		makePackagedPathAccessible(t, artifactRoot)
	}
	assertSecretAbsent(t, apiKey, pool, artifactRoot, repository,
		[]string{
			apiConfig, workflowConfig, promptPaths[0], promptPaths[1],
			manifestPaths[0], manifestPaths[1],
		}, processes)
	if canaryMode {
		for _, secret := range canaryCredentialValues {
			assertSecretAbsent(t, secret, pool, artifactRoot,
				repository, []string{apiConfig, workflowConfig}, processes)
		}
		artifactValue := publicationValue["publication_artifact"].(map[string]any)
		var verificationURI, verificationDigest, reviewURI, reviewDigest, approvalURI, approvalDigest string
		if err := pool.QueryRow(t.Context(), `SELECT verification_uri,verification_digest,
			review_uri,review_digest,approval_uri,approval_digest FROM workflow_tasks
			WHERE run_id=$1 AND task_id=$2`, runID, taskID).Scan(
			&verificationURI, &verificationDigest, &reviewURI, &reviewDigest,
			&approvalURI, &approvalDigest); err != nil {
			t.Fatal(err)
		}
		report := map[string]any{
			"run_id": runID, "task_id": taskID, "publication_id": publicationValue["publication_id"],
			"candidate_commit": candidate, "artifact_reference": artifactValue,
			"pull_request_url":      publicationValue["pull_request_url"],
			"verification_artifact": map[string]string{"uri": verificationURI, "digest": verificationDigest},
			"review_artifact":       map[string]string{"uri": reviewURI, "digest": reviewDigest},
			"approval_artifact":     map[string]string{"uri": approvalURI, "digest": approvalDigest},
			"images": map[string]string{
				"api_service":      requireEnvironment(t, "RUNTIME_API_BINARY"),
				"workflow_service": requireEnvironment(t, "RUNTIME_WORKFLOW_BINARY"),
				"execution_worker": executionImage, "verification_worker": verificationImage,
			},
			"cleanup_command": "make beta-canary-cleanup BETA_CONFIG=" +
				shellQuote(requireEnvironment(t, "BETA_CONFIG")) + " CANARY_PUBLICATION_ID=" +
				shellQuote(publicationValue["publication_id"].(string)),
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println(string(encoded))
	}
}

func verificationFailureOutput(t *testing.T, artifactRoot, resultDigest string) string {
	t.Helper()
	if len(resultDigest) != 64 {
		return "verification report digest invalid"
	}
	reportBody, err := os.ReadFile(filepath.Join(artifactRoot, resultDigest[:2], resultDigest))
	if err != nil {
		return "verification report unavailable"
	}
	var report struct {
		Checks []struct {
			Output workflow.ArtifactRef `json:"output"`
		} `json:"checks"`
	}
	if json.Unmarshal(reportBody, &report) != nil {
		return "verification report invalid"
	}
	var outputs []string
	for _, check := range report.Checks {
		if len(check.Output.Digest) != 64 {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(
			artifactRoot, check.Output.Digest[:2], check.Output.Digest,
		))
		if readErr == nil {
			outputs = append(outputs, string(body))
		}
	}
	return strings.Join(outputs, "\n")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type process struct {
	command *exec.Cmd
	logs    *bytes.Buffer
}

func startProcess(t *testing.T, ctx context.Context, binary, config string) *process {
	t.Helper()
	var command *exec.Cmd
	if os.Getenv("RUNTIME_PACKAGED") == "1" {
		arguments := []string{"run", "--rm", "--network", "host", "--read-only",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777",
			"--group-add", strconv.Itoa(os.Getgid())}
		body, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if json.Unmarshal(body, &values) != nil {
			t.Fatal("decode packaged process config")
		}
		arguments = append(arguments, packagedProcessMounts(t, config, values)...)
		if _, workflow := values["owner_id"]; workflow {
			arguments = append(arguments,
				"--group-add", requireEnvironment(t, "RUNTIME_DOCKER_GID"),
				"--mount", "type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock")
		}
		arguments = append(arguments, binary, "-config", config)
		command = exec.CommandContext(ctx, "docker", arguments...)
	} else {
		command = exec.CommandContext(ctx, binary, "-config", config)
	}
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

func makeTreeAccessible(t *testing.T, root string) {
	t.Helper()
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
}

func copyCredential(t *testing.T, source, target string) (string, string) {
	t.Helper()
	body, err := os.ReadFile(source)
	if err != nil || len(body) == 0 || len(body) > 1<<20 {
		t.Fatal("copy packaged credential")
	}
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatal("stage packaged credential")
	}
	return target, strings.TrimSpace(string(body))
}

func packagedProcessMounts(t *testing.T, config string, values map[string]any) []string {
	t.Helper()
	type mount struct {
		path     string
		readOnly bool
	}
	mounts := []mount{{path: config, readOnly: true}}
	add := func(path string, readOnly bool) {
		if path == "" {
			return
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, ",") {
			t.Fatal("invalid packaged mount path")
		}
		for index := range mounts {
			if mounts[index].path == path {
				mounts[index].readOnly = mounts[index].readOnly && readOnly
				return
			}
		}
		mounts = append(mounts, mount{path: path, readOnly: readOnly})
	}
	stringValue := func(values map[string]any, key string) string {
		value, _ := values[key].(string)
		return value
	}
	add(stringValue(values, "artifact_root"), false)
	add(stringValue(values, "repository_root"), values["owner_id"] == nil)
	if _, workflowProcess := values["owner_id"]; workflowProcess {
		add(stringValue(values, "worktree_root"), false)
		add(stringValue(values, "verification_workspace_root"), false)
		for _, key := range []string{"implementation_manifest_path", "implementation_prompt_path",
			"review_manifest_path", "review_prompt_path"} {
			add(stringValue(values, key), true)
		}
		provider, _ := values["provider"].(map[string]any)
		add(stringValue(provider, "api_key_file"), true)
	} else {
		publicationConfig, _ := values["publication"].(map[string]any)
		add(stringValue(publicationConfig, "token_file"), true)
		add(stringValue(publicationConfig, "git_push_credential_file"), true)
	}
	arguments := make([]string, 0, len(mounts)*2)
	for _, value := range mounts {
		specification := "type=bind,src=" + value.path + ",dst=" + value.path
		if value.readOnly {
			specification += ",readonly"
		}
		arguments = append(arguments, "--mount", specification)
	}
	return arguments
}

func provisionPackagedCanaryRoots(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		arguments := []string{"run", "--rm", "--mount", "type=bind,src=" + path + ",dst=/target",
			packagedHelperImage, "chown", "-R", "65532:" + strconv.Itoa(os.Getgid()), "/target"}
		if body, err := exec.CommandContext(t.Context(), "docker", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("provision packaged canary root: %v: %s", err, body)
		}
		arguments = []string{"run", "--rm", "--mount", "type=bind,src=" + path + ",dst=/target",
			packagedHelperImage, "chmod", "-R", "g+rwX", "/target"}
		if body, err := exec.CommandContext(t.Context(), "docker", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("share packaged canary root with test operator: %v: %s", err, body)
		}
	}
}

func restorePackagedCanaryRoots(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		arguments := []string{"run", "--rm", "--mount", "type=bind,src=" + path + ",dst=/target",
			packagedHelperImage, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/target"}
		if body, err := exec.CommandContext(context.Background(), "docker", arguments...).CombinedOutput(); err != nil {
			t.Errorf("restore packaged canary root: %v: %s", err, body)
		}
		arguments = []string{"run", "--rm", "--mount", "type=bind,src=" + path + ",dst=/target",
			packagedHelperImage, "chmod", "-R", "go-w", "/target"}
		if body, err := exec.CommandContext(context.Background(), "docker", arguments...).CombinedOutput(); err != nil {
			t.Errorf("restore packaged canary root permissions: %v: %s", err, body)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Errorf("restore packaged canary root mode: %v", err)
		}
	}
}

func makePackagedTreeAccessible(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o777)
		}
		return os.Chmod(path, 0o666)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func chownPackagedCredentials(t *testing.T, root string, paths ...string) {
	t.Helper()
	arguments := []string{"run", "--rm", "--mount", "type=bind,src=" + root + ",dst=/target",
		packagedHelperImage, "chown", "65532:65532", "/target"}
	for _, path := range paths {
		if path != "" {
			arguments = append(arguments, "/target/"+filepath.Base(path))
		}
	}
	body, err := exec.CommandContext(t.Context(), "docker", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("provision packaged credentials: %v: %s", err, body)
	}
}

func restorePackagedCredentialRoot(t *testing.T, root string) {
	t.Helper()
	arguments := []string{"run", "--rm", "--mount", "type=bind,src=" + root + ",dst=/target",
		packagedHelperImage, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/target"}
	body, err := exec.CommandContext(context.Background(), "docker", arguments...).CombinedOutput()
	if err != nil {
		t.Errorf("restore packaged credential ownership: %v: %s", err, body)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Errorf("restore packaged credential root mode: %v", err)
	}
}

func chownPackagedTree(t *testing.T, root string) {
	t.Helper()
	arguments := []string{"run", "--rm", "--mount", "type=bind,src=" + root + ",dst=/target",
		packagedHelperImage, "chown", "-R", "65532:65532", "/target"}
	body, err := exec.CommandContext(t.Context(), "docker", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("provision packaged tree: %v: %s", err, body)
	}
}

func restorePackagedTreeOwnership(t *testing.T, root string) {
	t.Helper()
	arguments := []string{"run", "--rm", "--mount", "type=bind,src=" + root + ",dst=/target",
		packagedHelperImage, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/target"}
	body, err := exec.CommandContext(context.Background(), "docker", arguments...).CombinedOutput()
	if err != nil {
		t.Errorf("restore packaged tree ownership: %v: %s", err, body)
	}
}

func makePackagedPathAccessible(t *testing.T, path string) {
	t.Helper()
	arguments := []string{"run", "--rm", "--mount", "type=bind,src=" + path + ",dst=/target",
		packagedHelperImage, "chmod", "-R", "a+rwX", "/target"}
	body, err := exec.CommandContext(t.Context(), "docker", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("make packaged path accessible: %v: %s", err, body)
	}
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

type mutationResponse struct {
	value  map[string]any
	body   []byte
	replay bool
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
	return c.mutateResponseWithKey(t, path, revision, idempotencyKey, body, status).value
}

func (c runtimeClient) mutateResponseWithKey(
	t *testing.T, path, revision, idempotencyKey string, body any, status int,
) mutationResponse {
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
	return mutationResponse{
		value: value, body: responseBody,
		replay: response.Header.Get("Idempotent-Replay") == "true",
	}
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

func fixturePythonRepository(
	t *testing.T, root, sourceRoot string,
) (string, string, string) {
	t.Helper()
	repository := filepath.Join(root, "repository")
	remote := filepath.Join(root, "remote.git")
	specification := filepath.Join(root, "project-spec.json")
	checks := filepath.Join(root, "checks")
	if err := os.Mkdir(checks, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, specification, map[string]any{
		"schema_version": "harness_python_project.v1",
		"name":           "live-demo", "package_name": "live_demo",
		"objective": "Implement main so it returns the exact string ready.",
		"acceptance_criteria": []map[string]string{{
			"id": "AC-001", "description": "main returns the exact string ready",
		}},
	})
	writeFile(t, filepath.Join(checks, "test_acceptance.py"),
		"from live_demo import main\n\n\ndef test_main_returns_ready() -> None:\n"+
			"    assert main() == \"ready\"\n")
	command := exec.Command("uv", "run", "--frozen", "harness-agents", "init", repository,
		"--project-spec", specification, "--checks", checks)
	command.Dir = sourceRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap generated Python fixture: %v: %s", err, output)
	}
	runGitE2E(t, root, "init", "--bare", "-q", remote)
	runGitE2E(t, repository, "remote", "add", "origin", remote)
	runGitE2E(t, repository, "push", "-q", "origin", "main:main")
	base := strings.TrimSpace(runGitOutput(t, repository, "rev-parse", "HEAD"))
	return repository, "origin", base
}

func smartGitRemote(t *testing.T, repository string) *httptest.Server {
	t.Helper()
	runGitE2E(t, repository, "config", "http.receivepack", "true")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		command := exec.CommandContext(request.Context(), "git", "http-backend")
		command.Env = append(os.Environ(),
			"GIT_PROJECT_ROOT="+filepath.Dir(repository), "GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory",
			"GIT_CONFIG_VALUE_0="+repository,
			"PATH_INFO="+request.URL.Path, "QUERY_STRING="+request.URL.RawQuery,
			"REQUEST_METHOD="+request.Method, "CONTENT_TYPE="+request.Header.Get("Content-Type"),
			fmt.Sprintf("CONTENT_LENGTH=%d", request.ContentLength), "REMOTE_USER=packaged-test")
		command.Stdin = request.Body
		var output, diagnostics bytes.Buffer
		command.Stdout, command.Stderr = &output, &diagnostics
		if err := command.Run(); err != nil {
			t.Logf("git http-backend path=%q failed: %v: %s", request.URL.Path, err, diagnostics.String())
			http.Error(w, diagnostics.String(), http.StatusInternalServerError)
			return
		}
		body := output.Bytes()
		separator, width := bytes.Index(body, []byte("\r\n\r\n")), 4
		if separator < 0 {
			separator, width = bytes.Index(body, []byte("\n\n")), 2
		}
		if separator < 0 {
			http.Error(w, "invalid Git CGI response", 500)
			return
		}
		status := http.StatusOK
		for _, line := range strings.Split(strings.ReplaceAll(string(body[:separator]), "\r\n", "\n"), "\n") {
			name, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			if strings.EqualFold(name, "Status") {
				fields := strings.Fields(value)
				if len(fields) > 0 {
					status, _ = strconv.Atoi(fields[0])
				}
				continue
			}
			w.Header().Add(name, value)
		}
		if status >= http.StatusBadRequest {
			t.Logf("git http-backend path=%q status=%d headers=%s diagnostics=%s body=%s",
				request.URL.Path, status, body[:separator], diagnostics.String(), body[separator+width:])
		}
		w.WriteHeader(status)
		_, _ = w.Write(body[separator+width:])
	}))
	t.Cleanup(server.Close)
	return server
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
		if os.Getenv("BETA_PYTHON_PROJECT") == "1" {
			prompt = "Return only a valid implementation_proposal.v1 JSON object for the " +
				"approved task. Set a non-empty summary and exactly one changes item: path " +
				"src/live_demo/__init__.py, operation update, expected_original_sha256 equal " +
				"to the supplied file digest. The decoded replacement_content value must be " +
				"exactly two lines: `def main() -> str:` and four spaces followed by " +
				"`return \"ready\"`, with a trailing newline. Encode those newlines and quotes " +
				"correctly so the entire response remains valid JSON. Set a non-empty rationale " +
				"and acceptance_criterion_ids " +
				"exactly [\"AC-001\"]. Set requested_declared_check_ids exactly " +
				"[\"make-check-v1\"]. Do not request a scope change and do not change tests, " +
				"lockfiles, configuration, metadata, or any other file."
		}
		if stageName == "review" {
			prompt = "Independently review the exact candidate diff and verification evidence. " +
				"Return advisory accept only when the candidate is in scope and all assigned " +
				"acceptance criteria are proven by the trusted check. Return exactly this JSON " +
				"when accepting without findings: {\"recommendation\":\"advisory_accept\"," +
				"\"findings\":[],\"required_actions\":[],\"unrequested_changes\":[]," +
				"\"residual_risks\":[],\"assumptions\":[]}"
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
	if os.Getenv("BETA_PYTHON_PROJECT") == "1" {
		request.ProblemStatement = "The trusted Python main function is not implemented"
		request.DesiredOutcome = "main returns the exact string ready"
		proposal.Title = "Implement live demo"
		proposal.Goal = "Return ready from main"
		proposal.AcceptanceCriteria[0].Description = "main returns the exact string ready"
	}
	return request, proposal
}

func planningPair(
	runID, taskID string, now time.Time,
	repository string, snapshot RepositorySnapshot, specificationDigest string,
) (*reasoningv1.TaskPlanningRequest, *reasoningv1.TaskGraphProposal) {
	envelope := reasoningEnvelope(runID, nil,
		reasoningv1.ReasoningStage_REASONING_STAGE_PLANNING, now.Add(time.Second))
	request := &reasoningv1.TaskPlanningRequest{
		Envelope: envelope, ApprovedSpecificationId: "SPEC-001",
		ApprovedSpecificationDigest: specificationDigest, RepositoryMap: snapshot.Entries,
		ReadablePaths:   []string{"add.go", "add_test.go"},
		WritablePaths:   []string{"add.go"},
		ProhibitedPaths: []string{"Makefile", "add_test.go", "go.mod"},
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
			ProhibitedPaths:        []string{"Makefile", "add_test.go", "go.mod"},
			RequiredCheckIds:       []string{"make-check-v1"},
			StopConditions:         []string{"make check passes"},
		}},
	}
	if os.Getenv("BETA_PYTHON_PROJECT") == "1" {
		request.ReadablePaths = []string{"Makefile", "pyproject.toml", "src", "tests", "uv.lock"}
		request.WritablePaths = []string{"src"}
		request.ProhibitedPaths = []string{".harness", "Makefile", "pyproject.toml", "tests", "uv.lock"}
		graph.Tasks[0].Objective = "Implement main so it returns ready"
		graph.Tasks[0].ReadablePaths = slices.Clone(request.ReadablePaths)
		graph.Tasks[0].WritablePaths = slices.Clone(request.WritablePaths)
		graph.Tasks[0].ProhibitedPaths = slices.Clone(request.ProhibitedPaths)
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

func dockerImageID(t *testing.T, image string) string {
	t.Helper()
	body := runCommandOutput(t, "docker", "image", "inspect", "--format", "{{.Id}}", image)
	return strings.TrimSpace(body)
}

func runCommandOutput(t *testing.T, name string, arguments ...string) string {
	t.Helper()
	body, err := exec.CommandContext(t.Context(), name, arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, arguments, err, body)
	}
	return string(body)
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
	command := exec.Command("git", append([]string{"-c", "safe.directory=" + root, "-C", root}, arguments...)...)
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

func assertPublishedCanary(
	t *testing.T, tokenFile, artifactRoot, repository, remote, base, candidate, branch string,
	value map[string]any,
) {
	t.Helper()
	remoteHead := strings.Fields(runGitOutput(
		t, repository, "ls-remote", "--heads", remote, "refs/heads/"+branch,
	))
	if len(remoteHead) != 2 || remoteHead[0] != candidate ||
		remoteHead[1] != "refs/heads/"+branch {
		t.Fatalf("exact canary branch = %v", remoteHead)
	}
	credential, err := publication.NewFileCredential(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	client, err := publication.NewGitHubRESTClient(
		"https://api.github.com", "2022-11-28", publication.DefaultMaxBodyBytes,
		publication.DefaultTimeout, credential,
	)
	if err != nil {
		t.Fatal(err)
	}
	number := int64(value["pull_request_number"].(float64))
	pull, err := client.InspectPullRequest(
		t.Context(), beta.CanaryOwner, beta.CanaryRepository, number,
	)
	if err != nil || pull.State != "open" || !pull.Draft || pull.Base != "main" ||
		pull.Head != branch || pull.BaseCommit != base || pull.HeadCommit != candidate ||
		pull.URL != value["pull_request_url"] ||
		!strings.Contains(pull.Body, "<!-- harness-publication-id:"+value["publication_id"].(string)+" -->") {
		t.Fatalf("real canary pull request = %#v error=%v", pull, err)
	}
	marker := "<!-- harness-publication-id:" + value["publication_id"].(string) + " -->"
	_, found, err := client.FindDraft(t.Context(), publication.DraftPullRequestInput{
		Owner: beta.CanaryOwner, Repo: beta.CanaryRepository, Head: branch,
		Base: "main", Marker: marker,
	})
	if err != nil || !found {
		t.Fatalf("unique canary draft lookup found=%v err=%v", found, err)
	}
	artifactValue, ok := value["publication_artifact"].(map[string]any)
	if !ok {
		t.Fatalf("publication artifact = %#v", value["publication_artifact"])
	}
	uri, uriOK := artifactValue["uri"].(string)
	digest, digestOK := artifactValue["digest"].(string)
	if !uriOK || !digestOK || uri != "artifact://sha256/"+digest {
		t.Fatalf("publication artifact reference = %#v", artifactValue)
	}
	assertArtifactIntegrity(t, artifactRoot, uri)
	encoded, err := os.ReadFile(filepath.Join(artifactRoot, digest[:2], digest))
	if err != nil {
		t.Fatal(err)
	}
	var artifactValueDecoded publication.DraftPullRequestArtifact
	if err := json.Unmarshal(encoded, &artifactValueDecoded); err != nil ||
		artifactValueDecoded.PublicationID != value["publication_id"] ||
		artifactValueDecoded.HeadBranch != branch || artifactValueDecoded.BaseCommit != base ||
		artifactValueDecoded.CandidateCommit != candidate ||
		artifactValueDecoded.PullRequestNumber != number || artifactValueDecoded.PullRequestURL != pull.URL {
		t.Fatalf("publication artifact = %#v error=%v", artifactValueDecoded, err)
	}
}

type sideEffects struct {
	reasoning, approvals, publications, pullRequests int
	remoteRefs                                       string
}

type runIntakeSideEffects struct {
	runs, commands, events, bindings, jobs, artifacts int
	providerLedger                                    bool
}

func snapshotRunIntakeSideEffects(
	t *testing.T, pool *pgxpool.Pool, artifactRoot, runID string,
) runIntakeSideEffects {
	t.Helper()
	var value runIntakeSideEffects
	queries := []struct {
		target *int
		query  string
	}{
		{&value.runs, `SELECT count(*) FROM workflow_runs WHERE run_id=$1`},
		{&value.commands, `SELECT count(*) FROM workflow_commands WHERE aggregate_id=$1`},
		{&value.events, `SELECT count(*) FROM workflow_events WHERE aggregate_id=$1`},
		{&value.bindings, `SELECT count(*) FROM runtime_run_bindings WHERE run_id=$1`},
		{&value.jobs, `SELECT count(*) FROM runtime_stage_jobs WHERE run_id=$1`},
	}
	for _, query := range queries {
		if err := pool.QueryRow(t.Context(), query.query, runID).Scan(query.target); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(t.Context(),
		`SELECT to_regclass('reasoning_invocations') IS NOT NULL`).Scan(&value.providerLedger); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(artifactRoot, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			value.artifacts++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func snapshotSideEffects(
	t *testing.T, pool *pgxpool.Pool, repository, remote, branchPrefix, runID, taskID string,
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
			WHERE published_branch=$1`, []any{branchPrefix + runID}},
	}
	for _, query := range queries {
		if err := pool.QueryRow(t.Context(), query.query, query.args...).Scan(query.target); err != nil {
			t.Fatal(err)
		}
	}
	if state != nil {
		state.mu.Lock()
		value.pullRequests = state.creates
		state.mu.Unlock()
	}
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
		command := exec.Command("git", "-c", "safe.directory="+repository,
			"-C", repository, "cat-file", "-p", objectID)
		body, err := command.Output()
		if err != nil {
			t.Fatalf("inspect reachable fixture Git object %s: %v", objectID, err)
		}
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("ANTHROPIC_API_KEY persisted in reachable fixture Git object %s", objectID)
		}
	}
}
