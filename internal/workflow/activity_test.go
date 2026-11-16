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
	prompt := mustOrchestratorPrompt(t, "Saved workflow", d, Vars{Run: "run-1"})
	for _, want := range []string{"at each step change", "outside party", "more than 10 minutes", "never type clock times", "omit --eta", "intelligence workflow status --run run-1 --now"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestActivityRemovesTypedTimes(t *testing.T) {
	for _, line := range []string{"Building since 2026-11-14 23:00 EST.", "Building since 23:00 EST.", "Building at 23:00 EST.", "Building 23:00 EST", "Building (since 23:00 EST)"} {
		got, _, err := activityValues(Move{Now: line})
		if err != nil || got != "Building" {
			t.Errorf("%q => %q, %v", line, got, err)
		}
	}
}

func TestActivityPreservesProseDurationsAndVersions(t *testing.T) {
	for _, line := range []string{"Checking changes since the last review", "Testing 10:30 duration parser", "Testing version 10:30", "Checking changes since v1.2.3", "Testing duration 10:30", "Testing at 10:30 duration parser"} {
		if got := CleanActivity(line); got != line {
			t.Errorf("%q became %q", line, got)
		}
	}
	for _, line := range []string{"Checking since 2026-11-14 23:00 EST", "Checking since 23:15", "Checking (since 9:05 PM ET)"} {
		if got := CleanActivity(line); got != "Checking" {
			t.Errorf("annotation: %q became %q", line, got)
		}
	}
}
