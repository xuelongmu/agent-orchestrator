package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestResolveReviewThread_UsesGraphQLMutation(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodPost, "/graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body.Query, "resolveReviewThread") || body.Variables["threadID"] != "PRRT_123" {
			t.Fatalf("graphql request = %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"resolveReviewThread": map[string]any{"thread": map[string]any{"id": "PRRT_123", "isResolved": true}},
		}})
	})

	got, err := newProviderForTest(t, f).ResolveReviewThread(ctx(), "PRRT_123")
	if err != nil {
		t.Fatal(err)
	}
	if got.ThreadID != "PRRT_123" || !got.Resolved {
		t.Fatalf("result = %#v", got)
	}
}

func TestResolveReviewThread_MapsGraphQLErrors(t *testing.T) {
	tests := []struct {
		name      string
		errorType string
		message   string
		want      error
	}{
		{name: "not found", errorType: "NOT_FOUND", message: "Could not resolve to a node", want: ports.ErrSCMNotFound},
		{name: "permission", errorType: "FORBIDDEN", message: "Resource not accessible by integration", want: ports.ErrSCMPermissionDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGH(t)
			f.on(http.MethodPost, "/graphql", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"type": tc.errorType, "message": tc.message}}})
			})
			_, err := newProviderForTest(t, f).ResolveReviewThread(ctx(), "PRRT_123")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestResolveReviewThread_DoesNotReportFalseSuccess(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodPost, "/graphql", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"resolveReviewThread": map[string]any{"thread": map[string]any{"id": "PRRT_123", "isResolved": false}},
		}})
	})
	got, err := newProviderForTest(t, f).ResolveReviewThread(ctx(), "PRRT_123")
	if err == nil || got.Resolved {
		t.Fatalf("result = %#v, error = %v; want failure", got, err)
	}
}

func TestResolveReviewThread_RejectsInvalidIDWithoutCallingGitHub(t *testing.T) {
	f := newFakeGH(t)
	for _, id := range []string{"", " ", "bad id", strings.Repeat("x", 257)} {
		if _, err := newProviderForTest(t, f).ResolveReviewThread(ctx(), id); err == nil {
			t.Fatalf("ResolveReviewThread(%q) succeeded", id)
		}
	}
	if got := f.callsTo(http.MethodPost, "/graphql"); got != 0 {
		t.Fatalf("graphql calls = %d", got)
	}
}
