package workflow

import (
	"strings"
	"testing"
)

func TestActivityCLIValidation(t *testing.T) {
	for _, args := range [][]string{{"status", "--run", "r"}, {"status", "--run", "r", "--now", "two\nlines"}, {"status", "--run", "r", "--now", "Waiting.", "--eta", "tomorrow"}} {
		if _, _, err := parseCLI(args, strings.NewReader("")); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestSavedOrchestratorsGetActivityContract(t *testing.T) {
	d := Starting
	d.Orchestrator.Instructions = "Saved custom instructions."
	prompt := OrchestratorPrompt("Saved workflow", d, Vars{Run: "run-1"})
	for _, want := range []string{"at each step change", "outside party", "more than 10 minutes", "since when", "omit --eta", "intelligence workflow status --run run-1 --now"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing %q", want)
		}
	}
}
