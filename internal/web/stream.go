package web

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/fleet"
)

// streamPing is how often the server pings each box's stream: it notices a box that went away,
// and records that the box is still there (it is online while seen within a minute).
const streamPing = 20 * time.Second

// stream serves a box's stream: hello, then the box's frames, each acked, while commands go down.
// The stream takes the box over from any stream it had, on this server instance or another, and
// ends when a newer one does.
func stream(cfg Config, reg *fleet.Registry, changes *fleet.Changes) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !devboxAuthorized(cfg, w, r) {
			return
		}
		box := r.PathValue("box")
		if !devbox.ValidName(box) {
			http.Error(w, "bad dev box name", http.StatusBadRequest)
			return
		}
		cursors, err := reg.TranscriptCursors(r.Context(), box)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(devbox.MaxFrame)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		// subscribe before attaching, so a newer stream attaching meanwhile is not missed
		replaced, stop := changes.Subscribe(fleet.StreamTopic(box))
		defer stop()
		id := newStreamID()
		if err := reg.Attach(ctx, box, id, cfg.Now()); err != nil {
			log.Printf("stream %s: %v", box, err)
			return
		}
		defer func() {
			dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := reg.Detach(dctx, box, id); err != nil {
				log.Printf("stream %s: %v", box, err)
			}
		}()
		if err := devbox.WriteFrame(ctx, conn, devbox.Frame{Type: devbox.HelloFrame, Hello: &devbox.Hello{Cursors: cursors}}); err != nil {
			return
		}
		go func() {
			defer cancel()
			if err := sendCommands(ctx, cfg, reg, changes, conn, box, id, replaced); err != nil && ctx.Err() == nil {
				log.Printf("stream %s: %v", box, err)
			}
		}()
		// transcript batches are stored on their own, in order, so a large one does not hold up
		// the box's reports and command updates behind it
		transcripts := make(chan devbox.Frame, 16)
		defer close(transcripts)
		go func() {
			for f := range transcripts {
				if err := devbox.WriteFrame(ctx, conn, receive(ctx, cfg, reg, box, f)); err != nil {
					cancel()
				}
			}
		}()
		for {
			f, err := devbox.ReadFrame(ctx, conn)
			if err != nil {
				return // the box went away, or dialed again
			}
			if f.Type == devbox.TranscriptFrame {
				select {
				case transcripts <- f:
				case <-ctx.Done():
					return
				}
				continue
			}
			if err := devbox.WriteFrame(ctx, conn, receive(ctx, cfg, reg, box, f)); err != nil {
				return
			}
		}
	}
}

// newStreamID is a random (version 4) uuid naming one stream.
func newStreamID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// receive stores one of the box's frames and says how it went.
func receive(ctx context.Context, cfg Config, reg *fleet.Registry, box string, f devbox.Frame) devbox.Frame {
	ack := devbox.Frame{Type: devbox.AckFrame, ID: f.ID}
	var err error
	switch {
	case f.Type == devbox.ReportFrame && f.Report != nil:
		if f.Report.Name != box {
			err = fmt.Errorf("report for %q on %q's stream", f.Report.Name, box)
			break
		}
		err = reg.Record(ctx, *f.Report, cfg.Now())
	case f.Type == devbox.TranscriptFrame && f.Transcript != nil:
		b := f.Transcript
		if b.Box != box || b.AgentID == "" || b.Epoch == "" {
			err = errors.New("bad transcript batch: box (this stream's), agentId and epoch are required")
			break
		}
		err = reg.StoreTranscript(ctx, *b, cfg.Now())
		switch {
		case errors.Is(err, fleet.ErrEpochChanged):
			ack.Code, err = devbox.AckResend, nil
		case errors.Is(err, fleet.ErrUnknownAgent):
			ack.Code, err = devbox.AckNotYet, nil
		}
	case f.Type == devbox.CommandUpdateFrame && f.Update != nil:
		err = reg.UpdateCommand(ctx, box, *f.Update, cfg.Now())
		if errors.Is(err, fleet.ErrUnknownCommand) {
			err = nil // nothing to record, and nothing the box can do about it
		}
	case f.Type == devbox.HostFrame && f.Host != nil:
		err = reg.RecordHost(ctx, box, *f.Host, cfg.Now())
	case f.Type == devbox.UsageFrame && f.Usage != nil:
		err = reg.RecordUsage(ctx, box, *f.Usage, cfg.Now())
	default:
		err = fmt.Errorf("unexpected %q frame", f.Type)
	}
	if err != nil {
		ack.Error = err.Error()
	}
	return ack
}

// sendCommands sends the box's commands down its stream: on connect every command without a
// result (it may not have arrived), then each new one as it is queued, on any server instance.
// It pings, so a stream whose box went away ends, and it ends the stream once a newer one has
// taken the box (replaced).
func sendCommands(ctx context.Context, cfg Config, reg *fleet.Registry, changes *fleet.Changes, conn *websocket.Conn,
	box, id string, replaced <-chan struct{}) error {
	queued, stop := changes.Subscribe(fleet.CommandsTopic(box))
	defer stop()
	ping := time.NewTicker(streamPing)
	defer ping.Stop()
	unfinished := true
	for {
		cmds, err := reg.Claim(ctx, box, unfinished, cfg.Now())
		if err != nil {
			return err
		}
		unfinished = false
		for _, c := range cmds {
			w := c.Wire()
			if err := devbox.WriteFrame(ctx, conn, devbox.Frame{Type: devbox.CommandFrame, Command: &w}); err != nil {
				return err // still sent: the next stream sends it again
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-queued:
			continue
		case <-replaced:
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		}
		// seen: and still the box's stream?
		current, err := reg.Seen(ctx, box, id, cfg.Now())
		if err != nil {
			return err
		}
		if !current {
			conn.Close(websocket.StatusPolicyViolation, "a newer stream took over")
			return nil
		}
	}
}

// maxCommand bounds a command request (a prompt can be long).
const maxCommand = 1 << 20

// maxCommandTTL is the longest a command may wait for its box.
const maxCommandTTL = 24 * time.Hour

// enqueueCommand queues a command for a box: {"kind", "args", "ttl"}. It needs the operator token.
func enqueueCommand(cfg Config, reg *fleet.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !operatorOrSession(cfg, w, r) {
			return
		}
		var req struct {
			Kind string          `json:"kind"`
			Args json.RawMessage `json:"args"`
			TTL  string          `json:"ttl"` // how long it may wait for the box; default 10m
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCommand))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "bad command: "+err.Error(), http.StatusBadRequest)
			return
		}
		ttl := 10 * time.Minute
		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil || d < time.Second || d > maxCommandTTL {
				http.Error(w, fmt.Sprintf("bad ttl %q: want a duration from 1s to %s", req.TTL, maxCommandTTL), http.StatusBadRequest)
				return
			}
			ttl = d
		}
		box := r.PathValue("box")
		c, err := reg.Enqueue(r.Context(), box, req.Kind, req.Args, ttl, cfg.Now())
		var bad *fleet.BadCommandError
		switch {
		case errors.As(err, &bad):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, fleet.ErrUnknownBox):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Location", "/api/devboxes/"+box+"/commands/"+c.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(c)
	}
}

// maxCommandWait bounds ?wait= on a command read.
const maxCommandWait = time.Minute

// showCommand returns a command; with ?wait=30s it waits up to that long for it to finish.
func showCommand(reg *fleet.Registry, changes *fleet.Changes) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var wait time.Duration
		if v := r.URL.Query().Get("wait"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 || d > maxCommandWait {
				http.Error(w, fmt.Sprintf("bad wait %q: want a duration up to %s", v, maxCommandWait), http.StatusBadRequest)
				return
			}
			wait = d
		}
		ctx, cancel := context.WithTimeout(r.Context(), wait)
		defer cancel()
		id := r.PathValue("id")
		changed, stop := changes.Subscribe(fleet.CommandTopic(id)) // before reading: no change is missed
		defer stop()
		for {
			c, err := reg.Command(r.Context(), id)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && c.Box != r.PathValue("box")) {
				http.Error(w, "no such command", http.StatusNotFound)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if c.Finished() || ctx.Err() != nil {
				writeJSON(w, c)
				return
			}
			select {
			case <-ctx.Done():
			case <-changed:
			}
		}
	}
}

// operatorAuthorized answers the request itself unless it carries the operator token.
func operatorAuthorized(cfg Config, w http.ResponseWriter, r *http.Request) bool {
	return bearerAuthorized(cfg.OperatorToken, "commands are not configured on this server (no OPERATOR_TOKEN)", "operator", w, r)
}

func bearerOf(r *http.Request) (string, bool) {
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}
