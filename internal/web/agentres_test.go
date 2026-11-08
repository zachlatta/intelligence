package web

import (
	"strings"
	"testing"
)

func TestTranscriptPageShowsTheAgentsResources(t *testing.T) {
	h, d := dbServer(t)
	seedHost(t, d)
	rec := get(t, h, "/devboxes/devbox-1/agents/"+agentA1, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	// 30 minutes of 6 CPU seconds each, 3 GB, 4 processes, 10 commands a minute, one kill.
	for _, want := range []string{`id="resources"`, "CPU 3m 0s", "0.10 cores average", "3.0 GB now", "3.0 GB peak",
		"4 processes", "300 commands", "30 minutes measured", "exact (eBPF)", "<polyline", "OOM kill", "node", "9.0 GB",
		"24 CPUs"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript page misses %q", want)
		}
	}
	frag := get(t, h, "/devboxes/devbox-1/agents/"+agentA1+"/resources", nil)
	if frag.Code != 200 || !strings.Contains(frag.Body.String(), "CPU 3m 0s") || strings.Contains(frag.Body.String(), "<html") {
		t.Errorf("resources fragment: %d %s", frag.Code, frag.Body.String())
	}
}

func TestTranscriptPageWithoutResourcesSaysSo(t *testing.T) {
	h := reported(t)
	body := get(t, h, "/devboxes/devbox-1/agents/"+agentA1, nil).Body.String()
	if !strings.Contains(body, "No resources recorded") {
		t.Errorf("an agent with no minutes should say it has none")
	}
}
