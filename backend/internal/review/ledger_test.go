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
	for i := range findings {
		findings[i].RunID = "run-3"
	}
	ledger := FindingLedger(findings)
	classTag := SimplificationClassForRun(findings, "run-3")
	if ledger.TotalFindings != 4 || ledger.Rounds != 3 || classTag != "missing-notify" {
		t.Fatalf("FindingLedger = %+v class=%q", ledger, classTag)
	}
	if len(ledger.Classes) != 1 || ledger.Classes[0].Count != 3 {
		t.Fatalf("active classes = %+v", ledger.Classes)
	}
}

func TestSimplificationClassRequiresCurrentOccurrence(t *testing.T) {
	findings := []domain.ReviewFinding{
		{RunID: "run-1", Round: 1, ClassTag: "missing-notify"},
		{RunID: "run-2", Round: 2, ClassTag: "missing-notify"},
		{RunID: "run-3", Round: 3, ClassTag: "missing-notify"},
		{RunID: "run-4", Round: 4, ClassTag: "nil-safety"},
	}
	if got := SimplificationClassForRun(findings, "run-4"); got != "" {
		t.Fatalf("historical class selected for unrelated run: %q", got)
	}
	if got := SimplificationClassForRun(findings, "run-3"); got != "missing-notify" {
		t.Fatalf("threshold run class = %q", got)
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
	findings[0].ThreadResolved = true
	findings[0].ThreadID = ""
	if currentRunFindingsDeflected(findings, "run-1") {
		t.Fatal("finding without a bound thread must remain in the fix loop")
	}
}
