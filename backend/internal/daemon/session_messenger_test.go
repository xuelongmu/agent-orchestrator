package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type sessionMessengerRuntime struct {
	calls   int
	handle  ports.RuntimeHandle
	message string
}

func (r *sessionMessengerRuntime) SendMessage(_ context.Context, handle ports.RuntimeHandle, message string) error {
	r.calls++
	r.handle = handle
	r.message = message
	return nil
}

func TestSessionMessengerUsesOnlyLiveRuntimeForInteractiveHarnesses(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		t.Run(string(harness), func(t *testing.T) {
			store, err := sqlite.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })

			ctx := context.Background()
			if err := store.UpsertProject(ctx, domain.ProjectRecord{
				ID: "p", Path: t.TempDir(), RegisteredAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			rec, err := store.CreateSession(ctx, domain.SessionRecord{
				ProjectID: "p",
				Kind:      domain.KindWorker,
				Harness:   harness,
				Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()},
				Metadata: domain.SessionMetadata{
					RuntimeHandleID: "live-runtime",
					AgentSessionID:  "native-conversation",
					WorkspacePath:   t.TempDir(),
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			runtime := &sessionMessengerRuntime{}
			messenger := newSessionMessenger(store, runtime, nil)
			if err := messenger.Send(ctx, rec.ID, "one message"); err != nil {
				t.Fatal(err)
			}
			if runtime.calls != 1 || runtime.handle.ID != "live-runtime" || runtime.message != "one message" {
				t.Fatalf("runtime delivery = calls:%d handle:%q message:%q", runtime.calls, runtime.handle.ID, runtime.message)
			}
		})
	}
}
