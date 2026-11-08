// Package probe is Intelligence's eBPF probe: a small root service on each Linux dev box that
// follows every process in the kernel and charges its CPU time to the Paseo agent it belongs to,
// counts the commands each agent starts, and records each process the kernel kills for memory,
// with its agent. Short-lived commands (git, tests, builds) are counted exactly, which reading
// /proc every few seconds misses.
//
// A process belongs to an agent when its environment names it (PASEO_AGENT_ID) or its parent
// belongs to it: the kernel side copies the parent's agent to each new process, and the user side
// reads the environment of each new program that has none. The dev box client reads the counters
// over a Unix socket (Fetch) and sends them to the server.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Snapshot is the probe's counters now. They count up from Epoch: a new epoch (the probe
// restarted, or ran out of agent slots and began again) restarts them.
type Snapshot struct {
	Version string              `json:"version"`
	Epoch   string              `json:"epoch"`
	Agents  map[string]Counters `json:"agents"`
	OOMs    []OOM               `json:"ooms,omitempty"` // those after the seq asked for
	Seq     uint64              `json:"seq"`            // the newest OOM's
	Tracked int                 `json:"tracked"`        // processes and threads with an agent, recently exited ones included
}

// Counters are one agent's totals since the epoch began.
type Counters struct {
	CPUNanos uint64 `json:"cpuNanos"` // CPU time of all its processes, finished ones included
	Execs    uint64 `json:"execs"`    // programs its processes started
}

// OOM is a process the kernel chose to kill for memory.
type OOM struct {
	Seq      uint64    `json:"seq"`
	At       time.Time `json:"at"`
	PID      int       `json:"pid"`
	Comm     string    `json:"comm,omitempty"`
	AgentID  string    `json:"agentId,omitempty"`
	AnonRSS  uint64    `json:"anonRss"` // bytes
	FileRSS  uint64    `json:"fileRss"`
	ShmemRSS uint64    `json:"shmemRss"`
}

// Source is what the socket serves.
type Source interface{ Snapshot(after uint64) Snapshot }

// ErrUnsupported: this system cannot run the probe.
var ErrUnsupported = errors.New("the eBPF probe needs Linux")

// Serve answers GET /snapshot?after=<seq> on a Unix socket at path until ctx ends. The socket is
// readable and writable by uid only (and root); uid < 0 leaves it to the current user. ready, if
// set, is called once it listens.
func Serve(ctx context.Context, path string, uid int, src Source, ready func()) error {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if uid >= 0 {
		if err := os.Chown(path, uid, -1); err != nil {
			return err
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /snapshot", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(src.Snapshot(after))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if ready != nil {
		ready()
	}
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Fetch reads the probe's snapshot, with the OOMs after seq after.
func Fetch(ctx context.Context, path string, after uint64) (Snapshot, error) {
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://probe/snapshot?after="+strconv.FormatUint(after, 10), nil)
	if err != nil {
		return Snapshot{}, err
	}
	res, err := c.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("probe: %s", res.Status)
	}
	var s Snapshot
	err = json.NewDecoder(res.Body).Decode(&s)
	return s, err
}

type field struct{ offset, size int }

// parseFormat reads a tracepoint's format file: each field's offset and size in the record the
// kernel passes the program. Offsets differ between kernels, so they are read, not assumed.
func parseFormat(text string) map[string]field {
	out := map[string]field{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		decl, ok := strings.CutPrefix(line, "field:")
		if !ok {
			continue
		}
		parts := strings.Split(decl, ";")
		if len(parts) < 3 {
			continue
		}
		words := strings.Fields(parts[0])
		if len(words) == 0 {
			continue
		}
		name := words[len(words)-1]
		if i := strings.IndexByte(name, '['); i >= 0 {
			name = name[:i]
		}
		var f field
		for _, p := range parts[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(p), ":")
			n, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			switch k {
			case "offset":
				f.offset = n
			case "size":
				f.size = n
			}
		}
		out[name] = f
	}
	return out
}
