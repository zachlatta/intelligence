package hostclient

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

// Watcher keeps one connection to the host's Paseo daemon and its workspaces and agents current:
// it reads them once per connection, then applies the updates the daemon pushes. When the
// connection ends it reports why and dials again.
type Watcher struct {
	Dial       func(ctx context.Context) (*paseo.Client, error)
	RetryEvery time.Duration // between dial attempts; default 5s
	Now        func() time.Time
	// Tags, when set, gives a workspace's tags (tags.go). It is asked once per workspace, and again
	// only after it fails.
	Tags func(workspaceID string) ([]string, error)

	mu         sync.Mutex
	client     *paseo.Client
	err        string
	info       paseo.ServerInfo
	status     paseo.DaemonStatus
	statusAt   time.Time
	statusErr  string
	catalog    map[string]paseo.ProviderEntry // each provider's last ready entry, kept across connections
	workspaces map[string]paseo.Workspace
	agents     map[string]paseo.Agent
	tags       map[string][]string // each workspace's, kept across connections: they do not change
	changed    chan struct{}
	once       sync.Once

	// queued messages (queue.go): each agent's, in order; when a queued one was last sent, until
	// word of its turn arrives; and a channel closed (and replaced) on every change to look again
	queues   map[string][]queuedMessage
	settling map[string]time.Time
	wake     chan struct{}
}

func (w *Watcher) init() {
	w.once.Do(func() {
		w.changed = make(chan struct{}, 1)
		if w.RetryEvery == 0 {
			w.RetryEvery = 5 * time.Second
		}
		if w.Now == nil {
			w.Now = time.Now
		}
	})
}

// Changed receives after the snapshot changes (coalesced: one pending signal at most).
func (w *Watcher) Changed() <-chan struct{} {
	w.init()
	return w.changed
}

func (w *Watcher) signal() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// Run connects and follows the daemon until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	w.init()
	for ctx.Err() == nil {
		err := w.session(ctx)
		w.mu.Lock()
		w.client, w.workspaces, w.agents = nil, nil, nil
		if ctx.Err() == nil {
			w.err = err.Error()
		}
		w.wakeLocked()
		w.mu.Unlock()
		w.signal()
		select {
		case <-ctx.Done():
		case <-time.After(w.RetryEvery):
		}
	}
}

// session is one connection: read everything, then apply updates until it ends. It returns why.
func (w *Watcher) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, err := w.Dial(dialCtx)
	cancel()
	if err != nil {
		return err
	}
	defer c.Close()
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// workspaces and agents first: they subscribe, so no update between the reads is lost
	workspaces, err := c.Workspaces(readCtx)
	if err != nil {
		return err
	}
	agents, err := c.Agents(readCtx)
	if err != nil {
		return err
	}
	// status too, so the first snapshot of this connection is whole
	status, statusErr := c.DaemonStatus(readCtx)
	catalog, _ := c.ProviderCatalog(readCtx) // a failure keeps the last; the next status read tries again
	ids := make([]string, len(workspaces))
	for i, x := range workspaces {
		ids[i] = x.ID
	}
	w.tag(ids)
	w.mu.Lock()
	if statusErr != nil {
		w.statusErr = statusErr.Error()
	} else {
		w.status, w.statusAt, w.statusErr = status, w.Now(), ""
	}
	w.mergeCatalogLocked(catalog)
	w.client, w.err, w.info = c, "", c.Info()
	w.workspaces = make(map[string]paseo.Workspace, len(workspaces))
	for _, x := range workspaces {
		w.workspaces[x.ID] = x
	}
	w.agents = make(map[string]paseo.Agent, len(agents))
	for _, a := range agents {
		w.agents[a.ID] = a
	}
	w.wakeLocked()
	w.mu.Unlock()
	cancel()
	w.signal()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.Done():
			return c.Err()
		case e := <-c.Events():
			w.apply(e)
		}
	}
}

func (w *Watcher) apply(e paseo.Event) {
	if e.Workspace != nil {
		w.tag([]string{e.Workspace.ID})
	}
	w.mu.Lock()
	switch {
	case e.Agent != nil:
		w.agents[e.Agent.ID] = *e.Agent
		delete(w.settling, e.Agent.ID) // word of the turn a queued message started
	case e.AgentRemoved != "":
		delete(w.agents, e.AgentRemoved)
	case e.Workspace != nil:
		w.workspaces[e.Workspace.ID] = *e.Workspace
	case e.WorkspaceRemoved != "":
		delete(w.workspaces, e.WorkspaceRemoved)
		delete(w.tags, e.WorkspaceRemoved)
	}
	w.wakeLocked()
	w.mu.Unlock()
	w.signal()
}

// RefreshStatus re-reads the daemon's status and providers. A failed read keeps the last one.
// It signals Changed only when what it read differs from before (a status that failed, or came
// back, counts).
func (w *Watcher) RefreshStatus(ctx context.Context) {
	w.init()
	w.mu.Lock()
	c := w.client
	w.mu.Unlock()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s, err := c.DaemonStatus(ctx)
	catalog, _ := c.ProviderCatalog(ctx) // a failure keeps the last
	w.mu.Lock()
	before, beforeErr := w.status, w.statusErr
	if err != nil {
		w.statusErr = err.Error()
	} else {
		w.status, w.statusAt, w.statusErr = s, w.Now(), ""
	}
	catalogChanged := w.mergeCatalogLocked(catalog)
	changed := catalogChanged || w.statusErr != beforeErr || !sameStatus(w.status, before)
	w.mu.Unlock()
	if changed {
		w.signal()
	}
}

// mergeCatalogLocked takes each ready provider's entry from entries and drops a provider the
// daemon says it cannot run; one still loading or failing keeps its last. It says whether the
// catalog changed. Called with w.mu held.
func (w *Watcher) mergeCatalogLocked(entries []paseo.ProviderEntry) bool {
	if w.catalog == nil {
		w.catalog = map[string]paseo.ProviderEntry{}
	}
	changed := false
	for _, e := range entries {
		old, had := w.catalog[e.Provider]
		switch {
		case e.Status == "ready" && (e.Enabled == nil || *e.Enabled):
			e.Enabled, e.Error = nil, ""
			if !had || !reflect.DeepEqual(old, e) {
				w.catalog[e.Provider], changed = e, true
			}
		case e.Status == "unavailable" || (e.Enabled != nil && !*e.Enabled):
			if had {
				delete(w.catalog, e.Provider)
				changed = true
			}
		}
	}
	return changed
}

func sameStatus(a, b paseo.DaemonStatus) bool {
	return a.Version == b.Version && a.Listen == b.Listen && a.PID == b.PID && a.ServerID == b.ServerID &&
		a.StartedAt.Equal(b.StartedAt) && slices.Equal(a.Providers, b.Providers)
}

// Snapshot is the daemon as the watcher knows it now. Workspaces are in name order and agents
// in creation order.
func (w *Watcher) Snapshot() PaseoStatus {
	w.init()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client == nil {
		return PaseoStatus{Error: cmp.Or(w.err, "connecting to the Paseo daemon")}
	}
	s := PaseoStatus{Connected: true, StatusError: w.statusErr, Version: cmp.Or(w.status.Version, w.info.Version),
		Hostname: w.info.Hostname, Listen: w.status.Listen, PID: w.status.PID, StartedAt: w.status.StartedAt, ProvidersAt: w.statusAt,
		Workspaces: []Workspace{}, Agents: []Agent{}}
	for _, p := range w.status.Providers {
		s.Providers = append(s.Providers, withCatalog(Provider{Name: p.Provider, Available: p.Available, Error: p.Error}, w.catalog[p.Provider]))
	}
	for _, x := range w.workspaces {
		s.Workspaces = append(s.Workspaces, Workspace{ID: x.ID, Name: cmp.Or(x.Title, x.Name), Kind: x.Kind, Status: x.Status,
			Directory: x.Directory, Project: x.Project, ProjectRoot: x.ProjectRoot, Branch: x.Branch(), Dirty: x.Dirty(), Additions: x.Additions(),
			Deletions: x.Deletions(), CreatedAt: x.CreatedAt, ActivityAt: x.ActivityAt, LastUserMessageAt: x.LastUserMessageAt,
			Tags: w.tags[x.ID]})
	}
	slices.SortFunc(s.Workspaces, func(a, b Workspace) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	for _, a := range w.agents {
		s.Agents = append(s.Agents, Agent{ID: a.ID, WorkspaceID: a.WorkspaceID, ParentID: a.ParentAgentID(), Title: a.Title,
			Provider: a.Provider, Model: a.ModelName(), Thinking: a.Thinking(), Status: a.Status, Cwd: a.Cwd,
			RequiresAttention: a.RequiresAttention, AttentionReason: a.AttentionReason, CreatedAt: a.CreatedAt,
			UpdatedAt: a.UpdatedAt, LastUserMessageAt: a.LastUserMessageAt})
	}
	slices.SortFunc(s.Agents, func(a, b Agent) int { return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID)) })
	return s
}

// withCatalog is p with the models and modes of its catalog entry e.
func withCatalog(p Provider, e paseo.ProviderEntry) Provider {
	p.Label, p.DefaultMode = e.Label, e.DefaultModeID
	for _, m := range e.Models {
		x := Model{ID: m.ID, Label: m.Label, Description: m.Description, Default: m.IsDefault, DefaultThinking: m.DefaultThinking}
		for _, o := range m.ThinkingOptions {
			x.Thinking = append(x.Thinking, o.ID)
		}
		p.Models = append(p.Models, x)
	}
	for _, m := range e.Modes {
		p.Modes = append(p.Modes, Mode{ID: m.ID, Label: m.Label, Description: m.Description})
	}
	return p
}

// Timeline reads an agent's timeline on the current connection (see paseo.Client.Timeline: only
// for loaded agents).
func (w *Watcher) Timeline(ctx context.Context, agentID string, after *paseo.Cursor) (paseo.Timeline, error) {
	w.mu.Lock()
	c := w.client
	w.mu.Unlock()
	if c == nil {
		return paseo.Timeline{}, errors.New("not connected to the Paseo daemon")
	}
	return c.Timeline(ctx, agentID, after)
}
