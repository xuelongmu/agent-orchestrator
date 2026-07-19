package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type verifyCapture struct {
	body       string
	capability string
}

func verifyServer(t *testing.T, status int, body string) (*httptest.Server, *verifyCapture) {
	t.Helper()
	capture := &verifyCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/ao-7/verify" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		capture.body = string(b)
		capture.capability = r.Header.Get("X-AO-Verification-Capability")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, capture
}

func TestVerifyPrintsOutcomeAndLog(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "ao-7")
	t.Setenv("AO_VERIFY_CAPABILITY", "cap-7")
	cfg := setConfigEnv(t)
	srv, request := verifyServer(t, http.StatusOK, `{"sessionId":"ao-7","profile":"backend","outcome":"passed","exitCode":0,"logPath":"C:\\work\\.ao\\verify-1.log","truncated":false,"durationMs":12}`)
	writeRunFileFor(t, cfg, srv)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "verify", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if request.body != `{"profile":"backend"}` || request.capability != "cap-7" {
		t.Fatalf("request=%#v", request)
	}
	if !strings.Contains(out, "outcome: passed") || !strings.Contains(out, "verify-1.log") {
		t.Fatalf("output=%q", out)
	}
}

func TestVerifyFailureReturnsRuntimeErrorAfterPrintingLog(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "ao-7")
	t.Setenv("AO_VERIFY_CAPABILITY", "cap-7")
	cfg := setConfigEnv(t)
	srv, _ := verifyServer(t, http.StatusOK, `{"sessionId":"ao-7","profile":"frontend","outcome":"failed","exitCode":2,"logPath":"/work/.ao/verify-2.log","truncated":true,"durationMs":12}`)
	writeRunFileFor(t, cfg, srv)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "verify", "frontend")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("error=%v", err)
	}
	if !strings.Contains(out, "log: /work/.ao/verify-2.log") || !strings.Contains(out, "log truncated: true") {
		t.Fatalf("output=%q", out)
	}
}

func TestVerifyRequiresSessionAndExactlyOneProfile(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "")
	setConfigEnv(t)
	_, _, err := executeCLI(t, Deps{}, "verify", "backend")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("error=%v", err)
	}
	_, _, err = executeCLI(t, Deps{}, "verify")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("error=%v", err)
	}
}

func TestVerifyRequiresInjectedCapability(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "ao-7")
	t.Setenv("AO_VERIFY_CAPABILITY", "")
	setConfigEnv(t)
	_, _, err := executeCLI(t, Deps{}, "verify", "backend")
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "AO_VERIFY_CAPABILITY") {
		t.Fatalf("error=%v", err)
	}
}

func TestVerifySurfacesDaemonErrorEnvelope(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "ao-7")
	t.Setenv("AO_VERIFY_CAPABILITY", "cap-7")
	cfg := setConfigEnv(t)
	srv, _ := verifyServer(t, http.StatusConflict, `{"message":"verification already running","code":"VERIFY_ALREADY_RUNNING","requestId":"req-7"}`)
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "verify", "backend")
	if err == nil || !strings.Contains(err.Error(), "VERIFY_ALREADY_RUNNING") || !strings.Contains(err.Error(), "req-7") {
		t.Fatalf("error = %v", err)
	}
}
