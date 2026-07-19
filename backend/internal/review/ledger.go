package review

import (
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// FindingLedger summarizes a durable finding history deterministically. Filed
// out-of-scope findings remain visible in TotalFindings but do not participate
// in class counts because they no longer belong to the fix loop.
func FindingLedger(findings []domain.ReviewFinding) domain.FindingLedgerSummary {
	rounds := map[int]struct{}{}
	counts := map[string]int{}
	for _, finding := range findings {
		rounds[finding.Round] = struct{}{}
		if finding.OutOfScope && finding.DeferredIssueURL != "" {
			continue
		}
		tag := strings.TrimSpace(finding.ClassTag)
		if tag != "" {
			counts[tag]++
		}
	}
	classes := make([]domain.FindingClassCount, 0, len(counts))
	for tag, count := range counts {
		classes = append(classes, domain.FindingClassCount{ClassTag: tag, Count: count})
	}
	sort.Slice(classes, func(i, j int) bool {
		if classes[i].Count != classes[j].Count {
			return classes[i].Count > classes[j].Count
		}
		return classes[i].ClassTag < classes[j].ClassTag
	})
	return domain.FindingLedgerSummary{
		TotalFindings: len(findings), Rounds: len(rounds), Classes: classes,
	}
}

// SimplificationClassForRun selects a repeated class only when it occurs in
// the current run. This prevents an old threshold-crossing class from forcing
// unrelated later findings into simplification mode.
func SimplificationClassForRun(findings []domain.ReviewFinding, runID string) string {
	current := map[string]bool{}
	for _, finding := range findings {
		if finding.RunID == runID && (!finding.OutOfScope || finding.DeferredIssueURL == "") {
			current[strings.TrimSpace(finding.ClassTag)] = true
		}
	}
	ledger := FindingLedger(findings)
	for _, class := range ledger.Classes {
		if class.Count >= 3 && current[class.ClassTag] {
			return class.ClassTag
		}
	}
	return ""
}
