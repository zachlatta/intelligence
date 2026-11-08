package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/devbox"
)

var (
	// ErrEpochChanged refuses an append the server cannot place: no transcript yet, or one of another
	// epoch. The box sends the transcript whole instead.
	ErrEpochChanged = errors.New("transcript epoch changed: send it whole")
	// ErrUnknownAgent refuses a transcript of an agent the box has not reported.
	ErrUnknownAgent = errors.New("agent not reported by this box")
)

// Transcript is one agent's synced transcript.
type Transcript struct {
	Epoch    string
	SyncedAt time.Time
	Entries  []Entry
}

// Entry is a transcript entry with the columns read from its item.
type Entry struct {
	devbox.TranscriptEntry
	Kind, Tool, Status, Text string
}

// StoreTranscript stores a batch received at at.
func (r *Registry) StoreTranscript(ctx context.Context, b devbox.TranscriptBatch, at time.Time) error {
	return pgx.BeginFunc(ctx, r.p, func(tx pgx.Tx) error {
		var known bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM agents WHERE box = $1 AND id = $2)", b.Box, b.AgentID).Scan(&known); err != nil {
			return err
		}
		if !known {
			return ErrUnknownAgent
		}
		if b.Replace {
			if _, err := tx.Exec(ctx, "DELETE FROM transcripts WHERE box = $1 AND agent_id = $2", b.Box, b.AgentID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO transcripts (box, agent_id, epoch, synced_at) VALUES ($1, $2, $3, $4)",
				b.Box, b.AgentID, b.Epoch, at); err != nil {
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, "UPDATE transcripts SET synced_at = $4 WHERE box = $1 AND agent_id = $2 AND epoch = $3",
				b.Box, b.AgentID, b.Epoch, at)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrEpochChanged
			}
		}
		for _, e := range b.Entries {
			item, err := storable(e.Item)
			if err != nil {
				return err
			}
			e.Item = item
			if _, err := tx.Exec(ctx, `INSERT INTO transcript_entries (box, agent_id, seq_start, seq_end, turn_id, provider, at, item)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (box, agent_id, seq_start) DO UPDATE SET seq_end = EXCLUDED.seq_end, turn_id = EXCLUDED.turn_id,
				provider = EXCLUDED.provider, at = EXCLUDED.at, item = EXCLUDED.item`,
				b.Box, b.AgentID, e.SeqStart, e.SeqEnd, e.TurnID, e.Provider, nullTime(e.At), []byte(e.Item)); err != nil {
				return err
			}
		}
		return nil
	})
}

// TranscriptCursors are where box's transcripts end, by agent.
func (r *Registry) TranscriptCursors(ctx context.Context, box string) (map[string]devbox.TranscriptCursor, error) {
	rows, err := r.p.Query(ctx, `SELECT t.agent_id, t.epoch, coalesce(max(e.seq_start), 0), coalesce(max(e.seq_end), 0)
		FROM transcripts t LEFT JOIN transcript_entries e ON e.box = t.box AND e.agent_id = t.agent_id
		WHERE t.box = $1 GROUP BY t.agent_id, t.epoch`, box)
	if err != nil {
		return nil, err
	}
	out := map[string]devbox.TranscriptCursor{}
	var id string
	var c devbox.TranscriptCursor
	_, err = pgx.ForEachRow(rows, []any{&id, &c.Epoch, &c.LastSeqStart, &c.LastSeqEnd}, func() error {
		out[id] = c
		return nil
	})
	return out, err
}

// Transcript is agentID's transcript on box from seq fromSeq on (0 for all), in seq order;
// pgx.ErrNoRows when none is synced.
func (r *Registry) Transcript(ctx context.Context, box, agentID string, fromSeq int) (Transcript, error) {
	var t Transcript
	if err := r.p.QueryRow(ctx, "SELECT epoch, synced_at FROM transcripts WHERE box = $1 AND agent_id = $2", box, agentID).
		Scan(&t.Epoch, &t.SyncedAt); err != nil {
		return t, err
	}
	t.SyncedAt = t.SyncedAt.UTC()
	rows, err := r.p.Query(ctx, `SELECT seq_start, seq_end, turn_id, provider, at, item, kind, tool, status, text
		FROM transcript_entries WHERE box = $1 AND agent_id = $2 AND seq_start >= $3 ORDER BY seq_start`, box, agentID, fromSeq)
	if err != nil {
		return t, err
	}
	var e Entry
	var at *time.Time
	var item []byte
	_, err = pgx.ForEachRow(rows, []any{&e.SeqStart, &e.SeqEnd, &e.TurnID, &e.Provider, &at, &item, &e.Kind, &e.Tool,
		&e.Status, &e.Text}, func() error {
		e.At, e.Item = deref(at), json.RawMessage(append([]byte(nil), item...))
		t.Entries = append(t.Entries, e)
		return nil
	})
	return t, err
}

// storable is item ready for a jsonb column. Postgres text cannot hold NUL, which tool output
// sometimes carries: each NUL in a string becomes U+FFFD.
func storable(item json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(item) {
		return nil, errors.New("transcript entry item is not JSON")
	}
	if !bytes.Contains(item, []byte(`\u0000`)) {
		return item, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(item))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(replaceNUL(v)); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

func replaceNUL(v any) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, "\x00", "\uFFFD")
	case []any:
		for i := range x {
			x[i] = replaceNUL(x[i])
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[strings.ReplaceAll(k, "\x00", "\uFFFD")] = replaceNUL(val)
		}
		return out
	}
	return v
}

// TranscriptEntry is the entry of agentID's transcript that starts at seq; pgx.ErrNoRows when none.
func (r *Registry) TranscriptEntry(ctx context.Context, box, agentID string, seq int) (Entry, error) {
	var e Entry
	var at *time.Time
	var item []byte
	err := r.p.QueryRow(ctx, `SELECT seq_start, seq_end, turn_id, provider, at, item, kind, tool, status, text
		FROM transcript_entries WHERE box = $1 AND agent_id = $2 AND seq_start = $3`, box, agentID, seq).
		Scan(&e.SeqStart, &e.SeqEnd, &e.TurnID, &e.Provider, &at, &item, &e.Kind, &e.Tool, &e.Status, &e.Text)
	e.At, e.Item = deref(at), json.RawMessage(item)
	return e, err
}
