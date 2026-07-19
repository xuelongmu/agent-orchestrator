package github

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestFileDeferredIssueUsesMarkerAndPRRepository(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodGet, "/repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{})
	})
	f.on(http.MethodPost, "/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["title"] != "review follow-up" || !strings.Contains(body["body"], "details") || !strings.Contains(body["body"], findingMarker("run-1:1")) {
			t.Fatalf("body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/o/r/issues/60"})
	})

	got, err := newProviderForTest(t, f).FileDeferredIssue(ctx(), ports.SCMDeferredIssueRequest{
		PRURL: "https://github.com/o/r/pull/1", Title: "review follow-up", Body: "details", ActionKey: "run-1:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://github.com/o/r/issues/60" {
		t.Fatalf("result = %+v", got)
	}
}

func TestFileDeferredIssueReusesMarkedIssue(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodGet, "/repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{
			"html_url": "https://github.com/o/r/issues/60", "body": "existing\n" + findingMarker("run-1:1"),
		}})
	})
	got, err := newProviderForTest(t, f).FileDeferredIssue(ctx(), ports.SCMDeferredIssueRequest{
		PRURL: "https://github.com/o/r/pull/1", Title: "review follow-up", ActionKey: "run-1:1",
	})
	if err != nil || got.URL != "https://github.com/o/r/issues/60" {
		t.Fatalf("result = %+v, %v", got, err)
	}
}

func TestDismissReviewClearsChangesRequestedAndReplaysDismissed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   string
		wantPut bool
	}{
		{name: "changes requested", state: "CHANGES_REQUESTED", wantPut: true},
		{name: "already dismissed", state: "DISMISSED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGH(t)
			f.on(http.MethodGet, "/repos/o/r/pulls/1/reviews/123", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "state": tc.state})
			})
			puts := 0
			if tc.wantPut {
				f.on(http.MethodPut, "/repos/o/r/pulls/1/reviews/123/dismissals", func(w http.ResponseWriter, r *http.Request) {
					puts++
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["message"] == "" {
						t.Fatalf("dismiss body = %+v, %v", body, err)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "state": "DISMISSED"})
				})
			}
			got, err := newProviderForTest(t, f).DismissReview(ctx(), ports.SCMReviewDismissalRequest{
				PRURL: "https://github.com/o/r/pull/1", ReviewID: "123", Message: "deferred",
			})
			if err != nil || !got.Cleared || puts != boolCount(tc.wantPut) {
				t.Fatalf("result/puts/err = %+v/%d/%v", got, puts, err)
			}
		})
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestDeflectReviewThreadBindsLinksThenResolves(t *testing.T) {
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
		switch calls {
		case 1:
			if !strings.Contains(body.Query, "PullRequestReviewThread") {
				t.Fatalf("binding query = %s", body.Query)
			}
			writeBoundThread(t, w, nil)
		case 2:
			if !strings.Contains(body.Query, "addPullRequestReviewThreadReply") || !strings.Contains(body.Variables["body"].(string), findingMarker("run-1:1")) {
				t.Fatalf("link mutation = %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"addPullRequestReviewThreadReply": map[string]any{"comment": map[string]any{"id": "PRRC_1"}}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"id": "PRRT_1", "isResolved": true}}}})
		}
	})

	binding := ports.SCMReviewThreadBinding{PRURL: "https://github.com/o/r/pull/1", ReviewID: "123", ThreadID: "PRRT_1", File: "a.go", Body: "finding", ActionKey: "run-1:1", IssueURL: "https://github.com/o/r/issues/60"}
	got, err := newProviderForTest(t, f).DeflectReviewThread(ctx(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || !got.Resolved || got.ReplyID != "PRRC_1" {
		t.Fatalf("calls/result = %d/%+v", calls, got)
	}
}

func TestDeflectReviewThreadReusesMarkedReply(t *testing.T) {
	f := newFakeGH(t)
	calls := 0
	f.on(http.MethodPost, "/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			writeBoundThread(t, w, []map[string]any{
				{"id": "original", "body": "finding", "pullRequestReview": map[string]any{"databaseId": 123}},
				{"id": "PRRC_existing", "body": "Deferred as https://github.com/o/r/issues/60.\n\n" + findingMarker("run-1:1"), "pullRequestReview": map[string]any{"databaseId": 123}},
			})
			return
		}
		if strings.Contains(body.Query, "addPullRequestReviewThreadReply") {
			t.Fatal("must not duplicate marked reply")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"id": "PRRT_1", "isResolved": true}}}})
	})
	binding := ports.SCMReviewThreadBinding{PRURL: "https://github.com/o/r/pull/1", ReviewID: "123", ThreadID: "PRRT_1", File: "a.go", Body: "finding", ActionKey: "run-1:1", IssueURL: "https://github.com/o/r/issues/60"}
	got, err := newProviderForTest(t, f).DeflectReviewThread(ctx(), binding)
	if err != nil || calls != 2 || got.ReplyID != "PRRC_existing" {
		t.Fatalf("calls/result/err = %d/%+v/%v", calls, got, err)
	}
}

func TestReviewThreadBoundRejectsDifferentPRReviewAndFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding ports.SCMReviewThreadBinding
	}{
		{name: "pr", binding: ports.SCMReviewThreadBinding{PRURL: "https://github.com/o/other/pull/1", ReviewID: "123", ThreadID: "PRRT_1", File: "a.go", Body: "finding"}},
		{name: "review", binding: ports.SCMReviewThreadBinding{PRURL: "https://github.com/o/r/pull/1", ReviewID: "999", ThreadID: "PRRT_1", File: "a.go", Body: "finding"}},
		{name: "file", binding: ports.SCMReviewThreadBinding{PRURL: "https://github.com/o/r/pull/1", ReviewID: "123", ThreadID: "PRRT_1", File: "other.go", Body: "finding"}},
		{name: "finding", binding: ports.SCMReviewThreadBinding{PRURL: "https://github.com/o/r/pull/1", ReviewID: "123", ThreadID: "PRRT_1", File: "a.go", Body: "other finding"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGH(t)
			f.on(http.MethodPost, "/graphql", func(w http.ResponseWriter, _ *http.Request) { writeBoundThread(t, w, nil) })
			bound, err := newProviderForTest(t, f).ReviewThreadBound(ctx(), tc.binding)
			if err != nil || bound {
				t.Fatalf("bound/err = %v/%v", bound, err)
			}
		})
	}
}

func writeBoundThread(t *testing.T, w http.ResponseWriter, comments []map[string]any) {
	t.Helper()
	if comments == nil {
		comments = []map[string]any{{"id": "original", "body": "finding", "pullRequestReview": map[string]any{"databaseId": 123}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{
		"id": "PRRT_1", "path": "a.go", "isResolved": false,
		"pullRequest": map[string]any{"url": "https://github.com/o/r/pull/1"},
		"comments":    map[string]any{"nodes": comments},
	}}})
}
