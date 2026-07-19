package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	verifysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/verification"
)

type fakeVerifier struct {
	session    domain.SessionID
	profile    string
	capability string
}

func (f *fakeVerifier) Run(_ context.Context, session domain.SessionID, profile, capability string) (verifysvc.Result, error) {
	f.session = session
	f.profile = profile
	f.capability = capability
	return verifysvc.Result{SessionID: session, Profile: profile, Outcome: verifysvc.OutcomePassed, LogPath: `C:\\ao-data\\verification\\session\\verify-1.log`}, nil
}

func TestVerificationAPI(t *testing.T) {
	fake := &fakeVerifier{}
	router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Verification: fake}, httpd.ControlDeps{})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/sessions/ao-7/verify", strings.NewReader(`{"profile":"backend"}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AO-Verification-Capability", "secret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if fake.session != "ao-7" || fake.profile != "backend" || fake.capability != "secret" {
		t.Fatalf("call=%q %q %q", fake.session, fake.profile, fake.capability)
	}
	var got verifysvc.Result
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome != verifysvc.OutcomePassed || got.LogPath == "" {
		t.Fatalf("response=%#v", got)
	}
}

func TestVerificationAPIRejectsUnknownFields(t *testing.T) {
	fake := &fakeVerifier{}
	router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Verification: fake}, httpd.ControlDeps{})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/sessions/ao-7/verify", strings.NewReader(`{"profile":"backend","argv":["evil"]}`))
	req.Host = "127.0.0.1"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if fake.profile != "" {
		t.Fatal("service called")
	}
}

func TestVerificationAPIRejectsTrailingJSONValue(t *testing.T) {
	fake := &fakeVerifier{}
	router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Verification: fake}, httpd.ControlDeps{})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/sessions/ao-7/verify", strings.NewReader(`{"profile":"backend"}{"profile":"frontend"}`))
	req.Host = "127.0.0.1"
	req.Header.Set("X-AO-Verification-Capability", "secret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if fake.profile != "" {
		t.Fatal("service called")
	}
}
