package hostclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/zachlatta/intelligence/internal/resources"
	"github.com/zachlatta/intelligence/internal/usage"
)

// StreamPath is the WebSocket a host holds open to the server. Everything between them goes
// over it: the host's reports, its agents' transcripts and its command updates go up; the server's
// commands come down. The host dials out, so it needs no inbound port.
func StreamPath(host string) string { return "/api/hosts/" + host + "/stream" }

// Frame is one message on the stream, as JSON. A host frame with an ID is answered by an ack frame
// with the same ID, saying whether the server stored it.
type Frame struct {
	Permission *PermissionResolution `json:"permission,omitempty"`
	Type       string                `json:"type"`
	ID         uint64                `json:"id,omitempty"`
	Hello      *Hello                `json:"hello,omitempty"`      // hello: server -> host, first
	Report     *Report               `json:"report,omitempty"`     // report: host -> server
	Transcript *TranscriptBatch      `json:"transcript,omitempty"` // transcript: host -> server
	Update     *CommandUpdate        `json:"update,omitempty"`     // command_update: host -> server
	Usage      *usage.Report         `json:"usage,omitempty"`      // usage: host -> server
	Host       *resources.Report     `json:"host,omitempty"`       // host: host -> server
	Schedules  *ScheduleReport       `json:"schedules,omitempty"`  // schedules: host -> server
	Diff       *WorkspaceDiff        `json:"diff,omitempty"`       // diff: host -> server
	Command    *Command              `json:"command,omitempty"`    // command: server -> host
	Error      string                `json:"error,omitempty"`      // ack: refused, and why
	Code       string                `json:"code,omitempty"`       // ack: AckResend or AckNotYet
}

// Frame types.
const (
	PermissionFrame    = "permission"
	HelloFrame         = "hello"
	ReportFrame        = "report"
	TranscriptFrame    = "transcript"
	CommandUpdateFrame = "command_update"
	UsageFrame         = "usage"
	HostFrame          = "host"
	SchedulesFrame     = "schedules"
	DiffFrame          = "diff"
	CommandFrame       = "command"
	AckFrame           = "ack"
)

// Ack codes for a refused transcript batch.
const (
	AckResend = "resend"  // the server cannot place an append: send the transcript whole (ErrResend)
	AckNotYet = "not_yet" // the server does not know the agent yet (ErrNotYet)
)

// Hello is what the server says first on each stream: where its copies of the host's transcripts
// end. (Its version comes in the dial's response header, which a refused dial has too.)
type Hello struct {
	Cursors map[string]TranscriptCursor `json:"cursors"`
}

var (
	// ErrNotConnected: the host has no stream to the server right now.
	ErrNotConnected = errors.New("not connected to the server")
	// ErrResend: the server cannot place a transcript append; send the transcript whole.
	ErrResend = errors.New("server asked for the whole transcript")
	// ErrNotYet: the server does not know the agent yet (its report has not landed).
	ErrNotYet = errors.New("agent not on the server yet")
)

// MaxFrame bounds a frame either way: a transcript batch is at most 4 MB of entries.
const MaxFrame = 16 << 20

// Link is a host's stream to the server. Run keeps it connected; the send methods fail with
// ErrNotConnected while it is not, and the caller tries again later.
type Link struct {
	Server, Host string
	// Keys is where the host's key comes from (joining an account when it has none); a key the
	// server refuses is forgotten there. Token is a fixed key instead, when Keys is nil.
	Keys       *Keys
	Token      string
	HTTP       *http.Client  // for the dial
	RetryEvery time.Duration // between dials; default 5s
	AckTimeout time.Duration // a frame not acked in this long drops the stream; default 30s
	PingEvery  time.Duration // ping the server this often; one not answered in AckTimeout drops the stream; default 30s
	// ServerVersion is called with the server's version after every dial, also a refused one, so
	// a host can update to the server it cannot talk to.
	ServerVersion func(v string)

	once      sync.Once
	commands  chan Command
	connected chan struct{}
	nextID    atomic.Uint64

	mu      sync.Mutex
	conn    *websocket.Conn
	hello   Hello
	pending map[uint64]chan Frame
	err     string
}

func (l *Link) init() {
	l.once.Do(func() {
		l.commands, l.connected = make(chan Command, 64), make(chan struct{}, 1)
		l.pending = map[uint64]chan Frame{}
		if l.RetryEvery == 0 {
			l.RetryEvery = 5 * time.Second
		}
		if l.AckTimeout == 0 {
			l.AckTimeout = 30 * time.Second
		}
		if l.PingEvery == 0 {
			l.PingEvery = 30 * time.Second
		}
		if l.HTTP == nil {
			l.HTTP = http.DefaultClient
		}
	})
}

// Commands delivers the server's commands.
func (l *Link) Commands() <-chan Command { l.init(); return l.commands }

// Connected receives after each new stream is up (coalesced).
func (l *Link) Connected() <-chan struct{} { l.init(); return l.connected }

// Hello is the current stream's hello; false while not connected.
func (l *Link) Hello() (Hello, bool) {
	l.init()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hello, l.conn != nil
}

// Err is why the last stream ended or the last dial failed, "" while connected.
func (l *Link) Err() string { l.mu.Lock(); defer l.mu.Unlock(); return l.err }

// Run keeps the stream up until ctx ends.
func (l *Link) Run(ctx context.Context) {
	l.init()
	for ctx.Err() == nil {
		err := l.session(ctx)
		l.mu.Lock()
		if ctx.Err() == nil && err != nil {
			l.err = err.Error()
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(l.RetryEvery):
		}
	}
}

func (l *Link) url() string {
	u := strings.TrimRight(l.Server, "/")
	if s, ok := strings.CutPrefix(u, "http"); ok { // http -> ws, https -> wss
		u = "ws" + s
	}
	return u + StreamPath(l.Host)
}

// session is one stream: dial, read the hello, then dispatch frames until it ends.
func (l *Link) session(ctx context.Context) error {
	key := l.Token
	if l.Keys != nil {
		if l.Keys.ServerVersion == nil {
			l.Keys.ServerVersion = l.ServerVersion
		}
		var err error
		if key, err = l.Keys.Key(ctx); err != nil {
			return fmt.Errorf("no key: %w", err)
		}
	}
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, l.url(), &websocket.DialOptions{HTTPClient: l.HTTP,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if resp != nil && l.ServerVersion != nil {
		l.ServerVersion(resp.Header.Get(VersionHeader))
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized && l.Keys != nil {
			l.Keys.Refused(key) // removed from its account, or never in one: join again
		}
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return fmt.Errorf("stream refused: %s", resp.Status)
		}
		return fmt.Errorf("stream: %w", err)
	}
	conn.SetReadLimit(MaxFrame)
	var hello Frame
	if err := readFrame(dialCtx, conn, &hello); err != nil || hello.Type != HelloFrame || hello.Hello == nil {
		conn.CloseNow()
		return fmt.Errorf("stream: no hello from the server (%v)", err)
	}
	cancel()
	l.mu.Lock()
	l.conn, l.hello, l.err = conn, *hello.Hello, ""
	l.mu.Unlock()
	select {
	case l.connected <- struct{}{}:
	default:
	}
	defer func() {
		l.mu.Lock()
		l.conn = nil
		for id, ch := range l.pending {
			close(ch)
			delete(l.pending, id)
		}
		l.mu.Unlock()
		conn.CloseNow()
	}()
	go l.ping(ctx, conn)
	for {
		var f Frame
		if err := readFrame(ctx, conn, &f); err != nil {
			return fmt.Errorf("stream: %w", err)
		}
		switch f.Type {
		case AckFrame:
			l.mu.Lock()
			ch := l.pending[f.ID]
			delete(l.pending, f.ID)
			l.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case CommandFrame:
			if f.Command != nil {
				select {
				case l.commands <- *f.Command:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
}

// ping pings the server until the stream ends, so a host with nothing to send still notices a
// server that stopped answering: it drops the stream, and Run dials again.
func (l *Link) ping(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(l.PingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, l.AckTimeout)
		err := conn.Ping(pctx)
		cancel()
		if err != nil {
			conn.CloseNow() // ends the stream's read loop, if it has not ended already
			return
		}
	}
}

// Report sends a report and waits for the server to store it.
func (l *Link) Report(ctx context.Context, r Report) error {
	return l.call(ctx, Frame{Type: ReportFrame, Report: &r})
}

// Transcript sends a transcript batch (ErrResend, ErrNotYet: see the ack codes).
func (l *Link) Transcript(ctx context.Context, b TranscriptBatch) error {
	return l.call(ctx, Frame{Type: TranscriptFrame, Transcript: &b})
}

// Update sends a command's progress.
func (l *Link) Update(ctx context.Context, u CommandUpdate) error {
	return l.call(ctx, Frame{Type: CommandUpdateFrame, Update: &u})
}

// Usage sends a read of the host's plans' usage.
func (l *Link) Usage(ctx context.Context, r usage.Report) error {
	return l.call(ctx, Frame{Type: UsageFrame, Usage: &r})
}

// Resources sends the host's finished minutes of host and agent resources.
func (l *Link) Resources(ctx context.Context, r resources.Report) error {
	return l.call(ctx, Frame{Type: HostFrame, Host: &r})
}

// Schedules sends the daemon's schedules.
func (l *Link) Schedules(ctx context.Context, r ScheduleReport) error {
	return l.call(ctx, Frame{Type: SchedulesFrame, Schedules: &r})
}

// Diff sends one workspace's diff.
func (l *Link) Diff(ctx context.Context, d WorkspaceDiff) error {
	return l.call(ctx, Frame{Type: DiffFrame, Diff: &d})
}

// TranscriptCursors are the server's transcript cursors from the current stream's hello.
func (l *Link) TranscriptCursors(context.Context) (map[string]TranscriptCursor, error) {
	h, ok := l.Hello()
	if !ok {
		return nil, ErrNotConnected
	}
	out := make(map[string]TranscriptCursor, len(h.Cursors))
	for k, v := range h.Cursors {
		out[k] = v
	}
	return out, nil
}

// call sends f and waits for its ack. A stream that does not ack in AckTimeout is dropped.
func (l *Link) call(ctx context.Context, f Frame) error {
	l.init()
	f.ID = l.nextID.Add(1)
	ch := make(chan Frame, 1)
	l.mu.Lock()
	conn := l.conn
	if conn == nil {
		l.mu.Unlock()
		return ErrNotConnected
	}
	l.pending[f.ID] = ch
	l.mu.Unlock()
	drop := func() {
		l.mu.Lock()
		delete(l.pending, f.ID)
		l.mu.Unlock()
	}
	b, err := json.Marshal(f)
	if err != nil {
		drop()
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, l.AckTimeout)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, b); err != nil {
		drop()
		conn.CloseNow()
		return fmt.Errorf("stream: %w", err)
	}
	select {
	case ack, ok := <-ch:
		if !ok {
			return fmt.Errorf("stream ended before the server acknowledged a %s", f.Type)
		}
		switch {
		case ack.Code == AckResend:
			return ErrResend
		case ack.Code == AckNotYet:
			return ErrNotYet
		case ack.Error != "":
			return fmt.Errorf("server refused a %s: %s", f.Type, ack.Error)
		}
		return nil
	case <-wctx.Done():
		drop()
		if ctx.Err() == nil {
			conn.CloseNow() // the server is not answering: dial again
			return fmt.Errorf("server did not acknowledge a %s in %s", f.Type, l.AckTimeout)
		}
		return ctx.Err()
	}
}

func readFrame(ctx context.Context, conn *websocket.Conn, f *Frame) error {
	_, b, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, f)
}

// WriteFrame writes f on conn (the server's side of the stream).
func WriteFrame(ctx context.Context, conn *websocket.Conn, f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// ReadFrame reads one frame from conn (the server's side of the stream).
func ReadFrame(ctx context.Context, conn *websocket.Conn) (Frame, error) {
	var f Frame
	err := readFrame(ctx, conn, &f)
	return f, err
}

func (l *Link) Permission(ctx context.Context, r PermissionResolution) error {
	return l.call(ctx, Frame{Type: PermissionFrame, Permission: &r})
}
