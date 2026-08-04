// Package release verifies a beta release manifest against immutable local,
// PostgreSQL, CAS, Git, Docker, and GitHub evidence.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/artifact"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/dockerengine"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/migration"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/registry"
	"github.com/Standard-Syntax/basic/go/internal/review"
	runtimeledger "github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/subprocess"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ReportVersion = "beta_readiness_report.v1"

const commandTimeout = 60 * time.Second

const (
	checkConfiguration   = "configuration"
	checkSourceCheckout  = "source_checkout"
	checkToolchains      = "toolchains"
	checkMigrations      = "migrations"
	checkFiles           = "files"
	checkImages          = "images"
	checkWorkflowRuntime = "workflow_runtime"
	checkReasoning       = "reasoning"
	checkVerification    = "verification"
	checkReview          = "review"
	checkApproval        = "approval"
	checkPublication     = "publication"
	checkGitHubDraft     = "github_draft"
	checkHumanDecision   = "human_decision"
	checkManifestCount   = "manifest_count_limit"
	checkManifestSize    = "manifest_size_limit"
	checkPromptCount     = "prompt_count_limit"
	checkPromptSize      = "prompt_size_limit"
	checkTimeout         = "timeout"
	checkUnknown         = "unknown"
)

type Report struct {
	SchemaVersion         string   `json:"schema_version"`
	Status                string   `json:"status"`
	ReleaseManifestDigest string   `json:"release_manifest_digest"`
	Checks                []string `json:"checks"`
	FailedCheck           string   `json:"failed_check,omitempty"`
}

type Verifier struct {
	command func(context.Context, string, ...string) (string, error)
}

type checkFailure struct {
	check string
	err   error
}

func (e *checkFailure) Error() string { return e.err.Error() }
func (e *checkFailure) Unwrap() error { return e.err }

func failed(check string, err error) error { return &checkFailure{check: check, err: err} }

func failedCheck(err error) string {
	var target *checkFailure
	if errors.As(err, &target) {
		return target.check
	}
	return checkUnknown
}

type invocationEvidence struct {
	stage, provider, model, requestID, providerRequestID string
	proposal, response                                   workflow.ArtifactRef
	requests                                             int
	input, output                                        int64
}

func NewVerifier() *Verifier { return &Verifier{command: commandOutput} }

func (v *Verifier) Verify(ctx context.Context, manifest *beta.ReleaseManifest) (Report, error) {
	report := Report{SchemaVersion: ReportVersion, Status: "not_ready"}
	digest, err := manifest.Digest()
	if err != nil {
		return classifyFailure(ctx, report, err)
	}
	report.ReleaseManifestDigest = digest
	canary, err := v.verifySupplyChain(ctx, manifest)
	if err != nil {
		return classifyFailure(ctx, report, err)
	}
	report.Checks = append(report.Checks, "supply_chain")
	if err := verifyDurableEvidence(ctx, manifest, &canary); err != nil {
		return classifyFailure(ctx, report, err)
	}
	report.Checks = append(report.Checks, "durable_evidence", "github_draft")
	if manifest.Decision.Status != "go" {
		return classifyFailure(ctx, report,
			failed(checkHumanDecision, errors.New("human release decision is no-go")))
	}
	report.Checks = append(report.Checks, "human_go_decision")
	report.Status = "ready"
	return report, nil
}

func classifyFailure(ctx context.Context, report Report, err error) (Report, error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		report.Status = "inconclusive"
		report.FailedCheck = checkTimeout
		return report, err
	}
	report.FailedCheck = failedCheck(err)
	return report, err
}

func (v *Verifier) verifySupplyChain(
	ctx context.Context, manifest *beta.ReleaseManifest,
) (beta.Config, error) {
	deployment, record, canary, err := loadReleaseInputs(manifest)
	if err != nil {
		return beta.Config{}, failed(checkConfiguration, err)
	}
	if err := verifyConfigurationBindings(&deployment, &record, &canary); err != nil {
		return beta.Config{}, failed(checkConfiguration, err)
	}
	if err := v.verifySourceAndToolchains(ctx, manifest); err != nil {
		return beta.Config{}, err
	}
	if err := verifyMigrations(ctx, canary.DatabaseURL, record.MigrationDigest); err != nil {
		return beta.Config{}, failed(checkMigrations, err)
	}
	if err := verifyFiles(&deployment, &record); err != nil {
		return beta.Config{}, err
	}
	if err := verifyImages(ctx, &record); err != nil {
		return beta.Config{}, failed(checkImages, err)
	}
	return canary, nil
}

func loadReleaseInputs(manifest *beta.ReleaseManifest) (beta.Deployment, beta.DeploymentRecord, beta.Config, error) {
	deployment, err := beta.LoadDeployment(manifest.DeploymentConfigPath)
	if err != nil {
		return beta.Deployment{}, beta.DeploymentRecord{}, beta.Config{}, fmt.Errorf("load deployment: %w", err)
	}
	record, err := beta.LoadDeploymentRecord(manifest.DeploymentRecordPath)
	if err != nil || !reflect.DeepEqual(record, manifest.Deployment) {
		return beta.Deployment{}, beta.DeploymentRecord{}, beta.Config{}, errors.New("deployment record mismatch")
	}
	canary, err := beta.LoadConfig(manifest.CanaryConfigPath)
	if err != nil || canary.ValidateCanary() != nil {
		return beta.Deployment{}, beta.DeploymentRecord{}, beta.Config{}, errors.New("invalid canary configuration")
	}
	return deployment, record, canary, nil
}

func verifyConfigurationBindings(deployment *beta.Deployment, record *beta.DeploymentRecord, canary *beta.Config) error {
	if deployment.SourceCommit != record.SourceCommit || !reflect.DeepEqual(deployment.Policy, canary.Policy) ||
		deployment.Mounts.CAS != canary.ArtifactRoot || deployment.Mounts.Worktrees != canary.WorktreeRoot ||
		deployment.Mounts.Verification != canary.VerificationWorkspaceRoot ||
		deployment.Credentials.GitHub != canary.PublicationCredentialFile ||
		deployment.Credentials.GitPush != canary.GitPushCredentialFile {
		return errors.New("release configuration bindings mismatch")
	}
	configDigest, err := deployment.Digest()
	if err != nil || configDigest != record.ConfigurationDigest {
		return errors.New("deployment configuration digest mismatch")
	}
	return nil
}

func verifyImages(ctx context.Context, record *beta.DeploymentRecord) error {
	engine, err := dockerengine.NewFromEnvironment()
	if err != nil {
		return err
	}
	defer engine.Close()
	for _, image := range []string{record.Images.API, record.Images.Workflow, record.Images.Execution, record.Images.Verification} {
		actual, imageErr := engine.ImageID(ctx, image)
		if imageErr != nil || actual != image {
			return errors.New("release image identity mismatch")
		}
	}
	return nil
}

func (v *Verifier) verifySourceAndToolchains(ctx context.Context, manifest *beta.ReleaseManifest) error {
	head, err := v.runCommand(ctx, "git", "-C", manifest.SourceRepositoryRoot, "rev-parse", "HEAD")
	if err != nil {
		return failed(checkSourceCheckout, fmt.Errorf("read release source commit: %w", err))
	}
	if head != manifest.Deployment.SourceCommit {
		return failed(checkSourceCheckout, errors.New("release source commit mismatch"))
	}
	status, err := v.runCommand(ctx, "git", "-C", manifest.SourceRepositoryRoot, "status", "--porcelain")
	if err != nil {
		return failed(checkSourceCheckout, fmt.Errorf("read release source status: %w", err))
	}
	if status != "" {
		return failed(checkSourceCheckout, errors.New("release source checkout is not clean"))
	}
	gitVersion, err := v.runCommand(ctx, "git", "--version")
	if err != nil {
		return failed(checkToolchains, fmt.Errorf("read release Git version: %w", err))
	}
	if gitVersion != manifest.Toolchains.Git || gitVersion != manifest.Deployment.GitVersion ||
		runtime.Version() != manifest.Toolchains.Go || runtime.Version() != manifest.Deployment.GoVersion ||
		runtime.Version() != manifest.Deployment.ToolchainVersion {
		return failed(checkToolchains, errors.New("release Git or Go toolchain mismatch"))
	}
	uvVersion, uvErr := v.runCommand(ctx, "uv", "--version")
	if uvErr != nil {
		return failed(checkToolchains, fmt.Errorf("read release uv version: %w", uvErr))
	}
	dockerVersion, dockerErr := v.runCommand(ctx, "docker", "--version")
	if dockerErr != nil {
		return failed(checkToolchains, fmt.Errorf("read release Docker version: %w", dockerErr))
	}
	if uvVersion != manifest.Toolchains.UV || dockerVersion != manifest.Toolchains.Docker {
		return failed(checkToolchains, errors.New("release uv or Docker toolchain mismatch"))
	}
	return nil
}

func (v *Verifier) runCommand(ctx context.Context, name string, arguments ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	output, err := v.command(commandCtx, name, arguments...)
	if commandCtx.Err() != nil {
		return output, commandCtx.Err()
	}
	return output, err
}

func verifyMigrations(ctx context.Context, databaseURL, expectedDigest string) error {
	values, err := migration.Verify(ctx, databaseURL, workflow.MigrationSource(), registry.MigrationSource(),
		gateway.MigrationSource(), execution.MigrationSource(), verification.MigrationSource(),
		approval.MigrationSource(), publication.MigrationSource())
	if err != nil {
		return fmt.Errorf("verify release migrations: %w", err)
	}
	body, err := json.Marshal(values)
	if err != nil || digest(body) != expectedDigest {
		return errors.New("release migration digest mismatch")
	}
	return nil
}

func verifyFiles(deployment *beta.Deployment, record *beta.DeploymentRecord) error {
	manifests, err := beta.DirectoryDigests(deployment.Mounts.Manifests)
	if err != nil {
		return classifyDirectoryFailure(err, checkManifestCount, checkManifestSize)
	}
	if !reflect.DeepEqual(manifests, record.ManifestDigests) {
		return failed(checkFiles, errors.New("release manifest file digests mismatch"))
	}
	prompts, err := beta.DirectoryDigests(deployment.Mounts.Prompts)
	if err != nil {
		return classifyDirectoryFailure(err, checkPromptCount, checkPromptSize)
	}
	if !reflect.DeepEqual(prompts, record.PromptDigests) {
		return failed(checkFiles, errors.New("release prompt file digests mismatch"))
	}
	return nil
}

func classifyDirectoryFailure(err error, countCheck, sizeCheck string) error {
	var countLimit *beta.EvidenceCountLimitError
	if errors.As(err, &countLimit) {
		return failed(countCheck, err)
	}
	var sizeLimit *beta.EvidenceSizeLimitError
	if errors.As(err, &sizeLimit) {
		return failed(sizeCheck, err)
	}
	return failed(checkFiles, err)
}

func verifyDurableEvidence(ctx context.Context, manifest *beta.ReleaseManifest, canary *beta.Config) error {
	config, err := pgxpool.ParseConfig(canary.DatabaseURL)
	if err != nil {
		return failed(checkWorkflowRuntime, errors.New("invalid evidence database configuration"))
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return failed(checkWorkflowRuntime, errors.New("open evidence database"))
	}
	defer pool.Close()
	store, err := artifact.OpenStore(canary.ArtifactRoot, 4<<20)
	if err != nil {
		return failed(checkWorkflowRuntime, errors.New("open evidence artifact store"))
	}
	defer store.Close()
	if err := verifyWorkflowBindings(ctx, pool, store, manifest, canary); err != nil {
		return err
	}
	return verifyPublicationEvidence(ctx, pool, store, manifest, canary)
}

func verifyPublicationEvidence(
	ctx context.Context, pool *pgxpool.Pool, store *artifact.Store,
	manifest *beta.ReleaseManifest, canary *beta.Config,
) error {
	repository, err := publication.NewPostgresPublicationRepository(pool)
	if err != nil {
		return failed(checkPublication, err)
	}
	completed, err := repository.LoadCompleted(ctx, manifest.Canary.PublicationID)
	if err != nil || !validCompletedPublication(&completed, manifest) {
		return failed(checkPublication, errors.New("completed publication binding mismatch"))
	}
	publicationBody, err := store.Get(ctx, manifest.Canary.Publication)
	if err != nil {
		return failed(checkPublication, errors.New("read publication artifact"))
	}
	var publicationArtifact publication.DraftPullRequestArtifact
	if strictJSON(publicationBody, &publicationArtifact) != nil || !validPublicationArtifact(&publicationArtifact, manifest) {
		return failed(checkPublication, errors.New("publication artifact binding mismatch"))
	}
	if err := verifyGitHubDraft(ctx, manifest, canary, &publicationArtifact); err != nil {
		return failed(checkGitHubDraft, err)
	}
	return nil
}

func validCompletedPublication(completed *publication.CompletedPublication, manifest *beta.ReleaseManifest) bool {
	return completed.CandidateCommit == manifest.Canary.CandidateCommit &&
		completed.PullRequestURL == manifest.Canary.PullRequestURL &&
		completed.PublicationArtifact.Equal(manifest.Canary.Publication)
}

func validPublicationArtifact(value *publication.DraftPullRequestArtifact, manifest *beta.ReleaseManifest) bool {
	return value.SchemaVersion == "1" && value.PublicationID == manifest.Canary.PublicationID &&
		value.CandidateCommit == manifest.Canary.CandidateCommit &&
		value.PullRequestURL == manifest.Canary.PullRequestURL && value.Draft &&
		value.Verification.Equal(manifest.Canary.Verification) && value.Review.Equal(manifest.Canary.Review) &&
		value.Approval.Equal(manifest.Canary.Approval)
}

func verifyGitHubDraft(
	ctx context.Context, manifest *beta.ReleaseManifest, canary *beta.Config,
	publicationArtifact *publication.DraftPullRequestArtifact,
) error {
	credential, err := publication.NewFileCredential(canary.PublicationCredentialFile)
	if err != nil {
		return errors.New("open publication credential")
	}
	client, err := publication.NewGitHubRESTClient("https://api.github.com", "2022-11-28",
		publication.DefaultMaxBodyBytes, publication.DefaultTimeout, credential)
	if err != nil {
		return err
	}
	pull, err := client.InspectPullRequest(ctx, beta.CanaryOwner, beta.CanaryRepository, publicationArtifact.PullRequestNumber)
	if err != nil || pull.State != "open" || !pull.Draft || pull.URL != manifest.Canary.PullRequestURL ||
		pull.Head != publicationArtifact.HeadBranch || pull.Base != publicationArtifact.BaseBranch ||
		pull.HeadCommit != manifest.Canary.CandidateCommit || pull.BaseCommit != canary.Policy.Repository.BaseCommit ||
		!strings.Contains(pull.Body, "<!-- harness-publication-id:"+manifest.Canary.PublicationID+" -->") {
		return errors.New("live GitHub draft binding mismatch")
	}
	return nil
}

func verifyWorkflowBindings(
	ctx context.Context, pool *pgxpool.Pool, store *artifact.Store,
	manifest *beta.ReleaseManifest, canary *beta.Config,
) error {
	workflowStore := workflow.NewStore(pool)
	run, err := workflowStore.GetRun(ctx, manifest.Canary.RunID)
	if err != nil || !validRunEvidence(&run, manifest) {
		return failed(checkWorkflowRuntime, errors.New("run evidence binding mismatch"))
	}
	tasks, err := workflowStore.ListTasks(ctx, manifest.Canary.RunID)
	if err != nil || len(tasks) != 1 || tasks[0].ID != manifest.Canary.TaskID {
		return failed(checkWorkflowRuntime, errors.New("release canary must contain exactly one bound task"))
	}
	task := &tasks[0]
	if !validTaskEvidence(task, manifest) {
		return failed(checkWorkflowRuntime, errors.New("task evidence binding mismatch"))
	}
	binding, err := runtimeledger.NewBindingRepository(pool).GetRun(ctx, manifest.Canary.RunID)
	if err != nil || !validRuntimeBinding(&binding, canary) {
		return failed(checkWorkflowRuntime, errors.New("immutable runtime binding mismatch"))
	}
	if err := verifyRuntimeArtifacts(ctx, store, &binding); err != nil {
		return failed(checkWorkflowRuntime, err)
	}
	return verifyStageEvidence(ctx, pool, store, manifest, task)
}

func validRunEvidence(run *workflow.Run, manifest *beta.ReleaseManifest) bool {
	return run.State == workflow.RunStateMergeReady && run.CandidateCommit == manifest.Canary.CandidateCommit &&
		run.Verification != nil && run.Verification.Equal(manifest.Canary.Verification) &&
		run.Review != nil && run.Review.Equal(manifest.Canary.Review) &&
		run.Approval != nil && run.Approval.Equal(manifest.Canary.Approval) &&
		run.Publication != nil && run.Publication.Equal(manifest.Canary.Publication)
}

func validTaskEvidence(task *workflow.Task, manifest *beta.ReleaseManifest) bool {
	return task.State == workflow.TaskStateAccepted && task.CandidateCommit == manifest.Canary.CandidateCommit &&
		task.Verification != nil && task.Verification.Equal(manifest.Canary.Verification) &&
		task.Review != nil && task.Review.Equal(manifest.Canary.Review) &&
		task.Approval != nil && task.Approval.Equal(manifest.Canary.Approval)
}

func validRuntimeBinding(binding *runtimeledger.RunBinding, canary *beta.Config) bool {
	return binding.BaseCommit == canary.Policy.Repository.BaseCommit && binding.RepositoryMap != nil && binding.Policy != nil &&
		binding.ExecutionImageDigest == canary.Policy.Images.Execution &&
		binding.VerificationImageDigest == canary.Policy.Images.Verification
}

func verifyRuntimeArtifacts(ctx context.Context, store *artifact.Store, binding *runtimeledger.RunBinding) error {
	for _, ref := range []workflow.ArtifactRef{binding.Intake, *binding.RepositoryMap, *binding.Policy} {
		if _, err := store.Get(ctx, ref); err != nil {
			return errors.New("runtime binding artifact unavailable")
		}
	}
	return nil
}

func verifyStageEvidence(
	ctx context.Context, pool *pgxpool.Pool, store *artifact.Store,
	manifest *beta.ReleaseManifest, task *workflow.Task,
) error {
	invocations, err := loadReasoningEvidence(ctx, pool, manifest)
	if err != nil {
		return failed(checkReasoning, err)
	}
	var specification, taskGraph workflow.ArtifactRef
	if err := pool.QueryRow(ctx, `SELECT specification_uri,specification_digest,
		task_graph_uri,task_graph_digest FROM workflow_runs WHERE run_id=$1`,
		manifest.Canary.RunID).Scan(&specification.URI, &specification.Digest,
		&taskGraph.URI, &taskGraph.Digest); err != nil || len(invocations) != 4 ||
		!invocations[0].proposal.Equal(specification) || !invocations[1].proposal.Equal(taskGraph) {
		return failed(checkReasoning, errors.New("planning proposal binding mismatch"))
	}
	reviewInvocation, err := verifyReasoningEvidence(ctx, store, invocations, task)
	if err != nil {
		return failed(checkReasoning, err)
	}
	if err := verifyVerificationEvidence(ctx, pool, store, manifest); err != nil {
		return failed(checkVerification, err)
	}
	if err := verifyReviewEvidence(ctx, store, manifest, reviewInvocation); err != nil {
		return failed(checkReview, err)
	}
	if err := verifyApprovalEvidence(ctx, pool, store, manifest); err != nil {
		return failed(checkApproval, err)
	}
	return nil
}

func loadReasoningEvidence(
	ctx context.Context, pool *pgxpool.Pool, manifest *beta.ReleaseManifest,
) ([]invocationEvidence, error) {
	rows, err := pool.Query(ctx, `SELECT stage,provider,model,request_id,provider_request_id,
		proposal_artifact_uri,proposal_digest,provider_response_artifact_uri,
		provider_response_digest,provider_requests,input_tokens,output_tokens
		FROM reasoning_invocations WHERE run_id=$1 AND (task_id=$2 OR task_id IS NULL) AND state='completed'
		AND final_status='accepted' ORDER BY CASE stage WHEN 'specification' THEN 1
		WHEN 'planning' THEN 2 WHEN 'implementation' THEN 3 WHEN 'review' THEN 4 ELSE 5 END`,
		manifest.Canary.RunID, manifest.Canary.TaskID)
	if err != nil {
		return nil, errors.New("read reasoning evidence")
	}
	defer rows.Close()
	var invocations []invocationEvidence
	for rows.Next() {
		var value invocationEvidence
		if err := rows.Scan(&value.stage, &value.provider, &value.model, &value.requestID,
			&value.providerRequestID, &value.proposal.URI, &value.proposal.Digest,
			&value.response.URI, &value.response.Digest, &value.requests, &value.input, &value.output); err != nil {
			return nil, errors.New("decode reasoning evidence")
		}
		invocations = append(invocations, value)
	}
	if rows.Err() != nil {
		return nil, errors.New("read reasoning evidence")
	}
	return invocations, nil
}

func verifyReasoningEvidence(
	ctx context.Context, store *artifact.Store, invocations []invocationEvidence, task *workflow.Task,
) (*invocationEvidence, error) {
	if !validInvocationSet(invocations, task) {
		return nil, errors.New("exactly four distinct reasoning stages are required")
	}
	for index := range invocations {
		if !validLiveInvocation(&invocations[index]) {
			return nil, errors.New("invalid live reasoning evidence")
		}
		if !reasoningArtifactsAvailable(ctx, store, &invocations[index]) {
			return nil, errors.New("reasoning artifact unavailable")
		}
	}
	return &invocations[3], nil
}

func validInvocationSet(invocations []invocationEvidence, task *workflow.Task) bool {
	if len(invocations) != 4 || task.Proposal == nil ||
		!task.Proposal.Equal(invocations[2].proposal) {
		return false
	}
	want := []string{"specification", "planning", "implementation", "review"}
	requestIDs, providerIDs := map[string]struct{}{}, map[string]struct{}{}
	for index := range invocations {
		if invocations[index].stage != want[index] {
			return false
		}
		if _, exists := requestIDs[invocations[index].requestID]; exists {
			return false
		}
		if _, exists := providerIDs[invocations[index].providerRequestID]; exists {
			return false
		}
		requestIDs[invocations[index].requestID] = struct{}{}
		providerIDs[invocations[index].providerRequestID] = struct{}{}
	}
	return true
}

func validLiveInvocation(value *invocationEvidence) bool {
	return value.provider == gateway.MiniMaxAnthropicProvider && value.model == gateway.MiniMaxModel &&
		value.requests == 1 && value.providerRequestID != "" && value.input > 0 && value.output > 0
}

func reasoningArtifactsAvailable(ctx context.Context, store *artifact.Store, value *invocationEvidence) bool {
	for _, ref := range []workflow.ArtifactRef{value.proposal, value.response} {
		if _, err := store.Get(ctx, ref); err != nil {
			return false
		}
	}
	return true
}

func verifyVerificationEvidence(
	ctx context.Context, pool *pgxpool.Pool, store *artifact.Store, manifest *beta.ReleaseManifest,
) error {
	verificationBody, err := store.Get(ctx, manifest.Canary.Verification)
	if err != nil {
		return errors.New("verification artifact unavailable")
	}
	var verificationReport verification.VerificationReport
	if strictJSON(verificationBody, &verificationReport) != nil || !verificationReport.Passed ||
		verificationReport.RunID != manifest.Canary.RunID || verificationReport.TaskID != manifest.Canary.TaskID ||
		verificationReport.CandidateCommit != manifest.Canary.CandidateCommit {
		return errors.New("verification artifact binding mismatch")
	}
	var state string
	var resultBody []byte
	if err := pool.QueryRow(ctx, `SELECT state,result_json FROM verification_ledger
		WHERE verification_id=$1 AND state='completed'`, verificationReport.VerificationID).Scan(&state, &resultBody); err != nil {
		return errors.New("verification ledger binding mismatch")
	}
	var result verification.Result
	if strictJSON(resultBody, &result) != nil ||
		!validVerificationResult(state, &result, &verificationReport, manifest) {
		return errors.New("verification ledger binding mismatch")
	}
	var candidateCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM verification_ledger
		WHERE state='completed' AND result_json->>'CandidateCommit'=$1`,
		manifest.Canary.CandidateCommit).Scan(&candidateCount); err != nil || candidateCount != 1 {
		return errors.New("verification ledger binding mismatch")
	}
	return nil
}

func validVerificationResult(
	state string, result *verification.Result, report *verification.VerificationReport,
	manifest *beta.ReleaseManifest,
) bool {
	return state == "completed" && result.VerificationID == report.VerificationID &&
		result.CandidateCommit == manifest.Canary.CandidateCommit &&
		result.ReportArtifact.Equal(manifest.Canary.Verification) && result.Passed
}

func verifyReviewEvidence(
	ctx context.Context, store *artifact.Store, manifest *beta.ReleaseManifest, reviewInvocation *invocationEvidence,
) error {
	reviewBody, err := store.Get(ctx, manifest.Canary.Review)
	if err != nil {
		return errors.New("review artifact unavailable")
	}
	var reviewReport review.ReviewReport
	if strictJSON(reviewBody, &reviewReport) != nil ||
		!validReviewReport(&reviewReport, manifest, reviewInvocation) {
		return errors.New("review artifact binding mismatch")
	}
	return nil
}

func validReviewReport(
	report *review.ReviewReport, manifest *beta.ReleaseManifest, invocation *invocationEvidence,
) bool {
	return report.Passed && report.RunID == manifest.Canary.RunID &&
		report.TaskID == manifest.Canary.TaskID &&
		report.CandidateCommit == manifest.Canary.CandidateCommit &&
		report.Proposal.Equal(invocation.proposal) &&
		report.Verification.Equal(manifest.Canary.Verification)
}

func verifyApprovalEvidence(
	ctx context.Context, pool *pgxpool.Pool, store *artifact.Store, manifest *beta.ReleaseManifest,
) error {
	approvalBody, err := store.Get(ctx, manifest.Canary.Approval)
	if err != nil {
		return errors.New("approval artifact unavailable")
	}
	var approvalArtifact approval.TaskApproval
	if strictJSON(approvalBody, &approvalArtifact) != nil ||
		!validApprovalArtifact(&approvalArtifact, manifest) {
		return errors.New("approval artifact binding mismatch")
	}
	var approvalCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_approvals WHERE run_id=$1 AND task_id=$2
		AND candidate_commit=$3 AND approval_artifact_digest=$4 AND decision='approve' AND state='completed'`,
		manifest.Canary.RunID, manifest.Canary.TaskID, manifest.Canary.CandidateCommit,
		manifest.Canary.Approval.Digest).Scan(&approvalCount); err != nil || approvalCount != 1 {
		return errors.New("approval ledger binding mismatch")
	}
	return nil
}

func validApprovalArtifact(value *approval.TaskApproval, manifest *beta.ReleaseManifest) bool {
	return value.Decision == "approve" && value.RunID == manifest.Canary.RunID &&
		value.TaskID == manifest.Canary.TaskID &&
		value.CandidateCommit == manifest.Canary.CandidateCommit &&
		value.Verification.Equal(manifest.Canary.Verification) &&
		value.Review.Equal(manifest.Canary.Review)
}

func strictJSON(body []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected JSON trailer")
	}
	return nil
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func commandOutput(ctx context.Context, name string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	if name == "git" {
		command.Env = subprocess.Git()
	} else {
		command.Env = subprocess.Docker("", "")
	}
	body, err := command.Output()
	return strings.TrimSpace(string(body)), err
}
