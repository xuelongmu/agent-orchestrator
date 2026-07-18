package scm

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const ciRerunPendingTimeout = 15 * time.Minute

type ciRerunDecision uint8

const (
	ciRerunNoAction ciRerunDecision = iota
	ciRerunRequested
	ciRerunStillPending
	ciRerunDispatchFailure
)

var failedUnitTestPath = regexp.MustCompile(`(?i)([a-z0-9_.@/-]+(?:\.test\.[cm]?[jt]sx?|_test\.go))`)

// packageRelativeRoots are conventional source/test roots reported by test
// runners when a workflow executes inside a package directory. Without the
// package prefix AO cannot safely compare them with repo-relative PR paths, so
// these ambiguous failures stay on the normal CI dispatch path.
var packageRelativeRoots = map[string]bool{
	"cmd": true, "internal": true, "lib": true, "pkg": true,
	"src": true, "test": true, "tests": true,
}

// knownInfrastructureFlakes is deliberately small and version-controlled.
// These messages are emitted by the Actions runner itself, not by repository
// tests. Additions need a concrete observed signature; fuzzy failure matching
// would send AO into rerun loops instead of surfacing real failures.
var knownInfrastructureFlakes = []string{
	"runner has received a shutdown signal",
}

// maybeRerunFlakyCI requests at most one rerun for one failed Actions job on an
// exact PR head. Its decision distinguishes a newly accepted mutation from an
// already-pending durable attempt so unchanged polls can enter CI pending once
// without re-running lifecycle every 30 seconds. Every ambiguity or
// collaborator error fails closed into the normal CI-failure path.
func (o *Observer) maybeRerunFlakyCI(ctx context.Context, subj *subject, obs ports.SCMObservation) ciRerunDecision {
	if obs.CI.Summary != string(domain.CIFailing) || len(obs.CI.FailedChecks) != 1 {
		return ciRerunNoAction
	}
	check := obs.CI.FailedChecks[0]
	prURL := firstNonEmpty(obs.PR.URL, obs.PR.HTMLURL, subj.known.URL)
	headSHA := firstNonEmpty(obs.CI.HeadSHA, obs.PR.HeadSHA)
	if prURL == "" || headSHA == "" || check.Name == "" || check.ProviderID == "" {
		return ciRerunNoAction
	}

	attempt, found, err := o.store.GetCIRerunAttempt(ctx, prURL, headSHA, check.Name)
	if err != nil {
		o.logger.Error("scm observer: read CI rerun attempt failed", "pr", prURL, "check", check.Name, "err", err)
		return ciRerunDispatchFailure
	}
	if found {
		if ciRerunAttemptStillPending(attempt, check.ProviderID, o.clock().UTC()) {
			return ciRerunStillPending
		}
		return ciRerunDispatchFailure
	}

	if !isKnownInfrastructureFlake(check) {
		ref := ports.SCMPRRef{Repo: subj.repo, Number: obs.PR.Number, URL: prURL}
		changedFiles, err := o.provider.FetchPullRequestFiles(ctx, ref)
		if err != nil {
			o.logger.Error("scm observer: fetch PR files for CI rerun failed", "pr", prURL, "check", check.Name, "err", err)
			return ciRerunDispatchFailure
		}
		if !isClearlyDiffUnrelatedUnitFailure(check, changedFiles) {
			return ciRerunNoAction
		}
	}

	attempt = ports.SCMCIRerunAttempt{
		PRURL: prURL, HeadSHA: headSHA, CheckName: check.Name, ProviderID: check.ProviderID,
		Status: ports.SCMCIRerunReserved, RequestedAt: o.clock().UTC(),
	}
	reserved, err := o.store.ReserveCIRerunAttempt(ctx, attempt)
	if err != nil {
		o.logger.Error("scm observer: reserve CI rerun attempt failed", "pr", prURL, "check", check.Name, "err", err)
		return ciRerunDispatchFailure
	}
	if !reserved {
		prior, ok, readErr := o.store.GetCIRerunAttempt(ctx, prURL, headSHA, check.Name)
		if readErr != nil {
			o.logger.Error("scm observer: read concurrent CI rerun attempt failed", "pr", prURL, "check", check.Name, "err", readErr)
			return ciRerunDispatchFailure
		}
		if ok && ciRerunAttemptStillPending(prior, check.ProviderID, o.clock().UTC()) {
			return ciRerunStillPending
		}
		return ciRerunDispatchFailure
	}

	if err := o.provider.RerunFailedCheck(ctx, subj.repo, check); err != nil {
		attempt.Status = ports.SCMCIRerunFailed
		if updateErr := o.store.UpdateCIRerunAttempt(ctx, attempt); updateErr != nil {
			o.logger.Error("scm observer: persist failed CI rerun attempt failed", "pr", prURL, "check", check.Name, "err", updateErr)
		}
		o.logger.Error("scm observer: provider CI rerun failed", "pr", prURL, "check", check.Name, "err", err)
		return ciRerunDispatchFailure
	}
	attempt.Status = ports.SCMCIRerunRequested
	if err := o.store.UpdateCIRerunAttempt(ctx, attempt); err != nil {
		// The durable reservation still prevents a duplicate rerun. Surface the
		// write failure, but keep this accepted mutation suppressed for the
		// bounded pending window.
		o.logger.Error("scm observer: persist accepted CI rerun failed", "pr", prURL, "check", check.Name, "err", err)
	}
	o.logger.Info("scm observer: rerunning clearly unrelated or known-flaky CI job", "pr", prURL, "check", check.Name)
	return ciRerunRequested
}

func ciRerunAttemptStillPending(attempt ports.SCMCIRerunAttempt, providerID string, now time.Time) bool {
	if attempt.Status != ports.SCMCIRerunRequested && attempt.Status != ports.SCMCIRerunReserved {
		return false
	}
	if attempt.ProviderID != providerID {
		// A different job/check-run id on the same head is the rerun result.
		return false
	}
	return attempt.RequestedAt.IsZero() || now.Before(attempt.RequestedAt.Add(ciRerunPendingTimeout))
}

func isKnownInfrastructureFlake(check ports.SCMCheckObservation) bool {
	logTail := strings.ToLower(check.LogTail)
	for _, signature := range knownInfrastructureFlakes {
		if strings.Contains(logTail, signature) {
			return true
		}
	}
	return false
}

func isClearlyDiffUnrelatedUnitFailure(check ports.SCMCheckObservation, changedFiles []string) bool {
	matches := failedUnitTestPath.FindAllString(check.LogTail, -1)
	if len(matches) == 0 || len(changedFiles) == 0 {
		return false
	}
	testAreas := map[string]bool{}
	for _, match := range matches {
		area, ok := topLevelArea(match)
		if !ok || packageRelativeRoots[area] {
			return false
		}
		testAreas[area] = true
	}
	for _, changed := range changedFiles {
		area, ok := topLevelArea(changed)
		if !ok || area == ".github" || testAreas[area] {
			return false
		}
	}
	return true
}

func topLevelArea(path string) (string, bool) {
	path = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(path), "\\", "/"), "./")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[0] == ".." {
		return "", false
	}
	return strings.ToLower(parts[0]), true
}
