package sessionmanager

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestBuildSystemPromptTextIncludesConfiguredP2ConvergencePolicy(t *testing.T) {
	project := promptProject{ID: "demo", Name: "Demo"}
	policy := domain.ReviewPolicyConfig{P2OnlyRoundLimit: 3}

	for _, tc := range []struct {
		name string
		role sessionPromptRole
		want string
	}{
		{name: "orchestrator", role: sessionPromptRoleOrchestrator, want: "stop routing those low-priority findings back to workers"},
		{name: "worker", role: sessionPromptRoleWorker, want: "stop making fixes solely for those low-priority findings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := buildSystemPromptText(systemPromptConfig{Role: tc.role, Project: project, ReviewPolicy: policy})
			for _, want := range []string{
				"After 3 consecutive completed automated review rounds",
				tc.want,
				"P0/P1 findings, untagged or ambiguous feedback",
				"Do not merge unless the human has authorized merging",
			} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("prompt missing %q:\n%s", want, prompt)
				}
			}
		})
	}
}

func TestBuildSystemPromptTextOmitsDisabledP2ConvergencePolicy(t *testing.T) {
	prompt := buildSystemPromptText(systemPromptConfig{Role: sessionPromptRoleOrchestrator, Project: promptProject{ID: "demo"}})
	if strings.Contains(prompt, "Project Review Convergence Policy") {
		t.Fatalf("disabled review policy leaked into prompt:\n%s", prompt)
	}
}
