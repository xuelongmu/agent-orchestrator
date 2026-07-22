package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectOrchestrationSet(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := projectServer(t, http.StatusOK, `{"projectId":"demo","policy":{"mode":"charter","checkInIntervalMinutes":45}}`)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "project", "orchestration", "set", "demo", "--mode", "charter", "--interval", "45m")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if capture.method != http.MethodPut || capture.path != "/api/v1/projects/demo/orchestration" {
		t.Fatalf("request = %s %s", capture.method, capture.path)
	}
	var req setProjectOrchestrationRequest
	if err := json.Unmarshal(capture.body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Policy.Mode != "charter" || req.Policy.CheckInIntervalMinutes != 45 {
		t.Fatalf("policy = %#v", req.Policy)
	}
	if !strings.Contains(out, "demo: charter") {
		t.Fatalf("output = %q", out)
	}
}

func TestProjectOrchestrationCurrentProject(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_PROJECT_ID", "current-demo")
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/current-demo":
			_, _ = io.WriteString(w, `{"status":"ok","project":{"id":"current-demo","path":"/repo"}}`)
		case "/api/v1/projects/current-demo/orchestration":
			_, _ = io.WriteString(w, `{"projectId":"current-demo","policy":{"mode":"mission","checkInIntervalMinutes":30}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	_, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "project", "orchestration", "get", "--current")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if len(paths) < 2 || paths[len(paths)-1] != "/api/v1/projects/current-demo/orchestration" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestProjectOrchestrationValidation(t *testing.T) {
	for _, args := range [][]string{
		{"project", "orchestration", "get"},
		{"project", "orchestration", "get", "demo", "--current"},
		{"project", "orchestration", "set", "demo", "--mode", "forever"},
		{"project", "orchestration", "set", "demo", "--mode", "charter", "--interval", "30s"},
	} {
		if _, _, err := executeCLI(t, Deps{}, args...); err == nil {
			t.Fatalf("%v: expected usage error", args)
		}
	}
}
