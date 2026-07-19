package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.SCMIssueFiler = (*Provider)(nil)
var _ ports.SCMFindingThreadDeflector = (*Provider)(nil)

const addReviewThreadReplyMutation = `mutation($threadID:ID!,$body:String!){
  addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$threadID,body:$body}){
    comment{id}
  }
}`

// FileDeferredIssue creates one backlog issue in the repository that owns the
// source PR and returns only provider-confirmed state.
func (p *Provider) FileDeferredIssue(ctx context.Context, request ports.SCMDeferredIssueRequest) (ports.SCMDeferredIssue, error) {
	if p == nil || p.client == nil {
		return ports.SCMDeferredIssue{}, fmt.Errorf("github scm: issue filer is not configured")
	}
	owner, repo, _, err := parsePRURL(request.PRURL)
	if err != nil {
		return ports.SCMDeferredIssue{}, err
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		return ports.SCMDeferredIssue{}, fmt.Errorf("github scm: deferred issue title is required")
	}
	resp, err := p.client.doREST(ctx, http.MethodPost, repoPath(owner, repo, "issues"), nil, map[string]string{
		"title": title,
		"body":  request.Body,
	})
	if err != nil {
		return ports.SCMDeferredIssue{}, fmt.Errorf("github scm: file deferred issue: %w", err)
	}
	var result struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return ports.SCMDeferredIssue{}, fmt.Errorf("github scm: decode deferred issue: %w", err)
	}
	if strings.TrimSpace(result.HTMLURL) == "" {
		return ports.SCMDeferredIssue{}, fmt.Errorf("github scm: deferred issue response has no url")
	}
	return ports.SCMDeferredIssue{URL: result.HTMLURL}, nil
}

// DeflectReviewThread leaves the backlog link in the review conversation,
// then resolves the thread through the existing confirmed resolver.
func (p *Provider) DeflectReviewThread(ctx context.Context, threadID, issueURL string) (ports.SCMReviewThreadResolution, error) {
	if p == nil || p.client == nil {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: finding deflector is not configured")
	}
	if strings.TrimSpace(threadID) != threadID || !reviewThreadIDPattern.MatchString(threadID) {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: invalid review thread id")
	}
	issueURL = strings.TrimSpace(issueURL)
	if issueURL == "" {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: deferred issue url is required")
	}
	data, err := p.client.doGraphQL(ctx, addReviewThreadReplyMutation, map[string]any{
		"threadID": threadID, "body": "Deferred as " + issueURL + ".",
	})
	if err != nil {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: link deferred issue in review thread %q: %w", threadID, err)
	}
	payload, _ := data["addPullRequestReviewThreadReply"].(map[string]any)
	comment, _ := payload["comment"].(map[string]any)
	if strings.TrimSpace(str(comment["id"])) == "" {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: deferred issue thread reply was not confirmed")
	}
	return p.ResolveReviewThread(ctx, threadID)
}
