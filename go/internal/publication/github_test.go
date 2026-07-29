package publication

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type staticCredential string

func (s staticCredential) Token(context.Context) (string, error) { return string(s), nil }

func TestGitHubClientListsBeforeCreatingDraftWithRequiredHeaders(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer secret" ||
			request.Header.Get("Accept") != githubAccept ||
			request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("missing required headers: %#v", request.Header)
		}
		switch request.Method {
		case http.MethodGet:
			if request.URL.Query().Get("head") != "owner:harness/run" ||
				request.URL.Query().Get("base") != "main" ||
				request.URL.Query().Get("state") != "all" {
				t.Errorf("lookup query = %s", request.URL.RawQuery)
			}
			_, _ = writer.Write([]byte("[]"))
		case http.MethodPost:
			var payload struct {
				Title string `json:"title"`
				Head  string `json:"head"`
				Base  string `json:"base"`
				Body  string `json:"body"`
				Draft bool   `json:"draft"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if !payload.Draft || payload.Head != "harness/run" ||
				!strings.Contains(payload.Body, "publication-id") {
				t.Errorf("create payload = %#v", payload)
			}
			writePullRequest(writer, payload.Head, payload.Base, payload.Body, true, "open")
		}
	}))
	defer server.Close()
	client := githubClient(t, server.URL, DefaultMaxBodyBytes)
	input := prInput()
	if _, exists, err := client.FindDraft(t.Context(), input); err != nil || exists {
		t.Fatalf("find exists=%v err=%v", exists, err)
	}
	pr, err := client.CreateDraft(t.Context(), input)
	if err != nil || pr.Number != 17 || !pr.Draft {
		t.Fatalf("created=%#v err=%v", pr, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestGitHubClientRecoversExistingDraftAfterCreateConflict(t *testing.T) {
	input := prInput()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch call := calls.Add(1); call {
		case 1:
			if request.Method != http.MethodPost {
				t.Errorf("first method = %s", request.Method)
			}
			http.Error(writer, "pull request already exists", http.StatusUnprocessableEntity)
		case 2:
			if request.Method != http.MethodGet {
				t.Errorf("second method = %s", request.Method)
			}
			if request.URL.Query().Get("head") != input.Owner+":"+input.Head ||
				request.URL.Query().Get("base") != input.Base ||
				request.URL.Query().Get("state") != "all" {
				t.Errorf("lookup query = %s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode([]githubPullRequest{
				testPullRequest(input.Head, input.Base, input.Body),
			})
		default:
			t.Errorf("unexpected request %d", call)
		}
	}))
	defer server.Close()

	pr, err := githubClient(t, server.URL, DefaultMaxBodyBytes).
		CreateDraft(t.Context(), input)
	if err != nil || pr.Number != 17 || !pr.Draft {
		t.Fatalf("recovered=%#v err=%v", pr, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestGitHubClientRecoversMatchingDraftAndRejectsMismatches(t *testing.T) {
	for name, mutate := range map[string]func(*githubPullRequest){
		"matching": func(*githubPullRequest) {},
		"closed": func(value *githubPullRequest) {
			value.State = "closed"
		},
		"not draft": func(value *githubPullRequest) {
			value.Draft = false
		},
		"marker": func(value *githubPullRequest) {
			value.Body = "human PR"
		},
		"head": func(value *githubPullRequest) {
			value.Head.Ref = "other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := prInput()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				value := testPullRequest(input.Head, input.Base, input.Body)
				mutate(&value)
				_ = json.NewEncoder(writer).Encode([]githubPullRequest{value})
			}))
			defer server.Close()
			pr, exists, err := githubClient(t, server.URL, DefaultMaxBodyBytes).
				FindDraft(t.Context(), input)
			if wantConflict := name != "matching"; wantConflict {
				if !errors.Is(err, ErrPullRequestConflict) || exists {
					t.Fatalf("pr=%#v exists=%v err=%v", pr, exists, err)
				}
			} else if err != nil || !exists || pr.Number != 17 {
				t.Fatalf("pr=%#v exists=%v err=%v", pr, exists, err)
			}
		})
	}
}

func TestGitHubClientBoundsResponsesRejectsRedirectsAndMalformedJSON(t *testing.T) {
	tests := map[string]struct {
		handler http.Handler
		limit   int64
	}{
		"bounded": {http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(strings.Repeat("x", 100)))
		}), 10},
		"malformed": {http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte("{"))
		}), 100},
		"redirect": {http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, "/other", http.StatusFound)
		}), 100},
		"rate": {http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "rate limited", http.StatusTooManyRequests)
		}), 100},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			_, _, err := githubClient(t, server.URL, test.limit).
				FindDraft(t.Context(), prInput())
			if err == nil {
				t.Fatal("invalid response succeeded")
			}
		})
	}
}

func TestGitHubClientRejectsInsecureNonLoopbackEndpoint(t *testing.T) {
	if _, err := NewGitHubRESTClient(
		"http://example.com", "2022-11-28", 100, time.Second, staticCredential("x"),
	); err == nil {
		t.Fatal("insecure endpoint accepted")
	}
}

func githubClient(t *testing.T, endpoint string, limit int64) *GitHubRESTClient {
	t.Helper()
	client, err := NewGitHubRESTClient(
		endpoint, "2022-11-28", limit, time.Second, staticCredential("secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func prInput() DraftPullRequestInput {
	marker := "<!-- harness-publication-id:00000000-0000-4000-8000-000000000001 -->"
	return DraftPullRequestInput{
		Owner: "owner", Repo: "repo", Head: "harness/run", Base: "main",
		Title: "Approved candidate", Body: marker + "\nbody", Marker: marker,
	}
}

func testPullRequest(head, base, body string) githubPullRequest {
	value := githubPullRequest{
		Number: 17, HTMLURL: "https://example.invalid/pull/17",
		State: "open", Draft: true, Body: body,
	}
	value.Head.Ref, value.Base.Ref = head, base
	return value
}

func writePullRequest(
	writer http.ResponseWriter, head, base, body string, draft bool, state string,
) {
	value := testPullRequest(head, base, body)
	value.Draft, value.State = draft, state
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}
