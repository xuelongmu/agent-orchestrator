package daemon

import (
	"context"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/observe/charter"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func startCharterObserver(ctx context.Context, store *sqlite.Store, sessions *sessionsvc.Service, log *slog.Logger) <-chan struct{} {
	return charter.New(store, sessions, charter.Config{Logger: log}).Start(ctx)
}
