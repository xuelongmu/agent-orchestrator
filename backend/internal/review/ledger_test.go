package review

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestFindingLedgerTriggersThirdActiveOccurrence(t *testing.T) {
	findings := []domain.ReviewFinding{
		{Round: 1, ClassTag: "missing-notify"},
		{Round: 2, ClassTag: "missing-notify"},
		{Round: 3, ClassTag: "missing-notify"},
		{Round: 3, ClassTag: "persistence", OutOfScope: true, DeferredIssueURL: "https://github.com/o/r/issues/9"},
	}
	ledger, classTag := FindingLedger(findings)
	if ledger.TotalFindings != 4 || ledger.Rounds != 3 || classTag != "missing-notify" {
		t.Fatalf("FindingLedger = %+v class=%q", ledger, classTag)
	}
	if len(ledger.Classes) != 1 || ledger.Classes[0].Count != 3 {
		t.Fatalf("active classes = %+v", ledger.Classes)
	}
}

func TestCurrentRunFindingsDeflected(t *testing.T) {
	findings := []domain.ReviewFinding{{RunID: "run-1", OutOfScope: true, DeferredIssueURL: "issue", ThreadID: "thread", ThreadResolved: true}}
	if !currentRunFindingsDeflected(findings, "run-1") {
		t.Fatal("expected fully filed and resolved run to be deflected")
	}
	findings[0].ThreadResolved = false
	if currentRunFindingsDeflected(findings, "run-1") {
		t.Fatal("unresolved thread must remain in the fix loop")
	}
}
