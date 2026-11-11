// Command intelligence is the Intelligence server and its host client.
//
//	intelligence [server]   serve the web app (the container's default)
//	intelligence host       run on a host: stream its Paseo daemon and its plans' usage to the server, run the server's commands, and stay on its version
//	intelligence probe      run as root on a Linux host: the eBPF probe that charges each agent's CPU and OOM kills
//	intelligence workflow   move a workflow run, from its agents on a host (intelligence workflow -h)
//	intelligence version    print this binary's version
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/email"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/probe"
	"github.com/zachlatta/intelligence/internal/resources"
	"github.com/zachlatta/intelligence/internal/usage"
	"github.com/zachlatta/intelligence/internal/web"
	"github.com/zachlatta/intelligence/internal/whatsnew"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// buildCommit is the commit this binary was built from, stamped with -ldflags "-X main.buildCommit=<sha>".
var buildCommit string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := command(os.Args)
	var err error
	switch cmd {
	case "server":
		err = serve(ctx)
	case "host":
		err = runHost(ctx)
	case "probe":
		if len(os.Args) > 2 && os.Args[2] == "version" {
			// a binary that can run the probe says so: the probe updates only to such a binary
			fmt.Println(version(os.Getenv))
			return
		}
		err = runProbe(ctx)
	case "workflow":
		os.Exit(workflow.CLI(ctx, os.Args[2:], os.Getenv, os.Stdin, os.Stdout, os.Stderr, http.DefaultClient))
	case "version":
		fmt.Println(version(os.Getenv))
	default:
		err = fmt.Errorf("unknown command %q: want server, host, probe, workflow or version", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// command is the command os.Args names: server when none. devbox is host, its name before dev
// boxes were called hosts, which a host installed before then runs from its old unit.
func command(args []string) string {
	switch {
	case len(args) < 2 || args[1] == "":
		return "server"
	case args[1] == "devbox":
		return "host"
	}
	return args[1]
}

// withDevboxNames reads a HOST_ variable, or else its DEVBOX_ name from before dev boxes were
// called hosts: a host installed then keeps its env file, and a server its settings.
func withDevboxNames(getenv func(string) string) func(string) string {
	return func(k string) string {
		if v := getenv(k); v != "" || !strings.HasPrefix(k, "HOST_") {
			return v
		}
		return getenv("DEVBOX_" + strings.TrimPrefix(k, "HOST_"))
	}
}

// serverSettings configure `intelligence server`, from the environment.
type serverSettings struct {
	DatabaseURL      string // DATABASE_URL: the app's database, as a role that owns its tables and can create roles
	ReadonlyURL      string // READONLY_DATABASE_URL: the same database as the read-only role the SQL API uses
	ReadonlyRole     string // the user in ReadonlyURL; the server creates or updates it
	ReadonlyPassword string
	BinDir           string // HOST_BINARIES: host binaries to serve; default /usr/local/share/intelligence/host
	AssemblyAIKey    string // ASSEMBLYAI_API_KEY: for the chat box's dictation; unset turns it off
	// PDW_URL (an HTTPS origin), PDW_API_KEY (a feed:voice-memos key) and PDW_INTELLIGENCE_ACCOUNT
	// (the username whose workflows its voice memos start) turn voice memo triggers on; unset, they are off.
	PDWURL, PDWKey, PDWAccount string
	// GOOGLE_OAUTH_CLIENT_JSON (a Google OAuth client, raw or base64) and CONNECTIONS_KEY (32
	// random bytes, base64: seals each mailbox's token) turn email triggers on; unset, they are off.
	EmailInterval time.Duration // EMAIL_POLL_INTERVAL, default 1s
	Google        *email.Google
	EmailKey      *email.Keyring
	PublicURL     string // PUBLIC_URL: the server's origin, for Google's redirect; default each request's
	// WORKFLOW_QUEUE_CPU_PCT, WORKFLOW_QUEUE_MEM_PCT and WORKFLOW_QUEUE_LOAD_PER_CPU: when a host
	// is near capacity, so a new workflow run queues; each defaults to workflow.DefaultLimits (85,
	// 90 and 1.5), and 0 or "off" turns it off.
	QueueLimits workflow.Limits
}

func serverConfig(getenv func(string) string) (serverSettings, error) {
	getenv = withDevboxNames(getenv)
	c := serverSettings{DatabaseURL: getenv("DATABASE_URL"), ReadonlyURL: getenv("READONLY_DATABASE_URL"), BinDir: getenv("HOST_BINARIES"),
		AssemblyAIKey: getenv("ASSEMBLYAI_API_KEY")}
	if c.BinDir == "" {
		c.BinDir = "/usr/local/share/intelligence/host"
	}
	switch {
	case c.DatabaseURL == "":
		return c, errors.New("DATABASE_URL is not set")
	case c.ReadonlyURL == "":
		return c, errors.New("READONLY_DATABASE_URL is not set: the SQL API's read-only role")
	}
	u, err := url.Parse(c.ReadonlyURL)
	if err != nil {
		return c, fmt.Errorf("READONLY_DATABASE_URL: %w", err)
	}
	c.ReadonlyRole = u.User.Username()
	c.ReadonlyPassword, _ = u.User.Password()
	if c.ReadonlyRole == "" || c.ReadonlyPassword == "" {
		return c, errors.New("READONLY_DATABASE_URL needs a user and a password")
	}
	c.EmailInterval = email.Interval
	if raw := getenv("EMAIL_POLL_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Second {
			return c, fmt.Errorf("EMAIL_POLL_INTERVAL %q: want a duration of at least 1s", raw)
		}
		c.EmailInterval = d
	}
	c.PublicURL = getenv("PUBLIC_URL")
	c.PDWURL, c.PDWKey, c.PDWAccount = getenv("PDW_URL"), getenv("PDW_API_KEY"), getenv("PDW_INTELLIGENCE_ACCOUNT")
	if c.PDWURL != "" || c.PDWKey != "" || c.PDWAccount != "" {
		u, e := url.Parse(c.PDWURL)
		if e != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || c.PDWKey == "" || c.PDWAccount == "" {
			return c, errors.New("voice memo triggers need PDW_URL (HTTPS origin), PDW_API_KEY and PDW_INTELLIGENCE_ACCOUNT")
		}
	}
	if c.QueueLimits, err = queueLimits(getenv); err != nil {
		return c, err
	}
	if raw := getenv("GOOGLE_OAUTH_CLIENT_JSON"); raw != "" {
		if c.Google, err = email.ParseClient(raw); err != nil {
			return c, err
		}
		if c.EmailKey, err = email.NewKeyring(getenv("CONNECTIONS_KEY")); err != nil {
			return c, err
		}
	}
	return c, nil
}

// queueLimits reads the workflow queue's limits over workflow.DefaultLimits.
func queueLimits(getenv func(string) string) (workflow.Limits, error) {
	l := workflow.DefaultLimits()
	for _, f := range []struct {
		name string
		to   *float64
	}{{"WORKFLOW_QUEUE_CPU_PCT", &l.CPUPct}, {"WORKFLOW_QUEUE_MEM_PCT", &l.MemPct}, {"WORKFLOW_QUEUE_LOAD_PER_CPU", &l.LoadPerCPU}} {
		switch v := strings.TrimSpace(getenv(f.name)); v {
		case "":
		case "off":
			*f.to = 0
		default:
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || n < 0 {
				return l, fmt.Errorf("%s %q: want a number, 0 or off", f.name, v)
			}
			*f.to = n
		}
	}
	return l, nil
}

func serve(ctx context.Context) error {
	c, err := serverConfig(os.Getenv)
	if err != nil {
		return err
	}
	v := version(os.Getenv)
	owner, err := pgxpool.New(ctx, c.DatabaseURL)
	if err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err)
	}
	defer owner.Close()
	if err := db.Migrate(ctx, owner); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := db.EnsureReadonlyRole(ctx, owner, c.ReadonlyRole, c.ReadonlyPassword); err != nil {
		return err
	}
	// the release notes this build ships, into whats_new for the What's New tab
	entries, err := whatsnew.Entries()
	if err != nil {
		return err
	}
	if err := whatsnew.Sync(ctx, owner, entries); err != nil {
		return fmt.Errorf("what's new: %w", err)
	}
	ro, err := pgxpool.New(ctx, c.ReadonlyURL)
	if err != nil {
		return fmt.Errorf("READONLY_DATABASE_URL: %w", err)
	}
	defer ro.Close()
	// the read-only SQL contract: a server whose state is not all readable does not start, so its
	// health check fails and the deploy fails
	if err := db.CheckContract(ctx, owner, ro); err != nil {
		return err
	}
	for _, gone := range []string{"DEVBOX_TOKEN", "OPERATOR_TOKEN"} {
		if os.Getenv(gone) != "" {
			log.Printf("%s is set but no longer used: accounts sign in with passwords, and each host has its own key", gone)
		}
	}
	srv := &http.Server{
		Addr: addr(os.Getenv),
		Handler: web.New(web.Config{Context: ctx, Version: v, DB: owner, Readonly: ro, BinDir: c.BinDir,
			AssemblyAIKey: c.AssemblyAIKey, Google: c.Google, EmailKey: c.EmailKey, PublicURL: c.PublicURL, EmailInterval: c.EmailInterval, QueueLimits: &c.QueueLimits,
			PDWURL: c.PDWURL, PDWKey: c.PDWKey, PDWAccount: c.PDWAccount}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go pruneHosts(ctx, fleet.New(owner))
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("intelligence %s listening on %s; read-only SQL contract holds", v, srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// hostSettings configure `intelligence host`, from the environment. Each HOST_ variable may also
// be set by its DEVBOX_ name (withDevboxNames).
type hostSettings struct {
	Server string // HOST_SERVER: the server's base URL
	// HOST_KEY_FILE: where the host keeps its key; default ~/.config/intelligence/host.key, or devbox.key
	// if only it exists
	KeyFile    string
	Name       string        // HOST_NAME: this host's name; default the lowercased hostname
	Interval   time.Duration // HOST_INTERVAL: how often to re-read the daemon's status; default 15s
	PaseoHome  string        // HOST_PASEO_HOME: the Paseo daemon's home; default ~/.paseo
	AutoUpdate bool          // HOST_AUTOUPDATE: "off" keeps this binary; default on
	// HOST_PROBE_SOCKET: the eBPF probe's socket; default /run/intelligence/probe.sock ("off": none)
	ProbeSocket string
}

func hostConfig(getenv func(string) string, hostname func() (string, error)) (hostSettings, error) {
	getenv = withDevboxNames(getenv)
	c := hostSettings{Server: getenv("HOST_SERVER"), KeyFile: getenv("HOST_KEY_FILE"), Name: getenv("HOST_NAME"),
		Interval: 15 * time.Second, PaseoHome: getenv("HOST_PASEO_HOME"), AutoUpdate: getenv("HOST_AUTOUPDATE") != "off",
		ProbeSocket: getenv("HOST_PROBE_SOCKET")}
	switch c.ProbeSocket {
	case "":
		c.ProbeSocket = defaultProbeSocket
	case "off":
		c.ProbeSocket = ""
	}
	if c.PaseoHome == "" {
		c.PaseoHome = filepath.Join(getenv("HOME"), ".paseo")
	}
	if c.KeyFile == "" {
		dir := filepath.Join(getenv("HOME"), ".config", "intelligence")
		c.KeyFile = filepath.Join(dir, "host.key")
		// a host that joined before dev boxes were called hosts keeps the key it has
		if _, err := os.Stat(c.KeyFile); errors.Is(err, fs.ErrNotExist) {
			if _, err := os.Stat(filepath.Join(dir, "devbox.key")); err == nil {
				c.KeyFile = filepath.Join(dir, "devbox.key")
			}
		}
	}
	if c.Server == "" {
		return c, errors.New("HOST_SERVER is not set: the server's base URL")
	}
	if v := getenv("HOST_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return c, fmt.Errorf("HOST_INTERVAL %q: want a duration of at least 1s", v)
		}
		c.Interval = d
	}
	if c.Name == "" {
		h, err := hostname()
		if err != nil {
			return c, fmt.Errorf("no HOST_NAME and no hostname: %w", err)
		}
		c.Name = strings.ToLower(h)
	}
	if !hostclient.ValidName(c.Name) {
		return c, fmt.Errorf("host name %q: use lowercase letters, digits, '.', '_' and '-'", c.Name)
	}
	return c, nil
}

// runHost runs the host client (hostclient.Client): it follows the host's Paseo daemon and holds one
// stream to the server, for reports, transcripts, plan usage and the server's commands, and stays
// on the server's version.
func runHost(ctx context.Context) error {
	c, err := hostConfig(os.Getenv, os.Hostname)
	if err != nil {
		return err
	}
	v := version(os.Getenv)
	b := &hostclient.Client{Name: c.Name, Version: v, StatusEvery: c.Interval,
		Watcher: &hostclient.Watcher{Dial: func(ctx context.Context) (*paseo.Client, error) {
			// re-read each time: the daemon's listen address and credential change when it restarts
			t, err := paseo.LocalTarget(c.PaseoHome, "intelligence-host")
			if err != nil {
				return nil, err
			}
			return paseo.Dial(ctx, t)
		}, Tags: hostclient.PaseoHomeTags(c.PaseoHome)},
		// with no key yet, the host joins an account: it logs a code to enter on the server's account page
		Link: &hostclient.Link{Server: c.Server, Host: c.Name, Keys: &hostclient.Keys{File: c.KeyFile, Server: c.Server, Host: c.Name}}}
	if c.AutoUpdate {
		if up, err := newUpdater(c, v); err != nil {
			log.Printf("self-update off: %v", err)
		} else {
			b.Update = up.Maybe
		}
	}
	// every 5 minutes: each plan's usage, per login, from each provider's own meter
	home, _ := os.UserHomeDir()
	b.Usage = &hostclient.UsageSync{Read: (&usage.Reader{Home: home}).Read}
	// every minute: the host, each agent's processes, and the eBPF probe's CPU and OOM kills
	probeSocket := c.ProbeSocket
	if runtime.GOOS != "linux" {
		probeSocket = ""
	}
	b.Resources = &resources.Collector{Reader: &resources.Reader{Home: home}, Walker: &resources.AgentWalker{}, ProbeSocket: probeSocket}
	log.Printf("intelligence host %s streaming as %s to %s (self-update %v)", v, c.Name, c.Server, b.Update != nil)
	return b.Run(ctx)
}

// hostRetention is how long host and agent minutes are kept (OOM kills are kept).
const hostRetention = 90 * 24 * time.Hour

func pruneHosts(ctx context.Context, reg *fleet.Registry) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := reg.PruneHosts(ctx, time.Now().Add(-hostRetention)); err != nil && ctx.Err() == nil {
			log.Printf("prune host minutes: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

const defaultProbeSocket = "/run/intelligence/probe.sock"

// probeSettings configure `intelligence probe`, from the environment (/etc/intelligence/probe.env).
type probeSettings struct {
	Server     string // PROBE_SERVER: the server's base URL, to follow its version; "" never updates
	User       string // PROBE_USER: the host user, who may read the socket
	Socket     string // PROBE_SOCKET: default /run/intelligence/probe.sock
	AutoUpdate bool   // PROBE_AUTOUPDATE: "off" keeps this binary; default on
}

func probeConfig(getenv func(string) string) probeSettings {
	c := probeSettings{Server: getenv("PROBE_SERVER"), User: getenv("PROBE_USER"), Socket: getenv("PROBE_SOCKET"),
		AutoUpdate: getenv("PROBE_AUTOUPDATE") != "off"}
	if c.Socket == "" {
		c.Socket = defaultProbeSocket
	}
	return c
}

// runProbe loads the eBPF probe and serves its counters on a socket only the host user (and
// root) can read. Like the host client it follows the server's version: every minute it asks
// the server's version and, when it differs, downloads the server's binary, checks its sha256 and
// that it runs and reports that version, replaces itself and re-executes.
func runProbe(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("the probe loads eBPF programs: run it as root (deploy/install-probe)")
	}
	c := probeConfig(os.Getenv)
	v := version(os.Getenv)
	uid := -1
	if c.User != "" {
		u, err := user.Lookup(c.User)
		if err != nil {
			return fmt.Errorf("PROBE_USER: %w", err)
		}
		if uid, err = strconv.Atoi(u.Uid); err != nil {
			return fmt.Errorf("PROBE_USER %s: uid %q", c.User, u.Uid)
		}
	}
	p, err := probe.Start(ctx, v)
	if err != nil {
		return err
	}
	defer p.Close()
	if err := os.MkdirAll(filepath.Dir(c.Socket), 0o755); err != nil {
		return err
	}
	served := make(chan error, 1)
	go func() { served <- probe.Serve(ctx, c.Socket, uid, p, nil) }()
	log.Printf("intelligence probe %s: loaded; serving %s", v, c.Socket)
	var up *hostclient.Updater
	if c.AutoUpdate && c.Server != "" {
		dc := hostSettings{Server: c.Server}
		if up, err = newUpdater(dc, v); err != nil {
			log.Printf("self-update off: %v", err)
		} else {
			// only to a binary that runs the probe: a server rolled back to before the probe keeps
			// the probe on its own version instead of replacing it with one that cannot run
			up.Verify = func(ctx context.Context, path string) (string, error) {
				out, err := exec.CommandContext(ctx, path, "probe", "version").Output()
				return string(out), err
			}
		}
	}
	t := time.NewTicker(hostclient.UpdateEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-served:
			return fmt.Errorf("socket: %w", err)
		case <-t.C:
			if up == nil {
				continue
			}
			if sv, err := serverVersion(ctx, c.Server); err != nil {
				log.Printf("server version: %v", err)
			} else if err := up.Maybe(ctx, sv); err != nil {
				log.Printf("self-update failed: %v", err)
			}
		}
	}
}

// serverVersion asks the server its version: every response names it.
func serverVersion(ctx context.Context, server string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(server, "/")+"/healthz", nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	res.Body.Close()
	return res.Header.Get(hostclient.VersionHeader), nil
}

func newUpdater(c hostSettings, current string) (*hostclient.Updater, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	return &hostclient.Updater{
		Server: c.Server, Client: &http.Client{Timeout: 2 * time.Minute}, Current: current, Exe: exe,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Now: time.Now,
		Verify: func(ctx context.Context, path string) (string, error) {
			out, err := exec.CommandContext(ctx, path, "version").Output()
			return string(out), err
		},
		Exec: func(path string) error {
			log.Printf("updated: restarting as %s", path)
			return syscall.Exec(path, os.Args, os.Environ())
		},
	}, nil
}

// addr is the listen address: all interfaces on $PORT, default 8080.
func addr(getenv func(string) string) string {
	if p := getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8080"
}

// version is the commit this binary was built from (buildCommit), else Coolify's SOURCE_COMMIT, else
// the VCS revision Go stamped, else "dev".
func version(getenv func(string) string) string {
	if buildCommit != "" {
		return buildCommit
	}
	if v := getenv("SOURCE_COMMIT"); v != "" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "dev"
}
