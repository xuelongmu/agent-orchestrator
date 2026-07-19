package review

import (
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// FindingLedger summarizes a durable finding history deterministically. Filed
// out-of-scope findings remain visible in TotalFindings but do not participate
// in repetition escalation because they no longer belong to the fix loop.
func FindingLedger(findings []domain.ReviewFinding) (domain.FindingLedgerSummary, string) {
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
	simplificationClass := ""
	if len(classes) > 0 && classes[0].Count >= 3 {
		simplificationClass = classes[0].ClassTag
	}
	return domain.FindingLedgerSummary{
		TotalFindings: len(findings), Rounds: len(rounds), Classes: classes,
	}, simplificationClass
}
