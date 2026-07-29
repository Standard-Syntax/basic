package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const githubAccept = "application/vnd.github+json"

type GitHubRESTClient struct {
	endpoint     *url.URL
	apiVersion   string
	maxBodyBytes int64
	client       *http.Client
	credentials  CredentialSource
}

func NewGitHubRESTClient(
	endpoint, apiVersion string,
	maxBodyBytes int64,
	timeout time.Duration,
	credentials CredentialSource,
) (*GitHubRESTClient, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && !isLoopbackHTTP(parsed)) ||
		strings.TrimSpace(apiVersion) == "" || maxBodyBytes <= 0 || timeout <= 0 ||
		credentials == nil {
		return nil, fmt.Errorf("%w: GitHub client configuration", ErrInvalidRequest)
	}
	return &GitHubRESTClient{
		endpoint: parsed, apiVersion: apiVersion, maxBodyBytes: maxBodyBytes,
		credentials: credentials,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("redirects are disabled")
			},
		},
	}, nil
}

func (c *GitHubRESTClient) FindDraft(
	ctx context.Context, input DraftPullRequestInput,
) (DraftPullRequest, bool, error) {
	query := url.Values{
		"state":    {"all"},
		"head":     {input.Owner + ":" + input.Head},
		"base":     {input.Base},
		"per_page": {"10"},
	}
	var response []githubPullRequest
	if err := c.do(ctx, http.MethodGet, c.pullPath(input)+"?"+query.Encode(), nil, &response); err != nil {
		return DraftPullRequest{}, false, err
	}
	if len(response) == 0 {
		return DraftPullRequest{}, false, nil
	}
	if len(response) != 1 {
		return DraftPullRequest{}, false, ErrPullRequestConflict
	}
	pr := response[0]
	if pr.Head.Ref != input.Head || pr.Base.Ref != input.Base ||
		pr.State != "open" || !pr.Draft || !strings.Contains(pr.Body, input.Marker) {
		return DraftPullRequest{}, false, ErrPullRequestConflict
	}
	return mapPullRequest(pr, input.Marker), true, nil
}

func (c *GitHubRESTClient) CreateDraft(
	ctx context.Context, input DraftPullRequestInput,
) (DraftPullRequest, error) {
	payload := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}{
		Title: input.Title, Head: input.Head, Base: input.Base,
		Body: input.Body, Draft: true,
	}
	var response githubPullRequest
	if err := c.do(ctx, http.MethodPost, c.pullPath(input), payload, &response); err != nil {
		return DraftPullRequest{}, err
	}
	if response.Head.Ref != input.Head || response.Base.Ref != input.Base ||
		response.State != "open" || !response.Draft ||
		!strings.Contains(response.Body, input.Marker) {
		return DraftPullRequest{}, ErrPullRequestConflict
	}
	return mapPullRequest(response, input.Marker), nil
}

type githubPullRequest struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	Body    string `json:"body"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (c *GitHubRESTClient) do(
	ctx context.Context, method, path string, requestBody, responseBody any,
) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode GitHub request: %w", err)
		}
		if int64(len(encoded)) > c.maxBodyBytes {
			return ErrResponseLimit
		}
		body = bytes.NewReader(encoded)
	}
	target := c.endpoint.ResolveReference(&url.URL{Path: path})
	if strings.Contains(path, "?") {
		parts := strings.SplitN(path, "?", 2)
		target = c.endpoint.ResolveReference(&url.URL{Path: parts[0], RawQuery: parts[1]})
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return fmt.Errorf("construct GitHub request: %w", err)
	}
	token, err := c.credentials.Token(ctx)
	if err != nil {
		return fmt.Errorf("load GitHub credential: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("GitHub credential is empty")
	}
	request.Header.Set("Accept", githubAccept)
	request.Header.Set("X-GitHub-Api-Version", c.apiVersion)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", "harness-publication/1")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("GitHub request failed: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, c.maxBodyBytes+1)
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read GitHub response: %w", err)
	}
	if int64(len(encoded)) > c.maxBodyBytes {
		return ErrResponseLimit
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("GitHub API status %s", strconv.Itoa(response.StatusCode))
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(responseBody); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode GitHub response: unexpected JSON trailer")
	}
	return nil
}

func (c *GitHubRESTClient) pullPath(input DraftPullRequestInput) string {
	return "/repos/" + url.PathEscape(input.Owner) + "/" + url.PathEscape(input.Repo) + "/pulls"
}

func mapPullRequest(value githubPullRequest, marker string) DraftPullRequest {
	return DraftPullRequest{
		Number: value.Number, URL: value.HTMLURL, State: value.State,
		Draft: value.Draft, Head: value.Head.Ref, Base: value.Base.Ref, Marker: marker,
	}
}

func isLoopbackHTTP(value *url.URL) bool {
	if value.Scheme != "http" {
		return false
	}
	host := value.Hostname()
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}
