// Package pdwsync checks Personal Data Warehouse (PDW) source sync health
// using read-only SQL against marts_ops.pipeline_health via the local `pdw` CLI.
package pdwsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// CLIUsage is `intelligence pdw`'s help text.
const CLIUsage = `usage: intelligence pdw sync-health --source <slack|gmail|voice-memos> [--json]

Reports a PDW source's last successful sync time, age and verdict (ok, behind or broken)
by querying marts_ops.pipeline_health read-only through the local pdw CLI.`

// Verdict summarizes whether a PDW source is syncing normally, lagging behind, or broken.
type Verdict string

const (
	VerdictOK     Verdict = "ok"
	VerdictBehind Verdict = "behind"
	VerdictBroken Verdict = "broken"
)

// PipelineRow mirrors the read-only columns selected from marts_ops.pipeline_health.
type PipelineRow struct {
	Pipeline                    string     `json:"pipeline"`
	Label                       string     `json:"label"`
	Status                      string     `json:"status"`
	RunStatus                   string     `json:"run_status"`
	DataStatus                  string     `json:"data_status"`
	LastRunAt                   *time.Time `json:"last_run_at"`
	LastWriteAt                 *time.Time `json:"last_write_at"`
	NewestEventAt               *time.Time `json:"newest_event_at"`
	RunAgeSeconds               *int64     `json:"run_age_seconds"`
	DataAgeSeconds              *int64     `json:"data_age_seconds"`
	ExpectedRunIntervalSeconds  *int64     `json:"expected_run_interval_seconds"`
	ExpectedDataIntervalSeconds *int64     `json:"expected_data_interval_seconds"`
	StateErrorRows              *int64     `json:"state_error_rows"`
	StateAttentionRows          *int64     `json:"state_attention_rows"`
	LastError                   *string    `json:"last_error"`
	LastErrorAt                 *time.Time `json:"last_error_at"`
	SLADetectorStatus           *string    `json:"sla_detector_status"`
	SnapshotAgeSeconds          *int64     `json:"snapshot_age_seconds"`
}

// PipelineReport is the evaluated health of one PDW pipeline.
type PipelineReport struct {
	Pipeline        string     `json:"pipeline"`
	Label           string     `json:"label,omitempty"`
	Verdict         Verdict    `json:"verdict"`
	LastSyncAt      *time.Time `json:"last_sync_at,omitempty"`
	AgeSeconds      *int64     `json:"age_seconds,omitempty"`
	Age             string     `json:"age,omitempty"`
	LastWriteAt     *time.Time `json:"last_write_at,omitempty"`
	NewestEventAt   *time.Time `json:"newest_event_at,omitempty"`
	PDWStatus       string     `json:"pdw_status,omitempty"`
	SLADetector     string     `json:"sla_detector_status,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	Reason          string     `json:"reason"`
	BehindThreshold string     `json:"behind_threshold"`
	BrokenThreshold string     `json:"broken_threshold"`
}

// Report is the evaluated health of a logical PDW source (one or more pipelines).
type Report struct {
	Source     string           `json:"source"`
	CheckedAt  time.Time        `json:"checked_at"`
	Verdict    Verdict          `json:"verdict"`
	LastSyncAt *time.Time       `json:"last_sync_at,omitempty"`
	AgeSeconds *int64           `json:"age_seconds,omitempty"`
	Age        string           `json:"age,omitempty"`
	LastError  string           `json:"last_error,omitempty"`
	Reason     string           `json:"reason"`
	Pipelines  []PipelineReport `json:"pipelines"`
}

// QueryFunc runs a read-only PDW SQL query and decodes pipeline health rows.
type QueryFunc func(ctx context.Context, why, sql string) ([]PipelineRow, error)

var nowFunc = func() time.Time { return time.Now().UTC() }

// NormalizeSource canonicalizes a --source flag value and returns its PDW pipeline names.
func NormalizeSource(raw string) (source string, pipelines []string, err error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "slack":
		return "slack", []string{"slack"}, nil
	case "gmail":
		return "gmail", []string{"gmail"}, nil
	case "voice-memos", "voice_memos":
		return "voice-memos", []string{"alice_voice_recordings", "apple_voice_memos", "google_recorder"}, nil
	default:
		return "", nil, fmt.Errorf("unknown --source %q: want slack, gmail or voice-memos", raw)
	}
}

// BuildQuery constructs the read-only SQL query against marts_ops.pipeline_health.
func BuildQuery(pipelines []string) string {
	quoted := make([]string, len(pipelines))
	for i, p := range pipelines {
		quoted[i] = "'" + strings.ReplaceAll(p, "'", "''") + "'"
	}
	return "SELECT pipeline, label, status, run_status, data_status, " +
		"last_run_at, last_write_at, newest_event_at, " +
		"run_age_seconds, data_age_seconds, " +
		"expected_run_interval_seconds, expected_data_interval_seconds, " +
		"state_error_rows, state_attention_rows, " +
		"last_error, last_error_at, " +
		"sla_detector_status, snapshot_age_seconds " +
		"FROM marts_ops.pipeline_health " +
		"WHERE pipeline IN (" + strings.Join(quoted, ", ") + ") " +
		"ORDER BY pipeline"
}

// ExecQuery executes a read-only SQL query through the local `pdw` CLI.
func ExecQuery(ctx context.Context, why, sql string) ([]PipelineRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pdw", "sql", "--output", "json", "-q", why, sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return nil, fmt.Errorf("pdw sql failed: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("pdw sql failed: %w", err)
	}
	var rows []PipelineRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		return nil, fmt.Errorf("decode pdw sql json: %w", err)
	}
	return rows, nil
}

// Evaluate computes a source's sync-health verdict from marts_ops.pipeline_health rows.
// It is pure and deterministic for a given now timestamp.
func Evaluate(source string, rows []PipelineRow, now time.Time) Report {
	now = now.UTC()
	rep := Report{
		Source:    source,
		CheckedAt: now,
		Verdict:   VerdictOK,
	}
	if len(rows) == 0 {
		rep.Verdict = VerdictBroken
		rep.Reason = fmt.Sprintf("no pipeline health row found in PDW for %s", source)
		return rep
	}

	rep.Pipelines = make([]PipelineReport, 0, len(rows))
	for _, row := range rows {
		rep.Pipelines = append(rep.Pipelines, evaluatePipeline(row, now))
	}

	// Pick worst verdict across pipelines (broken > behind > ok).
	worst := rep.Pipelines[0]
	var badReasons []string
	for _, p := range rep.Pipelines {
		if severity(p.Verdict) > severity(worst.Verdict) {
			worst = p
		} else if severity(p.Verdict) == severity(worst.Verdict) {
			if p.Verdict == VerdictOK {
				// When all ok, report the most recent sync across the source's pipelines.
				if p.LastSyncAt != nil && (worst.LastSyncAt == nil || p.LastSyncAt.After(*worst.LastSyncAt)) {
					worst = p
				}
			} else {
				// When behind/broken, prefer the stalest (oldest) sync timestamp.
				if worst.LastSyncAt != nil && (p.LastSyncAt == nil || p.LastSyncAt.Before(*worst.LastSyncAt)) {
					worst = p
				}
			}
		}
		if p.Verdict != VerdictOK {
			badReasons = append(badReasons, p.Reason)
		}
	}

	rep.Verdict = worst.Verdict
	rep.LastSyncAt = worst.LastSyncAt
	rep.AgeSeconds = worst.AgeSeconds
	rep.Age = worst.Age
	rep.LastError = worst.LastError
	if len(rep.Pipelines) == 1 {
		rep.Reason = worst.Reason
	} else if len(badReasons) > 0 {
		rep.Reason = strings.Join(badReasons, "; ")
	} else {
		rep.Reason = fmt.Sprintf("all %d %s pipelines ok (most recent sync %s ago)", len(rep.Pipelines), source, rep.Age)
	}
	return rep
}

func evaluatePipeline(row PipelineRow, now time.Time) PipelineReport {
	behindThresh, brokenThresh := thresholds(row)
	pr := PipelineReport{
		Pipeline:        row.Pipeline,
		Label:           row.Label,
		Verdict:         VerdictOK,
		LastWriteAt:     utcPtr(row.LastWriteAt),
		NewestEventAt:   utcPtr(row.NewestEventAt),
		PDWStatus:       strings.ToLower(strings.TrimSpace(row.Status)),
		BehindThreshold: formatDuration(behindThresh),
		BrokenThreshold: formatDuration(brokenThresh),
	}
	if row.SLADetectorStatus != nil {
		pr.SLADetector = strings.ToLower(strings.TrimSpace(*row.SLADetectorStatus))
	}
	if row.LastError != nil {
		pr.LastError = strings.TrimSpace(*row.LastError)
	}

	// Prefer last_run_at (poller/uploader heartbeat), falling back to last_write_at then newest_event_at.
	switch {
	case row.LastRunAt != nil:
		pr.LastSyncAt = utcPtr(row.LastRunAt)
	case row.LastWriteAt != nil:
		pr.LastSyncAt = utcPtr(row.LastWriteAt)
	case row.NewestEventAt != nil:
		pr.LastSyncAt = utcPtr(row.NewestEventAt)
	}

	var age time.Duration
	if pr.LastSyncAt != nil {
		age = now.Sub(*pr.LastSyncAt)
		if age < 0 {
			age = 0
		}
		sec := int64(age.Seconds())
		pr.AgeSeconds = &sec
		pr.Age = formatDuration(age)
	}

	runStatus := strings.ToLower(strings.TrimSpace(row.RunStatus))
	status := pr.PDWStatus
	errRows := int64Val(row.StateErrorRows)
	attnRows := int64Val(row.StateAttentionRows)
	hasActiveError := pr.LastError != "" && (status != "ok" || row.LastErrorAt == nil || pr.LastSyncAt == nil || !row.LastErrorAt.Before(*pr.LastSyncAt))

	var brokenReasons []string
	if isBrokenStatus(status) {
		brokenReasons = append(brokenReasons, "status is "+status)
	}
	if isBrokenStatus(runStatus) && runStatus != status {
		brokenReasons = append(brokenReasons, "run_status is "+runStatus)
	}
	if errRows > 0 {
		brokenReasons = append(brokenReasons, fmt.Sprintf("%d state error row(s)", errRows))
	}
	if attnRows > 0 {
		brokenReasons = append(brokenReasons, fmt.Sprintf("%d state attention row(s)", attnRows))
	}
	if hasActiveError {
		brokenReasons = append(brokenReasons, "error: "+pr.LastError)
	}
	if pr.LastSyncAt == nil || status == "no_data" {
		brokenReasons = append(brokenReasons, "no last sync timestamp recorded")
	} else if age >= brokenThresh {
		brokenReasons = append(brokenReasons, fmt.Sprintf("last sync %s ago (broken threshold >= %s)", pr.Age, pr.BrokenThreshold))
	}

	if len(brokenReasons) > 0 {
		pr.Verdict = VerdictBroken
		pr.Reason = fmt.Sprintf("%s: %s", row.Pipeline, strings.Join(brokenReasons, "; "))
		return pr
	}

	var behindReasons []string
	if status == "late" || status == "unknown" {
		behindReasons = append(behindReasons, "status is "+status)
	}
	if (runStatus == "late" || runStatus == "unknown") && runStatus != status {
		behindReasons = append(behindReasons, "run_status is "+runStatus)
	}
	if pr.LastSyncAt != nil && age >= behindThresh {
		behindReasons = append(behindReasons, fmt.Sprintf("last sync %s ago (behind threshold >= %s)", pr.Age, pr.BehindThreshold))
	}
	if isBehindSLA(pr.SLADetector) {
		behindReasons = append(behindReasons, fmt.Sprintf("poller ran %s ago, but SLA detector reports %s", pr.Age, pr.SLADetector))
	}

	if len(behindReasons) > 0 {
		pr.Verdict = VerdictBehind
		pr.Reason = fmt.Sprintf("%s: %s", row.Pipeline, strings.Join(behindReasons, "; "))
		return pr
	}

	pr.Verdict = VerdictOK
	pr.Reason = fmt.Sprintf("%s synced %s ago (status %s; thresholds: behind >= %s, broken >= %s)",
		row.Pipeline, pr.Age, orDefault(status, "ok"), pr.BehindThreshold, pr.BrokenThreshold)
	return pr
}

func thresholds(row PipelineRow) (behind, broken time.Duration) {
	if row.ExpectedRunIntervalSeconds != nil && *row.ExpectedRunIntervalSeconds > 0 {
		behind = time.Duration(*row.ExpectedRunIntervalSeconds) * time.Second
	} else {
		switch row.Pipeline {
		case "gmail":
			behind = 10 * time.Minute
		case "alice_voice_recordings":
			behind = 48 * time.Hour
		default:
			behind = 30 * time.Minute
		}
	}
	broken = 4 * behind
	if broken < 2*time.Hour {
		broken = 2 * time.Hour
	}
	return behind, broken
}

func isBrokenStatus(s string) bool {
	switch s {
	case "failing", "attention", "action_required", "error", "stale":
		return true
	}
	return false
}

func isBehindSLA(s string) bool {
	switch s {
	case "late", "stale", "failing", "attention":
		return true
	}
	return false
}

func severity(v Verdict) int {
	switch v {
	case VerdictBroken:
		return 2
	case VerdictBehind:
		return 1
	default:
		return 0
	}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func int64Val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int64(d.Seconds()))
	}
	if d < time.Hour {
		m := int64(d / time.Minute)
		s := int64((d % time.Minute) / time.Second)
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := int64(d / time.Hour)
	m := int64((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// Format renders r as compact key-value text for agents and operators.
func (r Report) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "source: %s\n", r.Source)
	fmt.Fprintf(&b, "verdict: %s\n", r.Verdict)
	if r.LastSyncAt != nil {
		fmt.Fprintf(&b, "last_sync_at: %s\n", r.LastSyncAt.UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "age: %s\n", r.Age)
	} else {
		b.WriteString("last_sync_at: unknown\n")
	}
	if r.LastError != "" {
		fmt.Fprintf(&b, "last_error: %s\n", r.LastError)
	}
	fmt.Fprintf(&b, "reason: %s\n", r.Reason)
	if len(r.Pipelines) > 1 {
		b.WriteString("pipelines:\n")
		for _, p := range r.Pipelines {
			syncStr := "unknown"
			if p.LastSyncAt != nil {
				syncStr = fmt.Sprintf("%s, age %s", p.LastSyncAt.UTC().Format(time.RFC3339), p.Age)
			}
			extra := ""
			if p.LastError != "" {
				extra = "; error: " + p.LastError
			}
			fmt.Fprintf(&b, "  - %s: %s (last sync %s; behind >= %s, broken >= %s%s)\n",
				p.Pipeline, p.Verdict, syncStr, p.BehindThreshold, p.BrokenThreshold, extra)
		}
	}
	return b.String()
}

// CLI runs `intelligence pdw args...` and returns its process exit code.
func CLI(ctx context.Context, args []string, stdout, stderr io.Writer, query QueryFunc) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		if len(args) == 0 {
			fmt.Fprintf(stderr, "missing subcommand\n\n%s\n", CLIUsage)
			return 2
		}
		fmt.Fprintln(stdout, CLIUsage)
		return 0
	}
	if args[0] != "sync-health" {
		fmt.Fprintf(stderr, "unknown pdw subcommand %q\n\n%s\n", args[0], CLIUsage)
		return 2
	}

	fs := flag.NewFlagSet("intelligence pdw sync-health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sourceFlag := fs.String("source", "", "PDW source: slack, gmail or voice-memos")
	asJSON := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, CLIUsage)
			return 0
		}
		fmt.Fprintf(stderr, "%v\n\n%s\n", err, CLIUsage)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n\n%s\n", fs.Arg(0), CLIUsage)
		return 2
	}
	if strings.TrimSpace(*sourceFlag) == "" {
		fmt.Fprintf(stderr, "--source is required\n\n%s\n", CLIUsage)
		return 2
	}
	source, pipelines, err := NormalizeSource(*sourceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s\n", err, CLIUsage)
		return 2
	}
	if query == nil {
		query = ExecQuery
	}

	rows, err := query(ctx, "check PDW sync health for "+source, BuildQuery(pipelines))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	rep := Evaluate(source, rows, nowFunc())
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stdout, rep.Format())
	return 0
}
