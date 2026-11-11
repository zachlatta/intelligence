package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zachlatta/intelligence/internal/workflow"
)

type voiceMemoStatus struct {
	Message     string
	CheckedAt   *time.Time
	Error       string
	Initialized bool
}

// Status reads only this viewer's rows. Credentials and the configured username never reach HTML.
func readVoiceMemoStatus(ctx context.Context, cfg Config, v viewer) (voiceMemoStatus, error) {
	s := voiceMemoStatus{Message: "Voice memo triggers are off on this server. Configure PDW to enable them."}
	if cfg.PDWURL != "" && cfg.PDWKey != "" && cfg.PDWAccount != "" {
		s.Message = "PDW is not configured for this account."
		if cfg.PDWAccount == v.acct.Username {
			s.Message = "PDW is configured for this account."
		}
	}
	err := v.pool.QueryRow(ctx, "SELECT checked_at,error,initialized FROM voice_memo_polling").Scan(&s.CheckedAt, &s.Error, &s.Initialized)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return s, err
}

type voiceMemoRow struct {
	ID, Source, Account, Title, Preview, Decision, Reason, Run string
	RecordedAt                                                 *time.Time
	DecidedAt                                                  time.Time
}

func readVoiceMemos(ctx context.Context, p *pgxpool.Pool, run string) ([]voiceMemoRow, error) {
	rows, err := p.Query(ctx, `SELECT recording_id,source,pdw_account,title,preview,decision,reason,coalesce(run_id::text,''),recorded_at,decided_at
 FROM voice_memos WHERE $1='' OR run_id::text=$1 ORDER BY decided_at DESC,source,pdw_account,recording_id LIMIT 200`, run)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (voiceMemoRow, error) {
		var m voiceMemoRow
		err := r.Scan(&m.ID, &m.Source, &m.Account, &m.Title, &m.Preview, &m.Decision, &m.Reason, &m.Run, &m.RecordedAt, &m.DecidedAt)
		return m, err
	})
}

func voiceMemosPage(cfg Config, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, err := readVoiceMemoStatus(r.Context(), cfg, v)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		memos, err := readVoiceMemos(r.Context(), v.pool, "")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		renderWorkflows(w, "voice-memos", map[string]any{"Version": cfg.Version, "VoiceStatus": status, "VoiceMemos": memos})
	}
}

func voiceMemoSummary(f *workflow.VoiceMemoFilter) string {
	if f == nil {
		return "Voice memo"
	}
	parts := []string{}
	for _, field := range []struct{ label, value string }{{"title contains", f.Title}, {"title or transcript contains", f.Words}, {"without", f.Without}} {
		if strings.TrimSpace(field.value) != "" {
			parts = append(parts, field.label+" “"+strings.TrimSpace(field.value)+"”")
		}
	}
	if len(parts) == 0 {
		return "Voice memo: all new recordings"
	}
	return "Voice memo: " + strings.Join(parts, ", ")
}
