package release

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/review"
	runtimeledger "github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

func testRef(value string) workflow.ArtifactRef {
	return workflow.ArtifactRef{URI: "artifact://sha256/" + value, Digest: value}
}

func testManifest() beta.ReleaseManifest {
	return beta.ReleaseManifest{
		Deployment: beta.DeploymentRecord{
			SourceCommit: strings.Repeat("c", 40), GitVersion: "git version test",
			GoVersion: runtime.Version(), ToolchainVersion: runtime.Version(),
		},
		Toolchains: beta.ReleaseToolchains{
			Git: "git version test", Go: runtime.Version(), UV: "uv test", Docker: "docker test",
		},
		Canary: beta.CanaryEvidence{
			RunID: "run", TaskID: "task", PublicationID: "publication",
			CandidateCommit: strings.Repeat("d", 40), Verification: testRef("verification"),
			Review: testRef("review"), Approval: testRef("approval"), Publication: testRef("publication"),
			PullRequestURL: "https://example.test/pull/1",
		},
	}
}

func TestStrictJSONRejectsUnknownFieldsAndTrailers(t *testing.T) {
	type value struct {
		Name string `json:"name"`
	}
	var decoded value
	if err := strictJSON([]byte(`{"name":"ready"}`), &decoded); err != nil || decoded.Name != "ready" {
		t.Fatalf("strict decode = %#v, %v", decoded, err)
	}
	for _, body := range []string{`{"name":"ready","unknown":true}`, `{"name":"ready"} {}`} {
		if err := strictJSON([]byte(body), &value{}); err == nil {
			t.Fatalf("invalid JSON was accepted: %s", body)
		}
	}
}

func TestCommandOutputComparesStandardOutputOnly(t *testing.T) {
	value, err := commandOutput(t.Context(), "sh", "-c", "printf ready; printf advisory >&2")
	if err != nil || value != "ready" {
		t.Fatalf("command output = %q, %v", value, err)
	}
}

type sourceCommand struct{ dirty bool }

func (command sourceCommand) run(ctx context.Context, name string, arguments ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	invocation := name + " " + strings.Join(arguments, " ")
	switch {
	case strings.Contains(invocation, "rev-parse"):
		return strings.Repeat("c", 40), nil
	case strings.Contains(invocation, "status --porcelain") && command.dirty:
		return " M changed", nil
	case strings.Contains(invocation, "status --porcelain"):
		return "", nil
	case name == "git":
		return "git version test", nil
	case name == "uv":
		return "uv test", nil
	case name == "docker":
		return "docker test", nil
	default:
		return "", errors.New("unexpected command")
	}
}

func TestSourceAndToolchainChecks(t *testing.T) {
	manifest := testManifest()
	tests := []struct {
		name      string
		mutate    func(*beta.ReleaseManifest)
		dirty     bool
		cancel    bool
		wantCheck string
	}{
		{name: "valid"},
		{name: "commit mismatch", mutate: func(value *beta.ReleaseManifest) { value.Deployment.SourceCommit = "wrong" }, wantCheck: checkSourceCheckout},
		{name: "dirty checkout", dirty: true, wantCheck: checkSourceCheckout},
		{name: "version mismatch", mutate: func(value *beta.ReleaseManifest) { value.Toolchains.UV = "wrong" }, wantCheck: checkToolchains},
		{name: "caller cancellation", cancel: true, wantCheck: checkSourceCheckout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := manifest
			if test.mutate != nil {
				test.mutate(&value)
			}
			verifier := &Verifier{command: sourceCommand{dirty: test.dirty}.run}
			ctx := t.Context()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := verifier.verifySourceAndToolchains(ctx, &value)
			if test.wantCheck == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || failedCheck(err) != test.wantCheck {
				t.Fatalf("error = %v, check = %q", err, failedCheck(err))
			}
		})
	}
}

func TestCommandSeamReceivesSixtySecondDeadline(t *testing.T) {
	var remaining time.Duration
	verifier := &Verifier{command: func(ctx context.Context, _ string, _ ...string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("command context has no deadline")
		}
		remaining = time.Until(deadline)
		return "", nil
	}}
	if _, err := verifier.runCommand(t.Context(), "git", "--version"); err != nil {
		t.Fatal(err)
	}
	if remaining <= 59*time.Second || remaining > commandTimeout {
		t.Fatalf("remaining deadline = %s", remaining)
	}
}

func TestRunCommandNormalizesExpiredCommandContext(t *testing.T) {
	verifier := &Verifier{command: func(ctx context.Context, _ string, _ ...string) (string, error) {
		<-ctx.Done()
		return "", errors.New("process terminated")
	}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, err := verifier.runCommand(ctx, "git", "--version")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("command error = %v", err)
	}
}

func TestSourceCommandErrorsPreserveCause(t *testing.T) {
	want := errors.New("toolchain unavailable")
	verifier := &Verifier{command: func(context.Context, string, ...string) (string, error) {
		return "", want
	}}
	manifest := testManifest()
	err := verifier.verifySourceAndToolchains(t.Context(), &manifest)
	if !errors.Is(err, want) || failedCheck(err) != checkSourceCheckout {
		t.Fatalf("source error = %v, check = %q", err, failedCheck(err))
	}
}

func TestVerifyClassifiesExpiredCallerContextAsInconclusive(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx, func() {}
		}},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := test.ctx()
			defer cancel()
			manifest := testManifest()
			report, err := NewVerifier().Verify(ctx, &manifest)
			if err == nil || report.Status != "inconclusive" || report.FailedCheck != checkTimeout {
				t.Fatalf("report/error = %#v, %v", report, err)
			}
		})
	}
}

func TestStableFailedCheckValuesAndUnknownFallback(t *testing.T) {
	checks := []string{
		checkConfiguration, checkSourceCheckout, checkToolchains, checkMigrations,
		checkFiles, checkImages, checkWorkflowRuntime, checkReasoning,
		checkVerification, checkReview, checkApproval, checkPublication,
		checkGitHubDraft, checkHumanDecision, checkManifestCount, checkManifestSize,
		checkPromptCount, checkPromptSize, checkTimeout, checkUnknown,
	}
	for _, check := range checks {
		t.Run(check, func(t *testing.T) {
			if got := failedCheck(failed(check, errors.New("detail"))); got != check {
				t.Fatalf("failed check = %q", got)
			}
		})
	}
	if got := failedCheck(errors.New("unwrapped")); got != checkUnknown {
		t.Fatalf("unwrapped failed check = %q", got)
	}
}

func TestDirectoryFailureClassification(t *testing.T) {
	tests := []struct {
		name, countCheck, sizeCheck, want string
		err                               error
	}{
		{"manifest count", checkManifestCount, checkManifestSize, checkManifestCount, &beta.EvidenceCountLimitError{}},
		{"manifest size", checkManifestCount, checkManifestSize, checkManifestSize, &beta.EvidenceSizeLimitError{}},
		{"prompt count", checkPromptCount, checkPromptSize, checkPromptCount, &beta.EvidenceCountLimitError{}},
		{"prompt size", checkPromptCount, checkPromptSize, checkPromptSize, &beta.EvidenceSizeLimitError{}},
		{"unsafe file", checkManifestCount, checkManifestSize, checkFiles, errors.New("symlink")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := failedCheck(classifyDirectoryFailure(test.err, test.countCheck, test.sizeCheck)); got != test.want {
				t.Fatalf("failed check = %q", got)
			}
		})
	}
}

func TestEvidenceBindingPredicates(t *testing.T) {
	tests := []struct {
		name  string
		check func(bool) bool
	}{
		{"run", func(mutated bool) bool {
			manifest := testManifest()
			value := workflow.Run{State: workflow.RunStateMergeReady, CandidateCommit: manifest.Canary.CandidateCommit,
				Verification: &manifest.Canary.Verification, Review: &manifest.Canary.Review, Approval: &manifest.Canary.Approval,
				Publication: &manifest.Canary.Publication}
			if mutated {
				value.CandidateCommit = "mutated"
			}
			return validRunEvidence(&value, &manifest)
		}},
		{"task", func(mutated bool) bool {
			manifest := testManifest()
			value := workflow.Task{State: workflow.TaskStateAccepted, CandidateCommit: manifest.Canary.CandidateCommit,
				Verification: &manifest.Canary.Verification, Review: &manifest.Canary.Review, Approval: &manifest.Canary.Approval}
			if mutated {
				value.Approval = &workflow.ArtifactRef{URI: "mutated"}
			}
			return validTaskEvidence(&value, &manifest)
		}},
		{"runtime", func(mutated bool) bool {
			canary := beta.Config{}
			canary.Policy.Repository.BaseCommit = "base"
			canary.Policy.Images.Execution = "execution-image"
			canary.Policy.Images.Verification = "verification-image"
			policy, repositoryMap := testRef("policy"), testRef("repository-map")
			value := runtimeledger.RunBinding{BaseCommit: "base", RepositoryMap: &repositoryMap, Policy: &policy,
				ExecutionImageDigest: "execution-image", VerificationImageDigest: "verification-image"}
			if mutated {
				value.ExecutionImageDigest = "mutated"
			}
			return validRuntimeBinding(&value, &canary)
		}},
		{"invocation", func(mutated bool) bool {
			proposal := testRef("proposal")
			values := []invocationEvidence{
				{stage: "implementation", provider: gateway.MiniMaxAnthropicProvider, model: gateway.MiniMaxModel,
					requestID: "implementation", providerRequestID: "provider-1", proposal: proposal, requests: 1, input: 1, output: 1},
				{stage: "review", provider: gateway.MiniMaxAnthropicProvider, model: gateway.MiniMaxModel,
					requestID: "review", providerRequestID: "provider-2", requests: 1, input: 1, output: 1},
			}
			if mutated {
				values[0].proposal = testRef("mutated")
			}
			task := workflow.Task{Proposal: &proposal}
			return validInvocationSet(values, &task) && validLiveInvocation(&values[0]) && validLiveInvocation(&values[1])
		}},
		{"publication", func(mutated bool) bool {
			manifest := testManifest()
			value := publication.CompletedPublication{CandidateCommit: manifest.Canary.CandidateCommit,
				PullRequestURL: manifest.Canary.PullRequestURL, PublicationArtifact: manifest.Canary.Publication}
			if mutated {
				value.PullRequestURL = "mutated"
			}
			return validCompletedPublication(&value, &manifest)
		}},
		{"configuration", func(mutated bool) bool {
			deployment := beta.Deployment{SourceCommit: "source"}
			record := beta.DeploymentRecord{SourceCommit: "source"}
			digest, _ := deployment.Digest()
			record.ConfigurationDigest = digest
			if mutated {
				record.SourceCommit = "mutated"
			}
			return verifyConfigurationBindings(&deployment, &record, &beta.Config{}) == nil
		}},
		{"verification", func(mutated bool) bool {
			manifest := testManifest()
			report := verification.VerificationReport{VerificationID: "verification-id"}
			result := verification.Result{VerificationID: "verification-id", CandidateCommit: manifest.Canary.CandidateCommit,
				ReportArtifact: manifest.Canary.Verification, Passed: true}
			if mutated {
				result.ReportArtifact = testRef("mutated")
			}
			return validVerificationResult("completed", &result, &report, &manifest)
		}},
		{"review", func(mutated bool) bool {
			manifest := testManifest()
			proposal := testRef("proposal")
			invocation := invocationEvidence{proposal: proposal}
			value := review.ReviewReport{Passed: true, RunID: manifest.Canary.RunID, TaskID: manifest.Canary.TaskID,
				CandidateCommit: manifest.Canary.CandidateCommit, Proposal: proposal, Verification: manifest.Canary.Verification}
			if mutated {
				value.TaskID = "mutated"
			}
			return validReviewReport(&value, &manifest, &invocation)
		}},
		{"approval", func(mutated bool) bool {
			manifest := testManifest()
			value := approval.TaskApproval{Decision: "approve", RunID: manifest.Canary.RunID, TaskID: manifest.Canary.TaskID,
				CandidateCommit: manifest.Canary.CandidateCommit, Verification: manifest.Canary.Verification, Review: manifest.Canary.Review}
			if mutated {
				value.Review = testRef("mutated")
			}
			return validApprovalArtifact(&value, &manifest)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !test.check(false) {
				t.Fatal("positive binding rejected")
			}
			if test.check(true) {
				t.Fatal("single binding mutation accepted")
			}
		})
	}
}

func TestVerificationResultRejectsEveryLedgerBindingMutation(t *testing.T) {
	manifest := testManifest()
	report := verification.VerificationReport{VerificationID: "verification-id"}
	base := verification.Result{VerificationID: report.VerificationID, CandidateCommit: manifest.Canary.CandidateCommit,
		ReportArtifact: manifest.Canary.Verification, Passed: true}
	tests := []struct {
		name   string
		state  string
		mutate func(*verification.Result)
	}{
		{"positive", "completed", nil},
		{"wrong verification id", "completed", func(value *verification.Result) { value.VerificationID = "wrong" }},
		{"wrong candidate commit", "completed", func(value *verification.Result) { value.CandidateCommit = "wrong" }},
		{"wrong report artifact", "completed", func(value *verification.Result) { value.ReportArtifact = testRef("wrong") }},
		{"wrong state", "evidence_ready", nil},
		{"failed result", "completed", func(value *verification.Result) { value.Passed = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			if test.mutate != nil {
				test.mutate(&value)
			}
			got := validVerificationResult(test.state, &value, &report, &manifest)
			if got != (test.name == "positive") {
				t.Fatalf("valid = %t", got)
			}
		})
	}
}

func TestReportFailedCheckIsAdditiveAndOptional(t *testing.T) {
	ready := Report{SchemaVersion: ReportVersion, Status: "ready"}
	failed := ready
	failed.FailedCheck = checkVerification
	if reflect.DeepEqual(ready, failed) {
		t.Fatal("failed check was not represented")
	}
}
