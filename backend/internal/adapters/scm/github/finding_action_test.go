package github

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestFileDeferredIssueUsesPRRepository(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodPost, "/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["title"] != "review follow-up" || body["body"] != "details" {
			t.Fatalf("body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/o/r/issues/60"})
	})

	got, err := newProviderForTest(t, f).FileDeferredIssue(ctx(), ports.SCMDeferredIssueRequest{
		PRURL: "https://github.com/o/r/pull/1", Title: "review follow-up", Body: "details",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://github.com/o/r/issues/60" {
		t.Fatalf("result = %+v", got)
	}
}

func TestDeflectReviewThreadLinksIssueThenResolves(t *testing.T) {
	f := newFakeGH(t)
	calls := 0
	f.on(http.MethodPost, "/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if !strings.Contains(body.Query, "addPullRequestReviewThreadReply") || body.Variables["body"] != "Deferred as https://github.com/o/r/issues/60." {
				t.Fatalf("link mutation = %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"addPullRequestReviewThreadReply": map[string]any{"comment": map[string]any{"id": "PRRC_1"}}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"id": "PRRT_1", "isResolved": true}}}})
	})

	got, err := newProviderForTest(t, f).DeflectReviewThread(ctx(), "PRRT_1", "https://github.com/o/r/issues/60")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !got.Resolved {
		t.Fatalf("calls/result = %d/%+v", calls, got)
	}
}
