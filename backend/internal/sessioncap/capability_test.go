package sessioncap

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestCapabilityIsBoundToSessionAndProjectAndPersists(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil { t.Fatal(err) }
	token := first.Token("ao-1", "ao")
	if !first.Verify("ao-1", "ao", token) { t.Fatal("owner token rejected") }
	if first.Verify("ao-2", "ao", token) || first.Verify("ao-1", "other", token) { t.Fatal("cross-session/project token accepted") }
	second, err := Open(dir)
	if err != nil { t.Fatal(err) }
	if !second.Verify(domain.SessionID("ao-1"), domain.ProjectID("ao"), token) { t.Fatal("token did not survive reopen") }
}
