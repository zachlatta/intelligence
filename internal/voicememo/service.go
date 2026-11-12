// Package voicememo reads PDW recordings for one explicitly configured Intelligence account.
package voicememo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zachlatta/intelligence/internal/workflow"
)

type Service struct {
	DB                 *pgxpool.Pool
	Engine             *workflow.Engine
	URL, Key, Username string
	Client             *http.Client
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.Poll(ctx); err != nil && ctx.Err() == nil {
			log.Printf("voice memos: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Poll serializes readers across server instances and persists the feed arrival cursor.
// The first successful scan establishes a baseline without starting historical runs.
func (s *Service) Poll(ctx context.Context) error {
	var account int64
	if err := s.DB.QueryRow(ctx, "SELECT id FROM accounts WHERE username = $1", s.Username).Scan(&account); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("PDW_INTELLIGENCE_ACCOUNT %q does not exist; set it to an existing Intelligence username", s.Username)
		}
		return err
	}
	if _, err := s.DB.Exec(ctx, "INSERT INTO voice_memo_polling (account_id) VALUES ($1) ON CONFLICT DO NOTHING", account); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var cursor string
		var initialized bool
		err := tx.QueryRow(ctx, "SELECT cursor, initialized FROM voice_memo_polling WHERE account_id=$1 FOR UPDATE SKIP LOCKED", account).Scan(&cursor, &initialized)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		pollErr := s.poll(ctx, account, &cursor, &initialized)
		message := ""
		if pollErr != nil {
			message = pollErr.Error()
		}
		_, err = tx.Exec(ctx, "UPDATE voice_memo_polling SET checked_at=now(), error=$2, cursor=$3, initialized=$4 WHERE account_id=$1", account, message, cursor, initialized)
		return err
	})
}

type page struct {
	Feed string               `json:"feed"`
	Rows []workflow.VoiceMemo `json:"recordings"`
	Next string               `json:"next_cursor"`
	More bool                 `json:"has_more"`
}

func (s *Service) read(ctx context.Context, cursor string) (page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.URL, "/")+"/api/external-feeds/voice-memos?since="+url.QueryEscape(cursor), nil)
	if err != nil {
		return page{}, err
	}
	req.Header.Set("Authorization", "Bearer intelligence:"+s.Key)
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 75 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return page{}, errors.New("PDW request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return page{}, fmt.Errorf("PDW HTTP %d", resp.StatusCode)
	}
	var result page
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return page{}, fmt.Errorf("decode PDW response: %w", err)
	}
	if result.Feed != "feed:voice-memos" || result.Rows == nil || result.Next == "" || (result.More && result.Next == cursor) {
		return page{}, errors.New("PDW returned an invalid feed page")
	}
	return result, nil
}

func (s *Service) poll(ctx context.Context, account int64, cursor *string, initialized *bool) error {
	for {
		p, err := s.read(ctx, *cursor)
		if err != nil {
			return err
		}
		for _, m := range p.Rows {
			if m.ID == "" || m.Source == "" {
				return errors.New("PDW returned an invalid recording")
			}
			m.TranscriptLength = len([]rune(m.Transcript))
			if err = s.keep(ctx, account, m, *initialized); err != nil {
				return err
			}
		}
		*cursor = p.Next
		if !p.More {
			*initialized = true
			return nil
		}
	}
}

// keep commits the receipt and run together. Workflow refusals are final ignored receipts;
// unexpected failures leave both retryable.
func (s *Service) keep(ctx context.Context, account int64, m workflow.VoiceMemo, start bool) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		reason := "No workflow matched"
		if !start {
			reason = "Baseline backlog skipped"
		}
		// A recording with no transcript can start nothing; its receipt keeps it from being retried.
		if strings.TrimSpace(m.Transcript) == "" {
			start, reason = false, "No transcript"
		}
		tag, err := tx.Exec(ctx, `INSERT INTO voice_memos(account_id,source,pdw_account,recording_id,title,preview,transcript_length,decision,recorded_at,reason)
 VALUES($1,$2,$3,$4,$5,$6,$7,'ignored',nullif($8,'')::timestamptz,$9) ON CONFLICT DO NOTHING`, account, m.Source, m.Account, m.ID, m.Title, string([]rune(m.Transcript)[:min(1000, len([]rune(m.Transcript)))]), m.TranscriptLength, m.RecordedAt, reason)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if !start {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT w.id::text,w.name,v.definition FROM workflows w JOIN workflow_versions v ON v.workflow_id=w.id AND v.version=w.version
 WHERE w.account_id=$1 AND w.deleted_at IS NULL AND v.definition->>'trigger'='voice_memo' ORDER BY w.created_at,w.id`, account)
		if err != nil {
			return err
		}
		type candidate struct {
			id, name string
			def      workflow.Definition
		}
		candidates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (candidate, error) {
			var c candidate
			var raw []byte
			err := r.Scan(&c.id, &c.name, &raw)
			if err == nil {
				err = json.Unmarshal(raw, &c.def)
			}
			return c, err
		})
		if err != nil {
			return err
		}
		var best *candidate
		score := -1
		for _, c := range candidates {
			if f := c.def.VoiceMemo; f != nil && f.Match(m) && f.Specificity() > score {
				picked := c
				best = &picked
				score = f.Specificity()
			}
		}
		if best == nil {
			return nil
		}
		var run string
		err = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			var startErr error
			requested, _ := time.Parse(time.RFC3339Nano, m.RecordedAt)
			run, startErr = s.Engine.StartRunTx(ctx, sp, account, best.id, m.Input(), requested)
			return startErr
		})
		if err != nil {
			var refusal *workflow.Refusal
			if !errors.As(err, &refusal) && !errors.Is(err, workflow.ErrNotFound) {
				return err
			}
			reason := fmt.Sprintf("could not start “%s”: %v", best.name, err)
			_, err = tx.Exec(ctx, `UPDATE voice_memos SET workflow_id=$5,reason=$6 WHERE account_id=$1 AND source=$2 AND pdw_account=$3 AND recording_id=$4`, account, m.Source, m.Account, m.ID, best.id, reason)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE voice_memos SET decision='started',workflow_id=$5,run_id=$6::uuid,reason='Started run ' || $6::uuid::text WHERE account_id=$1 AND source=$2 AND pdw_account=$3 AND recording_id=$4`, account, m.Source, m.Account, m.ID, best.id, run)
		return err
	})
}
