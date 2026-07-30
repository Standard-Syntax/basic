package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"google.golang.org/protobuf/proto"
)

var (
	ErrScopeLimit = errors.New("repository context exceeds approved limit")
	ErrScope      = errors.New("repository path is outside approved scope")
)

type ArtifactWriter interface {
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
}

type RepositorySnapshot struct {
	BaseCommit string
	Entries    []*reasoningv1.RepositoryEntry
}

type ContextLimits struct {
	MaxFiles int
	MaxBytes int64
}

func BuildApprovedSpecification(
	ctx context.Context,
	store ArtifactWriter,
	request *reasoningv1.SpecificationRequest,
	proposal *reasoningv1.SpecificationProposal,
) (workflow.ArtifactRef, error) {
	mapped, err := contracts.MapSpecificationRequest(request)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	if _, err := contracts.MapSpecificationProposal(proposal, mapped); err != nil {
		return workflow.ArtifactRef{}, err
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	return store.Put(ctx, body)
}

func BuildApprovedTask(
	ctx context.Context,
	store ArtifactWriter,
	request *reasoningv1.TaskPlanningRequest,
	proposal *reasoningv1.TaskGraphProposal,
	trustedChecks map[string]struct{},
) (workflow.ArtifactRef, *reasoningv1.PlannedTask, error) {
	mapped, err := contracts.MapTaskPlanningRequest(request)
	if err != nil {
		return workflow.ArtifactRef{}, nil, err
	}
	graph, err := contracts.MapTaskGraphProposal(proposal, mapped)
	if err != nil {
		return workflow.ArtifactRef{}, nil, err
	}
	if len(graph.Tasks) != 1 || len(graph.Tasks[0].Dependencies) != 0 ||
		len(proposal.GetTasks()) != 1 {
		return workflow.ArtifactRef{}, nil, fmt.Errorf("%w: first slice requires one dependency-free task", ErrScope)
	}
	for _, check := range graph.Tasks[0].RequiredCheckIDs {
		if _, ok := trustedChecks[check]; !ok {
			return workflow.ArtifactRef{}, nil, fmt.Errorf("%w: untrusted check %q", ErrScope, check)
		}
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal.GetTasks()[0])
	if err != nil {
		return workflow.ArtifactRef{}, nil, err
	}
	ref, err := store.Put(ctx, body)
	if err != nil {
		return workflow.ArtifactRef{}, nil, err
	}
	return ref, proto.Clone(proposal.GetTasks()[0]).(*reasoningv1.PlannedTask), nil
}

func SnapshotRepository(ctx context.Context, root, base string) (RepositorySnapshot, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return RepositorySnapshot{}, ErrScope
	}
	commitBody, err := gitOutput(ctx, root, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return RepositorySnapshot{}, fmt.Errorf("resolve base commit: %w", err)
	}
	commit := strings.TrimSpace(string(commitBody))
	if len(commit) != 40 {
		return RepositorySnapshot{}, ErrScope
	}
	tree, err := gitOutput(ctx, root, "ls-tree", "-rz", "--full-tree", commit)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	var entries []*reasoningv1.RepositoryEntry
	for _, record := range bytes.Split(tree, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, name, ok := bytes.Cut(record, []byte{'\t'})
		fields := bytes.Fields(header)
		if !ok || len(fields) != 3 || string(fields[1]) != "blob" || !safeRepoPath(string(name)) {
			return RepositorySnapshot{}, ErrScope
		}
		body, err := gitOutput(ctx, root, "cat-file", "blob", string(fields[2]))
		if err != nil {
			return RepositorySnapshot{}, err
		}
		sum := sha256.Sum256(body)
		entries = append(entries, &reasoningv1.RepositoryEntry{
			Path: string(name), Kind: "blob", Sha256: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return RepositorySnapshot{BaseCommit: commit, Entries: entries}, nil
}

func BuildImplementationContext(
	ctx context.Context,
	root, commit string,
	snapshot RepositorySnapshot,
	readable, prohibited []string,
	limits ContextLimits,
) ([]*reasoningv1.RepositoryContextFile, error) {
	if commit != snapshot.BaseCommit || limits.MaxFiles <= 0 || limits.MaxBytes <= 0 {
		return nil, ErrScope
	}
	var result []*reasoningv1.RepositoryContextFile
	var total int64
	for _, entry := range snapshot.Entries {
		if !withinAny(entry.Path, readable) || withinAny(entry.Path, prohibited) {
			continue
		}
		if len(result) == limits.MaxFiles {
			return nil, ErrScopeLimit
		}
		body, err := gitOutput(ctx, root, "show", commit+":"+entry.Path)
		if err != nil {
			return nil, err
		}
		total += int64(len(body))
		if total > limits.MaxBytes {
			return nil, ErrScopeLimit
		}
		if Digest(body) != entry.Sha256 {
			return nil, ErrScope
		}
		result = append(result, &reasoningv1.RepositoryContextFile{
			Path: entry.Path, Sha256: entry.Sha256, Content: string(body),
		})
	}
	if len(result) == 0 {
		return nil, ErrScope
	}
	return result, nil
}

func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	base := []string{"-C", root, "-c", "core.hooksPath=/dev/null", "-c", "diff.external=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.clean="}
	command := exec.CommandContext(ctx, "git", append(base, args...)...)
	command.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0"}
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return output, nil
}

func safeRepoPath(value string) bool {
	return value != "" && value == path.Clean(value) && !path.IsAbs(value) &&
		value != "." && value != ".git" && !strings.HasPrefix(value, "../") &&
		!strings.HasPrefix(value, ".git/")
}

func withinAny(value string, roots []string) bool {
	for _, root := range roots {
		root = strings.TrimSuffix(path.Clean(root), "/")
		if safeRepoPath(root) && (value == root || strings.HasPrefix(value, root+"/")) {
			return true
		}
	}
	return false
}
