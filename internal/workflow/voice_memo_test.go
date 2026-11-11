package workflow

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestVoiceMemoFilterAndBoundedInput(t *testing.T) {
	d := Starting
	d.Host = "host-1"
	d.Trigger = TriggerVoiceMemo
	d.VoiceMemo = &VoiceMemoFilter{Title: " ideas ", Words: "build", Without: "private"}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	m := VoiceMemo{Source: "apple_voice_memos", Account: "owner", ID: "memo'1", Title: "Ideas", Transcript: strings.Repeat("界", 1000) + "build SECRET_SUFFIX", TranscriptLength: 1019}
	if !d.VoiceMemo.Match(m) {
		t.Fatal("should match")
	}
	input := m.Input()
	m.Transcript += "private"
	if d.VoiceMemo.Match(m) {
		t.Fatal("excluded text matched")
	}
	if strings.Contains(input, "SECRET_SUFFIX") || !strings.Contains(input, "truncated") || !strings.Contains(input, "1019 characters") || !strings.Contains(input, "memo''1") || !utf8.ValidString(input) {
		t.Fatal(input)
	}
	if !strings.Contains(input, strings.Repeat("界", 1000)) {
		t.Fatal("preview split unicode characters")
	}
	d.Trigger = TriggerTask
	if d.Validate() == nil {
		t.Fatal("voice memo filter on task accepted")
	}
}

func TestVoiceMemoPreviewBoundary(t *testing.T) {
	for _, n := range []int{0, 999, 1000, 1001} {
		m := VoiceMemo{Transcript: strings.Repeat("é", n)}
		input := m.Input()
		if strings.Contains(input, "truncated") != (n > 1000) {
			t.Errorf("length %d: incorrect truncation notice", n)
		}
		if strings.Contains(input, strings.Repeat("é", 1001)) {
			t.Fatal("unbounded preview")
		}
	}
}

func TestVoiceMemoIncludesBoundedPDWSummary(t *testing.T) {
	m := VoiceMemo{Summary: strings.Repeat("界", 1000) + "summary-tail", Transcript: "hello"}
	input := m.Input()
	if !strings.Contains(input, "PDW summary") || !strings.Contains(input, strings.Repeat("界", 1000)) || strings.Contains(input, "summary-tail") {
		t.Fatal("missing or unbounded summary")
	}
}
