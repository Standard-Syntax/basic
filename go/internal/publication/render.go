package publication

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/verification"
)

func renderPullRequest(
	config Config, request Request, inputs validatedInputs,
) (DraftPullRequestInput, error) {
	title := specificationTitle(inputs.specification)
	if title == "" {
		title = "Approved candidate " + request.CandidateCommit[:12]
	}
	title = boundedLine(title, 240)
	marker := "<!-- harness-publication-id:" + request.PublicationID + " -->"
	var body strings.Builder
	fmt.Fprintf(&body, "%s\n\n## Approved publication\n\n", marker)
	fmt.Fprintf(&body, "- Candidate: `%s`\n- Reviewed base: `%s`\n", request.CandidateCommit, request.BaseCommit)
	fmt.Fprintf(&body, "- Run: `%s`\n- Approval: `%s`\n\n", request.RunID, inputs.approval.ApprovalID)
	body.WriteString("## Independent verification\n\n")
	checks := append([]verification.CheckResult(nil), inputs.verification.Checks...)
	sort.Slice(checks, func(i, j int) bool { return checks[i].CheckID < checks[j].CheckID })
	for _, check := range checks {
		fmt.Fprintf(&body, "- `%s`: passed\n", boundedLine(check.CheckID, 200))
	}
	body.WriteString("\n## Advisory review\n\n")
	fmt.Fprintf(&body, "- Recommendation: `%s`\n", inputs.review.Recommendation)
	if len(inputs.review.Findings) == 0 {
		body.WriteString("- Findings: none\n")
	} else {
		findings := append([]*reasoningv1.ReviewFinding(nil), inputs.review.Findings...)
		sort.Slice(findings, func(i, j int) bool {
			return findings[i].GetFindingId() < findings[j].GetFindingId()
		})
		for _, finding := range findings {
			fmt.Fprintf(
				&body, "- `%s` (%s): %s\n",
				boundedLine(finding.GetFindingId(), 100),
				boundedLine(finding.GetSeverity().String(), 80),
				boundedLine(finding.GetSummary(), 500),
			)
		}
	}
	body.WriteString("\n## Immutable evidence\n\n")
	for _, item := range []struct {
		name   string
		digest string
	}{
		{"specification", request.Specification.Digest},
		{"implementation", request.Implementation.Digest},
		{"execution", request.Execution.Digest},
		{"verification", request.Verification.Digest},
		{"review", request.Review.Digest},
		{"approval", request.Approval.Digest},
	} {
		fmt.Fprintf(&body, "- %s: `%s`\n", item.name, item.digest)
	}
	if int64(body.Len()) > config.MaxBodyBytes {
		return DraftPullRequestInput{}, ErrResponseLimit
	}
	return DraftPullRequestInput{
		Owner: config.RepositoryOwner, Repo: config.RepositoryName,
		Head: config.BranchPrefix + request.RunID, Base: config.BaseBranch,
		Title: title, Body: body.String(), Marker: marker,
	}, nil
}

func specificationTitle(encoded []byte) string {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &value); err != nil {
		return ""
	}
	for _, key := range []string{"title", "objective", "summary", "name"} {
		var text string
		if body, ok := value[key]; ok && json.Unmarshal(body, &text) == nil &&
			strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func boundedLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit-1] + "…"
}
