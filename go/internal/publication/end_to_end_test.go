package publication

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestApprovedCandidatePublishesToBareRemoteAndHTTPDraftAPI(t *testing.T) {
	root, remote, base, candidate := gitFixture(t)
	fixture, request, _, _, workflowPort := publicationFixtureForCommits(t, base, candidate)
	var (
		mu      sync.Mutex
		created *githubPullRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch incoming.Method {
		case http.MethodGet:
			if created == nil {
				_, _ = writer.Write([]byte("[]"))
				return
			}
			_ = json.NewEncoder(writer).Encode([]githubPullRequest{*created})
		case http.MethodPost:
			var payload struct {
				Title string `json:"title"`
				Head  string `json:"head"`
				Base  string `json:"base"`
				Body  string `json:"body"`
				Draft bool   `json:"draft"`
			}
			if err := json.NewDecoder(incoming.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			value := testPullRequest(payload.Head, payload.Base, payload.Body)
			created = &value
			_ = json.NewEncoder(writer).Encode(value)
		default:
			http.Error(writer, "unsupported", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	gitPublisher, err := NewGitCommandPublisher(root, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewGitHubRESTClient(
		server.URL, "2022-11-28", DefaultMaxBodyBytes,
		DefaultTimeout, staticCredential("ephemeral-test-token"),
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		fixture.config, fixture.artifacts, workflowPort, gitPublisher, client,
		NewMemoryPublicationLedger(),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Publish(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, remote, "rev-parse", "refs/heads/"+result.Branch); got != candidate {
		t.Fatalf("remote branch = %s", got)
	}
	if created == nil || !created.Draft || created.State != "open" ||
		result.PullRequestNumber != created.Number {
		t.Fatalf("created=%#v result=%#v", created, result)
	}
	replay, err := service.Publish(t.Context(), request)
	if err != nil || !replay.Replay {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
}
