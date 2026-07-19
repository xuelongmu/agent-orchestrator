package specgen_test

import (
	"bytes"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec/specgen"
)

// TestBuild_MatchesEmbedded is the drift guard: the committed (embedded)
// openapi.yaml must equal fresh Build() output. If this fails, run
// `go generate ./...` and commit the result.
func TestBuild_MatchesEmbedded(t *testing.T) {
	got, err := specgen.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	embedded := apispec.Default().YAML()
	// Git may check the generated YAML out with CRLF on Windows. Compare the
	// canonical LF representation so this remains a content drift guard.
	embedded = bytes.ReplaceAll(embedded, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, embedded) {
		t.Fatalf("embedded openapi.yaml is stale — run `go generate ./...` and commit.\n"+
			"len(fresh)=%d len(embedded)=%d", len(got), len(embedded))
	}
}

// TestBuild_Deterministic guards against nondeterministic output (which would
// make the drift check flaky in CI).
func TestBuild_Deterministic(t *testing.T) {
	a, err := specgen.Build()
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	b, err := specgen.Build()
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("Build() is not deterministic across calls")
	}
}

func TestBuild_UsesStableLifecycleDiagnosticSchemaName(t *testing.T) {
	got, err := specgen.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if bytes.Contains(got, []byte("DomainLifecycleDiagnostic")) {
		t.Fatal("generated API contract leaked the Go package name for LifecycleDiagnostic")
	}
	if !bytes.Contains(got, []byte("LifecycleDiagnostic:")) {
		t.Fatal("generated API contract is missing the stable LifecycleDiagnostic schema")
	}
}

func TestBuild_VerificationAuthorizationContract(t *testing.T) {
	got, err := specgen.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, want := range [][]byte{
		[]byte("name: X-AO-Verification-Capability"),
		[]byte(`"403":`),
		[]byte("writeOnly: true"),
	} {
		if !bytes.Contains(got, want) {
			t.Fatalf("generated verification contract is missing %q", want)
		}
	}
}

func TestBuild_HandoffDocumentsByteLimitsWithoutCodePointMaxLength(t *testing.T) {
	got, err := specgen.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	start := bytes.Index(got, []byte("    SubmitSessionHandoffRequest:"))
	end := bytes.Index(got[start+1:], []byte("\n    SubmitSessionHandoffResponse:"))
	if start < 0 || end < 0 {
		t.Fatal("generated contract is missing handoff request schemas")
	}
	schema := got[start : start+1+end]
	normalized := bytes.Join(bytes.Fields(schema), []byte(" "))
	if bytes.Contains(schema, []byte("maxLength:")) {
		t.Fatalf("handoff schema advertises code-point maxLength for byte limits:\n%s", schema)
	}
	for _, want := range [][]byte{[]byte("1024 UTF-8 bytes"), []byte("4096 UTF-8 bytes"), []byte("8192 UTF-8 bytes")} {
		if !bytes.Contains(normalized, want) {
			t.Fatalf("handoff schema missing byte-limit description %q:\n%s", want, schema)
		}
	}
}
