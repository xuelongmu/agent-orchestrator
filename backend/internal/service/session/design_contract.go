package session

import (
	"context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type designContractWriter interface {
	AddPRDesignContractInvariant(ctx context.Context, sessionID domain.SessionID, prURL, invariant string, updatedAt time.Time) (string, error)
}

// AddDesignContractInvariant is the trusted fixer/human write path for one
// explicit invariant. PR normalization and ownership are verified before the
// canonical SQLite append; the workspace projection is refreshed afterward.
func (s *Service) AddDesignContractInvariant(ctx context.Context, id domain.SessionID, ref, invariant string) (string, error) {
	rec, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return "", err
	}
	if !ok || rec.IsTerminated {
		return "", fmt.Errorf("session %s is not active", id)
	}
	project, ok, err := s.store.GetProject(ctx, string(rec.ProjectID))
	if err != nil {
		return "", fmt.Errorf("resolve project for %s: %w", id, err)
	}
	if !ok {
		return "", fmt.Errorf("resolve project for %s: project not found", id)
	}
	prURL, _, err := normalizePRRef(ref, project.RepoOriginURL)
	if err != nil {
		return "", err
	}
	if err := requireSameGitHubRepo(prURL, project.RepoOriginURL); err != nil {
		return "", err
	}
	invariant, err = designcontract.NormalizeInvariant(invariant)
	if err != nil {
		return "", err
	}
	writer, ok := s.store.(designContractWriter)
	if !ok {
		return "", fmt.Errorf("design contract storage is unavailable")
	}
	contract, err := writer.AddPRDesignContractInvariant(ctx, id, prURL, invariant, s.clock().UTC())
	if err != nil {
		return "", err
	}
	if err := designcontract.MaterializePR(ctx, rec.Metadata.WorkspacePath, prURL, contract); err != nil {
		// Canonical success must not be reported as failure because the
		// checkout projection is optional and untrusted.
		return contract, nil
	}
	return contract, nil
}
