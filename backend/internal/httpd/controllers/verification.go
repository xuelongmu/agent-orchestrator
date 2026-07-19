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
	Run(context.Context, domain.SessionID, string) (verifysvc.Result, error)
}

// VerifyRequest selects one configured profile. Executables and arguments are
// deliberately absent: callers cannot turn this API into an arbitrary shell.
type VerifyRequest struct {
	Profile string `json:"profile" minLength:"1"`
}

// VerifyResponse reports the completed run and its bounded workspace-local log.
type VerifyResponse = verifysvc.Result

type VerificationController struct{ Svc VerificationService }

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
	res, err := c.Svc.Run(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), in.Profile)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, res)
}
