package controllers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	verifysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/verification"
)

// VerificationService is the daemon boundary for an out-of-band workspace check.
type VerificationService interface {
	Run(context.Context, domain.SessionID, string, string) (verifysvc.Result, error)
}

// VerifyRequest selects one configured profile. Executables and arguments are
// deliberately absent: callers cannot turn this API into an arbitrary shell.
type VerifyRequest struct {
	Profile string `json:"profile" minLength:"1"`
}

// VerificationCapabilityHeader carries the unforgeable, session-scoped
// capability issued to the worker when its session starts. Keeping it out of
// the JSON body makes the operation's authorization requirement explicit in
// the generated API contract without exposing it as worker-controlled policy.
type VerificationCapabilityHeader struct {
	Capability string `header:"X-AO-Verification-Capability" required:"true" minLength:"1" writeOnly:"true" description:"Session-scoped verification capability issued by the daemon."`
}

// VerifyResponse reports the completed run and its bounded daemon-owned log.
type VerifyResponse = verifysvc.Result

// VerificationController owns the loopback-only session verification route.
type VerificationController struct{ Svc VerificationService }

// Register mounts the verification route on the supplied router.
func (c *VerificationController) Register(r chi.Router) {
	r.Post("/sessions/{sessionId}/verify", c.run)
}

func (c *VerificationController) run(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/verify")
		return
	}
	var in VerifyRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_BODY", "Invalid request body", nil)
		return
	}
	capability := r.Header.Get("X-AO-Verification-Capability")
	res, err := c.Svc.Run(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), in.Profile, capability)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, res)
}
