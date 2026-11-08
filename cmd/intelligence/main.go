// Command intelligence is the Intelligence server and its dev-box client.
//
//	intelligence [server]   serve the web app (the container's default)
//	intelligence devbox     run on a dev box: stream its Paseo daemon to the server, run the server's commands, and stay on its version
//	intelligence version    print this binary's version
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/web"
)

// buildCommit is the commit this binary was built from, stamped with -ldflags "-X main.buildCommit=<sha>".
var buildCommit string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "", "server":
		err = serve(ctx)
	case "devbox":
		err = runDevbox(ctx)
	case "version":
		fmt.Println(version(os.Getenv))
	default:
		err = fmt.Errorf("unknown command %q: want server, devbox or version", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// serverSettings configure `intelligence server`, from the environment.
type serverSettings struct {
	DatabaseURL      string // DATABASE_URL: the app's database, as a role that owns its tables and can create roles
	ReadonlyURL      string // READONLY_DATABASE_URL: the same database as the read-only role the SQL API uses
	ReadonlyRole     string // the user in ReadonlyURL; the server creates or updates it
	ReadonlyPassword string
	BinDir           string // DEVBOX_BINARIES: dev box binaries to serve; default /usr/local/share/intelligence/devbox
}

func serverConfig(getenv func(string) string) (serverSettings, error) {
	c := serverSettings{DatabaseURL: getenv("DATABASE_URL"), ReadonlyURL: getenv("READONLY_DATABASE_URL"), BinDir: getenv("DEVBOX_BINARIES")}
	if c.BinDir == "" {
		c.BinDir = "/usr/local/share/intelligence/devbox"
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
	return c, nil
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
	if os.Getenv("DEVBOX_TOKEN") == "" {
		log.Printf("DEVBOX_TOKEN is not set: dev box streams are refused")
	}
	if os.Getenv("OPERATOR_TOKEN") == "" {
		log.Printf("OPERATOR_TOKEN is not set: commands to dev boxes are refused")
	}
	srv := &http.Server{
		Addr: addr(os.Getenv),
		Handler: web.New(web.Config{Version: v, DB: owner, Readonly: ro, DevboxToken: os.Getenv("DEVBOX_TOKEN"),
			OperatorToken: os.Getenv("OPERATOR_TOKEN"), BinDir: c.BinDir}),
		ReadHeaderTimeout: 10 * time.Second,
	}
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

// devboxSettings configure `intelligence devbox`, from the environment.
type devboxSettings struct {
	Server     string        // DEVBOX_SERVER: the server's base URL
	Token      string        // DEVBOX_TOKEN: the server's dev-box token
	Name       string        // DEVBOX_NAME: this box's name; default the lowercased hostname
	Interval   time.Duration // DEVBOX_INTERVAL: time between reports; default 15s
	PaseoHome  string        // DEVBOX_PASEO_HOME: the Paseo daemon's home; default ~/.paseo
	AutoUpdate bool          // DEVBOX_AUTOUPDATE: "off" keeps this binary; default on
}

func devboxConfig(getenv func(string) string, hostname func() (string, error)) (devboxSettings, error) {
	c := devboxSettings{Server: getenv("DEVBOX_SERVER"), Token: getenv("DEVBOX_TOKEN"), Name: getenv("DEVBOX_NAME"),
		Interval: 15 * time.Second, PaseoHome: getenv("DEVBOX_PASEO_HOME"), AutoUpdate: getenv("DEVBOX_AUTOUPDATE") != "off"}
	if c.PaseoHome == "" {
		c.PaseoHome = filepath.Join(getenv("HOME"), ".paseo")
	}
	switch {
	case c.Server == "":
		return c, errors.New("DEVBOX_SERVER is not set: the server's base URL")
	case c.Token == "":
		return c, errors.New("DEVBOX_TOKEN is not set: the server's dev box token")
	}
	if v := getenv("DEVBOX_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return c, fmt.Errorf("DEVBOX_INTERVAL %q: want a duration of at least 1s", v)
		}
		c.Interval = d
	}
	if c.Name == "" {
		h, err := hostname()
		if err != nil {
			return c, fmt.Errorf("no DEVBOX_NAME and no hostname: %w", err)
		}
		c.Name = strings.ToLower(h)
	}
	if !devbox.ValidName(c.Name) {
		return c, fmt.Errorf("dev box name %q: use lowercase letters, digits, '.', '_' and '-'", c.Name)
	}
	return c, nil
}

// runDevbox follows the box's Paseo daemon over its protocol and holds one stream to the server.
// Up the stream go reports (within a second of a change, and every interval, which also re-reads
// the daemon's status), the transcripts of loaded agents that changed (every 2 seconds while one
// is running) and command results; down it come the server's commands, which run against the
// daemon. The box updates itself to the server's version when they differ (at most once a minute).
func runDevbox(ctx context.Context) error {
	c, err := devboxConfig(os.Getenv, os.Hostname)
	if err != nil {
		return err
	}
	v := version(os.Getenv)
	w := &devbox.Watcher{Dial: func(ctx context.Context) (*paseo.Client, error) {
		// re-read each time: the daemon's listen address and credential change when it restarts
		t, err := paseo.LocalTarget(c.PaseoHome, "intelligence-devbox")
		if err != nil {
			return nil, err
		}
		return paseo.Dial(ctx, t)
	}}
	go w.Run(ctx)
	var up *devbox.Updater
	if c.AutoUpdate {
		if up, err = newUpdater(c, v); err != nil {
			log.Printf("self-update off: %v", err)
		}
	}
	versions := make(chan string, 1)
	link := &devbox.Link{Server: c.Server, Token: c.Token, Box: c.Name, ServerVersion: func(sv string) {
		select {
		case versions <- sv:
		default:
		}
	}}
	go link.Run(ctx)
	runner := &devbox.Runner{Exec: w.Execute, Report: link.Update}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cmd := <-link.Commands():
				log.Printf("command %s: %s", cmd.ID, cmd.Kind)
				runner.Handle(ctx, cmd)
			}
		}
	}()
	transcripts := &devbox.TranscriptSync{Box: c.Name, Timeline: w.Timeline, Cursors: link.TranscriptCursors, Send: link.Transcript}
	log.Printf("intelligence devbox %s streaming as %s to %s (self-update %v)", v, c.Name, c.Server, up != nil)
	heartbeat := time.NewTicker(c.Interval)
	defer heartbeat.Stop()
	live := time.NewTicker(2 * time.Second)
	defer live.Stop()
	lastErr, lastLinkErr, lastSyncErr := "", "", ""
	for {
		report := true
		select {
		case <-ctx.Done():
			return nil
		case sv := <-versions:
			if up != nil {
				if err := up.Maybe(ctx, sv); err != nil {
					log.Printf("self-update failed: %v", err)
				}
			}
			continue
		case <-link.Connected():
			log.Printf("stream to %s: connected", c.Server)
			transcripts.Reset() // the server's copies may have changed while the stream was down
		case <-heartbeat.C:
			w.RefreshStatus(ctx)
		case <-w.Changed():
			// a burst of updates (an agent's turn starting, its workspace moving) goes in one report
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		case <-live.C:
			report = false // between reports, keep running agents' transcripts current
		}
		snap := w.Snapshot()
		if snap.Error != lastErr {
			if snap.Error != "" {
				log.Printf("paseo daemon: %s", snap.Error)
			} else {
				log.Printf("paseo daemon: connected")
			}
			lastErr = snap.Error
		}
		if !report && !anyRunning(snap.Agents) {
			continue
		}
		var err error
		if report {
			sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err = link.Report(sendCtx, devbox.Report{Name: c.Name, Version: v, Paseo: snap, CollectedAt: time.Now()})
			cancel()
			if msg := fmt.Sprint(err); err != nil && msg != lastLinkErr && ctx.Err() == nil {
				log.Printf("report failed: %v (stream: %s)", err, link.Err())
				lastLinkErr = msg
			} else if err == nil && lastLinkErr != "" {
				log.Printf("reporting again")
				lastLinkErr = ""
			}
		}
		if snap.Connected && err == nil {
			syncCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			serr := transcripts.Sync(syncCtx, snap.Agents)
			cancel()
			if msg := fmt.Sprint(serr); serr != nil && msg != lastSyncErr && ctx.Err() == nil {
				log.Printf("transcript sync: %v", serr)
				lastSyncErr = msg
			} else if serr == nil {
				lastSyncErr = ""
			}
		}
	}
}

func anyRunning(agents []devbox.Agent) bool {
	for _, a := range agents {
		if a.Status == "running" {
			return true
		}
	}
	return false
}

func newUpdater(c devboxSettings, current string) (*devbox.Updater, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	return &devbox.Updater{
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
