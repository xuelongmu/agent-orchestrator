package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type providerTestRuntime struct {
	calls   int
	handle  ports.RuntimeHandle
	message string
}

func (r *providerTestRuntime) SendMessage(_ context.Context, handle ports.RuntimeHandle, message string) error {
	r.calls++
	r.handle = handle
	r.message = message
	return nil
}

func newProviderTestMessenger(t *testing.T, provider providerSendFunc) (*providerMessenger, domain.SessionRecord, *providerTestRuntime) {
	t.Helper()

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
		Harness:   domain.HarnessCodex,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()},
		Metadata: domain.SessionMetadata{
			RuntimeHandleID: "ao-1/terminal_0",
			AgentSessionID:  "provider-session-1",
			WorkspacePath:   t.TempDir(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	runtime := &providerTestRuntime{}
	messenger := providerMessenger{
		fallback: runtimeMessenger{store: store, runtime: runtime},
		providers: map[domain.AgentHarness]providerSendFunc{
			domain.HarnessCodex: provider,
		},
		environment: func(_ context.Context, id domain.SessionID) (map[string]string, error) {
			return map[string]string{"AO_SESSION_ID": string(id)}, nil
		},
		restoreConfig: func(_ context.Context, _ domain.SessionID) (ports.RestoreConfig, error) {
			return ports.RestoreConfig{
				Session: ports.SessionRef{
					ID:            string(rec.ID),
					WorkspacePath: rec.Metadata.WorkspacePath,
					Metadata: map[string]string{
						ports.MetadataKeyAgentSessionID: rec.Metadata.AgentSessionID,
					},
				},
			}, nil
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &messenger, rec, runtime
}

func TestProviderMessengerUsesStructuredDeliveryAfterAcknowledgement(t *testing.T) {
	var gotMessage string
	provider := func(ctx context.Context, delivery providerDelivery) (bool, error) {
		gotMessage = delivery.message
		if err := delivery.finalCheck(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	messenger, rec, runtime := newProviderTestMessenger(t, provider)

	delivery, err := messenger.SendWithDelivery(context.Background(), rec.ID, "hello provider")
	if err != nil {
		t.Fatalf("SendWithDelivery: %v", err)
	}
	if delivery != ports.MessageDeliveryStructured {
		t.Fatalf("delivery = %v, want structured", delivery)
	}
	if gotMessage != "hello provider" {
		t.Fatalf("provider message = %q, want hello provider", gotMessage)
	}
	if runtime.calls != 0 {
		t.Fatalf("runtime fallback called %d times after acknowledgement, want 0", runtime.calls)
	}
}

func TestProviderMessengerDoesNotFallbackWhenActivityChangesDuringHandshake(t *testing.T) {
	var store *sqlite.Store
	provider := func(ctx context.Context, delivery providerDelivery) (bool, error) {
		rec := delivery.record
		rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: time.Now()}
		if err := store.UpdateSession(ctx, rec); err != nil {
			return false, err
		}
		return false, delivery.finalCheck(ctx)
	}
	messenger, rec, runtime := newProviderTestMessenger(t, provider)
	store = messenger.fallback.store

	err := messenger.Send(context.Background(), rec.ID, "racy message")
	if !errors.Is(err, errProviderSessionChanged) {
		t.Fatalf("Send error = %v, want errProviderSessionChanged", err)
	}
	if runtime.calls != 0 {
		t.Fatalf("runtime fallback called %d times after activity changed, want 0", runtime.calls)
	}
}

func TestProviderMessengerRechecksActivityBeforeTerminalFallback(t *testing.T) {
	var store *sqlite.Store
	provider := func(ctx context.Context, delivery providerDelivery) (bool, error) {
		rec := delivery.record
		rec.Activity = domain.Activity{State: domain.ActivityBlocked, LastActivityAt: time.Now()}
		if err := store.UpdateSession(ctx, rec); err != nil {
			return false, err
		}
		return false, errors.New("provider unavailable")
	}
	messenger, rec, runtime := newProviderTestMessenger(t, provider)
	store = messenger.fallback.store

	err := messenger.Send(context.Background(), rec.ID, "do not paste into decision")
	if !errors.Is(err, errProviderSessionChanged) {
		t.Fatalf("Send error = %v, want errProviderSessionChanged", err)
	}
	if runtime.calls != 0 {
		t.Fatalf("runtime fallback called %d times after activity changed, want 0", runtime.calls)
	}
}

func TestProviderMessengerFallsBackBeforeAcknowledgement(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	provider := func(context.Context, providerDelivery) (bool, error) {
		return false, providerErr
	}
	messenger, rec, runtime := newProviderTestMessenger(t, provider)

	delivery, err := messenger.SendWithDelivery(context.Background(), rec.ID, "fallback message")
	if err != nil {
		t.Fatalf("SendWithDelivery: %v", err)
	}
	if delivery != ports.MessageDeliveryTerminal {
		t.Fatalf("delivery = %v, want terminal", delivery)
	}
	if runtime.calls != 1 {
		t.Fatalf("runtime fallback calls = %d, want 1", runtime.calls)
	}
	if runtime.handle.ID != "ao-1/terminal_0" || runtime.message != "fallback message" {
		t.Fatalf("runtime fallback got handle=%q message=%q", runtime.handle.ID, runtime.message)
	}
}

func TestProviderMessengerNeverFallsBackAfterAcknowledgement(t *testing.T) {
	laterErr := errors.New("provider exited after accepting the turn")
	provider := func(context.Context, providerDelivery) (bool, error) {
		return true, laterErr
	}
	messenger, rec, runtime := newProviderTestMessenger(t, provider)

	if err := messenger.Send(context.Background(), rec.ID, "accepted message"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if runtime.calls != 0 {
		t.Fatalf("runtime fallback called %d times after acknowledgement, want 0", runtime.calls)
	}
}

func TestProviderMessengerUsesRuntimeForEnterOnlyRecovery(t *testing.T) {
	providerCalls := 0
	provider := func(context.Context, providerDelivery) (bool, error) {
		providerCalls++
		return true, nil
	}
	messenger, rec, runtime := newProviderTestMessenger(t, provider)

	if err := messenger.Send(context.Background(), rec.ID, ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if providerCalls != 0 {
		t.Fatalf("provider called %d times for an Enter-only recovery, want 0", providerCalls)
	}
	if runtime.calls != 1 || runtime.message != "" {
		t.Fatalf("runtime fallback calls=%d message=%q, want one empty message", runtime.calls, runtime.message)
	}
}

func TestWaitRPCResponseIgnoresNotificationsAndMatchesID(t *testing.T) {
	lines := make(chan []byte, 2)
	lines <- []byte(`{"method":"thread/started","params":{}}`)
	lines <- []byte(`{"id":3,"result":{"turn":{"id":"turn-1"}}}`)
	close(lines)
	done := make(chan error, 1)
	done <- nil
	close(done)

	proc := &providerProcess{
		lines:  lines,
		stderr: &boundedBuffer{limit: 1024},
		done:   done,
	}
	if err := waitRPCResponse(context.Background(), proc, 3); err != nil {
		t.Fatalf("waitRPCResponse: %v", err)
	}
}

func TestWriteCodexServerResponseDeclinesAndRejectsUnsupportedRequests(t *testing.T) {
	tests := []struct {
		name   string
		method string
		want   string
	}{
		{
			name:   "mcp elicitation",
			method: "mcpServer/elicitation/request",
			want:   `{"id":7,"result":{"action":"decline"}}` + "\n",
		},
		{
			name:   "unknown request",
			method: "currentTime/read",
			want:   `{"error":{"code":-32601,"message":"unsupported by agent-orchestrator structured delivery"},"id":7}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeCodexServerResponse(&output, rpcEnvelope{
				ID: json.RawMessage("7"), Method: tt.method,
			}); err != nil {
				t.Fatalf("writeCodexServerResponse: %v", err)
			}
			if output.String() != tt.want {
				t.Fatalf("response = %q, want %q", output.String(), tt.want)
			}
		})
	}
}
