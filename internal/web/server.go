// Package web serves Intelligence's HTTP interface.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/email"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/grimm"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/hostwatch"
	"github.com/zachlatta/intelligence/internal/secretbox"
	"github.com/zachlatta/intelligence/internal/timing"
	"github.com/zachlatta/intelligence/internal/voicememo"
	"github.com/zachlatta/intelligence/internal/webpush"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// Config is what the server needs. All app state is in DB; each account reads it through its own
// role, made with Readonly's settings (db.AccountRoles).
type Config struct {
	// PDWURL, PDWKey and PDWAccount turn voice memo triggers on for the account named PDWAccount.
	PDWURL, PDWKey, PDWAccount string
	// HostAlerts, when set, gets one phone notification when a host of PDWAccount's with active
	// runs stops reporting for hostwatch.After, and one when it reports again (internal/hostwatch),
	// and one when an answer-expected run of PDWAccount's fails (workflow.Engine.Notify).
	// Every account's silent hosts show as a red banner either way. NotifyLocation is the zone
	// the notifications give times in.
	HostAlerts     hostwatch.Notifier
	NotifyLocation *time.Location
	// VoiceMemoInterval is how often PDW's voice memo feed is read (default voicememo.Interval).
	VoiceMemoInterval time.Duration
	// Context is the server's lifetime: its listener for fleet changes stops when it ends. Default: never.
	Context    context.Context
	Version    string        // the deployed commit, shown on every page and by /healthz
	DB         *pgxpool.Pool // the app's database, migrated (db.Migrate)
	Readonly   *pgxpool.Pool // the same database as the read-only role (db.EnsureReadonlyRole)
	SQLTimeout time.Duration // per SQL API query; default 5s
	BinDir     string        // host binaries, intelligence-<os>-<arch>, served for self-update; "" = none
	// GrimmDefaultHost is the host Grimm's new-session form picks first when it is online
	// (GRIMM_DEFAULT_HOST); "" picks the first online host.
	GrimmDefaultHost string
	// AssemblyAIKey mints the chat box's dictation tokens; "" turns dictation off.
	AssemblyAIKey      string
	AssemblyAITokenURL string // default AssemblyAI's own
	Now                func() time.Time
	// Google (GOOGLE_OAUTH_CLIENT_JSON) and EmailKey (CONNECTIONS_KEY) turn email triggers on: an
	// account connects a Gmail mailbox, read every EmailInterval (default email.Interval).
	Google        *email.Google
	EmailKey      *email.Keyring
	EmailInterval time.Duration
	// EmailQuotaRate is the Gmail quota units each mailbox is charged per second (default
	// email.QuotaRate); tests on a fake Gmail raise it.
	EmailQuotaRate int
	PublicURL      string // PUBLIC_URL: the server's origin for Google's redirect; default the request's
	// QueueLimits are when a host is too busy to start another workflow run, which then queues;
	// default workflow.DefaultLimits. Queued runs are checked every QueueInterval (default
	// workflow.QueueEvery), on every host minute and whenever a run ends.
	QueueLimits   *workflow.Limits
	QueueInterval time.Duration
	// VAPID enables Web Push notifications (VAPID_PUBLIC_KEY, VAPID_PRIVATE_KEY, VAPID_SUBJECT).
	// Unset turns Web Push off cleanly. PushHTTP overrides the HTTP client used to post pushes (tests).
	VAPID    *webpush.VAPID
	PushHTTP *http.Client
	// SecretBox seals the temporary secrets Zach gives workflow runs on their asks
	// (INTELLIGENCE_SECRETS_KEY). Nil: the page says storage is not configured and storing returns 503.
	SecretBox *secretbox.Box
	// Timing records every request's route, status and duration (http_requests). Its owner runs
	// it (timing.Recorder.Run), so it can flush after the server has drained. Default: one the
	// server runs itself until Context ends.
	Timing *timing.Recorder
}

type app struct {
	cfg              Config
	routes           *http.ServeMux
	store            *accounts.Store
	reg              *fleet.Registry // as the owner: for the host streams and for writes
	changes          *fleet.Changes
	wf               *workflow.Engine // as the owner: workflows and their runs
	grimm            *grimm.Service   // as the owner: Grimm sessions
	grimmWatchMu     sync.Mutex
	grimmWatching    map[string]int // open worker live logs, by session/worker: the first and last flip its pace
	mail             *email.Service // reads connected mailboxes; nil when email triggers are off
	push             *webpush.Store // nil when VAPID is unset
	sseKeepalive     time.Duration  // override for tests; default defaultSSEInterval
	sseTerminalGrace time.Duration  // override for tests; default defaultSSETerminalGrace
}

//go:embed index.html
var indexHTML string

var indexTmpl = template.Must(template.Must(template.New("index").Funcs(localFuncs).Funcs(template.FuncMap{
	"ago":            ago,
	"agents":         agentSummary,
	"short":          short,
	"shortID":        func(id string) string { return id[:min(7, len(id))] }, // the prefix the paseo CLI accepts
	"plural":         plural,
	"allAgents":      allAgents,
	"idleWorkspaces": idleWorkspaces,
	"openWorkspace":  openWorkspace,
	"running":        runningAgents,
	"working":        working,
	"losesChanges":   losesChanges,
}).Funcs(usageFuncs).Funcs(hostFuncs).Parse(indexHTML)).Parse(planHTML + layoutHTML))

// New returns the app's HTTP handler.
func New(cfg Config) http.Handler {
	return newHandler(cfg, true)
}

// newHandler can leave workers to a synchronous test driver. New always starts them.
func newHandler(cfg Config, workers bool) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SQLTimeout == 0 {
		cfg.SQLTimeout = 5 * time.Second
	}
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	if cfg.AssemblyAITokenURL == "" {
		cfg.AssemblyAITokenURL = assemblyAITokenURL
	}
	if cfg.Timing == nil {
		cfg.Timing = timing.New(cfg.DB, timing.QueueSize)
		if workers {
			go cfg.Timing.Run(cfg.Context, timing.FlushEvery)
		}
	}
	reg := fleet.New(cfg.DB)
	changes := fleet.NewChanges(cfg.DB)
	if workers {
		go changes.Run(cfg.Context)
	}
	roles := db.NewAccountRoles(cfg.DB, cfg.Readonly)
	context.AfterFunc(cfg.Context, roles.Close)
	store := accounts.New(cfg.DB, roles)
	a := &app{cfg: cfg, routes: http.NewServeMux(), store: store, reg: reg, changes: changes, wf: workflow.NewEngine(cfg.DB, cfg.Now)}
	a.wf.Changes = changes
	a.grimm = &grimm.Service{DB: cfg.DB, Now: cfg.Now, Workflows: a.wf}
	if cfg.VAPID != nil {
		a.push = &webpush.Store{
			DB:     cfg.DB,
			Client: &webpush.Client{VAPID: cfg.VAPID, HTTP: cfg.PushHTTP, Now: cfg.Now},
			Now:    cfg.Now,
		}
		a.wf.Push = a.push
	}
	a.wf.Notify = cfg.HostAlerts
	a.wf.NotifyAccount = cfg.PDWAccount
	a.wf.SecretBox = cfg.SecretBox
	if cfg.QueueLimits != nil {
		a.wf.Limits = *cfg.QueueLimits
	}
	if cfg.QueueInterval == 0 {
		cfg.QueueInterval = workflow.QueueEvery
	}
	if workers {
		go a.wf.RunQueue(cfg.Context, cfg.QueueInterval)
		go a.wf.RunStartRetries(cfg.Context)
		go a.wf.RunStallWatchdog(cfg.Context)
		go a.wf.RunInterruptionRecovery(cfg.Context)
		go a.wf.RunModelQuotaWatcher(cfg.Context)
		go a.wf.RunSecretPurge(cfg.Context)
		go a.grimm.Run(cfg.Context, changes)
		watch := &hostwatch.Watcher{DB: cfg.DB, Notify: cfg.HostAlerts, Push: a.wf.Push, Account: cfg.PDWAccount, Location: cfg.NotifyLocation,
			Started: cfg.Now(), Now: cfg.Now}
		go watch.Run(cfg.Context)
	}
	if cfg.PDWURL != "" && cfg.PDWKey != "" && cfg.PDWAccount != "" {
		voice := &voicememo.Service{DB: cfg.DB, Engine: a.wf, URL: cfg.PDWURL, Key: cfg.PDWKey, Username: cfg.PDWAccount, Interval: cfg.VoiceMemoInterval}
		if workers {
			go voice.Run(cfg.Context)
		}
	}
	if cfg.Google != nil && cfg.EmailKey != nil {
		a.mail = &email.Service{DB: cfg.DB, Engine: a.wf, Google: cfg.Google, Key: cfg.EmailKey, Now: cfg.Now, QuotaRate: cfg.EmailQuotaRate}
		interval := cfg.EmailInterval
		if interval == 0 {
			interval = email.Interval
		}
		if workers {
			go a.mail.Run(cfg.Context, interval)
		}
	}
	mux := a.routes
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := cfg.DB.Ping(ctx); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "database unreachable", "version": cfg.Version})
			return
		}
		writeJSON(w, map[string]string{"status": "ok", "version": cfg.Version, "database": "ok"})
	})
	mux.HandleFunc("POST /api/accounts", a.signUp)
	mux.HandleFunc("POST /api/session", a.signIn)
	mux.HandleFunc("DELETE /api/session", a.signOut)
	mux.HandleFunc("PUT /api/account/password", a.write(a.changePassword))
	mux.HandleFunc("POST /api/mcp-tokens", a.write(a.createMCPToken))
	mux.HandleFunc("DELETE /api/mcp-tokens/{id}", a.write(a.revokeMCPToken))
	mux.HandleFunc("/mcp", a.mcp)
	mux.HandleFunc("/mcp/{$}", a.mcp)
	mux.HandleFunc("GET /login", loginPage(cfg))
	mux.HandleFunc("GET /manifest.webmanifest", manifestAsset)
	mux.HandleFunc("GET /sw.js", serviceWorkerScript)
	mux.HandleFunc("GET /pwa.js", pwaScript)
	mux.HandleFunc("GET /icon.svg", iconSVGAsset)
	mux.HandleFunc("GET /icon-192.png", icon192Asset)
	mux.HandleFunc("GET /icon-512.png", icon512Asset)
	mux.HandleFunc("GET /icon-maskable-192.png", iconMaskable192Asset)
	mux.HandleFunc("GET /icon-maskable-512.png", iconMaskable512Asset)
	mux.HandleFunc("GET /apple-touch-icon.png", appleTouchIconAsset)
	mux.HandleFunc("GET /notification-badge.png", notificationBadgeAsset)
	mux.HandleFunc("POST /api/push-subscriptions", a.write(a.savePushSubscription))
	mux.HandleFunc("DELETE /api/push-subscriptions", a.write(a.deletePushSubscriptionByEndpoint))
	mux.HandleFunc("DELETE /api/push-subscriptions/{id}", a.write(a.deletePushSubscriptionByID))
	mux.HandleFunc("POST /api/push-subscriptions/test", a.write(a.testPushNotification))
	mux.HandleFunc("GET /ui.css", stylesheet)
	mux.HandleFunc("GET /dictation.js", dictationScript)
	mux.HandleFunc("GET /attachments.js", attachmentsScript)
	mux.HandleFunc("POST /api/attachments", a.write(a.uploadAttachment))
	mux.HandleFunc("GET /submitkeys.js", submitKeysScript)
	// What's New: the release notes (whatsnew), and which of them the account has seen
	mux.HandleFunc("GET /whatsnew.js", whatsNewScript)
	mux.HandleFunc("GET /localtime.js", localTimeScript)
	mux.HandleFunc("GET /pageload.js", pageLoadScript)
	mux.HandleFunc("POST /api/page-loads", a.write(a.recordPageLoad))
	mux.HandleFunc("GET /whats-new/media/{name}", whatsNewMedia())
	mux.HandleFunc("GET /whats-new", a.page(func(v viewer) http.HandlerFunc { return whatsNewPage(cfg, v) }))
	mux.HandleFunc("POST /api/whats-new/seen", a.write(a.markWhatsNewSeen))
	mux.HandleFunc("GET /account", a.page(func(v viewer) http.HandlerFunc { return accountPage(cfg, v) }))
	// hosts: joining an account, and their streams
	mux.HandleFunc("POST /api/host-enrollments", a.enroll)
	mux.HandleFunc("POST /api/host-enrollments/{id}/key", a.collectKey)
	mux.HandleFunc("POST /api/host-enrollments/claim", a.write(a.claimHost))
	mux.HandleFunc("DELETE /api/hosts/{host}", a.write(a.removeHost))
	mux.HandleFunc("GET "+hostclient.StreamPath("{host}"), stream(a))
	mux.Handle("GET /api/host/binary/{os}/{arch}", binaries(cfg.BinDir))
	// before dev boxes were called hosts: a client of then updates itself from here, and old
	// links to its pages still land
	mux.Handle("GET /api/devbox/binary/{os}/{arch}", binaries(cfg.BinDir))
	mux.HandleFunc("GET /devboxes/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		u := *r.URL
		u.Path, u.RawPath = "/hosts/"+r.PathValue("rest"), ""
		http.Redirect(w, r, u.RequestURI(), http.StatusMovedPermanently)
	})
	// what a signed-in account reads: its own hosts, through its own role
	mux.HandleFunc("GET /api/hosts", a.read(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			list, err := v.reg.List(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, list)
		}
	}))
	mux.HandleFunc("POST /api/hosts/{host}/commands", a.write(func(v viewer) http.HandlerFunc { return enqueueCommand(a, v) }))
	mux.HandleFunc("GET /api/hosts/{host}/commands/{id}", a.read(func(v viewer) http.HandlerFunc { return showCommand(a, v) }))
	mux.HandleFunc("POST "+dictationTokenPath, a.write(func(viewer) http.HandlerFunc { return dictationToken(cfg) }))
	mux.HandleFunc("GET /api/sql", a.read(func(v viewer) http.HandlerFunc { return sqlHandler(cfg, v.pool) }))
	mux.HandleFunc("POST /api/sql", a.read(func(v viewer) http.HandlerFunc { return sqlHandler(cfg, v.pool) }))
	mux.HandleFunc("GET /api/hosts/resources", a.read(func(v viewer) http.HandlerFunc { return hostResourcesAPI(v.reg) }))
	mux.HandleFunc("GET /api/usage", a.read(func(v viewer) http.HandlerFunc { return usageAPI(v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/workspaces/new", a.page(func(v viewer) http.HandlerFunc { return newWorkspacePage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/workspaces/{ws}/agents/new", a.page(func(v viewer) http.HandlerFunc { return newAgentPage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/agents/{agent}", a.page(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptPage(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /hosts/{host}/agents/{agent}/blocks", a.read(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptBlocks(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /hosts/{host}/agents/{agent}/resources", a.read(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptResources(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /hosts/{host}/agents/{agent}/entries/{seq}", a.page(func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { transcriptEntry(cfg, v.reg, w, r) }
	}))
	mux.HandleFunc("GET /schedules", a.page(func(v viewer) http.HandlerFunc { return schedulesPage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/schedules/new", a.page(func(v viewer) http.HandlerFunc { return newSchedulePage(cfg, v.reg) }))
	mux.HandleFunc("GET /hosts/{host}/schedules/{id}", a.page(func(v viewer) http.HandlerFunc { return schedulePage(cfg, v.reg) }))
	// workflows: written through the API, read through the account's role; a host's agents move runs with its key
	mux.HandleFunc("POST /api/workflow-proposals", a.hostWrite(a.proposeWorkflow))
	mux.HandleFunc("POST /api/workflow-proposals/{id}/comments", a.write(a.reviewerComment))
	mux.HandleFunc("POST /api/workflow-proposals/{id}/comments/agent", a.hostWrite(a.agentComment))
	mux.HandleFunc("POST /api/workflow-proposals/{id}/questions/{question}/retry", a.write(a.retryProposalAnswer))
	mux.HandleFunc("POST /api/workflow-proposals/{id}/withdraw", a.hostWrite(a.withdrawProposal))
	for _, action := range []string{"approve", "reject", "undo", "reopen"} {
		mux.HandleFunc("POST /api/workflow-proposals/{id}/"+action, a.write(a.decideProposal(action)))
	}
	mux.HandleFunc("GET /workflow-proposals", a.page(proposalsPage))
	mux.HandleFunc("GET /workflow-proposals/{id}", a.page(func(v viewer) http.HandlerFunc { return proposalPage(cfg, v) }))
	mux.HandleFunc("GET /workflows/{id}/versions/{version}", a.page(workflowVersionPage))
	mux.HandleFunc("POST /api/workflows", a.write(a.createWorkflow))
	mux.HandleFunc("POST /api/model-groups", a.write(a.saveModelGroup))
	mux.HandleFunc("PUT /api/model-groups/{id}", a.write(a.saveModelGroup))
	mux.HandleFunc("DELETE /api/model-groups/{id}", a.write(a.deleteModelGroup))
	mux.HandleFunc("POST /api/workflow-runs/{id}/suggestions", a.suggestModel)
	mux.HandleFunc("GET /model-groups.js", modelGroupsScript)
	mux.HandleFunc("PUT /api/workflows/{id}", a.write(a.updateWorkflow))
	mux.HandleFunc("PUT /api/workflows/{id}/default", a.write(a.setDefaultWorkflow))
	mux.HandleFunc("DELETE /api/workflows/{id}", a.write(a.deleteWorkflow))
	mux.HandleFunc("POST /api/workflows/{id}/api-keys", a.write(a.createWorkflowAPIKey))
	mux.HandleFunc("DELETE /api/workflows/{id}/api-keys/{keyId}", a.write(a.revokeWorkflowAPIKey))
	// scoped workflow API-key routes (never accept browser cookies, host keys or MCP tokens)
	mux.HandleFunc("GET /api/workflow-api/workflow", a.workflowAPI(a.apiGetWorkflow))
	mux.HandleFunc("POST /api/workflow-api/runs", a.workflowAPIWrite(a.apiTriggerRun))
	mux.HandleFunc("GET /api/workflow-api/runs", a.workflowAPI(a.apiListRuns))
	mux.HandleFunc("GET /api/workflow-api/runs/{id}", a.workflowAPI(a.apiGetRun))
	mux.HandleFunc("GET /api/workflow-api/runs/{id}/transcripts", a.workflowAPI(a.apiListRunTranscripts))
	mux.HandleFunc("GET /api/workflow-api/runs/{id}/transcripts/{agentId}", a.workflowAPI(a.apiGetRunTranscript))
	mux.HandleFunc("GET /api/workflow-api/runs/{id}/stream", a.workflowAPI(a.apiStreamRun))
	mux.HandleFunc("POST /api/workflow-api/runs/{id}/cancel", a.workflowAPIWrite(a.apiCancelRun))
	mux.HandleFunc("POST /api/workflow-api/runs/{id}/reply", a.workflowAPIWrite(a.apiReplyToRun))
	mux.HandleFunc("POST /api/workflow-api/runs/{id}/input", a.workflowAPIWrite(a.apiAddRunInput))
	mux.HandleFunc("POST /api/workflow-api/runs/{id}/retry", a.workflowAPIWrite(a.apiRetryRun))
	mux.HandleFunc("POST /api/workflow-api/runs/{id}/resume", a.workflowAPIWrite(a.apiResumeRun))
	mux.HandleFunc("POST /api/workflow-api/runs/{id}/start", a.workflowAPIWrite(a.apiStartQueuedRun))
	mux.HandleFunc("POST /api/workflow-runs", a.write(a.startRun))
	mux.HandleFunc("POST /api/workflow-runs/{id}/reply", a.write(a.replyToRun))
	mux.HandleFunc("POST /api/workflow-runs/{id}/secrets", a.write(a.submitRunSecrets))
	mux.HandleFunc("POST /api/workflow-runs/{id}/secrets/fetch", a.fetchRunSecret)
	mux.HandleFunc("POST /api/workflow-runs/{id}/secrets/{secret}/delete", a.write(a.deleteRunSecret))
	mux.HandleFunc("POST /api/workflow-runs/{id}/input", a.write(a.addRunInput))
	mux.HandleFunc("POST /api/workflow-runs/{id}/cancel", a.write(a.cancelRun))
	mux.HandleFunc("POST /api/workflow-runs/{id}/start", a.write(a.startQueuedRun))
	mux.HandleFunc("POST /api/workflow-runs/{id}/resume", a.write(a.resumeRun))
	mux.HandleFunc("POST /api/workflow-runs/{id}/retry", a.write(a.retryRun))
	mux.HandleFunc("POST /api/workflow-runs/{id}/moves", a.moveRun)
	mux.HandleFunc("POST /api/workflow-runs/{id}/answers", a.recordRunAnswer)
	mux.HandleFunc("POST /api/workflow-runs/{id}/answer-resolution", a.recordRunAnswerResolution)
	mux.HandleFunc("GET /workflows", a.page(func(v viewer) http.HandlerFunc { return workflowsPage(cfg, v) }))
	// email triggers: the account's Gmail mailbox, connected through Google's sign-in
	mux.HandleFunc("POST /api/gmail/sign-in", a.write(a.gmailSignIn))
	mux.HandleFunc("POST /api/gmail/connect", a.write(a.gmailConnect))
	mux.HandleFunc("PUT /api/gmail/owners", a.write(a.gmailOwners))
	mux.HandleFunc("DELETE /api/gmail", a.write(a.gmailDisconnect))
	mux.HandleFunc("GET /gmail/callback", gmailCallback)
	mux.HandleFunc("GET /workflows/voice-memos", a.page(func(v viewer) http.HandlerFunc { return voiceMemosPage(cfg, v) }))
	mux.HandleFunc("GET /workflows/email", a.page(func(v viewer) http.HandlerFunc { return emailPage(cfg, a, v) }))
	mux.HandleFunc("GET /workflows/{id}", a.page(func(v viewer) http.HandlerFunc { return workflowPage(cfg, a, v) }))
	mux.HandleFunc("GET /workflow-runs/{id}", a.page(func(v viewer) http.HandlerFunc { return runPage(cfg, v) }))
	mux.HandleFunc("POST /api/login-incidents/{id}/resolve", a.write(a.resolveLoginIncident))
	mux.HandleFunc("GET /logins.js", loginsScript)
	mux.HandleFunc("GET /hostalerts.js", hostAlertsScript)
	mux.HandleFunc("GET /usage", a.page(func(v viewer) http.HandlerFunc { return usagePage(cfg, v.reg) }))
	mux.HandleFunc("GET /resources", a.page(func(v viewer) http.HandlerFunc { return hostResourcesPage(cfg, v.reg) }))
	hosts := a.page(func(v viewer) http.HandlerFunc { return homePage(cfg, v) })
	mux.HandleFunc("GET /hosts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("hours") { // host monitoring's link from when it was /hosts
			u := *r.URL
			u.Path, u.RawPath = "/resources", ""
			http.Redirect(w, r, u.RequestURI(), http.StatusMovedPermanently)
			return
		}
		hosts(w, r)
	})
	a.grimmRoutes(mux)
	mux.HandleFunc("GET /{$}", a.page(func(v viewer) http.HandlerFunc { return a.grimmPage(v) }))
	// every request is timed (http_requests)
	served := timed(cfg.Timing, time.Now, mux)
	// every response names the server's version, so a host sees on any call that it should update
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(hostclient.VersionHeader, cfg.Version)
		served.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// homePage is Hosts (/hosts): the tree of the account's hosts > workspaces > agents, and its plans.
func homePage(cfg Config, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := v.reg.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		plans, err := v.reg.Plans(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		res, err := v.reg.HostResources(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = indexTmpl.Execute(w, map[string]any{
			"Version":   cfg.Version,
			"Short":     short(cfg.Version),
			"Hosts":     list,
			"Plans":     plans,
			"Resources": res,
			"Now":       cfg.Now(),
			"Username":  v.acct.Username,
		})
	}
}

// short is a commit id cut to 12 characters for display; anything shorter is kept whole.
func short(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

func humanDuration(d time.Duration) string {
	d = max(0, d).Truncate(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// agentSummary counts agents by status, as "2 running, 8 idle": running first, then the rest by name.
func agentSummary(agents []fleet.AgentView) string {
	if len(agents) == 0 {
		return "no agents"
	}
	counts := map[string]int{}
	for _, a := range agents {
		counts[a.Status]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if (a == "running") != (b == "running") {
			if a == "running" {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d %s", counts[k], k)
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if stem, ok := strings.CutSuffix(word, "y"); ok {
		return fmt.Sprintf("%d %sies", n, stem)
	}
	for _, end := range []string{"ch", "sh", "s", "x"} {
		if strings.HasSuffix(word, end) {
			return fmt.Sprintf("%d %ses", n, word)
		}
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func allAgents(b fleet.Host) []fleet.AgentView {
	var out []fleet.AgentView
	for _, w := range b.Workspaces {
		out = append(out, w.Agents...)
	}
	return out
}

func idleWorkspaces(b fleet.Host) []fleet.WorkspaceAgents {
	var out []fleet.WorkspaceAgents
	for _, w := range b.Workspaces {
		if len(w.Agents) == 0 {
			out = append(out, w)
		}
	}
	return out
}

// runningAgents counts the agents working on a turn, which archiving would stop.
func runningAgents(agents []fleet.AgentView) int {
	n := 0
	for _, a := range agents {
		if working(a.Status) {
			n++
		}
	}
	return n
}

// losesChanges: archiving the workspace would remove a worktree holding uncommitted changes.
func losesChanges(w fleet.WorkspaceAgents) bool { return w.Kind == "worktree" && w.Dirty }

// openWorkspace says whether w starts open on the home page: it has an agent running or needing attention.
func openWorkspace(w fleet.WorkspaceAgents) bool {
	for _, a := range w.Agents {
		if a.Status == "running" || a.RequiresAttention {
			return true
		}
	}
	return false
}
