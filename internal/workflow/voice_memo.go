package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// VoiceMemoFilter matches all supplied fields, without case sensitivity.
// An empty filter deliberately accepts every new, transcribed recording.
type VoiceMemoFilter struct {
	Title   string `json:"title,omitempty"`
	Words   string `json:"words,omitempty"`
	Without string `json:"without,omitempty"`
}

type VoiceMemo struct {
	Source           string      `json:"source"`
	Account          string      `json:"account"`
	ID               string      `json:"recording_id"`
	Title            string      `json:"title"`
	RecordedAt       string      `json:"recorded_at"`
	Duration         json.Number `json:"duration_seconds"`
	Transcript       string      `json:"transcript"`
	TranscriptLength int         `json:"transcript_length"`
}

func (f VoiceMemoFilter) Match(m VoiceMemo) bool {
	has := func(s, sub string) bool {
		return strings.Contains(strings.ToLower(s), strings.ToLower(strings.TrimSpace(sub)))
	}
	return has(m.Title, f.Title) && has(m.Title+"\n"+m.Transcript, f.Words) && (strings.TrimSpace(f.Without) == "" || !has(m.Title+"\n"+m.Transcript, f.Without))
}

func (f VoiceMemoFilter) Specificity() int {
	n := 0
	for _, v := range []string{f.Title, f.Words, f.Without} {
		if strings.TrimSpace(v) != "" {
			n++
		}
	}
	return n
}

// Input never carries more than 1000 Unicode characters of transcript.
func (m VoiceMemo) Input() string {
	r := []rune(m.Transcript)
	total := max(len(r), m.TranscriptLength)
	note := fmt.Sprintf("%d characters", total)
	if total > 1000 {
		note += "; truncated to first 1000 characters"
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	sql := "SELECT transcript FROM marts_voice_memos.recordings WHERE source = " + quote(m.Source) + " AND account = " + quote(m.Account) + " AND recording_id = " + quote(m.ID) + " LIMIT 1"
	// POSIX shell quoting also handles quotes in source identifiers.
	command := "pdw sql '" + strings.ReplaceAll(sql, "'", "'\"'\"'") + "'"
	return fmt.Sprintf("Voice memo: %s\nRecorded: %s\nDuration (seconds): %s\nPDW source: %s; account: %s; recording ID: %s\n\nTranscript preview (%s):\n%s\n\nFetch the full transcript from PDW before acting on details beyond this preview. Exact SQL:\n%s\nCommand:\n%s\n", m.Title, m.RecordedAt, m.Duration, m.Source, m.Account, m.ID, note, string(r[:min(len(r), 1000)]), sql, command)
}
