package sessioncap

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestCapabilityIsBoundToSessionAndProjectAndPersists(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	token := first.Token("ao-1", "ao")
	if !first.Verify("ao-1", "ao", token) {
		t.Fatal("owner token rejected")
	}
	if first.Verify("ao-2", "ao", token) || first.Verify("ao-1", "other", token) {
		t.Fatal("cross-session/project token accepted")
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Verify(domain.SessionID("ao-1"), domain.ProjectID("ao"), token) {
		t.Fatal("token did not survive reopen")
	}
}

func TestOpenRecoversIncompletePublishedKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keyFileName), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !manager.Verify("ao-1", "ao", manager.Token("ao-1", "ao")) {
		t.Fatal("recovered manager rejected its token")
	}
	info, err := os.Stat(filepath.Join(dir, keyFileName))
	if err != nil || info.Size() != keyBytes {
		t.Fatalf("recovered key info = %v, %v", info, err)
	}
}

func TestConcurrentOpenPublishesOneKey(t *testing.T) {
	dir := t.TempDir()
	const count = 8
	managers := make([]*Manager, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			managers[i], errs[i] = Open(dir)
		}()
	}
	wg.Wait()
	for i := range count {
		if errs[i] != nil {
			t.Fatalf("Open[%d]: %v", i, errs[i])
		}
	}
	token := managers[0].Token("ao-1", "ao")
	for i := range count {
		if !managers[i].Verify("ao-1", "ao", token) {
			t.Fatalf("manager %d loaded a different key", i)
		}
	}
}
