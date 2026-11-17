package workflow

import (
	"strings"
	"testing"
)

func TestRenderedPromptsRefuseCredentials(t *testing.T) {
	token := "pdw_" + strings.Repeat("a", 24) + "_" + strings.Repeat("X", 30)
	d := Starting
	d.Steps = append([]Step(nil), d.Steps...)
	d.Steps[0].Instructions = "Task: {{input}}"
	for name, render := range map[string]func() (string, error){
		"project":      func() (string, error) { return ProjectPrompt("r", d, token, nil) },
		"orchestrator": func() (string, error) { return OrchestratorPrompt("Saved", d, Vars{Input: token}) },
		"step":         func() (string, error) { return StepMessage(d, 0, Vars{Input: token}) },
	} {
		t.Run(name, func(t *testing.T) {
			p, err := render()
			if err == nil || p != "" {
				t.Error("render must refuse credential and return no prompt")
			}
			if err != nil && strings.Contains(err.Error(), token) {
				t.Error("error disclosed credential")
			}
		})
	}
	// A safe brief names an existing credential source, without reading its value.
	p, err := StepMessage(d, 0, Vars{Input: "Use scoped credentials from PDW_TOKEN_FILE, a 0600 file supplied on your host."})
	if err != nil || !strings.Contains(p, "PDW_TOKEN_FILE") || strings.Contains(p, token) {
		t.Error("safe credential reference failed")
	}
}
