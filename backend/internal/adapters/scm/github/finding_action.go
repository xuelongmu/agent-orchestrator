package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.SCMIssueFiler = (*Provider)(nil)
var _ ports.SCMFindingThreadDeflector = (*Provider)(nil)
var _ ports.SCMReviewDismisser = (*Provider)(nil)

const (
	addReviewThreadReplyMutation = `mutation($threadID:ID!,$body:String!){
  addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$threadID,body:$body}){
    comment{id}
  }
}`
	reviewThreadBindingQuery = `query($threadID:ID!){
  node(id:$threadID){
    ... on PullRequestReviewThread{
      id path isResolved pullRequest{url}
      comments(first:100){nodes{id body pullRequestReview{databaseId}}}
    }
  }
}`
)

func findingMarker(actionKey string) string {
	return "<!-- ao-review-finding:" + actionKey + " -->"
}

// FileDeferredIssue ensures one backlog issue exists in the repository that
// owns the source PR. The stable action marker makes a crash after provider
// success but before the local receipt safely replayable.
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
	actionKey := strings.TrimSpace(request.ActionKey)
	if actionKey == "" {
		return ports.SCMDeferredIssue{}, fmt.Errorf("github scm: deferred issue action key is required")
	}
	marker := findingMarker(actionKey)
	for page := 1; ; page++ {
		listed, err := p.client.doREST(ctx, http.MethodGet, repoPath(owner, repo, "issues"), url.Values{
			"state": {"all"}, "per_page": {"100"}, "sort": {"created"}, "direction": {"desc"},
			"page": {strconv.Itoa(page)},
		}, nil)
		if err != nil {
			return ports.SCMDeferredIssue{}, fmt.Errorf("github scm: search deferred issue: %w", err)
		}
		var issues []struct {
			HTMLURL string `json:"html_url"`
			Body    string `json:"body"`
		}
		if err := json.Unmarshal(listed.Body, &issues); err != nil {
			return ports.SCMDeferredIssue{}, fmt.Errorf("github scm: decode deferred issue search: %w", err)
		}
		for _, issue := range issues {
			if strings.Contains(issue.Body, marker) && strings.TrimSpace(issue.HTMLURL) != "" {
				return ports.SCMDeferredIssue{URL: issue.HTMLURL}, nil
			}
		}
		if len(issues) < 100 {
			break
		}
	}
	body := strings.TrimSpace(request.Body)
	if body != "" {
		body += "\n\n"
	}
	body += marker
	resp, err := p.client.doREST(ctx, http.MethodPost, repoPath(owner, repo, "issues"), nil, map[string]string{
		"title": title,
		"body":  body,
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

// DismissReview clears a changes-requested review after every blocking finding
// has been deflected. Reading the current state first makes retry after an
// ambiguous provider response idempotent.
func (p *Provider) DismissReview(ctx context.Context, request ports.SCMReviewDismissalRequest) (ports.SCMReviewDismissal, error) {
	if p == nil || p.client == nil {
		return ports.SCMReviewDismissal{}, fmt.Errorf("github scm: review dismisser is not configured")
	}
	owner, repo, number, err := parsePRURL(request.PRURL)
	if err != nil {
		return ports.SCMReviewDismissal{}, err
	}
	reviewID := strings.TrimSpace(request.ReviewID)
	parsedID, err := strconv.ParseInt(reviewID, 10, 64)
	if err != nil || parsedID <= 0 {
		return ports.SCMReviewDismissal{}, fmt.Errorf("github scm: invalid review id")
	}
	path := repoPath(owner, repo, "pulls", strconv.Itoa(number), "reviews", reviewID)
	resp, err := p.client.doREST(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return ports.SCMReviewDismissal{}, fmt.Errorf("github scm: inspect review %s: %w", reviewID, err)
	}
	var review struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(resp.Body, &review); err != nil {
		return ports.SCMReviewDismissal{}, fmt.Errorf("github scm: decode review %s: %w", reviewID, err)
	}
	if review.ID != parsedID {
		return ports.SCMReviewDismissal{}, fmt.Errorf("github scm: review id was not confirmed")
	}
	if !strings.EqualFold(review.State, "changes_requested") {
		return ports.SCMReviewDismissal{Cleared: true}, nil
	}
	resp, err = p.client.doREST(ctx, http.MethodPut, path+"/dismissals", nil, map[string]string{"message": strings.TrimSpace(request.Message)})
	if err != nil {
		return ports.SCMReviewDismissal{}, fmt.Errorf("github scm: dismiss review %s: %w", reviewID, err)
	}
	if err := json.Unmarshal(resp.Body, &review); err != nil {
		return ports.SCMReviewDismissal{}, fmt.Errorf("github scm: decode dismissed review %s: %w", reviewID, err)
	}
	return ports.SCMReviewDismissal{Cleared: review.ID == parsedID && strings.EqualFold(review.State, "dismissed")}, nil
}

// ReviewThreadBound confirms that a provider thread belongs to the submitted
// review on the expected PR and, when supplied, the expected finding file.
func (p *Provider) ReviewThreadBound(ctx context.Context, binding ports.SCMReviewThreadBinding) (bool, error) {
	_, bound, err := p.boundReviewThread(ctx, binding)
	return bound, err
}

// DeflectReviewThread idempotently leaves the backlog link in the bound review
// conversation, then resolves the thread through the confirmed resolver.
func (p *Provider) DeflectReviewThread(ctx context.Context, binding ports.SCMReviewThreadBinding) (ports.SCMReviewThreadResolution, error) {
	if strings.TrimSpace(binding.IssueURL) == "" {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: deferred issue url is required")
	}
	if strings.TrimSpace(binding.ActionKey) == "" {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: thread action key is required")
	}
	node, bound, err := p.boundReviewThread(ctx, binding)
	if err != nil {
		return ports.SCMReviewThreadResolution{}, err
	}
	if !bound {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: review thread is not bound to the requested PR review")
	}
	marker := findingMarker(binding.ActionKey)
	replyID := ""
	comments, _ := node["comments"].(map[string]any)
	for _, comment := range nodes(comments["nodes"]) {
		commentBody := str(comment["body"])
		if strings.Contains(commentBody, marker) {
			if !strings.Contains(commentBody, "Deferred as "+strings.TrimSpace(binding.IssueURL)+".") {
				return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: deferred issue marker is already linked to a different issue")
			}
			replyID = str(comment["id"])
			break
		}
	}
	if replyID == "" {
		data, err := p.client.doGraphQL(ctx, addReviewThreadReplyMutation, map[string]any{
			"threadID": binding.ThreadID,
			"body":     "Deferred as " + strings.TrimSpace(binding.IssueURL) + ".\n\n" + marker,
		})
		if err != nil {
			return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: link deferred issue in review thread %q: %w", binding.ThreadID, err)
		}
		payload, _ := data["addPullRequestReviewThreadReply"].(map[string]any)
		comment, _ := payload["comment"].(map[string]any)
		replyID = strings.TrimSpace(str(comment["id"]))
		if replyID == "" {
			return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: deferred issue thread reply was not confirmed")
		}
	}
	resolved, err := p.ResolveReviewThread(ctx, binding.ThreadID)
	if err != nil {
		return ports.SCMReviewThreadResolution{}, err
	}
	resolved.ReplyID = replyID
	return resolved, nil
}

func (p *Provider) boundReviewThread(ctx context.Context, binding ports.SCMReviewThreadBinding) (map[string]any, bool, error) {
	if p == nil || p.client == nil {
		return nil, false, fmt.Errorf("github scm: finding deflector is not configured")
	}
	if strings.TrimSpace(binding.ThreadID) != binding.ThreadID || !reviewThreadIDPattern.MatchString(binding.ThreadID) {
		return nil, false, fmt.Errorf("github scm: invalid review thread id")
	}
	if strings.TrimSpace(binding.PRURL) == "" || strings.TrimSpace(binding.ReviewID) == "" || strings.TrimSpace(binding.File) == "" || strings.TrimSpace(binding.Body) == "" {
		return nil, false, nil
	}
	data, err := p.client.doGraphQL(ctx, reviewThreadBindingQuery, map[string]any{"threadID": binding.ThreadID})
	if err != nil {
		return nil, false, fmt.Errorf("github scm: inspect review thread %q: %w", binding.ThreadID, err)
	}
	node, _ := data["node"].(map[string]any)
	pr, _ := node["pullRequest"].(map[string]any)
	if str(node["id"]) != binding.ThreadID || str(pr["url"]) != binding.PRURL {
		return node, false, nil
	}
	if str(node["path"]) != strings.TrimSpace(binding.File) {
		return node, false, nil
	}
	comments, _ := node["comments"].(map[string]any)
	for _, comment := range nodes(comments["nodes"]) {
		review, _ := comment["pullRequestReview"].(map[string]any)
		if strconv.FormatInt(int64(num(review["databaseId"])), 10) == binding.ReviewID && strings.TrimSpace(str(comment["body"])) == strings.TrimSpace(binding.Body) {
			return node, true, nil
		}
	}
	return node, false, nil
}
