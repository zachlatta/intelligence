package workflow

import (
	"encoding/json"
	"testing"
)

func TestAVariantTakesItsSourceWithItsOwnModelAndTrigger(t *testing.T) {
	t.Parallel()
	source := Starting
	source.Host = "host-1"
	source.Priority = PriorityImmediate
	source.Trigger = TriggerEmail
	source.Email = &EmailFilter{From: []string{"zach@example.com"}}
	source.Orchestrator.Instructions = "The source's prompt."
	source.Steps = []Step{{ID: "do", Kind: KindStep, Title: "Do", Instructions: "The source's step."}}

	own := Starting
	own.Host = "elsewhere"
	own.Trigger = TriggerEmail
	own.Email = &EmailFilter{From: []string{"zach@example.com"}, To: "bot+fast@example.com"}
	own.APITrigger = true
	own.Orchestrator = Orchestrator{Provider: "codex", Model: "gpt-6.2-sol", Thinking: "high", ServiceTier: "ultrafast", Instructions: "A stale copy of the prompt."}
	own.Steps = []Step{{ID: "old", Kind: KindStep, Title: "Old", Instructions: "A stale copy of the step."}}

	got := own.AsVariantOf(source)
	want := source
	want.Email = own.Email
	want.APITrigger = true
	want.Orchestrator = Orchestrator{Provider: "codex", Model: "gpt-6.2-sol", Thinking: "high", ServiceTier: "ultrafast", Instructions: "The source's prompt."}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("variant =\n%s\nwant\n%s", g, w)
	}
	if len(source.Steps) != 1 || source.Steps[0].ID != "do" || source.Email.To != "" {
		t.Fatalf("composing changed the source: %+v", source)
	}
	if !got.InheritsFrom(source) {
		t.Fatal("a composed variant does not read as inheriting from its source")
	}
	if own.InheritsFrom(source) {
		t.Fatal("a stale copy reads as inheriting from its source")
	}
	got.Steps[0].Title = "Changed"
	if source.Steps[0].Title != "Do" {
		t.Fatal("the variant shares its source's steps")
	}
}
