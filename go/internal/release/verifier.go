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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"

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
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ReportVersion = "beta_readiness_report.v1"

type Report struct {
	SchemaVersion         string   `json:"schema_version"`
	Status                string   `json:"status"`
	ReleaseManifestDigest string   `json:"release_manifest_digest"`
	Checks                []string `json:"checks"`
}

type Verifier struct {
	command func(string, ...string) (string, error)
}

type invocationEvidence struct {
	stage, provider, model, requestID, providerRequestID string
	proposal, response                                   workflow.ArtifactRef
	requests                                             int
	input, output                                        int64
}

func NewVerifier() *Verifier { return &Verifier{command: commandOutput} }

func (v *Verifier) Verify(ctx context.Context, manifest *beta.ReleaseManifest) (Report, error) {
	digest, err := manifest.Digest()
	if err != nil {
		return Report{}, err
	}
	report := Report{SchemaVersion: ReportVersion, Status: "not_ready", ReleaseManifestDigest: digest}
	_, canary, err := v.verifySupplyChain(ctx, manifest)
	if err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "supply_chain")
	if err := verifyDurableEvidence(ctx, manifest, &canary); err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, "durable_evidence", "github_draft")
	if manifest.Decision.Status != "go" {
		return report, errors.New("human release decision is no-go")
	}
	report.Checks = append(report.Checks, "human_go_decision")
	report.Status = "ready"
	return report, nil
}

func (v *Verifier) verifySupplyChain(
	ctx context.Context, manifest *beta.ReleaseManifest,
) (beta.Deployment, beta.Config, error) {
	deployment, record, canary, err := loadReleaseInputs(manifest)
	if err != nil {
		return beta.Deployment{}, beta.Config{}, err
	}
	if err := verifyConfigurationBindings(&deployment, &record, &canary); err != nil {
		return beta.Deployment{}, beta.Config{}, err
	}
	if err := v.verifySourceAndToolchains(manifest); err != nil {
		return beta.Deployment{}, beta.Config{}, err
	}
	if err := verifyMigrations(ctx, canary.DatabaseURL, record.MigrationDigest); err != nil {
		return beta.Deployment{}, beta.Config{}, err
	}
	if err := verifyFiles(&deployment, &record); err != nil {
		return beta.Deployment{}, beta.Config{}, err
	}
	if err := verifyImages(ctx, &record); err != nil {
		return beta.Deployment{}, beta.Config{}, err
	}
	return deployment, canary, nil
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

func (v *Verifier) verifySourceAndToolchains(manifest *beta.ReleaseManifest) error {
	head, err := v.command("git", "-C", manifest.SourceRepositoryRoot, "rev-parse", "HEAD")
	if err != nil || head != manifest.Deployment.SourceCommit {
		return errors.New("release source commit mismatch")
	}
	status, err := v.command("git", "-C", manifest.SourceRepositoryRoot, "status", "--porcelain")
	if err != nil || status != "" {
		return errors.New("release source checkout is not clean")
	}
	gitVersion, err := v.command("git", "--version")
	if err != nil || gitVersion != manifest.Toolchains.Git || gitVersion != manifest.Deployment.GitVersion ||
		runtime.Version() != manifest.Toolchains.Go || runtime.Version() != manifest.Deployment.GoVersion ||
		runtime.Version() != manifest.Deployment.ToolchainVersion {
		return errors.New("release Git or Go toolchain mismatch")
	}
	uvVersion, uvErr := v.command("uv", "--version")
	dockerVersion, dockerErr := v.command("docker", "--version")
	if uvErr != nil || dockerErr != nil || uvVersion != manifest.Toolchains.UV || dockerVersion != manifest.Toolchains.Docker {
		return errors.New("release uv or Docker toolchain mismatch")
	}
	return nil
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
	manifests, err := directoryDigests(deployment.Mounts.Manifests)
	if err != nil || !reflect.DeepEqual(manifests, record.ManifestDigests) {
		return errors.New("release manifest file digests mismatch")
	}
	prompts, err := directoryDigests(deployment.Mounts.Prompts)
	if err != nil || !reflect.DeepEqual(prompts, record.PromptDigests) {
		return errors.New("release prompt file digests mismatch")
	}
	return nil
}

func verifyDurableEvidence(ctx context.Context, manifest *beta.ReleaseManifest, canary *beta.Config) error {
	config, err := pgxpool.ParseConfig(canary.DatabaseURL)
	if err != nil {
		return errors.New("invalid evidence database configuration")
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return errors.New("open evidence database")
	}
	defer pool.Close()
	store, err := artifact.OpenStore(canary.ArtifactRoot, 4<<20)
	if err != nil {
		return errors.New("open evidence artifact store")
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
		return err
	}
	completed, err := repository.LoadCompleted(ctx, manifest.Canary.PublicationID)
	if err != nil || completed.CandidateCommit != manifest.Canary.CandidateCommit ||
		completed.PullRequestURL != manifest.Canary.PullRequestURL ||
		!completed.PublicationArtifact.Equal(manifest.Canary.Publication) {
		return errors.New("completed publication binding mismatch")
	}
	publicationBody, err := store.Get(ctx, manifest.Canary.Publication)
	if err != nil {
		return errors.New("read publication artifact")
	}
	var publicationArtifact publication.DraftPullRequestArtifact
	if strictJSON(publicationBody, &publicationArtifact) != nil || publicationArtifact.SchemaVersion != "1" ||
		publicationArtifact.PublicationID != manifest.Canary.PublicationID ||
		publicationArtifact.CandidateCommit != manifest.Canary.CandidateCommit ||
		publicationArtifact.PullRequestURL != manifest.Canary.PullRequestURL || !publicationArtifact.Draft ||
		!publicationArtifact.Verification.Equal(manifest.Canary.Verification) ||
		!publicationArtifact.Review.Equal(manifest.Canary.Review) ||
		!publicationArtifact.Approval.Equal(manifest.Canary.Approval) {
		return errors.New("publication artifact binding mismatch")
	}
	return verifyGitHubDraft(ctx, manifest, canary, &publicationArtifact)
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
		return errors.New("run evidence binding mismatch")
	}
	tasks, err := workflowStore.ListTasks(ctx, manifest.Canary.RunID)
	if err != nil || len(tasks) != 1 || tasks[0].ID != manifest.Canary.TaskID {
		return errors.New("release canary must contain exactly one bound task")
	}
	task := &tasks[0]
	if !validTaskEvidence(task, manifest) {
		return errors.New("task evidence binding mismatch")
	}
	binding, err := runtimeledger.NewBindingRepository(pool).GetRun(ctx, manifest.Canary.RunID)
	if err != nil || !validRuntimeBinding(&binding, canary) {
		return errors.New("immutable runtime binding mismatch")
	}
	if err := verifyRuntimeArtifacts(ctx, store, &binding); err != nil {
		return err
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
		return err
	}
	if err := verifyReasoningEvidence(ctx, store, invocations, task); err != nil {
		return err
	}
	if err := verifyVerificationEvidence(ctx, pool, store, manifest); err != nil {
		return err
	}
	if err := verifyReviewEvidence(ctx, store, manifest, &invocations[1]); err != nil {
		return err
	}
	return verifyApprovalEvidence(ctx, pool, store, manifest)
}

func loadReasoningEvidence(
	ctx context.Context, pool *pgxpool.Pool, manifest *beta.ReleaseManifest,
) ([]invocationEvidence, error) {
	rows, err := pool.Query(ctx, `SELECT stage,provider,model,request_id,provider_request_id,
		proposal_artifact_uri,proposal_digest,provider_response_artifact_uri,
		provider_response_digest,provider_requests,input_tokens,output_tokens
		FROM reasoning_invocations WHERE run_id=$1 AND task_id=$2 AND state='completed'
		AND final_status='accepted' ORDER BY stage`, manifest.Canary.RunID, manifest.Canary.TaskID)
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
) error {
	if len(invocations) != 2 || invocations[0].stage != "implementation" ||
		invocations[1].stage != "review" || invocations[0].requestID == invocations[1].requestID ||
		task.Proposal == nil || !task.Proposal.Equal(invocations[0].proposal) {
		return errors.New("exactly two distinct reasoning stages are required")
	}
	for _, value := range invocations {
		if value.provider != gateway.MiniMaxAnthropicProvider || value.model != gateway.MiniMaxModel ||
			value.requests != 1 || value.providerRequestID == "" || value.input <= 0 || value.output <= 0 {
			return errors.New("invalid live reasoning evidence")
		}
		for _, ref := range []workflow.ArtifactRef{value.proposal, value.response} {
			if _, err := store.Get(ctx, ref); err != nil {
				return errors.New("reasoning artifact unavailable")
			}
		}
	}
	return nil
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
	var verificationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM verification_ledger WHERE state='completed'
		AND result_json->>'CandidateCommit'=$1`, manifest.Canary.CandidateCommit).Scan(&verificationCount); err != nil || verificationCount != 1 {
		return errors.New("verification ledger binding mismatch")
	}
	return nil
}

func verifyReviewEvidence(
	ctx context.Context, store *artifact.Store, manifest *beta.ReleaseManifest, reviewInvocation *invocationEvidence,
) error {
	reviewBody, err := store.Get(ctx, manifest.Canary.Review)
	if err != nil {
		return errors.New("review artifact unavailable")
	}
	var reviewReport review.ReviewReport
	if strictJSON(reviewBody, &reviewReport) != nil || !reviewReport.Passed ||
		reviewReport.RunID != manifest.Canary.RunID || reviewReport.TaskID != manifest.Canary.TaskID ||
		reviewReport.CandidateCommit != manifest.Canary.CandidateCommit ||
		!reviewReport.Proposal.Equal(reviewInvocation.proposal) ||
		!reviewReport.Verification.Equal(manifest.Canary.Verification) {
		return errors.New("review artifact binding mismatch")
	}
	return nil
}

func verifyApprovalEvidence(
	ctx context.Context, pool *pgxpool.Pool, store *artifact.Store, manifest *beta.ReleaseManifest,
) error {
	approvalBody, err := store.Get(ctx, manifest.Canary.Approval)
	if err != nil {
		return errors.New("approval artifact unavailable")
	}
	var approvalArtifact approval.TaskApproval
	if strictJSON(approvalBody, &approvalArtifact) != nil || approvalArtifact.Decision != "approve" ||
		approvalArtifact.RunID != manifest.Canary.RunID || approvalArtifact.TaskID != manifest.Canary.TaskID ||
		approvalArtifact.CandidateCommit != manifest.Canary.CandidateCommit ||
		!approvalArtifact.Verification.Equal(manifest.Canary.Verification) ||
		!approvalArtifact.Review.Equal(manifest.Canary.Review) {
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

func directoryDigests(root string) (map[string]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("release evidence directory contains a symlink")
		}
		if !entry.IsDir() {
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				return errors.New("release evidence directory contains a non-regular file")
			}
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	result := make(map[string]string, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		result[filepath.ToSlash(relative)] = digest(body)
	}
	return result, nil
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func commandOutput(name string, arguments ...string) (string, error) {
	body, err := exec.Command(name, arguments...).CombinedOutput()
	return strings.TrimSpace(string(body)), err
}
