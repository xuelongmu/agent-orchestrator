package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	prsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pr"
)

type fakePRService struct {
	mergeResult    prsvc.MergeResult
	mergeErr       error
	mergeRequest   prsvc.MergeRequest
	resolveResult  prsvc.ResolveResult
	resolveErr     error
	resolveRequest prsvc.ResolveRequest
}

func (f *fakePRService) Merge(_ context.Context, request prsvc.MergeRequest) (prsvc.MergeResult, error) {
	f.mergeRequest = request
	return f.mergeResult, f.mergeErr
}

func (f *fakePRService) ResolveComments(_ context.Context, request prsvc.ResolveRequest) (prsvc.ResolveResult, error) {
	f.resolveRequest = request
	return f.resolveResult, f.resolveErr
}

func newPRTestServer(t *testing.T, svc prsvc.ActionManager) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{PRs: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- Nil service → 503 SCM_NOT_CONFIGURED ----

func TestPRsRoutes_NilService_MergeReturns501(t *testing.T) {
	srv := newPRTestServer(t, nil)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/1/merge", "")
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusNotImplemented, "NOT_IMPLEMENTED")
}

func TestPRsRoutes_NilService_ResolveCommentsReturns501(t *testing.T) {
	srv := newPRTestServer(t, nil)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/1/resolve-comments", "")
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusNotImplemented, "NOT_IMPLEMENTED")
}

// ---- Merge: 200 ----

func TestPRsRoutes_Merge_200(t *testing.T) {
	svc := &fakePRService{mergeResult: prsvc.MergeResult{PRNumber: 42, Method: "squash"}}
	srv := newPRTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/prs/42/merge", `{"prUrl":"https://github.com/acme/widgets/pull/42","expectedHeadSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp struct {
		OK       bool   `json:"ok"`
		PRNumber int    `json:"prNumber"`
		Method   string `json:"method"`
	}
	mustJSON(t, body, &resp)
	if !resp.OK || resp.PRNumber != 42 || resp.Method != "squash" {
		t.Errorf("resp = %+v, want {ok:true prNumber:42 method:squash}", resp)
	}
	if svc.mergeRequest.PRID != "42" || svc.mergeRequest.PRURL != "https://github.com/acme/widgets/pull/42" || svc.mergeRequest.ExpectedHeadSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("merge request = %#v", svc.mergeRequest)
	}
}

func TestPRsRoutes_Merge_InvalidIDReturns400WithoutCallingService(t *testing.T) {
	svc := &fakePRService{}
	srv := newPRTestServer(t, svc)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/not-a-number/merge", `{"prUrl":"https://github.com/acme/widgets/pull/42"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_PR")
	if svc.mergeRequest.PRID != "" {
		t.Fatalf("service was called: %#v", svc.mergeRequest)
	}
}

func TestPRsRoutes_Merge_MissingBodyReturns400(t *testing.T) {
	svc := &fakePRService{}
	srv := newPRTestServer(t, svc)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/42/merge", "")
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_JSON")
}

func TestPRsRoutes_Merge_MissingExpectedHeadReturns400WithoutCallingService(t *testing.T) {
	svc := &fakePRService{}
	srv := newPRTestServer(t, svc)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/42/merge", `{"prUrl":"https://github.com/acme/widgets/pull/42"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_PR")
	if svc.mergeRequest.PRID != "" {
		t.Fatalf("service was called: %#v", svc.mergeRequest)
	}
}

// ---- Merge: 404 ----

func TestPRsRoutes_Merge_404(t *testing.T) {
	svc := &fakePRService{mergeErr: prsvc.ErrPRNotFound}
	srv := newPRTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/99/merge", `{"prUrl":"https://github.com/acme/widgets/pull/99","expectedHeadSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusNotFound, "PR_NOT_FOUND")
}

// ---- Merge: 409 ----

func TestPRsRoutes_Merge_409(t *testing.T) {
	svc := &fakePRService{mergeErr: prsvc.ErrPRNotMergeable}
	srv := newPRTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/1/merge", `{"prUrl":"https://github.com/acme/widgets/pull/1","expectedHeadSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusConflict, "PR_NOT_MERGEABLE")
}

// ---- Merge: 422 ----

func TestPRsRoutes_Merge_422(t *testing.T) {
	svc := &fakePRService{mergeErr: prsvc.ErrPRPreconditions}
	srv := newPRTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/1/merge", `{"prUrl":"https://github.com/acme/widgets/pull/1","expectedHeadSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusUnprocessableEntity, "PR_PRECONDITIONS_UNMET")
}

func TestPRsRoutes_Merge_HeadChangedReturns409(t *testing.T) {
	svc := &fakePRService{mergeErr: prsvc.ErrPRHeadChanged}
	srv := newPRTestServer(t, svc)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/1/merge", `{"prUrl":"https://github.com/acme/widgets/pull/1","expectedHeadSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusConflict, "PR_HEAD_CHANGED")
}

func TestPRsRoutes_Merge_PermissionDeniedReturns403(t *testing.T) {
	svc := &fakePRService{mergeErr: prsvc.ErrPRPermissionDenied}
	srv := newPRTestServer(t, svc)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/1/merge", `{"prUrl":"https://github.com/acme/widgets/pull/1","expectedHeadSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusForbidden, "PR_PERMISSION_DENIED")
}

// ---- ResolveComments: 200 ----

func TestPRsRoutes_ResolveComments_200(t *testing.T) {
	svc := &fakePRService{resolveResult: prsvc.ResolveResult{Resolved: 3}}
	srv := newPRTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/prs/42/resolve-comments", `{"prUrl":"https://github.com/acme/widgets/pull/42","commentIds":["T_1","T_2","T_3"]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp struct {
		OK       bool `json:"ok"`
		Resolved int  `json:"resolved"`
	}
	mustJSON(t, body, &resp)
	if !resp.OK || resp.Resolved != 3 {
		t.Errorf("resp = %+v, want {ok:true resolved:3}", resp)
	}
	if svc.resolveRequest.PRID != "42" || svc.resolveRequest.PRURL != "https://github.com/acme/widgets/pull/42" || len(svc.resolveRequest.ThreadIDs) != 3 {
		t.Fatalf("resolve request = %#v", svc.resolveRequest)
	}
}

func TestPRsRoutes_ResolveComments_200_AllThreads(t *testing.T) {
	svc := &fakePRService{resolveResult: prsvc.ResolveResult{Resolved: 2}}
	srv := newPRTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/prs/42/resolve-comments", `{"prUrl":"https://github.com/acme/widgets/pull/42"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
}

// ---- ResolveComments: 404 ----

func TestPRsRoutes_ResolveComments_404(t *testing.T) {
	svc := &fakePRService{resolveErr: prsvc.ErrPRNotFound}
	srv := newPRTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/99/resolve-comments", `{"prUrl":"https://github.com/acme/widgets/pull/99"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusNotFound, "PR_NOT_FOUND")
}

// ---- ResolveComments: 422 ----

func TestPRsRoutes_ResolveComments_422(t *testing.T) {
	svc := &fakePRService{resolveErr: prsvc.ErrNothingToResolve}
	srv := newPRTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/1/resolve-comments", `{"prUrl":"https://github.com/acme/widgets/pull/1"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusUnprocessableEntity, "NOTHING_TO_RESOLVE")
}

func TestPRsRoutes_ResolveComments_UnconfiguredReturns501(t *testing.T) {
	svc := &fakePRService{resolveErr: prsvc.ErrActionNotConfigured}
	srv := newPRTestServer(t, svc)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/1/resolve-comments", `{"prUrl":"https://github.com/acme/widgets/pull/1"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusNotImplemented, "NOT_IMPLEMENTED")
}

func TestPRsRoutes_ResolveComments_PermissionDeniedReturns403(t *testing.T) {
	svc := &fakePRService{resolveErr: prsvc.ErrPRPermissionDenied}
	srv := newPRTestServer(t, svc)
	body, status, headers := doRequest(t, srv, "POST", "/api/v1/prs/1/resolve-comments", `{"prUrl":"https://github.com/acme/widgets/pull/1"}`)
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusForbidden, "PR_PERMISSION_DENIED")
}
