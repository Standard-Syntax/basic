//go:build integration

package runtime

import (
	"bytes"
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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const pythonProjectReportSchema = "harness_python_project_e2e_report.v2"

type pythonProjectReport struct {
	SchemaVersion           string                    `json:"schema_version"`
	Status                  string                    `json:"status"`
	Failure                 pythonReportFailure       `json:"failure"`
	RunID                   string                    `json:"run_id"`
	TaskID                  string                    `json:"task_id"`
	BaseCommit              string                    `json:"base_commit"`
	CandidateCommit         string                    `json:"candidate_commit"`
	ArtifactDigests         []string                  `json:"artifact_digests"`
	LifecycleStates         map[string]string         `json:"lifecycle_states"`
	StageTimings            []pythonReportStageTiming `json:"stage_timings"`
	HarnessVersion          string                    `json:"installed_harness_version"`
	SourceCommit            string                    `json:"source_commit"`
	WorkerImageIDs          map[string]string         `json:"worker_image_ids"`
	ReplayCounts            map[string]int            `json:"replay_counts"`
	ConsoleResult           pythonReportCommandResult `json:"console_result"`
	CheckResult             pythonReportCommandResult `json:"check_result"`
	GeneratedCommand        string                    `json:"generated_command"`
	InvocationCounts        map[string]int            `json:"reasoning_invocation_counts"`
	PreservationStatus      string                    `json:"preservation_status"`
	PreservedCheckoutCommit string                    `json:"preserved_checkout_commit"`
	output                  string
	databaseURL             string
	repository              string
	preserveProject         string
	credentialValues        []string
}

type pythonReportFailure struct {
	Stage string `json:"stage"`
	Code  string `json:"code"`
}

type pythonReportStageTiming struct {
	Stage               string `json:"stage"`
	CompletedAt         string `json:"completed_at"`
	ElapsedMilliseconds int64  `json:"elapsed_milliseconds"`
}

type pythonReportCommandResult struct {
	Executed     bool   `json:"executed"`
	Passed       bool   `json:"passed"`
	ExitCode     int    `json:"exit_code"`
	OutputSHA256 string `json:"output_sha256"`
}

func newPythonProjectReport(output, databaseURL, repository, preserveProject string) *pythonProjectReport {
	return &pythonProjectReport{
		SchemaVersion: pythonProjectReportSchema, Status: "failed",
		Failure:         pythonReportFailure{Stage: "setup", Code: "setup_failed"},
		ArtifactDigests: []string{}, LifecycleStates: map[string]string{},
		StageTimings:       []pythonReportStageTiming{},
		WorkerImageIDs:     map[string]string{"execution": "", "verification": ""},
		ReplayCounts:       map[string]int{"run": 0, "approval": 0, "publication": 0},
		InvocationCounts:   map[string]int{"specification": 0, "planning": 0, "implementation": 0, "review": 0},
		PreservationStatus: "not_requested",
		output:             output, databaseURL: databaseURL, repository: repository,
		preserveProject: preserveProject,
	}
}

func (r *pythonProjectReport) setStage(stage string) {
	r.Failure = pythonReportFailure{Stage: stage, Code: strings.ReplaceAll(stage, "-", "_") + "_failed"}
}

func (r *pythonProjectReport) markPassed() {
	r.Status = "passed"
	r.Failure = pythonReportFailure{}
}

func (r *pythonProjectReport) finish() error {
	r.collectDurableEvidence()
	if r.preserveProject != "" {
		r.PreservationStatus = "failed"
		_, sourceErr := os.Lstat(r.repository)
		if sourceErr == nil {
			revision := r.BaseCommit
			if r.CandidateCommit != "" {
				revision = r.CandidateCommit
			}
			if err := scanAndPreserveProject(
				r.repository, r.preserveProject, revision, r.credentialValues,
			); err != nil {
				if r.Status == "passed" {
					r.Status = "failed"
					r.Failure = pythonReportFailure{Stage: "preservation", Code: "preservation_failed"}
				}
				if writeErr := writePythonProjectReport(r.output, r, r.credentialValues); writeErr != nil {
					return errors.Join(err, writeErr)
				}
				return err
			}
			r.PreservationStatus = "preserved"
			r.PreservedCheckoutCommit = revision
		} else if !errors.Is(sourceErr, os.ErrNotExist) {
			if writeErr := writePythonProjectReport(r.output, r, r.credentialValues); writeErr != nil {
				return errors.Join(sourceErr, writeErr)
			}
			return sourceErr
		}
	}
	return writePythonProjectReport(r.output, r, r.credentialValues)
}

func (r *pythonProjectReport) collectDurableEvidence() {
	if r.databaseURL == "" || r.RunID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, r.databaseURL)
	if err != nil {
		return
	}
	defer pool.Close()
	var runState string
	if pool.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE run_id=$1`, r.RunID).
		Scan(&runState) == nil {
		r.LifecycleStates["run"] = runState
	}
	if r.TaskID != "" {
		var taskState string
		if pool.QueryRow(ctx, `SELECT state FROM workflow_tasks WHERE run_id=$1 AND task_id=$2`,
			r.RunID, r.TaskID).Scan(&taskState) == nil {
			r.LifecycleStates["task"] = taskState
		}
	}
	rows, err := pool.Query(ctx, `SELECT stage,attempt,state FROM runtime_stage_jobs
		WHERE run_id=$1 ORDER BY stage,attempt`, r.RunID)
	if err == nil {
		for rows.Next() {
			var stage, state string
			var attempt int
			if rows.Scan(&stage, &attempt, &state) == nil {
				r.LifecycleStates[fmt.Sprintf("stage.%s.%d", stage, attempt)] = state
			}
		}
		rows.Close()
	}
	r.collectEventTimings(ctx, pool)
	r.collectArtifactDigests(ctx, pool)
}

func (r *pythonProjectReport) collectEventTimings(ctx context.Context, pool *pgxpool.Pool) {
	rows, err := pool.Query(ctx, `SELECT event_type,occurred_at FROM workflow_events
		WHERE aggregate_id=$1 OR aggregate_id=$2 ORDER BY occurred_at,sequence`, r.RunID, r.TaskID)
	if err != nil {
		return
	}
	defer rows.Close()
	var previous time.Time
	for rows.Next() {
		var stage string
		var occurred time.Time
		if rows.Scan(&stage, &occurred) != nil {
			return
		}
		elapsed := int64(0)
		if !previous.IsZero() && occurred.After(previous) {
			elapsed = occurred.Sub(previous).Milliseconds()
		}
		r.StageTimings = append(r.StageTimings, pythonReportStageTiming{
			Stage: stage, CompletedAt: occurred.UTC().Format(time.RFC3339Nano),
			ElapsedMilliseconds: elapsed,
		})
		previous = occurred
	}
}

func (r *pythonProjectReport) collectArtifactDigests(ctx context.Context, pool *pgxpool.Pool) {
	values := map[string]struct{}{}
	queries := []string{
		`SELECT COALESCE(specification_digest,''),COALESCE(task_graph_digest,'')
		 FROM workflow_runs WHERE run_id=$1`,
		`SELECT COALESCE(proposal_digest,''),COALESCE(execution_digest,''),
		 COALESCE(verification_digest,''),COALESCE(review_digest,''),COALESCE(approval_digest,'')
		 FROM workflow_tasks WHERE run_id=$1`,
		`SELECT COALESCE(intake_digest,''),COALESCE(repository_map_digest,''),
		 COALESCE(approved_specification_digest,''),COALESCE(approved_task_graph_digest,''),
		 COALESCE(composite_approval_digest,'') FROM runtime_run_bindings WHERE run_id=$1`,
		`SELECT COALESCE(result_digest,''),COALESCE(failure_digest,'')
		 FROM runtime_stage_jobs WHERE run_id=$1`,
		`SELECT COALESCE(proposal_artifact_uri,''),COALESCE(provider_response_artifact_uri,'')
		 FROM reasoning_invocations WHERE run_id=$1`,
	}
	for _, query := range queries {
		rows, err := pool.Query(ctx, query, r.RunID)
		if err != nil {
			continue
		}
		for rows.Next() {
			raw := make([]any, len(rows.FieldDescriptions()))
			pointers := make([]any, len(raw))
			for index := range raw {
				pointers[index] = &raw[index]
			}
			if rows.Scan(pointers...) != nil {
				continue
			}
			for _, item := range raw {
				value, ok := item.(string)
				if !ok {
					continue
				}
				value = strings.TrimPrefix(value, "artifact://sha256/")
				if validArtifactDigest(value) {
					values[value] = struct{}{}
				}
			}
		}
		rows.Close()
	}
	r.ArtifactDigests = r.ArtifactDigests[:0]
	for value := range values {
		r.ArtifactDigests = append(r.ArtifactDigests, value)
	}
	sort.Strings(r.ArtifactDigests)
}

func validArtifactDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func writePythonProjectReport(
	output string, report *pythonProjectReport, credentialValues []string,
) error {
	if err := validatePythonProjectReport(report); err != nil {
		return err
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal Python project report: %w", err)
	}
	body = append(body, '\n')
	for _, secret := range credentialValues {
		if secret != "" && bytes.Contains(body, []byte(secret)) {
			return errors.New("credential found in Python project report")
		}
	}
	parent := filepath.Dir(output)
	if !filepath.IsAbs(output) || filepath.Clean(output) != output {
		return errors.New("report output must be clean and absolute")
	}
	if err := validateNonSymlinkedDirectoryChain(parent); err != nil {
		return errors.New("report output parent must be a non-symlinked directory chain")
	}
	if info, statErr := os.Lstat(output); statErr == nil && !info.Mode().IsRegular() {
		return errors.New("report output must replace only a regular file")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	temporary, err := os.CreateTemp(parent, ".python-project-report-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, output); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validatePythonProjectReport(report *pythonProjectReport) error {
	if report.SchemaVersion != pythonProjectReportSchema ||
		(report.Status != "passed" && report.Status != "failed") {
		return errors.New("invalid Python project report identity or status")
	}
	if report.Status == "passed" && (report.Failure.Stage != "" || report.Failure.Code != "") {
		return errors.New("passing Python project report contains a failure")
	}
	if report.Status == "failed" && (report.Failure.Stage == "" || report.Failure.Code == "") {
		return errors.New("failed Python project report lacks a stable failure")
	}
	if report.Status == "passed" {
		if !validCommit(report.SourceCommit) || !validCommit(report.BaseCommit) ||
			!validCommit(report.CandidateCommit) || report.RunID == "" || report.TaskID == "" ||
			report.HarnessVersion == "" || report.GeneratedCommand == "" ||
			!report.CheckResult.Executed || !report.CheckResult.Passed ||
			len(report.ArtifactDigests) == 0 || len(report.StageTimings) == 0 ||
			report.LifecycleStates["run"] == "" || report.LifecycleStates["task"] == "" ||
			report.ReplayCounts["run"] != 1 || report.ReplayCounts["approval"] != 1 ||
			report.ReplayCounts["publication"] != 1 ||
			!validImageID(report.WorkerImageIDs["execution"]) ||
			!validImageID(report.WorkerImageIDs["verification"]) {
			return errors.New("passing Python project report is incomplete")
		}
	}
	if (report.PreservationStatus != "not_requested" && report.PreservationStatus != "preserved" &&
		report.PreservationStatus != "failed") ||
		(report.PreservationStatus == "preserved" && !validCommit(report.PreservedCheckoutCommit)) {
		return errors.New("Python project report contains invalid handoff metadata")
	}
	for _, digest := range report.ArtifactDigests {
		if !validArtifactDigest(digest) {
			return errors.New("Python project report contains an invalid artifact digest")
		}
	}
	return nil
}

func validCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func validImageID(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validArtifactDigest(strings.TrimPrefix(value, "sha256:"))
}

func scanAndPreserveProject(source, destination, revision string, secrets []string) error {
	if source == "" {
		return errors.New("generated project is unavailable")
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return errors.New("preservation destination must be clean and absolute")
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("preservation destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(destination)
	if err := validateNonSymlinkedDirectoryChain(parent); err != nil {
		return err
	}
	status, err := preservationGitOutput(source, "status", "--porcelain")
	if err != nil || status != "" {
		return errors.New("generated project worktree must be clean")
	}
	if err := scanProject(source, secrets); err != nil {
		return err
	}
	if !validCommit(revision) {
		return errors.New("preservation revision is invalid")
	}
	if err := runPreservationGit(source, "cat-file", "-e", revision+"^{commit}"); err != nil {
		return errors.New("preservation revision is unreachable")
	}
	temporary, err := os.MkdirTemp(parent, ".preserved-project-*.tmp")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	if err := copyProjectTree(source, temporary); err != nil {
		return err
	}
	if err := runPreservationGit(temporary, "checkout", "--detach", "--force", revision); err != nil {
		return err
	}
	head, err := preservationGitOutput(temporary, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != revision {
		return errors.New("preserved checkout does not match requested revision")
	}
	status, err = preservationGitOutput(temporary, "status", "--porcelain")
	if err != nil || status != "" {
		return errors.New("preserved checkout is not clean")
	}
	if err := runPreservationGit(temporary, "fsck", "--full", "--no-reflogs"); err != nil {
		return errors.New("preserved repository has invalid Git objects")
	}
	if err := scanProject(temporary, secrets); err != nil {
		return err
	}
	if err := scanGitObjects(temporary, secrets); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	return nil
}

func preservationGitCommand(repository string, arguments ...string) *exec.Cmd {
	command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	return command
}

func runPreservationGit(repository string, arguments ...string) error {
	return preservationGitCommand(repository, arguments...).Run()
}

func preservationGitOutput(repository string, arguments ...string) (string, error) {
	body, err := preservationGitCommand(repository, arguments...).Output()
	return strings.TrimSpace(string(body)), err
}

func scanGitObjects(repository string, secrets []string) error {
	objects, err := preservationGitOutput(repository, "rev-list", "--objects", "--all")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(objects, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		body, err := preservationGitCommand(repository, "cat-file", "-p", fields[0]).Output()
		if err != nil {
			return err
		}
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(body, []byte(secret)) {
				return errors.New("credential found while scanning generated Git objects")
			}
		}
	}
	return nil
}

func validateNonSymlinkedDirectoryChain(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("preservation parent chain must contain only directories")
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func scanProject(source string, secrets []string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("generated project contains unsafe file type: %s", path)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(body, []byte(secret)) {
				return errors.New("credential found while scanning generated project")
			}
		}
		return nil
	})
}

func copyProjectTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == "." {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("generated project contains unsafe file type: %s", path)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		inputErr := input.Close()
		closeErr := output.Close()
		return errors.Join(copyErr, inputErr, closeErr)
	})
}

func TestPythonProjectReportShapeAtomicReplacementAndRedaction(t *testing.T) {
	output := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(output, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := newPythonProjectReport(output, "", "", "")
	report.SourceCommit = strings.Repeat("a", 40)
	report.BaseCommit = strings.Repeat("b", 40)
	report.CandidateCommit = strings.Repeat("c", 40)
	report.RunID, report.TaskID = "run-id", "task-id"
	report.HarnessVersion = "0.3.0"
	report.GeneratedCommand = "uv run --frozen live-demo"
	report.ArtifactDigests = []string{strings.Repeat("d", 64)}
	report.LifecycleStates = map[string]string{"run": "MERGE_READY", "task": "ACCEPTED"}
	report.StageTimings = []pythonReportStageTiming{{
		Stage: "RUN_CREATED", CompletedAt: "2026-08-03T00:00:00Z",
	}}
	report.WorkerImageIDs = map[string]string{
		"execution":    "sha256:" + strings.Repeat("e", 64),
		"verification": "sha256:" + strings.Repeat("f", 64),
	}
	report.ReplayCounts = map[string]int{"run": 1, "approval": 1, "publication": 1}
	report.CheckResult = commandResult([]byte("passed"), nil)
	report.markPassed()
	if err := writePythonProjectReport(output, report, []string{"operator-secret"}); err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, output, 0o600)
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("operator-secret")) {
		t.Fatal("credential persisted in report")
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		t.Fatal("report is not JSON")
	}
	expected := []string{"artifact_digests", "base_commit", "candidate_commit", "check_result",
		"console_result", "failure", "generated_command", "installed_harness_version", "lifecycle_states",
		"preservation_status", "preserved_checkout_commit", "reasoning_invocation_counts",
		"replay_counts", "run_id", "schema_version", "source_commit", "stage_timings",
		"status", "task_id", "worker_image_ids"}
	actual := make([]string, 0, len(value))
	for key := range value {
		actual = append(actual, key)
	}
	sort.Strings(actual)
	if !slicesEqual(actual, expected) {
		t.Fatalf("report fields = %v", actual)
	}
	temporaries, _ := filepath.Glob(filepath.Join(filepath.Dir(output), ".python-project-report-*.tmp"))
	if len(temporaries) != 0 {
		t.Fatalf("atomic report temporaries remain: %v", temporaries)
	}
	poisoned := *report
	poisoned.RunID = "operator-secret"
	if err := writePythonProjectReport(filepath.Join(filepath.Dir(output), "poisoned.json"),
		&poisoned, []string{"operator-secret"}); err == nil {
		t.Fatal("credentialed report was written")
	}
}

func TestPythonProjectPartialFailureReport(t *testing.T) {
	output := filepath.Join(t.TempDir(), "failure.json")
	report := newPythonProjectReport(output, "", "", "")
	report.setStage("implementation")
	if err := report.finish(); err != nil {
		t.Fatal(err)
	}
	value := readJSONObject(t, output)
	failure := value["failure"].(map[string]any)
	if value["status"] != "failed" || failure["code"] != "implementation_failed" ||
		value["run_id"] != "" {
		t.Fatalf("partial failure report = %#v", value)
	}
}

func TestPythonProjectPreservationRequiresScannedNonexistentDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, ".git", "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "app.py"), "safe\n")
	destination := filepath.Join(root, "preserved")
	runGitE2E(t, source, "init", "-q")
	runGitE2E(t, source, "add", "app.py")
	runGitE2E(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.com",
		"commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGitOutput(t, source, "rev-parse", "HEAD"))
	if err := scanAndPreserveProject(source, destination, revision, []string{"credential"}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(destination, "app.py")); err != nil || string(body) != "safe\n" {
		t.Fatalf("preserved project = %q, %v", body, err)
	}
	if err := scanAndPreserveProject(source, destination, revision, nil); err == nil {
		t.Fatal("existing preservation destination was accepted")
	}
	unsafe := filepath.Join(root, "unsafe")
	writeFile(t, filepath.Join(source, "secret"), "credential")
	if err := scanAndPreserveProject(source, unsafe, revision, []string{"credential"}); err == nil {
		t.Fatal("credentialed project was preserved")
	}
	if _, err := os.Lstat(unsafe); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe preservation destination exists: %v", err)
	}
}

func TestPythonProjectPreservationChecksOutExactReachableRevision(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "app.py"), "base\n")
	runGitE2E(t, source, "init", "-q")
	runGitE2E(t, source, "add", "app.py")
	runGitE2E(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.com",
		"commit", "-qm", "base")
	base := strings.TrimSpace(runGitOutput(t, source, "rev-parse", "HEAD"))
	writeFile(t, filepath.Join(source, "app.py"), "candidate\n")
	runGitE2E(t, source, "add", "app.py")
	runGitE2E(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.com",
		"commit", "-qm", "candidate")
	candidate := strings.TrimSpace(runGitOutput(t, source, "rev-parse", "HEAD"))
	runGitE2E(t, source, "checkout", "-q", base)

	destination := filepath.Join(root, "preserved")
	if err := scanAndPreserveProject(source, destination, candidate, nil); err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(runGitOutput(t, destination, "rev-parse", "HEAD")); head != candidate {
		t.Fatalf("preserved HEAD=%s want=%s", head, candidate)
	}
	if branch := strings.TrimSpace(runGitOutput(t, destination, "branch", "--show-current")); branch != "" {
		t.Fatalf("preserved checkout is attached to %q", branch)
	}
	if body, err := os.ReadFile(filepath.Join(destination, "app.py")); err != nil || string(body) != "candidate\n" {
		t.Fatalf("preserved candidate body=%q err=%v", body, err)
	}

	if err := scanAndPreserveProject(source, filepath.Join(root, "missing"),
		strings.Repeat("f", 40), nil); err == nil {
		t.Fatal("unreachable candidate was accepted")
	}
	writeFile(t, filepath.Join(source, "dirty.py"), "dirty\n")
	if err := scanAndPreserveProject(source, filepath.Join(root, "dirty"), base, nil); err == nil {
		t.Fatal("dirty source was accepted")
	}
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func commandResult(output []byte, err error) pythonReportCommandResult {
	digest := sha256.Sum256(output)
	exitCode := 0
	if err != nil {
		exitCode = -1
		if exit, ok := err.(*exec.ExitError); ok {
			exitCode = exit.ExitCode()
		}
	}
	return pythonReportCommandResult{
		Executed: true, Passed: err == nil, ExitCode: exitCode,
		OutputSHA256: hex.EncodeToString(digest[:]),
	}
}
