package workflow

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CLIUsage is `intelligence workflow`'s help.
const CLIUsage = `usage: intelligence workflow <move> --run <id> [...]

The agents of a workflow run move it with these, on the run's host:

  project   --run <id> <directory>|none   the project finder's answer
  summarize --run <id> --summary <text>   save a short task summary without moving the run
  next      --run <id> [--summary <text>]  this step is done; prints the next step (after the last, finishes the run)
  back      --run <id> <step id> --why <text>
  skip      --run <id> --why <text>
  ask       --run <id> --note <file>|-     ask Zach (Markdown); his reply comes as your next message
  done      --run <id> [--summary <text>]  finish the run now
  fail      --run <id> --why <text>

It reaches the server with this host's key (HOST_SERVER and the key, as the host client has them).`

// CLI runs `intelligence workflow args...` and returns its exit code.
func CLI(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, client *http.Client) int {
	m, run, err := parseCLI(args, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s\n", err, CLIUsage)
		return 2
	}
	server, key, err := hostConfig(getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	body, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+"/api/workflow-runs/"+run+"/moves", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &out) != nil || out.Message == "" {
		out.Message = strings.TrimSpace(string(b))
	}
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(stderr, "%s (%s)\n", out.Message, resp.Status)
		return 1
	}
	fmt.Fprintln(stdout, out.Message)
	return 0
}

// parseCLI reads a move and its run from args; flags may come before or after the positional argument.
func parseCLI(args []string, stdin io.Reader) (Move, string, error) {
	if len(args) == 0 {
		return Move{}, "", errors.New("which move?")
	}
	m := Move{Move: args[0]}
	fs := flag.NewFlagSet("intelligence workflow "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	run := fs.String("run", "", "the run's id")
	fs.StringVar(&m.Summary, "summary", "", "")
	fs.StringVar(&m.Why, "why", "", "")
	note := fs.String("note", "", "")
	var pos []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return m, "", err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if *run == "" {
		return m, "", errors.New("--run is required")
	}
	one := func(what string) (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("%s needs %s", m.Move, what)
		}
		return pos[0], nil
	}
	var err error
	switch m.Move {
	case "project":
		m.Directory, err = one("a directory, or none")
	case "back":
		m.Step, err = one("the step's id")
	case "summarize":
		if strings.TrimSpace(m.Summary) == "" {
			err = errors.New("summarize needs --summary <text>")
		}
	case "ask":
		switch *note {
		case "":
			err = errors.New("ask needs --note <file> (or - for standard input)")
		case "-":
			b, rerr := io.ReadAll(bufio.NewReader(stdin))
			m.Note, err = string(b), rerr
		default:
			b, rerr := os.ReadFile(*note)
			m.Note, err = string(b), rerr
		}
	case "next", "skip", "done", "fail":
	default:
		err = fmt.Errorf("unknown move %q", m.Move)
	}
	if err == nil && len(pos) > 0 && m.Move != "project" && m.Move != "back" {
		err = fmt.Errorf("%s takes no argument %q", m.Move, pos[0])
	}
	return m, *run, err
}

// hostConfig is the server and this host's key, as the host client has them: HOST_SERVER and
// HOST_KEY_FILE from the environment, else from ~/.config/intelligence/host.env; the key in
// host.key. A host installed when hosts were called dev boxes has DEVBOX_ names, devbox.env and
// devbox.key instead.
func hostConfig(getenv func(string) string) (server, key string, err error) {
	dir := filepath.Join(getenv("HOME"), ".config", "intelligence")
	file := map[string]string{}
	for _, name := range []string{"devbox.env", "host.env"} { // host.env wins
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
				file[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	get := func(k string) string {
		for _, name := range []string{"HOST_" + k, "DEVBOX_" + k} {
			if v := getenv(name); v != "" {
				return v
			}
		}
		for _, name := range []string{"HOST_" + k, "DEVBOX_" + k} {
			if v := file[name]; v != "" {
				return v
			}
		}
		return ""
	}
	server = get("SERVER")
	if server == "" {
		return "", "", errors.New("HOST_SERVER is not set, and " + filepath.Join(dir, "host.env") + " does not name it: is this a host?")
	}
	keyFile := get("KEY_FILE")
	if keyFile == "" {
		keyFile = filepath.Join(dir, "host.key")
		if _, err := os.Stat(keyFile); err != nil {
			keyFile = filepath.Join(dir, "devbox.key")
		}
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return "", "", fmt.Errorf("this host's key: %w", err)
	}
	return server, strings.TrimSpace(string(b)), nil
}
