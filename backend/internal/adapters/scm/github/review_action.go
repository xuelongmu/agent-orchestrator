package github

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var reviewThreadIDPattern = regexp.MustCompile(`^[A-Za-z0-9_./+:=-]{1,256}$`)

var _ ports.SCMReviewThreadResolver = (*Provider)(nil)

const resolveReviewThreadMutation = `mutation($threadID:ID!){
  resolveReviewThread(input:{threadId:$threadID}){
    thread{id isResolved}
  }
}`

// ResolveReviewThread marks one GitHub pull-request review thread resolved.
// GitHub's mutation is idempotent; AO still requires the response to confirm
// both the requested node ID and isResolved=true before reporting success.
func (p *Provider) ResolveReviewThread(ctx context.Context, threadID string) (ports.SCMReviewThreadResolution, error) {
	if p == nil || p.client == nil {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: review-thread resolver is not configured")
	}
	if strings.TrimSpace(threadID) != threadID || !reviewThreadIDPattern.MatchString(threadID) {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: invalid review thread id")
	}
	data, err := p.client.doGraphQL(ctx, resolveReviewThreadMutation, map[string]any{"threadID": threadID})
	if err != nil {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: resolve review thread %q: %w", threadID, err)
	}
	payload, _ := data["resolveReviewThread"].(map[string]any)
	thread, _ := payload["thread"].(map[string]any)
	resolvedID := strings.TrimSpace(str(thread["id"]))
	resolved := boolv(thread["isResolved"])
	if resolvedID != threadID || !resolved {
		return ports.SCMReviewThreadResolution{}, fmt.Errorf("github scm: resolve review thread %q was not confirmed", threadID)
	}
	return ports.SCMReviewThreadResolution{ThreadID: resolvedID, Resolved: true}, nil
}
