// Command beta-canary-cleanup performs narrowly bound, replay-safe cleanup of
// one completed canary publication. It has no merge or deployment authority.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/artifact"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/jackc/pgx/v5/pgxpool"
)

type cleanupReport struct {
	Status            string `json:"status"`
	PublicationID     string `json:"publication_id"`
	Branch            string `json:"branch"`
	PullRequestURL    string `json:"pull_request_url"`
	BranchReplay      bool   `json:"branch_replay"`
	PullRequestReplay bool   `json:"pull_request_replay"`
}

func main() { os.Exit(mainExit()) }

func mainExit() int {
	configPath := flag.String("config", "", "absolute path to strict canary configuration")
	publicationID := flag.String("publication", "", "completed immutable publication ID")
	flag.Parse()
	value, err := beta.LoadConfig(*configPath)
	if err == nil {
		err = value.ValidateCanary()
	}
	if err != nil || strings.TrimSpace(*publicationID) == "" {
		fmt.Fprintln(os.Stderr, "invalid canary cleanup configuration")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := cleanup(ctx, value, *publicationID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary cleanup failed:", err)
		return 1
	}
	if err := writeCleanupReport(os.Stdout, report); err != nil {
		fmt.Fprintln(os.Stderr, "write canary cleanup report:", err)
		return 1
	}
	return 0
}

func writeCleanupReport(writer io.Writer, report cleanupReport) error {
	return json.NewEncoder(writer).Encode(report)
}

func cleanup(ctx context.Context, config beta.Config, publicationID string) (cleanupReport, error) {
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return cleanupReport{}, err
	}
	defer pool.Close()
	ledger, err := publication.NewPostgresPublicationRepository(pool)
	if err != nil {
		return cleanupReport{}, err
	}
	record, err := ledger.LoadCompleted(ctx, publicationID)
	if err != nil {
		return cleanupReport{}, err
	}
	store, err := artifact.NewStore(config.ArtifactRoot, publication.DefaultMaxArtifactBytes)
	if err != nil {
		return cleanupReport{}, err
	}
	defer store.Close()
	body, err := store.Get(ctx, record.PublicationArtifact)
	if err != nil {
		return cleanupReport{}, err
	}
	artifactValue, err := decodeArtifact(body)
	if err != nil || !matchesCanaryRecord(config, record, artifactValue) {
		return cleanupReport{}, publication.ErrPublicationConflict
	}
	git, err := publication.NewAuthenticatedGitCommandPublisher(
		config.Policy.Repository.Root, config.Policy.Repository.Remote,
		config.Policy.Repository.BaseBranch, config.GitPushCredentialFile,
	)
	if err != nil {
		return cleanupReport{}, err
	}
	credential, err := publication.NewFileCredential(config.PublicationCredentialFile)
	if err != nil {
		return cleanupReport{}, err
	}
	github, err := publication.NewGitHubRESTClient(
		"https://api.github.com", "2022-11-28", publication.DefaultMaxBodyBytes,
		publication.DefaultTimeout, credential,
	)
	if err != nil {
		return cleanupReport{}, err
	}
	expected := publication.PullRequestExpectation{
		Owner: beta.CanaryOwner, Repo: beta.CanaryRepository,
		Number: record.PullRequestNumber, URL: record.PullRequestURL,
		Marker: "<!-- harness-publication-id:" + record.PublicationID + " -->",
		Base:   record.BaseBranch, Head: record.HeadBranch,
		BaseCommit: record.BaseCommit, CandidateCommit: record.CandidateCommit,
	}
	branchReplay, pullReplay, err := cleanupResources(ctx, git, github, expected)
	if err != nil {
		return cleanupReport{}, err
	}
	return cleanupReport{Status: "cleaned", PublicationID: publicationID,
		Branch: record.HeadBranch, PullRequestURL: record.PullRequestURL,
		BranchReplay: branchReplay, PullRequestReplay: pullReplay}, nil
}

func cleanupResources(
	ctx context.Context, git *publication.GitCommandPublisher,
	github *publication.GitHubRESTClient, expected publication.PullRequestExpectation,
) (bool, bool, error) {
	pullReplay, err := github.CloseDraft(ctx, expected)
	if err != nil {
		return false, false, err
	}
	branchReplay, err := git.DeleteBranch(ctx, expected.Head, expected.CandidateCommit)
	if err != nil {
		return false, pullReplay, err
	}
	return branchReplay, pullReplay, nil
}

func decodeArtifact(body []byte) (publication.DraftPullRequestArtifact, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value publication.DraftPullRequestArtifact
	if err := decoder.Decode(&value); err != nil {
		return publication.DraftPullRequestArtifact{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return publication.DraftPullRequestArtifact{}, errors.New("publication artifact has trailing content")
	}
	return value, nil
}

func matchesCanaryRecord(
	config beta.Config, record publication.CompletedPublication,
	value publication.DraftPullRequestArtifact,
) bool {
	repository := beta.CanaryOwner + "/" + beta.CanaryRepository
	return record.Repository == repository && record.PublicationID == value.PublicationID &&
		record.BaseBranch == config.Policy.Repository.BaseBranch &&
		record.BaseCommit == config.Policy.Repository.BaseCommit &&
		strings.HasPrefix(record.HeadBranch, "harness/canary/") &&
		record.HeadBranch == value.HeadBranch && record.BaseBranch == value.BaseBranch &&
		record.BaseCommit == value.BaseCommit && record.CandidateCommit == value.CandidateCommit &&
		record.PullRequestNumber == value.PullRequestNumber &&
		record.PullRequestURL == value.PullRequestURL && value.Draft &&
		value.RepositoryOwner == beta.CanaryOwner && value.RepositoryName == beta.CanaryRepository
}
