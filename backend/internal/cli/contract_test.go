package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestContractAddUsesCurrentSessionAndExactPR(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "mer-1")
	cfg := setConfigEnv(t)
	srv, capture := reviewServer(t, http.StatusOK, `{"ok":true}`)
	writeRunFileFor(t, cfg, srv)
	out, errOut, err := executeCLI(t, aliveDeps(), "contract", "add", "--pr", "https://github.com/o/r/pull/7", "--invariant", "Every path reaches one ownership chokepoint.")
	if err != nil {
		t.Fatalf("contract add: %v stderr=%s", err, errOut)
	}
	if capture.method != http.MethodPost || capture.path != "/api/v1/sessions/mer-1/design-contract/invariants" {
		t.Fatalf("request = %s %s", capture.method, capture.path)
	}
	var req contractAddRequest
	if err := json.Unmarshal([]byte(capture.body), &req); err != nil {
		t.Fatal(err)
	}
	if req.PR != "https://github.com/o/r/pull/7" || req.Invariant != "Every path reaches one ownership chokepoint." || out == "" {
		t.Fatalf("request/output = %+v %q", req, out)
	}
}

func TestContractAddRequiresSessionPRAndInvariant(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "")
	_, _, err := executeCLI(t, aliveDeps(), "contract", "add", "--pr", "7", "--invariant", "one line")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("missing session error = %v", err)
	}
}

func TestContractAddSurfacesDaemonOwnershipErrorAsRuntimeFailure(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "mer-1")
	cfg := setConfigEnv(t)
	srv, _ := reviewServer(t, http.StatusNotFound, `{"error":"not_found","code":"PR_NOT_OWNED","message":"PR is not owned by this session","requestId":"req-contract"}`)
	writeRunFileFor(t, cfg, srv)
	_, errOut, err := executeCLI(t, aliveDeps(), "contract", "add", "--pr", "17", "--invariant", "Every path reaches one ownership chokepoint.")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("daemon ownership error = %v (exit %d), want runtime failure", err, ExitCode(err))
	}
	if !strings.Contains(err.Error()+errOut, "PR_NOT_OWNED") || !strings.Contains(err.Error()+errOut, "req-contract") {
		t.Fatalf("daemon envelope not preserved: err=%v stderr=%q", err, errOut)
	}
}

func TestContractShowPrintsFullCanonicalFallbackAndSanitizesControls(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "mer-1")
	cfg := setConfigEnv(t)
	canonical := strings.Repeat("head-", 4000) + "MIDDLE-INVARIANT\x1b[31m\u0085" + strings.Repeat("-tail", 4000)
	payload, err := json.Marshal(map[string]any{"ok": true, "contract": canonical})
	if err != nil {
		t.Fatal(err)
	}
	srv, capture := reviewServer(t, http.StatusOK, string(payload))
	writeRunFileFor(t, cfg, srv)
	out, errOut, err := executeCLI(t, aliveDeps(), "contract", "show", "--pr", "https://gitlab.example.com/g/r/-/merge_requests/17")
	if err != nil {
		t.Fatalf("contract show: %v stderr=%s", err, errOut)
	}
	if capture.method != http.MethodGet || capture.path != "/api/v1/sessions/mer-1/design-contract" {
		t.Fatalf("request = %s %s", capture.method, capture.path)
	}
	if !strings.Contains(out, "MIDDLE-INVARIANT") || len(out) < len(canonical)-2 {
		t.Fatalf("full canonical fallback was truncated: output=%d canonical=%d", len(out), len(canonical))
	}
	for _, control := range []string{"\x1b", "\u0085"} {
		if strings.Contains(out, control) {
			t.Fatalf("terminal output retained control %q", control)
		}
	}
}
