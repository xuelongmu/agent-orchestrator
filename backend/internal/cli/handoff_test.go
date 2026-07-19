package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandoffSubmitsCurrentSessionTypedPayload(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "ao-30")
	cfg := setConfigEnv(t)
	var path string
	var request handoffRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"sessionId":"ao-30","created":true}`)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"handoff", "--changed-file", "a.go", "--changed-file", "b.ts", "--verification-command", "go test ./x", "--residual-risk", "CI pending")
	if err != nil {
		t.Fatalf("handoff: %v\nstderr=%s", err, errOut)
	}
	if path != "/api/v1/sessions/ao-30/handoff" {
		t.Fatalf("path = %q", path)
	}
	if len(request.ChangedFiles) != 2 || request.ChangedFiles[0] != "a.go" || len(request.VerificationCommands) != 1 || request.ResidualRisk != "CI pending" {
		t.Fatalf("request = %#v", request)
	}
	if !strings.Contains(out, "Handoff submitted for ao-30") {
		t.Fatalf("output = %q", out)
	}
}

func TestHandoffExactReplayMessage(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "ao-30")
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"sessionId":"ao-30","created":false}`)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "handoff")
	if err != nil || !strings.Contains(out, "exact replay") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestHandoffRequiresSessionEnvironment(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "")
	_, _, err := executeCLI(t, Deps{}, "handoff")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("err=%v exit=%d", err, ExitCode(err))
	}
}
