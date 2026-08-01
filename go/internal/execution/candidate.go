package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/subprocess"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

var refComponentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type treeEntry struct {
	mode     string
	objectID string
	body     []byte
}

func buildCandidate(
	ctx context.Context,
	config Config,
	worktree string,
	request Request,
	mappedRequest contracts.ImplementationRequest,
	proposal contracts.ImplementationProposal,
) (string, string, []DiffEntry, bool, error) {
	indexPath, err := prepareCandidateIndex(
		ctx, config.RepositoryRoot, config.WorktreeRoot, mappedRequest.BaseCommit,
	)
	if err != nil {
		return "", "", nil, false, err
	}
	defer os.RemoveAll(filepath.Dir(indexPath))
	if err := applyCandidateChanges(
		ctx, config.RepositoryRoot, indexPath, worktree, mappedRequest, proposal.Changes,
	); err != nil {
		return "", "", nil, false, err
	}
	candidate, err := writeCandidateCommit(
		ctx, config, indexPath, request, mappedRequest,
	)
	if err != nil {
		return "", "", nil, false, err
	}
	actual, err := verifyCandidateDiff(
		ctx, config.RepositoryRoot, mappedRequest.BaseCommit, candidate, proposal.Changes,
	)
	if err != nil {
		return "", "", nil, false, err
	}
	ref, err := candidateRef(mappedRequest, request.Lease.FencingToken)
	if err != nil {
		return "", "", nil, false, err
	}
	created, err := retainCandidateRef(
		ctx, config.RepositoryRoot, indexPath, ref, candidate,
	)
	if err != nil {
		return "", "", nil, false, err
	}
	return candidate, ref, actual, created, nil
}

func prepareCandidateIndex(
	ctx context.Context, repository, worktreeRoot, baseCommit string,
) (string, error) {
	indexRoot, err := os.MkdirTemp(worktreeRoot, ".candidate-index-*")
	if err != nil {
		return "", fmt.Errorf("create candidate index directory: %w", err)
	}
	indexPath := filepath.Join(indexRoot, "index")
	if _, err := gitIndexOutput(
		ctx, repository, indexPath, nil, "read-tree", baseCommit,
	); err != nil {
		return "", fmt.Errorf("read candidate base tree: %w", err)
	}
	return indexPath, nil
}

func applyCandidateChanges(
	ctx context.Context,
	repository, indexPath, worktree string,
	request contracts.ImplementationRequest,
	changes []contracts.FileChange,
) error {
	for _, change := range changes {
		if err := applyCandidateChange(
			ctx, repository, indexPath, worktree, request.BaseCommit, change,
		); err != nil {
			return err
		}
	}
	return nil
}

func applyCandidateChange(
	ctx context.Context,
	repository, indexPath, worktree, baseCommit string,
	change contracts.FileChange,
) error {
	if change.Operation == contracts.FileDelete {
		if _, err := gitIndexOutput(
			ctx, repository, indexPath, nil,
			"update-index", "--force-remove", "--", change.Path,
		); err != nil {
			return fmt.Errorf("remove candidate path %q: %w", change.Path, err)
		}
		return nil
	}
	if change.Operation != contracts.FileCreate && change.Operation != contracts.FileUpdate {
		return fmt.Errorf("%w: unsupported operation", ErrInvalidRequest)
	}
	body, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(change.Path)))
	if err != nil {
		return fmt.Errorf("read applied path %q: %w", change.Path, err)
	}
	if change.ReplacementContent == nil ||
		sha256Hex(body) != sha256Hex([]byte(*change.ReplacementContent)) {
		return fmt.Errorf("%w: applied content %q", ErrArtifactIntegrity, change.Path)
	}
	objectID, err := gitIndexOutput(
		ctx, repository, indexPath, body, "hash-object", "-w", "--stdin",
	)
	if err != nil {
		return fmt.Errorf("hash candidate path %q: %w", change.Path, err)
	}
	mode, err := candidateMode(ctx, repository, baseCommit, change)
	if err != nil {
		return err
	}
	cacheInfo := mode + "," + strings.TrimSpace(string(objectID)) + "," + change.Path
	if _, err := gitIndexOutput(
		ctx, repository, indexPath, nil,
		"update-index", "--add", "--cacheinfo", cacheInfo,
	); err != nil {
		return fmt.Errorf("index candidate path %q: %w", change.Path, err)
	}
	return nil
}

func candidateMode(
	ctx context.Context,
	repository, baseCommit string,
	change contracts.FileChange,
) (string, error) {
	if change.Operation == contracts.FileCreate {
		return "100644", nil
	}
	base, err := readTreeEntry(ctx, repository, baseCommit, change.Path)
	if err != nil {
		return "", err
	}
	return base.mode, nil
}

func writeCandidateCommit(
	ctx context.Context,
	config Config,
	indexPath string,
	request Request,
	mappedRequest contracts.ImplementationRequest,
) (string, error) {
	tree, err := gitIndexOutput(
		ctx, config.RepositoryRoot, indexPath, nil, "write-tree",
	)
	if err != nil {
		return "", fmt.Errorf("write candidate tree: %w", err)
	}
	message := fmt.Sprintf(
		"harness: apply task %s attempt %d\n",
		mappedRequest.ApprovedTaskID, mappedRequest.Envelope.Attempt,
	)
	environment := []string{
		"GIT_AUTHOR_NAME=" + config.AuthorName,
		"GIT_AUTHOR_EMAIL=" + config.AuthorEmail,
		"GIT_COMMITTER_NAME=" + config.AuthorName,
		"GIT_COMMITTER_EMAIL=" + config.AuthorEmail,
		"GIT_AUTHOR_DATE=" + request.ExecutionTimestamp.UTC().Format("2006-01-02T15:04:05.999999999Z"),
		"GIT_COMMITTER_DATE=" + request.ExecutionTimestamp.UTC().Format("2006-01-02T15:04:05.999999999Z"),
	}
	commit, err := gitIndexOutputEnv(
		ctx, config.RepositoryRoot, indexPath, []byte(message), environment,
		"commit-tree", strings.TrimSpace(string(tree)), "-p", mappedRequest.BaseCommit,
	)
	if err != nil {
		return "", fmt.Errorf("create candidate commit: %w", err)
	}
	return strings.TrimSpace(string(commit)), nil
}

func retainCandidateRef(
	ctx context.Context, repository, indexPath, ref, candidate string,
) (bool, error) {
	existing, resolveErr := gitOutput(
		ctx, repository, "rev-parse", "--verify", "--end-of-options", ref,
	)
	if resolveErr == nil {
		if strings.TrimSpace(string(existing)) != candidate {
			return false, fmt.Errorf("%w: candidate ref conflict", ErrArtifactIntegrity)
		}
		return false, nil
	}
	if _, err := gitIndexOutput(
		ctx, repository, indexPath, nil,
		"update-ref", ref, candidate, strings.Repeat("0", 40),
	); err != nil {
		return false, fmt.Errorf("retain candidate ref: %w", err)
	}
	return true, nil
}

func verifyCandidateDiff(
	ctx context.Context,
	repository, baseCommit, candidate string,
	changes []contracts.FileChange,
) ([]DiffEntry, error) {
	actualPaths, err := candidatePathStatuses(ctx, repository, baseCommit, candidate)
	if err != nil {
		return nil, err
	}
	if len(actualPaths) != len(changes) {
		return nil, fmt.Errorf("%w: candidate changed unauthorized paths", ErrArtifactIntegrity)
	}
	result := make([]DiffEntry, 0, len(changes))
	for _, change := range changes {
		entry, err := verifyCandidateChange(
			ctx, repository, baseCommit, candidate, change, actualPaths,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	slices.SortFunc(result, func(left, right DiffEntry) int {
		return strings.Compare(left.Path, right.Path)
	})
	return result, nil
}

func candidatePathStatuses(
	ctx context.Context, repository, baseCommit, candidate string,
) (map[string]string, error) {
	names, err := gitOutput(
		ctx, repository, "diff-tree", "-r", "--no-commit-id",
		"--name-status", "-z", "--no-renames", baseCommit, candidate,
	)
	if err != nil {
		return nil, fmt.Errorf("list candidate diff: %w", err)
	}
	fields := bytes.Split(names, []byte{0})
	actualPaths := make(map[string]string, len(fields)/2)
	for index := 0; index+1 < len(fields); index += 2 {
		if len(fields[index]) == 0 {
			continue
		}
		actualPaths[string(fields[index+1])] = string(fields[index])
	}
	return actualPaths, nil
}

func verifyCandidateChange(
	ctx context.Context,
	repository, baseCommit, candidate string,
	change contracts.FileChange,
	actualPaths map[string]string,
) (DiffEntry, error) {
	status, exists := actualPaths[change.Path]
	wantStatus := map[contracts.FileOperation]string{
		contracts.FileCreate: "A", contracts.FileUpdate: "M", contracts.FileDelete: "D",
	}[change.Operation]
	if !exists || status != wantStatus {
		return DiffEntry{},
			fmt.Errorf("%w: candidate operation %q", ErrArtifactIntegrity, change.Path)
	}
	before, err := verifiedBeforeEntry(ctx, repository, baseCommit, change)
	if err != nil {
		return DiffEntry{}, err
	}
	after, err := verifiedAfterEntry(ctx, repository, candidate, change, before.mode)
	if err != nil {
		return DiffEntry{}, err
	}
	mode := after.mode
	if change.Operation == contracts.FileDelete {
		mode = before.mode
	}
	return DiffEntry{
		Operation: change.Operation, Path: change.Path, Mode: mode,
		BeforeSHA256: digestOrEmpty(before.body, change.Operation == contracts.FileCreate),
		AfterSHA256:  digestOrEmpty(after.body, change.Operation == contracts.FileDelete),
	}, nil
}

func verifiedBeforeEntry(
	ctx context.Context, repository, baseCommit string, change contracts.FileChange,
) (treeEntry, error) {
	if change.Operation == contracts.FileCreate {
		return treeEntry{}, nil
	}
	before, err := readTreeEntry(ctx, repository, baseCommit, change.Path)
	if err != nil || sha256Hex(before.body) != change.ExpectedOriginalSHA256 {
		return treeEntry{},
			fmt.Errorf("%w: before digest %q", ErrArtifactIntegrity, change.Path)
	}
	return before, nil
}

func verifiedAfterEntry(
	ctx context.Context,
	repository, candidate string,
	change contracts.FileChange,
	beforeMode string,
) (treeEntry, error) {
	if change.Operation == contracts.FileDelete {
		return treeEntry{}, nil
	}
	after, err := readTreeEntry(ctx, repository, candidate, change.Path)
	if err != nil || change.ReplacementContent == nil ||
		sha256Hex(after.body) != sha256Hex([]byte(*change.ReplacementContent)) {
		return treeEntry{}, fmt.Errorf("%w: after digest %q", ErrArtifactIntegrity, change.Path)
	}
	if change.Operation == contracts.FileCreate && after.mode != "100644" ||
		change.Operation == contracts.FileUpdate && after.mode != beforeMode {
		return treeEntry{}, fmt.Errorf("%w: candidate mode %q", ErrArtifactIntegrity, change.Path)
	}
	return after, nil
}

func readTreeEntry(
	ctx context.Context, repository, commit, filePath string,
) (treeEntry, error) {
	output, err := gitOutput(ctx, repository, "ls-tree", "-z", commit, "--", filePath)
	if err != nil {
		return treeEntry{}, err
	}
	record := bytes.TrimSuffix(output, []byte{0})
	header, actualPath, ok := bytes.Cut(record, []byte{'\t'})
	fields := bytes.Fields(header)
	if !ok || string(actualPath) != filePath || len(fields) != 3 ||
		string(fields[1]) != "blob" ||
		(string(fields[0]) != "100644" && string(fields[0]) != "100755") {
		return treeEntry{}, fmt.Errorf("%w: unsupported tree entry %q", ErrUnsafePath, filePath)
	}
	body, err := gitOutput(ctx, repository, "cat-file", "blob", string(fields[2]))
	if err != nil {
		return treeEntry{}, err
	}
	return treeEntry{mode: string(fields[0]), objectID: string(fields[2]), body: body}, nil
}

func gitIndexOutput(
	ctx context.Context,
	repository, index string,
	input []byte,
	arguments ...string,
) ([]byte, error) {
	return gitIndexOutputEnv(ctx, repository, index, input, nil, arguments...)
}

func gitIndexOutputEnv(
	ctx context.Context,
	repository, index string,
	input []byte,
	extraEnvironment []string,
	arguments ...string,
) ([]byte, error) {
	base := []string{
		"--literal-pathspecs",
		"-c", "core.hooksPath=/dev/null",
		"-c", "diff.external=",
		"-c", "core.attributesFile=/dev/null",
		"-C", repository,
	}
	command := exec.CommandContext(ctx, "git", append(base, arguments...)...)
	command.Env = subprocess.Git(
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_INDEX_FILE="+index,
	)
	command.Env = append(command.Env, extraEnvironment...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf(
			"git %s: %w: %s", arguments[0], err, strings.TrimSpace(stderr.String()),
		)
	}
	return stdout.Bytes(), nil
}

func candidateRef(request contracts.ImplementationRequest, fence uint32) (string, error) {
	runID, taskID := request.Envelope.RunID, request.ApprovedTaskID
	if !refComponentPattern.MatchString(runID) || !refComponentPattern.MatchString(taskID) {
		return "", fmt.Errorf("%w: candidate ref identity", ErrInvalidRequest)
	}
	return "refs/harness/candidates/" + runID + "/" + taskID + "/" +
		strconv.FormatUint(uint64(request.Envelope.Attempt), 10) + "-" +
		strconv.FormatUint(uint64(fence), 10), nil
}

func deleteCandidateRef(
	ctx context.Context, repository, ref, candidate string,
) error {
	_, err := gitOutput(ctx, repository, "update-ref", "-d", ref, candidate)
	return err
}

func marshalReport(report ExecutionReport) ([]byte, error) {
	body, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("marshal execution report: %w", err)
	}
	return body, nil
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func digestOrEmpty(body []byte, empty bool) string {
	if empty {
		return emptySHA256
	}
	return sha256Hex(body)
}
