package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/grimm"
)

// Grimm is the home page: a multi-agent coding session (internal/grimm). The page reads its
// state as rows of the grimm_* views, the same ones /api/sql reads; its stream pushes those rows
// as they change (server-sent events woken by the change hub, never polled), and it changes
// anything only through the JSON routes under /api/grimm/.

//go:embed grimm.html
var grimmHTML string

//go:embed grimm.js
var grimmJS []byte

//go:embed grimm.css
var grimmCSS []byte

//go:embed grimm_host.js
var grimmHostJS []byte

//go:embed grimm_host.css
var grimmHostCSS []byte

var (
	grimmTmpl       = template.Must(template.New("grimm").Funcs(localFuncs).Funcs(template.FuncMap{"short": short}).Parse(grimmHTML + layoutHTML))
	grimmScript     = asset("grimm.js", "text/javascript; charset=utf-8", grimmJS)
	grimmStyle      = asset("grimm.css", "text/css; charset=utf-8", grimmCSS)
	grimmHostScript = asset("grimm_host.js", "text/javascript; charset=utf-8", grimmHostJS)
	grimmHostStyle  = asset("grimm_host.css", "text/css; charset=utf-8", grimmHostCSS)
)

func (a *app) grimmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /grimm.js", grimmScript)
	mux.HandleFunc("GET /grimm.css", grimmStyle)
	mux.HandleFunc("GET /grimm_host.js", grimmHostScript)
	mux.HandleFunc("GET /grimm_host.css", grimmHostStyle)
	mux.HandleFunc("GET /grimm/{id}", a.page(func(v viewer) http.HandlerFunc { return a.grimmPage(v) }))
	mux.HandleFunc("POST /api/grimm/sessions", a.write(a.grimmCreate))
	mux.HandleFunc("PUT /api/grimm/sessions/{id}", a.write(a.grimmUpdate))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/archive", a.write(a.grimmArchive))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/messages", a.write(a.grimmMessage))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/manager/interrupt", a.write(a.grimmSimple(a.grimm.Interrupt)))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/manager/new", a.write(a.grimmSimple(a.grimm.NewManager)))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/workers", a.write(a.grimmHire))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/workers/{name}/message", a.write(a.grimmWorkerMessage))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/workers/{name}/stop", a.write(a.grimmWorkerStop))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/workers/{name}/archive", a.write(a.grimmWorkerArchive(false)))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/archive-workers", a.write(a.grimmWorkerArchive(true)))
	mux.HandleFunc("POST /api/grimm/sessions/{id}/directives", a.write(a.grimmDirectiveAdd))
	mux.HandleFunc("DELETE /api/grimm/sessions/{id}/directives/{seq}", a.write(a.grimmDirectiveRemove))
	mux.HandleFunc("GET /api/grimm/sessions/{id}/stream", a.read(a.grimmStream))
	mux.HandleFunc("GET /api/grimm/sessions/{id}/workers/{name}/stream", a.read(a.grimmWorkerStream))
	mux.HandleFunc("POST "+grimm.AgentPath, a.hostWrite(a.grimmAgent))
	a.grimmWorkflowRoutes(mux)
}

// grimmError answers err: a refusal with its status and message, anything else as 500.
func grimmError(w http.ResponseWriter, err error) {
	var e *grimm.Error
	if errors.As(err, &e) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.Status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": e.Msg, "message": e.Msg})
		return
	}
	log.Printf("grimm: %v", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "Grimm could not do that; try again.", "message": "internal error"})
}

func decodeGrimm(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		grimmError(w, &grimm.Error{Status: http.StatusBadRequest, Msg: "bad request: " + err.Error()})
		return false
	}
	return true
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// grimmCreate is POST /api/grimm/sessions {"host", "repo", "title", "base", "maxWorkers"}.
func (a *app) grimmCreate(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var n grimm.NewSession
		if !decodeGrimm(w, r, &n) {
			return
		}
		id, err := a.grimm.CreateSession(r.Context(), v.acct.ID, n)
		if err != nil {
			grimmError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusCreated, map[string]string{"id": id, "url": "/grimm/" + id})
	}
}

// grimmUpdate is PUT /api/grimm/sessions/{id} {"maxWorkers", "title"}.
func (a *app) grimmUpdate(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var u grimm.SessionUpdate
		if !decodeGrimm(w, r, &u) {
			return
		}
		if err := a.grimm.UpdateSession(r.Context(), v.acct.ID, r.PathValue("id"), u); err != nil {
			grimmError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// grimmArchive is POST /api/grimm/sessions/{id}/archive {}: ends the session and returns the
// next URL for the page to open (another active session, or / for the start-a-session form).
func (a *app) grimmArchive(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var empty struct{}
		if !decodeGrimm(w, r, &empty) {
			return
		}
		if err := a.grimm.ArchiveSession(r.Context(), v.acct.ID, r.PathValue("id")); err != nil {
			grimmError(w, err)
			return
		}
		nextURL := "/"
		var nextID string
		if err := v.pool.QueryRow(r.Context(), `SELECT id::text FROM grimm_sessions WHERE status = 'active' ORDER BY created_at DESC LIMIT 1`).Scan(&nextID); err == nil && nextID != "" {
			nextURL = "/grimm/" + nextID
		}
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": "archived", "nextUrl": nextURL})
	}
}

// grimmSimple is a bodiless action ({} body) on a session.
func (a *app) grimmSimple(fn func(context.Context, int64, string) error) func(v viewer) http.HandlerFunc {
	return func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var empty struct{}
			if !decodeGrimm(w, r, &empty) {
				return
			}
			if err := fn(r.Context(), v.acct.ID, r.PathValue("id")); err != nil {
				grimmError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// grimmMessage is POST /api/grimm/sessions/{id}/messages {"text"}: to Grimm, @name or a /command.
// Attachments are [attachment:<id>] references from POST /api/attachments in the text.
func (a *app) grimmMessage(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if !decodeGrimm(w, r, &req) {
			return
		}
		row, err := a.grimm.Post(r.Context(), v.acct.ID, r.PathValue("id"), req.Text)
		if err != nil {
			grimmError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]any{"messageId": row})
	}
}

func (a *app) grimmHire(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var h struct {
			Role, Name, Provider, Task string
		}
		if !decodeGrimm(w, r, &h) {
			return
		}
		wk, err := a.grimm.HireWorker(r.Context(), v.acct.ID, r.PathValue("id"), grimm.Hire{Role: h.Role, Name: h.Name, Provider: h.Provider, Task: h.Task})
		if err != nil {
			grimmError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusCreated, map[string]string{"name": wk.Name, "provider": wk.Provider, "model": wk.Model, "message": grimm.HiredLine(wk)})
	}
}

func (a *app) grimmWorkerMessage(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if !decodeGrimm(w, r, &req) {
			return
		}
		out, err := a.grimm.SayToWorker(r.Context(), v.acct.ID, r.PathValue("id"), r.PathValue("name"), req.Text)
		if err != nil {
			grimmError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"message": out})
	}
}

func (a *app) grimmWorkerStop(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var empty struct{}
		if !decodeGrimm(w, r, &empty) {
			return
		}
		out, err := a.grimm.StopWorker(r.Context(), v.acct.ID, r.PathValue("id"), r.PathValue("name"))
		if err != nil {
			grimmError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"message": out})
	}
}

func (a *app) grimmWorkerArchive(all bool) func(v viewer) http.HandlerFunc {
	return func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var empty struct{}
			if !decodeGrimm(w, r, &empty) {
				return
			}
			name := r.PathValue("name")
			if all {
				name = ""
			}
			names, err := a.grimm.ArchiveWorkers(r.Context(), v.acct.ID, r.PathValue("id"), name)
			if err != nil {
				grimmError(w, err)
				return
			}
			writeJSONStatus(w, http.StatusAccepted, map[string]any{"archived": names})
		}
	}
}

func (a *app) grimmDirectiveAdd(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if !decodeGrimm(w, r, &req) {
			return
		}
		seq, pushed, err := a.grimm.AddDirective(r.Context(), v.acct.ID, r.PathValue("id"), req.Text)
		if err != nil {
			grimmError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusCreated, map[string]any{"seq": seq, "pushedTo": pushed})
	}
}

func (a *app) grimmDirectiveRemove(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		seq, err := strconv.Atoi(r.PathValue("seq"))
		if err != nil {
			grimmError(w, grimm.ErrNotFound)
			return
		}
		if err := a.grimm.RemoveDirective(r.Context(), v.acct.ID, r.PathValue("id"), seq); err != nil {
			grimmError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// grimmAgent is POST /api/grimm/agent: an `intelligence grimm` command from an agent on a host,
// with the host's key, as the agent PASEO_AGENT_ID names (grimm.AgentHeader).
func (a *app) grimmAgent(acct accounts.Account, host string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var c grimm.Call
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			grimmError(w, &grimm.Error{Status: http.StatusBadRequest, Msg: "bad request: " + err.Error()})
			return
		}
		out, err := a.grimm.AgentCall(r.Context(), a.changes, acct.ID, host, r.Header.Get(grimm.AgentHeader), c)
		if err != nil {
			grimmError(w, err)
			return
		}
		writeJSON(w, map[string]string{"message": out})
	}
}

// grimmHost is a host the new-session form offers.
type grimmHost struct {
	Name   string
	Online bool
	Repos  []string
	Desk   string // its desk checkout, the form's repository by default; "" when it has none
}

// grimmPage is the Grimm page: session id's, or with none (the home page) the newest active one,
// or the new-session form when the account has none.
func (a *app) grimmPage(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		if id != "" && !grimm.ValidID(id) {
			http.NotFound(w, r)
			return
		}
		type sessionItem struct {
			ID, Title, Host, Repo, Status, Branch string
		}
		rows, err := v.pool.Query(ctx, `SELECT id::text, title, host, repo, status, branch FROM grimm_sessions
			ORDER BY status = 'active' DESC, created_at DESC LIMIT 50`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sessions, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (sessionItem, error) {
			var s sessionItem
			return s, r.Scan(&s.ID, &s.Title, &s.Host, &s.Repo, &s.Status, &s.Branch)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var activeSessions, endedSessions []sessionItem
		for _, s := range sessions {
			if s.Status == "active" {
				activeSessions = append(activeSessions, s)
			} else {
				endedSessions = append(endedSessions, s)
			}
		}
		var cur sessionItem
		if id != "" {
			for _, s := range sessions {
				if s.ID == id {
					cur = s
					break
				}
			}
			if cur.ID == "" {
				if err := v.pool.QueryRow(ctx, `SELECT id::text, title, host, repo, status, branch FROM grimm_sessions WHERE id = $1`, id).
					Scan(&cur.ID, &cur.Title, &cur.Host, &cur.Repo, &cur.Status, &cur.Branch); err != nil {
					http.NotFound(w, r)
					return
				}
			}
		} else if len(activeSessions) > 0 && r.URL.Query().Get("new") == "" {
			cur = activeSessions[0]
		}
		hosts, err := grimmHosts(ctx, v.pool)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := grimmTmpl.ExecuteTemplate(w, "grimm", map[string]any{
			"Version": a.cfg.Version, "Session": cur.ID, "CurrentStatus": cur.Status, "CurrentTitle": cur.Title,
			"CurrentHost": cur.Host, "CurrentBranch": cur.Branch,
			"ActiveSessions": activeSessions, "EndedSessions": endedSessions, "Hosts": hosts,
			"DefaultHost": defaultGrimmHost(hosts, a.cfg.GrimmDefaultHost), "OlderSQL": grimmOlderSQL, "DefaultCap": grimm.DefaultMaxWorkers, "CapLimit": grimm.MaxWorkersLimit,
			"Dictation": dictationPath(a.cfg),
		}); err != nil {
			log.Printf("grimm page: %v", err)
		}
	}
}

// grimmHosts are the account's hosts, online first, with the repositories each has registered.
func grimmHosts(ctx context.Context, p *pgxpool.Pool) ([]grimmHost, error) {
	rows, err := p.Query(ctx, `SELECT h.name, coalesce(h.online, false),
		coalesce((SELECT array_agg(DISTINCT r.directory ORDER BY r.directory) FROM host_repositories r WHERE r.host = h.name), '{}')
		FROM host_status h ORDER BY coalesce(h.online, false) DESC, h.name`)
	if err != nil {
		return nil, err
	}
	hosts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (grimmHost, error) {
		var h grimmHost
		return h, r.Scan(&h.Name, &h.Online, &h.Repos)
	})
	if err != nil {
		return nil, err
	}
	for i := range hosts {
		if hosts[i].Desk, err = grimm.DeskRepo(ctx, p, hosts[i].Name); err != nil {
			return nil, err
		}
	}
	return hosts, nil
}

// defaultGrimmHost is the configured host when it is online, else the first online host with a
// desk checkout, else the first online host.
func defaultGrimmHost(hosts []grimmHost, preferred string) string {
	for _, h := range hosts {
		if h.Online && h.Name == preferred {
			return h.Name
		}
	}
	for _, h := range hosts {
		if h.Online && h.Desk != "" {
			return h.Name
		}
	}
	for _, h := range hosts {
		if h.Online {
			return h.Name
		}
	}
	return ""
}

// grimmJSON reads query's rows as JSON objects (to_jsonb of a view row), through the viewer's pool.
func grimmJSON(ctx context.Context, p *pgxpool.Pool, query string, args ...any) ([]json.RawMessage, error) {
	rows, err := p.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[json.RawMessage])
	if out == nil {
		out = []json.RawMessage{}
	}
	return out, err
}

const grimmMaxField = 8000

// trimEntry cuts a transcript item's long text fields (a tool's output, a file's content) so a
// thread or log stays small; the whole is in transcript_entries.
func trimEntry(raw json.RawMessage) json.RawMessage {
	if len(raw) < 2*grimmMaxField {
		return raw
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	var trim func(v any) any
	trim = func(v any) any {
		switch x := v.(type) {
		case string:
			if len(x) > grimmMaxField {
				return strings.ToValidUTF8(x[:grimmMaxField], "") + fmt.Sprintf("\n… (%d more bytes)", len(x)-grimmMaxField)
			}
		case map[string]any:
			for k, vv := range x {
				x[k] = trim(vv)
			}
		case []any:
			for i, vv := range x {
				x[i] = trim(vv)
			}
		}
		return v
	}
	b, err := json.Marshal(trim(m))
	if err != nil {
		return raw
	}
	return b
}

// grimmThreadPage is how many thread items the stream starts with; the page reads older ones a
// page at a time through /api/sql with grimmOlderSQL.
const grimmThreadPage = 200

// grimmOlderSQL is the page's read of the thread items before one it has, newest first: the page
// puts the session's id, and the item's at and item_id, as quoted literals in place of :session,
// :at and :item.
const grimmOlderSQL = `SELECT to_jsonb(t) FROM grimm_thread t WHERE t.session_id = :session
AND (t.at, t.item_id) < (:at::timestamptz, :item) ORDER BY t.at DESC, t.item_id DESC LIMIT 200`

// grimmStream is GET /api/grimm/sessions/{id}/stream: the session's rows as server-sent events,
// pushed as they change. It starts with a snapshot (state, the last 200 thread items, the bus and
// the activity feed), then sends what changes; a reconnect starts with a fresh snapshot.
//
//	state     {"session": grimm_session_status, "workers": [grimm_worker_status], "directives": [grimm_directives], "quota": [grimm_quota]}
//	thread    {"items": [grimm_thread]}       manager words and tool calls grow in place: upsert by item_id
//	bus       {"items": [grimm_messages]}     channel bus
//	activity  {"items": [grimm_events]}
func (a *app) grimmStream(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		flusher, ok := w.(http.Flusher)
		if !ok || !grimm.ValidID(id) {
			http.NotFound(w, r)
			return
		}
		wake, stop := a.changes.Subscribe(fleet.GrimmTopic(id))
		defer stop()
		var host string
		if err := v.pool.QueryRow(ctx, `SELECT host FROM grimm_sessions WHERE id = $1`, id).Scan(&host); err != nil {
			http.NotFound(w, r)
			return
		}
		agentsWake, stopAgents := a.changes.Subscribe(fleet.AgentsTopic(host))
		defer stopAgents()
		hostResWake, stopHostRes := a.changes.Subscribe(fleet.HostResourcesTopic(host))
		defer stopHostRes()
		transcripts := make(chan struct{}, 1)
		wfRuns := make(chan struct{}, 1)
		subs := map[string]func(){}
		runSubs := map[string]func(){}
		defer func() {
			for _, s := range subs {
				s()
			}
			for _, s := range runSubs {
				s()
			}
		}()
		follow := func() error { // every manager's transcript: its words stream into the thread
			rows, err := v.pool.Query(ctx, `SELECT agent_id FROM grimm_managers WHERE session_id = $1 AND agent_id <> ''`, id)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, agent := range ids {
				if subs[agent] != nil {
					continue
				}
				ch, stop := a.changes.Subscribe(fleet.TranscriptTopic(host, agent))
				done := make(chan struct{})
				go relay(ctx, done, ch, transcripts)
				subs[agent] = func() { close(done); stop() }
			}
			return a.grimmFollowWorkflowRuns(ctx, v.pool, id, runSubs, wfRuns)
		}
		if err := follow(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		var lastState, lastWFRuns string
		var quota []json.RawMessage
		sendState := func(withQuota bool) error {
			session, err := grimmJSON(ctx, v.pool, `SELECT to_jsonb(s) FROM grimm_session_status s WHERE id = $1`, id)
			if err != nil {
				return err
			}
			workers, err := grimmJSON(ctx, v.pool, `SELECT to_jsonb(w) - 'activity' || jsonb_build_object('activity', w.activity) FROM grimm_worker_status w
				WHERE session_id = $1 ORDER BY created_at`, id)
			if err != nil {
				return err
			}
			for i := range workers {
				workers[i] = trimEntry(workers[i])
			}
			directives, err := grimmJSON(ctx, v.pool, `SELECT to_jsonb(d) FROM grimm_directives d WHERE session_id = $1 AND removed_at IS NULL ORDER BY seq`, id)
			if err != nil {
				return err
			}
			if withQuota || quota == nil { // the plans view is the costly read: on session changes and every 15 seconds
				if quota, err = grimmJSON(ctx, v.pool, `SELECT to_jsonb(q) FROM grimm_quota q WHERE session_id = $1
					ORDER BY eligible DESC, score DESC NULLS LAST, provider, model, login`, id); err != nil {
					return err
				}
			}
			if len(session) == 0 {
				return errors.New("session gone")
			}
			b, _ := json.Marshal(map[string]any{"session": session[0], "workers": workers, "directives": directives, "quota": quota})
			if string(b) == lastState {
				return nil
			}
			lastState = string(b)
			_, err = fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
			flusher.Flush()
			return err
		}
		lastMsg, lastBus, lastEvent := int64(0), int64(0), int64(0)
		lastEpoch, lastSeq := 0, 0
		sent := map[string]string{} // item_id -> what was sent, for items that can still grow
		sendThread := func(first bool) error {
			q := `SELECT to_jsonb(t), t.item_id, coalesce(t.message_id, 0), coalesce(t.epoch, 0), coalesce(t.seq, 0) FROM grimm_thread t
				WHERE t.session_id = $1 AND (t.message_id > $2 OR (t.epoch IS NOT NULL AND (t.epoch > $3 OR (t.epoch = $3 AND t.seq >= $4))))
				ORDER BY t.at, t.item_id`
			if first {
				q = `SELECT * FROM (SELECT to_jsonb(t), t.item_id, coalesce(t.message_id, 0) AS m, coalesce(t.epoch, 0) AS e, coalesce(t.seq, 0) AS s, t.at
					FROM grimm_thread t WHERE t.session_id = $1 AND $2::bigint >= 0 AND $3::int >= 0 AND $4::int >= 0
					ORDER BY t.at DESC, t.item_id DESC LIMIT ` + strconv.Itoa(grimmThreadPage+1) + `) x ORDER BY x.at, x.item_id`
			}
			rows, err := v.pool.Query(ctx, q, id, lastMsg, lastEpoch, lastSeq)
			if err != nil {
				return err
			}
			type threadRow struct {
				raw        json.RawMessage
				itemID     string
				msg        int64
				epoch, seq int
			}
			var got []threadRow
			for rows.Next() {
				var r threadRow
				var at any
				dest := []any{&r.raw, &r.itemID, &r.msg, &r.epoch, &r.seq}
				if first {
					dest = append(dest, &at)
				}
				if err := rows.Scan(dest...); err != nil {
					rows.Close()
					return err
				}
				got = append(got, r)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			// a first page reads one row more than it sends: that row says there are older ones
			more := first && len(got) > grimmThreadPage
			if more {
				got = got[1:]
			}
			var items []json.RawMessage
			for _, r := range got {
				lastMsg = max(lastMsg, r.msg)
				if r.epoch > lastEpoch || (r.epoch == lastEpoch && r.seq > lastSeq) {
					lastEpoch, lastSeq = r.epoch, r.seq
				}
				raw := trimEntry(r.raw)
				if sent[r.itemID] == string(raw) {
					continue
				}
				if r.epoch > 0 {
					sent[r.itemID] = string(raw)
				}
				items = append(items, raw)
			}
			if len(sent) > 400 { // only the newest entries still grow
				for k := range sent {
					delete(sent, k)
					if len(sent) <= 200 {
						break
					}
				}
			}
			if len(items) == 0 && !first {
				return nil
			}
			return writeSSE(w, flusher, "thread", "", map[string]any{"items": items, "reset": first, "more": more})
		}
		sendRows := func(event, q string, last *int64, limit int) error {
			rows, err := v.pool.Query(ctx, `SELECT * FROM (SELECT to_jsonb(x), x.id FROM (`+q+`) x WHERE x.session_id = $1 AND x.id > $2
				ORDER BY x.id DESC LIMIT `+strconv.Itoa(limit)+`) y ORDER BY y.id`, id, *last)
			if err != nil {
				return err
			}
			var items []json.RawMessage
			for rows.Next() {
				var raw json.RawMessage
				var rid int64
				if err := rows.Scan(&raw, &rid); err != nil {
					rows.Close()
					return err
				}
				*last = max(*last, rid)
				items = append(items, raw)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(items) == 0 {
				return nil
			}
			return writeSSE(w, flusher, event, "", map[string]any{"items": items})
		}
		busQ := `SELECT id, session_id, sender, recipient, kind, body, created_at FROM grimm_messages WHERE channel = 'bus'`
		actQ := `SELECT id, session_id, at, actor, event, level, text FROM grimm_events`
		// What a wake reads: a manager's transcript only the thread (so its words keep up), a host
		// report the state too, a session change and the 15-second tick everything.
		const (
			readThread = 1 << iota
			readState
			readAll
		)
		push := func(what int, first bool) error {
			if what >= readAll {
				if err := follow(); err != nil {
					return err
				}
			}
			if what >= readState {
				if err := sendState(what >= readAll); err != nil {
					return err
				}
			}
			if err := sendThread(first); err != nil {
				return err
			}
			if what < readAll {
				return nil
			}
			if err := sendRows("bus", busQ, &lastBus, 150); err != nil {
				return err
			}
			if err := grimmSendWorkflowRuns(ctx, v.pool, w, flusher, id, &lastWFRuns); err != nil {
				return err
			}
			return sendRows("activity", actQ, &lastEvent, 100)
		}
		if err := push(readAll, true); err != nil {
			return
		}
		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()
		for {
			what := 0
			select {
			case <-ctx.Done():
				return
			case <-wake:
				what = readAll
			case <-agentsWake:
				what = readState
			case <-hostResWake:
				if err := writeSSE(w, flusher, "host_resources", "", map[string]any{"host": host}); err != nil {
					return
				}
				continue
			case <-transcripts:
				what = readThread
			case <-wfRuns:
				if err := grimmSendWorkflowRuns(ctx, v.pool, w, flusher, id, &lastWFRuns); err != nil {
					return
				}
				continue
			case <-keepalive.C:
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				what = readAll
			}
			if err := push(what, false); err != nil {
				return
			}
		}
	}
}

func relay(ctx context.Context, done <-chan struct{}, from <-chan struct{}, to chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-from:
			select {
			case to <- struct{}{}:
			default:
			}
		}
	}
}

// grimmWorkerStream is GET /api/grimm/sessions/{id}/workers/{name}/stream: one worker's
// transcript (its live log), pushed as it grows.
//
//	worker   grimm_worker_status
//	entries  {"items": [transcript_entries], "reset"}  upsert by seq_start
func (a *app) grimmWorkerStream(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, name := r.PathValue("id"), r.PathValue("name")
		flusher, ok := w.(http.Flusher)
		if !ok || !grimm.ValidID(id) {
			http.NotFound(w, r)
			return
		}
		wake, stop := a.changes.Subscribe(fleet.GrimmTopic(id))
		defer stop()
		var host, agent string
		read := func() error {
			return v.pool.QueryRow(ctx, `SELECT host, agent_id FROM grimm_workers WHERE session_id = $1 AND name = $2`, id, name).Scan(&host, &agent)
		}
		if err := read(); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		var tWake <-chan struct{}
		var tStop func()
		defer func() {
			if tStop != nil {
				tStop()
			}
		}()
		agentsWake, stopAgents := a.changes.Subscribe(fleet.AgentsTopic(host))
		defer stopAgents()
		watched := false
		defer func() {
			if watched {
				a.grimmWatch(v.acct.ID, id, name, false)
			}
		}()
		lastSeq, lastWorker, lastText := -1, "", ""
		push := func() error {
			if agent == "" {
				if err := read(); err != nil {
					return err
				}
			}
			if agent != "" && !watched {
				watched = true
				a.grimmWatch(v.acct.ID, id, name, true)
			}
			if agent != "" && tWake == nil {
				tWake, tStop = a.changes.Subscribe(fleet.TranscriptTopic(host, agent))
			}
			wk, err := grimmJSON(ctx, v.pool, `SELECT to_jsonb(w) FROM grimm_worker_status w WHERE session_id = $1 AND name = $2`, id, name)
			if err != nil || len(wk) == 0 {
				return errors.New("worker gone")
			}
			if b := string(trimEntry(wk[0])); b != lastWorker {
				lastWorker = b
				if err := writeSSE(w, flusher, "worker", "", json.RawMessage(b)); err != nil {
					return err
				}
			}
			if agent == "" {
				return nil
			}
			first := lastSeq < 0
			q := `SELECT to_jsonb(e), e.seq_start FROM transcript_entries e WHERE e.host = $1 AND e.agent_id = $2 AND e.seq_start >= $3 ORDER BY e.seq_start`
			if first {
				q = `SELECT * FROM (SELECT to_jsonb(e), e.seq_start FROM transcript_entries e WHERE e.host = $1 AND e.agent_id = $2 AND $3::int >= -1
					ORDER BY e.seq_start DESC LIMIT 300) x ORDER BY 2`
			}
			rows, err := v.pool.Query(ctx, q, host, agent, lastSeq)
			if err != nil {
				return err
			}
			var items []json.RawMessage
			newLast, newText := lastSeq, lastText
			for rows.Next() {
				var raw json.RawMessage
				var seq int
				if err := rows.Scan(&raw, &seq); err != nil {
					rows.Close()
					return err
				}
				raw = trimEntry(raw)
				if seq == lastSeq && string(raw) == lastText {
					continue
				}
				items = append(items, raw)
				if seq >= newLast {
					newLast, newText = seq, string(raw)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			lastSeq, lastText = max(newLast, 0), newText
			if len(items) == 0 && !first {
				return nil
			}
			if items == nil {
				items = []json.RawMessage{}
			}
			return writeSSE(w, flusher, "entries", "", map[string]any{"items": items, "reset": first})
		}
		if err := push(); err != nil {
			return
		}
		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-agentsWake:
			case <-tWake:
			case <-keepalive.C:
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
			}
			if err := push(); err != nil {
				return
			}
		}
	}
}

// grimmWatch counts the open live logs of a worker: the first asks its host to copy its transcript
// at once, and closing the last sets it back to the steady pace.
func (a *app) grimmWatch(account int64, session, name string, open bool) {
	key := session + "/" + name
	a.grimmWatchMu.Lock()
	if a.grimmWatching == nil {
		a.grimmWatching = map[string]int{}
	}
	before := a.grimmWatching[key]
	if open {
		a.grimmWatching[key]++
	} else if before > 0 {
		a.grimmWatching[key]--
	}
	after := a.grimmWatching[key]
	if after == 0 {
		delete(a.grimmWatching, key)
	}
	a.grimmWatchMu.Unlock()
	if (before == 0) == (after == 0) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.grimm.WatchWorker(ctx, account, session, name, after > 0); err != nil {
		log.Printf("grimm: watch %s: %v", key, err)
	}
}
