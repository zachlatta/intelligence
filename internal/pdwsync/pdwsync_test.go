package pdwsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func ptrTime(t time.Time) *time.Time { return &t }
func ptrInt64(n int64) *int64        { return &n }
func ptrStr(s string) *string        { return &s }

func TestEvaluateVerdicts(t *testing.T) {
	now := time.Date(2026, 11, 22, 17, 15, 0, 0, time.UTC)

	t.Run("slack ok", func(t *testing.T) {
		rows := []PipelineRow{{
			Pipeline:                   "slack",
			Label:                      "Slack",
			Status:                     "ok",
			RunStatus:                  "ok",
			DataStatus:                 "ok",
			LastRunAt:                  ptrTime(now.Add(-4 * time.Minute)),
			LastWriteAt:                ptrTime(now.Add(-4 * time.Minute)),
			ExpectedRunIntervalSeconds: ptrInt64(1800),
		}}
		r := Evaluate("slack", rows, now)
		if r.Verdict != VerdictOK {
			t.Fatalf("verdict = %s, want ok (reason: %s)", r.Verdict, r.Reason)
		}
		if r.LastSyncAt == nil || !r.LastSyncAt.Equal(now.Add(-4*time.Minute)) {
			t.Fatalf("last_sync_at = %v", r.LastSyncAt)
		}
		if r.AgeSeconds == nil || *r.AgeSeconds != 240 {
			t.Fatalf("age_seconds = %v, want 240", r.AgeSeconds)
		}
	})

	t.Run("slack behind by age", func(t *testing.T) {
		rows := []PipelineRow{{
			Pipeline:                   "slack",
			Status:                     "ok",
			RunStatus:                  "ok",
			LastRunAt:                  ptrTime(now.Add(-45 * time.Minute)),
			ExpectedRunIntervalSeconds: ptrInt64(1800),
		}}
		r := Evaluate("slack", rows, now)
		if r.Verdict != VerdictBehind {
			t.Fatalf("verdict = %s, want behind (reason: %s)", r.Verdict, r.Reason)
		}
	})

	t.Run("slack behind by sla detector while poller ok", func(t *testing.T) {
		rows := []PipelineRow{{
			Pipeline:                   "slack",
			Status:                     "ok",
			RunStatus:                  "ok",
			LastRunAt:                  ptrTime(now.Add(-3 * time.Minute)),
			ExpectedRunIntervalSeconds: ptrInt64(1800),
			SLADetectorStatus:          ptrStr("stale"),
		}}
		r := Evaluate("slack", rows, now)
		if r.Verdict != VerdictBehind {
			t.Fatalf("verdict = %s, want behind (reason: %s)", r.Verdict, r.Reason)
		}
		if !strings.Contains(r.Reason, "SLA") {
			t.Fatalf("reason = %q, want mention of SLA detector", r.Reason)
		}
	})

	t.Run("slack broken by attention and error", func(t *testing.T) {
		rows := []PipelineRow{{
			Pipeline:                   "slack",
			Status:                     "attention",
			RunStatus:                  "failing",
			LastRunAt:                  ptrTime(now.Add(-6 * time.Hour)),
			ExpectedRunIntervalSeconds: ptrInt64(1800),
			LastError:                  ptrStr("token_revoked: Slack OAuth needs reconnect"),
			LastErrorAt:                ptrTime(now.Add(-10 * time.Minute)),
		}}
		r := Evaluate("slack", rows, now)
		if r.Verdict != VerdictBroken {
			t.Fatalf("verdict = %s, want broken (reason: %s)", r.Verdict, r.Reason)
		}
		if !strings.Contains(r.Reason, "token_revoked") {
			t.Fatalf("reason = %q, want error detail", r.Reason)
		}
	})

	t.Run("slack broken by age threshold", func(t *testing.T) {
		rows := []PipelineRow{{
			Pipeline:                   "slack",
			Status:                     "ok",
			RunStatus:                  "ok",
			LastRunAt:                  ptrTime(now.Add(-3 * time.Hour)),
			ExpectedRunIntervalSeconds: ptrInt64(1800),
		}}
		r := Evaluate("slack", rows, now)
		if r.Verdict != VerdictBroken {
			t.Fatalf("verdict = %s, want broken (reason: %s)", r.Verdict, r.Reason)
		}
	})

	t.Run("gmail behind and broken thresholds", func(t *testing.T) {
		behind := Evaluate("gmail", []PipelineRow{{
			Pipeline:                   "gmail",
			Status:                     "ok",
			RunStatus:                  "ok",
			LastRunAt:                  ptrTime(now.Add(-15 * time.Minute)),
			ExpectedRunIntervalSeconds: ptrInt64(600),
		}}, now)
		if behind.Verdict != VerdictBehind {
			t.Fatalf("gmail 15m: verdict = %s, want behind", behind.Verdict)
		}
		broken := Evaluate("gmail", []PipelineRow{{
			Pipeline:                   "gmail",
			Status:                     "ok",
			RunStatus:                  "ok",
			LastRunAt:                  ptrTime(now.Add(-2*time.Hour - time.Minute)),
			ExpectedRunIntervalSeconds: ptrInt64(600),
		}}, now)
		if broken.Verdict != VerdictBroken {
			t.Fatalf("gmail 2h1m: verdict = %s, want broken", broken.Verdict)
		}
	})

	t.Run("voice-memos aggregates multiple pipelines and ignores unknown sla", func(t *testing.T) {
		rows := []PipelineRow{
			{
				Pipeline:                   "alice_voice_recordings",
				Status:                     "ok",
				RunStatus:                  "ok",
				LastRunAt:                  ptrTime(now.Add(-13 * time.Hour)),
				ExpectedRunIntervalSeconds: ptrInt64(172800),
				SLADetectorStatus:          ptrStr("ok"),
			},
			{
				Pipeline:                   "apple_voice_memos",
				Status:                     "ok",
				RunStatus:                  "ok",
				LastRunAt:                  ptrTime(now.Add(-6 * time.Minute)),
				ExpectedRunIntervalSeconds: ptrInt64(1800),
				SLADetectorStatus:          ptrStr("unknown"),
			},
			{
				Pipeline:                   "google_recorder",
				Status:                     "ok",
				RunStatus:                  "ok",
				LastRunAt:                  ptrTime(now.Add(-4 * time.Minute)),
				ExpectedRunIntervalSeconds: ptrInt64(1800),
				SLADetectorStatus:          ptrStr("ok"),
			},
		}
		r := Evaluate("voice-memos", rows, now)
		if r.Verdict != VerdictOK {
			t.Fatalf("verdict = %s, want ok (reason: %s)", r.Verdict, r.Reason)
		}
		if len(r.Pipelines) != 3 {
			t.Fatalf("pipelines = %d, want 3", len(r.Pipelines))
		}

		// Break one pipeline: overall verdict becomes broken.
		rows[2].Status = "failing"
		rows[2].LastError = ptrStr("uploader failed")
		r2 := Evaluate("voice-memos", rows, now)
		if r2.Verdict != VerdictBroken {
			t.Fatalf("verdict = %s, want broken", r2.Verdict)
		}
		if !strings.Contains(r2.Reason, "google_recorder") {
			t.Fatalf("reason = %q, want google_recorder named", r2.Reason)
		}
	})

	t.Run("no rows is broken", func(t *testing.T) {
		r := Evaluate("slack", nil, now)
		if r.Verdict != VerdictBroken {
			t.Fatalf("verdict = %s, want broken", r.Verdict)
		}
	})

	t.Run("falls back to last_write_at when last_run_at is nil", func(t *testing.T) {
		rows := []PipelineRow{{
			Pipeline:                   "slack",
			Status:                     "ok",
			LastWriteAt:                ptrTime(now.Add(-5 * time.Minute)),
			ExpectedRunIntervalSeconds: ptrInt64(1800),
		}}
		r := Evaluate("slack", rows, now)
		if r.Verdict != VerdictOK || r.LastSyncAt == nil || !r.LastSyncAt.Equal(now.Add(-5*time.Minute)) {
			t.Fatalf("fallback failed: %+v", r)
		}
	})
}

func TestCLI(t *testing.T) {
	now := time.Date(2026, 11, 22, 17, 15, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = oldNow })

	t.Run("text output for slack", func(t *testing.T) {
		var gotSQL string
		query := func(_ context.Context, _, sql string) ([]PipelineRow, error) {
			gotSQL = sql
			return []PipelineRow{{
				Pipeline:                   "slack",
				Status:                     "ok",
				RunStatus:                  "ok",
				LastRunAt:                  ptrTime(now.Add(-2 * time.Minute)),
				ExpectedRunIntervalSeconds: ptrInt64(1800),
			}}, nil
		}
		var out, errOut bytes.Buffer
		code := CLI(context.Background(), []string{"sync-health", "--source", "slack"}, &out, &errOut, query)
		if code != 0 {
			t.Fatalf("exit %d: stderr=%s", code, errOut.String())
		}
		if !strings.Contains(gotSQL, "marts_ops.pipeline_health") || !strings.Contains(gotSQL, "'slack'") {
			t.Fatalf("unexpected sql: %s", gotSQL)
		}
		text := out.String()
		for _, want := range []string{"source: slack", "verdict: ok", "last_sync_at: 2026-11-22T17:13:00Z", "age: 2m"} {
			if !strings.Contains(text, want) {
				t.Errorf("output missing %q:\n%s", want, text)
			}
		}
	})

	t.Run("json output and voice_memos alias", func(t *testing.T) {
		query := func(_ context.Context, _, sql string) ([]PipelineRow, error) {
			for _, p := range []string{"'google_recorder'", "'apple_voice_memos'", "'alice_voice_recordings'"} {
				if !strings.Contains(sql, p) {
					t.Errorf("voice-memos sql missing %s: %s", p, sql)
				}
			}
			return []PipelineRow{{
				Pipeline:                   "google_recorder",
				Status:                     "ok",
				RunStatus:                  "ok",
				LastRunAt:                  ptrTime(now.Add(-3 * time.Minute)),
				ExpectedRunIntervalSeconds: ptrInt64(1800),
			}}, nil
		}
		var out, errOut bytes.Buffer
		code := CLI(context.Background(), []string{"sync-health", "--source", "voice_memos", "--json"}, &out, &errOut, query)
		if code != 0 {
			t.Fatalf("exit %d: stderr=%s", code, errOut.String())
		}
		var r Report
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("invalid json: %v\n%s", err, out.String())
		}
		if r.Source != "voice-memos" || r.Verdict != VerdictOK {
			t.Fatalf("report = %+v", r)
		}
	})

	t.Run("bad args return exit 2", func(t *testing.T) {
		for _, args := range [][]string{
			{},
			{"unknown"},
			{"sync-health"},
			{"sync-health", "--source", "notion"},
		} {
			var out, errOut bytes.Buffer
			if code := CLI(context.Background(), args, &out, &errOut, nil); code != 2 {
				t.Errorf("args %v: exit = %d, want 2 (stderr=%s)", args, code, errOut.String())
			}
		}
	})

	t.Run("query error returns exit 1", func(t *testing.T) {
		query := func(context.Context, string, string) ([]PipelineRow, error) {
			return nil, errors.New("pdw unavailable")
		}
		var out, errOut bytes.Buffer
		if code := CLI(context.Background(), []string{"sync-health", "--source", "gmail"}, &out, &errOut, query); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(errOut.String(), "pdw unavailable") {
			t.Fatalf("stderr = %q", errOut.String())
		}
	})
}
