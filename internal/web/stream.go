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

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// streamPing is how often the server pings each host's stream: it notices a host that went away,
// and records that the host is still there (it is online while seen within a minute).
const streamPing = 20 * time.Second

// stream serves a host's stream: hello, then the host's frames, each acked, while commands go down.
// The stream takes the host over from any stream it had, on this server instance or another, and
// ends when a newer one does.
// A host streams with its own key (accounts.Store.Collect), which is good for its name only.
func stream(a *app) http.HandlerFunc {
	cfg, reg, changes := a.cfg, a.reg, a.changes
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.PathValue("host")
		if !hostclient.ValidName(host) {
			http.Error(w, "bad host name", http.StatusBadRequest)
			return
		}
		key, _ := bearerOf(r)
		ok, err := a.store.HostKey(r.Context(), host, key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "bad or missing host key: join an account (POST /api/host-enrollments)", http.StatusUnauthorized)
			return
		}
		cursors, err := reg.TranscriptCursors(r.Context(), host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(hostclient.MaxFrame)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		// subscribe before attaching, so a newer stream attaching meanwhile is not missed
		replaced, stop := changes.Subscribe(fleet.StreamTopic(host))
		defer stop()
		id := newStreamID()
		if err := reg.Attach(ctx, host, id, cfg.Now()); err != nil {
			log.Printf("stream %s: %v", host, err)
			return
		}
		defer func() {
			dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := reg.Detach(dctx, host, id); err != nil {
				log.Printf("stream %s: %v", host, err)
			}
		}()
		if err := hostclient.WriteFrame(ctx, conn, hostclient.Frame{Type: hostclient.HelloFrame, Hello: &hostclient.Hello{Cursors: cursors}}); err != nil {
			return
		}
		go func() {
			defer cancel()
			if err := sendCommands(ctx, cfg, reg, changes, conn, host, id, replaced); err != nil && ctx.Err() == nil {
				log.Printf("stream %s: %v", host, err)
			}
		}()
		// transcript batches are stored on their own, in order, so a large one does not hold up
		// the host's reports and command updates behind it
		transcripts := make(chan hostclient.Frame, 16)
		defer close(transcripts)
		go func() {
			for f := range transcripts {
				if err := hostclient.WriteFrame(ctx, conn, receive(ctx, a, host, f)); err != nil {
					cancel()
				}
			}
		}()
		for {
			f, err := hostclient.ReadFrame(ctx, conn)
			if err != nil {
				return // the host went away, or dialed again
			}
			if f.Type == hostclient.TranscriptFrame {
				select {
				case transcripts <- f:
				case <-ctx.Done():
					return
				}
				continue
			}
			if err := hostclient.WriteFrame(ctx, conn, receive(ctx, a, host, f)); err != nil {
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

// receive stores one of the host's frames and says how it went.
func receive(ctx context.Context, a *app, host string, f hostclient.Frame) hostclient.Frame {
	cfg, reg := a.cfg, a.reg
	ack := hostclient.Frame{Type: hostclient.AckFrame, ID: f.ID}
	var err error
	switch {
	case f.Type == hostclient.ReportFrame && f.Report != nil:
		if f.Report.Name != host {
			err = fmt.Errorf("report for %q on %q's stream", f.Report.Name, host)
			break
		}
		err = reg.Record(ctx, *f.Report, cfg.Now())
	case f.Type == hostclient.TranscriptFrame && f.Transcript != nil:
		b := f.Transcript
		if b.Host != host || b.AgentID == "" || b.Epoch == "" {
			err = errors.New("bad transcript batch: host (this stream's), agentId and epoch are required")
			break
		}
		err = reg.StoreTranscript(ctx, *b, cfg.Now())
		switch {
		case errors.Is(err, fleet.ErrEpochChanged):
			ack.Code, err = hostclient.AckResend, nil
		case errors.Is(err, fleet.ErrUnknownAgent):
			ack.Code, err = hostclient.AckNotYet, nil
		}
	case f.Type == hostclient.CommandUpdateFrame && f.Update != nil:
		err = reg.UpdateCommand(ctx, host, *f.Update, cfg.Now())
		if errors.Is(err, fleet.ErrUnknownCommand) {
			err = nil // nothing to record, and nothing the host can do about it
		} else if err == nil {
			err = a.wf.CommandUpdated(ctx, f.Update.ID) // a workflow run waiting on it moves on
		}
	case f.Type == hostclient.HostFrame && f.Host != nil:
		err = reg.RecordHost(ctx, host, *f.Host, cfg.Now())
	case f.Type == hostclient.UsageFrame && f.Usage != nil:
		err = reg.RecordUsage(ctx, host, *f.Usage, cfg.Now())
	case f.Type == hostclient.SchedulesFrame && f.Schedules != nil:
		err = reg.RecordSchedules(ctx, host, *f.Schedules, cfg.Now())
	case f.Type == hostclient.DiffFrame && f.Diff != nil:
		if f.Diff.WorkspaceID == "" {
			err = errors.New("bad diff: workspaceId is required")
			break
		}
		err = reg.RecordDiff(ctx, host, *f.Diff, cfg.Now())
	default:
		err = fmt.Errorf("unexpected %q frame", f.Type)
	}
	if err != nil {
		ack.Error = err.Error()
	}
	return ack
}

// sendCommands sends the host's commands down its stream: on connect every command without a
// result (it may not have arrived), then each new one as it is queued, on any server instance.
// It pings, so a stream whose host went away ends, and it ends the stream once a newer one has
// taken the host (replaced).
func sendCommands(ctx context.Context, cfg Config, reg *fleet.Registry, changes *fleet.Changes, conn *websocket.Conn,
	host, id string, replaced <-chan struct{}) error {
	queued, stop := changes.Subscribe(fleet.CommandsTopic(host))
	defer stop()
	ping := time.NewTicker(streamPing)
	defer ping.Stop()
	unfinished := true
	for {
		cmds, err := reg.Claim(ctx, host, unfinished, cfg.Now())
		if err != nil {
			return err
		}
		unfinished = false
		for _, c := range cmds {
			w := c.Wire()
			if err := hostclient.WriteFrame(ctx, conn, hostclient.Frame{Type: hostclient.CommandFrame, Command: &w}); err != nil {
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
		// seen: and still the host's stream?
		current, err := reg.Seen(ctx, host, id, cfg.Now())
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

// maxCommandTTL is the longest a command may wait for its host.
const maxCommandTTL = 24 * time.Hour

// enqueueCommand queues a command for one of v's hosts: {"kind", "args", "ttl"}.
func enqueueCommand(a *app, v viewer) http.HandlerFunc {
	cfg, reg := a.cfg, a.reg
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.owned(v, w, r, r.PathValue("host")) {
			return
		}
		var req struct {
			Kind string          `json:"kind"`
			Args json.RawMessage `json:"args"`
			TTL  string          `json:"ttl"` // how long it may wait for the host; default 10m
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
		host := r.PathValue("host")
		c, err := reg.Enqueue(r.Context(), host, req.Kind, req.Args, ttl, cfg.Now())
		var bad *fleet.BadCommandError
		switch {
		case errors.As(err, &bad):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, fleet.ErrArchivedAgent):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case errors.Is(err, fleet.ErrUnknownHost):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Location", "/api/hosts/"+host+"/commands/"+c.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(c)
	}
}

// maxCommandWait bounds ?wait= on a command read.
const maxCommandWait = time.Minute

// showCommand returns a command to one of v's hosts; with ?wait=30s it waits up to that long for
// it to finish.
func showCommand(a *app, v viewer) http.HandlerFunc {
	reg, changes := a.reg, a.changes
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.owned(v, w, r, r.PathValue("host")) {
			return
		}
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
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && c.Host != r.PathValue("host")) {
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

func bearerOf(r *http.Request) (string, bool) {
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}
