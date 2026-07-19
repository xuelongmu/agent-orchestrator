package session

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type designContractWriter interface {
	AddPRDesignContractInvariant(ctx context.Context, sessionID domain.SessionID, prURL, invariant string, updatedAt time.Time) (string, error)
}

type designContractReader interface {
	GetOwnedPRDesignContract(ctx context.Context, sessionID domain.SessionID, prURL string) (string, bool, error)
}

// GetDesignContract returns the full canonical bytes for one exact owned PR.
// HTTP JSON escapes control bytes; terminal clients must additionally sanitize
// immediately before rendering.
func (s *Service) GetDesignContract(ctx context.Context, id domain.SessionID, ref string) (string, error) {
	rec, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if rec.IsTerminated {
		return "", apierr.Conflict("SESSION_TERMINATED", "Session is terminated", nil)
	}
	prs, err := s.store.ListPRsBySession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("list PRs owned by %s: %w", id, err)
	}
	prURL, err := resolveOwnedPR(ref, prs)
	if err != nil {
		return "", err
	}
	reader, ok := s.store.(designContractReader)
	if !ok {
		return "", apierr.Internal("CONTRACT_STORAGE_UNAVAILABLE", "Design contract storage is unavailable")
	}
	contract, found, err := reader.GetOwnedPRDesignContract(ctx, id, prURL)
	if err != nil {
		return "", designContractStoreError(err)
	}
	if !found {
		return "", apierr.NotFound("CONTRACT_NOT_FOUND", "Design contract is unavailable")
	}
	return contract, nil
}

// AddDesignContractInvariant is the trusted fixer/human write path for one
// explicit invariant. PR normalization and ownership are verified before the
// canonical SQLite append; the workspace projection is refreshed afterward.
func (s *Service) AddDesignContractInvariant(ctx context.Context, id domain.SessionID, ref, invariant string) (string, error) {
	rec, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if rec.IsTerminated {
		return "", apierr.Conflict("SESSION_TERMINATED", "Session is terminated", nil)
	}
	prs, err := s.store.ListPRsBySession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("list PRs owned by %s: %w", id, err)
	}
	prURL, err := resolveOwnedPR(ref, prs)
	if err != nil {
		return "", err
	}
	invariant, err = designcontract.NormalizeInvariant(invariant)
	if err != nil {
		return "", apierr.Invalid("INVALID_CONTRACT_INVARIANT", err.Error(), nil)
	}
	writer, ok := s.store.(designContractWriter)
	if !ok {
		return "", apierr.Internal("CONTRACT_STORAGE_UNAVAILABLE", "Design contract storage is unavailable")
	}
	contract, err := writer.AddPRDesignContractInvariant(ctx, id, prURL, invariant, s.clock().UTC())
	if err != nil {
		return "", designContractStoreError(err)
	}
	// Canonical success must not be reported as failure because the checkout
	// projection is optional and untrusted.
	_ = designcontract.MaterializePR(ctx, rec.Metadata.WorkspacePath, prURL, contract)
	return contract, nil
}

func designContractStoreError(err error) error {
	switch {
	case errors.Is(err, designcontract.ErrPRNotOwned):
		return apierr.NotFound("PR_NOT_OWNED", "PR is not owned by this session")
	case errors.Is(err, designcontract.ErrContractCapacityExceeded):
		return apierr.Conflict("CONTRACT_CAPACITY_EXCEEDED", "Design contract has reached its canonical capacity", nil)
	default:
		return err
	}
}

// resolveOwnedPR binds a user reference to the canonical URL of an existing
// PR row owned by this session. It deliberately resolves from provider-neutral
// durable SCM identity instead of reconstructing GitHub URLs from a project.
func resolveOwnedPR(ref string, prs []domain.PullRequest) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", apierr.Invalid("INVALID_PR_REF", "PR reference is required", nil)
	}
	numeric := strings.TrimPrefix(ref, "#")
	if number, err := strconv.Atoi(numeric); err == nil && number > 0 {
		matches := make([]string, 0, 1)
		for _, pr := range prs {
			if pr.Number == number {
				matches = append(matches, pr.URL)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return "", apierr.NotFound("PR_NOT_OWNED", "PR is not owned by this session")
		default:
			return "", apierr.Invalid("AMBIGUOUS_PR_REF", "PR number matches more than one owned repository; pass the full URL", nil)
		}
	}
	for _, pr := range prs {
		if sameCanonicalPRURL(ref, pr.URL) {
			return pr.URL, nil
		}
	}
	return "", apierr.NotFound("PR_NOT_OWNED", "PR is not owned by this session")
}

func sameCanonicalPRURL(a, b string) bool {
	parse := func(raw string) (*url.URL, bool) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, false
		}
		u.Path = strings.TrimSuffix(u.EscapedPath(), "/")
		u.RawPath = ""
		return u, true
	}
	left, ok := parse(a)
	if !ok {
		return false
	}
	right, ok := parse(b)
	return ok && strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host) && left.Path == right.Path
}
