// Command intelligence is the Intelligence server and its dev-box client.
//
//	intelligence [server]   serve the web app (the container's default)
//	intelligence devbox     run on a dev box: report its Paseo daemon to the server
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/web"
)

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
	default:
		err = fmt.Errorf("unknown command %q: want server or devbox", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func serve(ctx context.Context) error {
	v := version(os.Getenv)
	srv := &http.Server{
		Addr:              addr(os.Getenv),
		Handler:           web.New(web.Config{Version: v, DevboxToken: os.Getenv("DEVBOX_TOKEN")}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if os.Getenv("DEVBOX_TOKEN") == "" {
		log.Printf("DEVBOX_TOKEN is not set: dev box reports are refused")
	}
	log.Printf("intelligence %s listening on %s", v, srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// devboxSettings configure `intelligence devbox`, from the environment.
type devboxSettings struct {
	Server   string        // DEVBOX_SERVER: the server's base URL
	Token    string        // DEVBOX_TOKEN: the server's dev-box token
	Name     string        // DEVBOX_NAME: this box's name; default the lowercased hostname
	Interval time.Duration // DEVBOX_INTERVAL: time between reports; default 15s
	Paseo    string        // DEVBOX_PASEO: the paseo executable; default paseo on PATH
}

func devboxConfig(getenv func(string) string, hostname func() (string, error)) (devboxSettings, error) {
	c := devboxSettings{Server: getenv("DEVBOX_SERVER"), Token: getenv("DEVBOX_TOKEN"), Name: getenv("DEVBOX_NAME"),
		Interval: 15 * time.Second, Paseo: getenv("DEVBOX_PASEO")}
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

// runDevbox reports this box to the server every interval until ctx ends.
func runDevbox(ctx context.Context) error {
	c, err := devboxConfig(os.Getenv, os.Hostname)
	if err != nil {
		return err
	}
	v := version(os.Getenv)
	col := &devbox.Collector{Now: time.Now, Bin: c.Paseo, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}}
	client := &http.Client{Timeout: 20 * time.Second}
	log.Printf("intelligence devbox %s reporting as %s to %s every %s", v, c.Name, c.Server, c.Interval)
	failing := false
	for {
		readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rep := devbox.Report{Name: c.Name, Version: v, Paseo: col.Paseo(readCtx), CollectedAt: time.Now()}
		err := devbox.Send(readCtx, client, c.Server, c.Token, rep)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil:
			log.Printf("report failed: %v", err)
			failing = true
		case err == nil && failing:
			log.Printf("reporting again")
			failing = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(c.Interval):
		}
	}
}

// addr is the listen address: all interfaces on $PORT, default 8080.
func addr(getenv func(string) string) string {
	if p := getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8080"
}

// version is the deployed commit: Coolify's SOURCE_COMMIT, else the VCS revision Go stamped, else "dev".
func version(getenv func(string) string) string {
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
