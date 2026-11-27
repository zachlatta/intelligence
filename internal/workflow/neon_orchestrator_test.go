package workflow

import (
	"encoding/json"
	"testing"
)

func TestTheStartingOrchestratorRunsOnNeonMedium(t *testing.T) {
	o := Starting.Orchestrator
	if o.Provider != "antigravity" || o.Model != "gemini-4-neon-medium" {
		t.Errorf("starting orchestrator = %s/%s, want antigravity/gemini-4-neon-medium", o.Provider, o.Model)
	}
	// Antigravity takes its effort from the model id; a thinking level would not start.
	if o.Thinking != "" {
		t.Errorf("starting orchestrator thinking = %q, want empty", o.Thinking)
	}
	if o.Instructions != OrchestratorInstructions {
		t.Errorf("starting orchestrator instructions changed")
	}
	// The project picker is out of scope and stays on Opus 5.5.
	if p := Starting.Project; p.Provider != "claude" || p.Model != "claude-opus-5-5" || p.Thinking != "medium" {
		t.Errorf("starting project finder = %s/%s/%s, want claude/claude-opus-5-5/medium", p.Provider, p.Model, p.Thinking)
	}
}

func TestTheNeonStartingWorkflowStillValidatesAndRoundTrips(t *testing.T) {
	d := onHost(clone(Starting))
	if err := d.Validate(); err != nil {
		t.Fatalf("Validate(Starting on a host) = %v", err)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse(Starting) = %v", err)
	}
	if p.Orchestrator.Provider != "antigravity" || p.Orchestrator.Model != "gemini-4-neon-medium" || p.Orchestrator.Thinking != "" {
		t.Errorf("round-tripped orchestrator = %+v", p.Orchestrator)
	}
	if agentMode(p.Orchestrator.Provider) != "full-access" {
		t.Errorf("orchestrator mode = %q, want antigravity's full-access", agentMode(p.Orchestrator.Provider))
	}
	if _, err := OrchestratorPrompt("Starting", p, Vars{Run: "run-1", Input: "a task"}); err != nil {
		t.Errorf("OrchestratorPrompt = %v", err)
	}
}
