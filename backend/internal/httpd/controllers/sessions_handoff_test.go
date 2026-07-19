package controllers_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

func TestSubmitSessionHandoffAcceptsTypedPayload(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions/ao-1/handoff", `{"changedFiles":["a.go"],"verificationCommands":["go test ./x"],"residualRisk":"CI pending"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body=%s", status, body)
	}
	if svc.handoff == nil || !svc.handoff.Equal(domain.AgentHandoff{ChangedFiles: []string{"a.go"}, VerificationCommands: []string{"go test ./x"}, ResidualRisk: "CI pending"}) {
		t.Fatalf("handoff = %#v", svc.handoff)
	}
	var response struct {
		OK      bool `json:"ok"`
		Created bool `json:"created"`
	}
	if err := json.Unmarshal(body, &response); err != nil || !response.OK || !response.Created {
		t.Fatalf("response = %#v err=%v", response, err)
	}
}

func TestSubmitSessionHandoffRejectsMalformedUnknownAndMissingFields(t *testing.T) {
	for _, body := range []string{
		`{`,
		`{"changedFiles":[],"verificationCommands":[],"residualRisk":"","extra":true}`,
		`{"changedFiles":[],"changedFiles":["other"],"verificationCommands":[],"residualRisk":""}`,
		`{"changedFiles":[],"ChangedFiles":["other"],"verificationCommands":[],"residualRisk":""}`,
		`{"changedFiles":[],"verificationCommands":[]}`,
		`{"verificationCommands":[],"residualRisk":""}`,
	} {
		srv := newSessionTestServer(t, newFakeSessionService())
		response, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions/ao-1/handoff", body)
		if status != http.StatusBadRequest {
			t.Errorf("body %q: status=%d response=%s", body, status, response)
		}
		srv.Close()
	}
}

func TestSubmitSessionHandoffRejectsInvalidUTF8WithoutCallingService(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	body := string(append([]byte(`{"changedFiles":[],"verificationCommands":[],"residualRisk":"`), append([]byte{0xff}, []byte(`"}`)...)...))
	response, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions/ao-1/handoff", body)
	if status != http.StatusBadRequest || !strings.Contains(string(response), "valid UTF-8") {
		t.Fatalf("status=%d response=%s", status, response)
	}
	if svc.handoff != nil {
		t.Fatal("invalid UTF-8 reached the service")
	}
}

func TestSubmitSessionHandoffRejectsOversizeRequest(t *testing.T) {
	srv := newSessionTestServer(t, newFakeSessionService())
	body := `{"changedFiles":[],"verificationCommands":[],"residualRisk":"` + strings.Repeat("x", domain.MaxHandoffPayloadBytes) + `"}`
	response, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions/ao-1/handoff", body)
	if status != http.StatusBadRequest || !strings.Contains(string(response), "HANDOFF_TOO_LARGE") {
		t.Fatalf("status=%d response=%s", status, response)
	}
}

func TestSubmitSessionHandoffRejectsCanonicalEscapingOverPayloadLimit(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	files := make([]string, domain.MaxHandoffChangedFiles)
	for i := range files {
		files[i] = strings.Repeat("<", domain.MaxHandoffChangedFileBytes)
	}
	payload, err := json.Marshal(struct {
		ChangedFiles         []string `json:"changedFiles"`
		VerificationCommands []string `json:"verificationCommands"`
		ResidualRisk         string   `json:"residualRisk"`
	}{files, []string{}, ""})
	if err != nil {
		t.Fatal(err)
	}
	// json.Marshal escapes '<', so construct the equivalent valid raw JSON
	// spelling that passes the transport byte cap but exceeds it canonically.
	payload = []byte(strings.ReplaceAll(string(payload), `\u003c`, "<"))
	if len(payload) >= domain.MaxHandoffPayloadBytes {
		t.Fatalf("test setup raw payload size = %d", len(payload))
	}
	response, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions/ao-1/handoff", string(payload))
	if status != http.StatusBadRequest || !strings.Contains(string(response), "payload is too large") {
		t.Fatalf("status=%d response=%s", status, response)
	}
	if svc.handoff != nil {
		t.Fatal("canonically oversized payload reached the service")
	}
}

func TestSubmitSessionHandoffSurfacesImmutableConflict(t *testing.T) {
	svc := newFakeSessionService()
	svc.handoffErr = apierr.Conflict("HANDOFF_ALREADY_SUBMITTED", "different handoff", nil)
	srv := newSessionTestServer(t, svc)
	response, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions/ao-1/handoff", `{"changedFiles":[],"verificationCommands":[],"residualRisk":""}`)
	if status != http.StatusConflict || !strings.Contains(string(response), "HANDOFF_ALREADY_SUBMITTED") {
		t.Fatalf("status=%d response=%s", status, response)
	}
}
