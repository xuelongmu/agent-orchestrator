package cli

import (
	"encoding/json"
	"net/http"
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
