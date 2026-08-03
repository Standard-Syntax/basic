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
	"regexp"
	"strconv"
	"strings"
	"time"
)

const githubAccept = "application/vnd.github+json"

var ownerSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
var repositorySegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

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
	path, err := c.pullPath(input)
	if err != nil {
		return DraftPullRequest{}, false, err
	}
	var response []githubPullRequest
	if err := c.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &response); err != nil {
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
	path, err := c.pullPath(input)
	if err != nil {
		return DraftPullRequest{}, err
	}
	if createErr := c.do(ctx, http.MethodPost, path, payload, &response); createErr != nil {
		recovered, exists, findErr := c.FindDraft(ctx, input)
		if findErr != nil {
			return DraftPullRequest{}, errors.Join(createErr, findErr)
		}
		if exists {
			return recovered, nil
		}
		return DraftPullRequest{}, createErr
	}
	if response.Head.Ref != input.Head || response.Base.Ref != input.Base ||
		response.State != "open" || !response.Draft ||
		!strings.Contains(response.Body, input.Marker) {
		return DraftPullRequest{}, ErrPullRequestConflict
	}
	return mapPullRequest(response, input.Marker), nil
}

// InspectPullRequest reads exactly one pull request by immutable repository and
// number. It is intentionally a concrete-client operation, not runtime
// PullRequestClient authority.
func (c *GitHubRESTClient) InspectPullRequest(
	ctx context.Context, owner, repo string, number int64,
) (DraftPullRequest, error) {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" || number <= 0 {
		return DraftPullRequest{}, ErrInvalidRequest
	}
	var response githubPullRequest
	path, err := exactPullPath(owner, repo, number)
	if err != nil {
		return DraftPullRequest{}, err
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return DraftPullRequest{}, err
	}
	return mapPullRequest(response, ""), nil
}

// CloseDraft closes only the exact open draft bound to the expected marker,
// refs, commits, and URL. A matching already-closed draft is replay success.
func (c *GitHubRESTClient) CloseDraft(
	ctx context.Context, expected PullRequestExpectation,
) (bool, error) {
	if err := validatePullRequestExpectation(expected); err != nil {
		return false, err
	}
	var current githubPullRequest
	path, err := exactPullPath(expected.Owner, expected.Repo, expected.Number)
	if err != nil {
		return false, err
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &current); err != nil {
		return false, err
	}
	if !matchesExpectedPullRequest(current, expected) {
		return false, ErrPullRequestConflict
	}
	if current.State == "closed" {
		return true, nil
	}
	if current.State != "open" {
		return false, ErrPullRequestConflict
	}
	var closed githubPullRequest
	if err := c.do(ctx, http.MethodPatch, path, struct {
		State string `json:"state"`
	}{State: "closed"}, &closed); err != nil {
		return false, err
	}
	if closed.State != "closed" || !matchesExpectedPullRequest(closed, expected) {
		return false, ErrPullRequestConflict
	}
	return false, nil
}

type githubPullRequest struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	Body    string `json:"body"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

func validatePullRequestExpectation(value PullRequestExpectation) error {
	if value.Owner == "" || value.Repo == "" || value.Number <= 0 || value.URL == "" ||
		value.Marker == "" || !branchPattern.MatchString(value.Base) ||
		!branchPattern.MatchString(value.Head) || !commitPattern.MatchString(value.BaseCommit) ||
		!commitPattern.MatchString(value.CandidateCommit) {
		return ErrInvalidRequest
	}
	return nil
}

func matchesExpectedPullRequest(value githubPullRequest, expected PullRequestExpectation) bool {
	return value.Number == expected.Number && value.HTMLURL == expected.URL && value.Draft &&
		strings.Contains(value.Body, expected.Marker) && value.Base.Ref == expected.Base &&
		value.Head.Ref == expected.Head && value.Base.SHA == expected.BaseCommit &&
		value.Head.SHA == expected.CandidateCommit &&
		(value.State == "open" || value.State == "closed")
}

func exactPullPath(owner, repo string, number int64) (string, error) {
	if !validOwnerSegment(owner) || !validRepositorySegment(repo) || number <= 0 {
		return "", ErrInvalidRequest
	}
	return url.JoinPath("/repos", owner, repo, "pulls", strconv.FormatInt(number, 10))
}

func (c *GitHubRESTClient) do(
	ctx context.Context, method, path string, requestBody, responseBody any,
) error {
	request, err := c.newRequest(ctx, method, path, requestBody)
	if err != nil {
		return err
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
	return decodeGitHubResponse(encoded, responseBody)
}

func (c *GitHubRESTClient) newRequest(
	ctx context.Context, method, path string, requestBody any,
) (*http.Request, error) {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return nil, fmt.Errorf("encode GitHub request: %w", err)
		}
		if int64(len(encoded)) > c.maxBodyBytes {
			return nil, ErrResponseLimit
		}
		body = bytes.NewReader(encoded)
	}
	relative, err := url.Parse(path)
	if err != nil || relative.IsAbs() || relative.Host != "" {
		return nil, fmt.Errorf("construct GitHub request path: %w", ErrInvalidRequest)
	}
	target := c.endpoint.JoinPath(strings.TrimPrefix(relative.Path, "/"))
	target.RawQuery = relative.RawQuery
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("construct GitHub request: %w", err)
	}
	token, err := c.credentials.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("load GitHub credential: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("GitHub credential is empty")
	}
	request.Header.Set("Accept", githubAccept)
	request.Header.Set("X-GitHub-Api-Version", c.apiVersion)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", "harness-publication/1")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func decodeGitHubResponse(encoded []byte, responseBody any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(responseBody); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode GitHub response: unexpected JSON trailer")
	}
	return nil
}

func (c *GitHubRESTClient) pullPath(input DraftPullRequestInput) (string, error) {
	if !validOwnerSegment(input.Owner) || !validRepositorySegment(input.Repo) {
		return "", ErrInvalidRequest
	}
	return url.JoinPath("/repos", input.Owner, input.Repo, "pulls")
}

func validOwnerSegment(value string) bool {
	return ownerSegmentPattern.MatchString(value)
}

func validRepositorySegment(value string) bool {
	return value != "." && value != ".." && repositorySegmentPattern.MatchString(value)
}

func mapPullRequest(value githubPullRequest, marker string) DraftPullRequest {
	return DraftPullRequest{
		Number: value.Number, URL: value.HTMLURL, State: value.State,
		Draft: value.Draft, Head: value.Head.Ref, Base: value.Base.Ref,
		HeadCommit: value.Head.SHA, BaseCommit: value.Base.SHA, Body: value.Body, Marker: marker,
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
