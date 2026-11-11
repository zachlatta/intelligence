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
  suggest   --run <id> <group_token> [--exclude <provider/model>]  record a fresh suggestion without moving

  propose <file.json>|-                propose a workflow change or run; prints its approval URL
  withdraw <id> --reason <text>        withdraw your pending proposal with a reason
  answer-proposal --proposal <id> --question <id> --note <file>|-
                                      post a dispatched question's Markdown answer

It reaches the server with this host's key (HOST_SERVER and the key, as the host client has them).`

// CLI runs `intelligence workflow args...` and returns its exit code.
func CLI(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, client *http.Client) int {
	var body []byte
	var path string
	var err error
	if len(args) > 0 && args[0] == "answer-proposal" {
		path, body, err = parseAnswerProposalCLI(args[1:], stdin)
	} else if len(args) > 0 && (args[0] == "propose" || args[0] == "withdraw") {
		path, body, err = parseProposalCLI(args, stdin)
	} else {
		var m Move
		var run string
		m, run, err = parseCLI(args, stdin)
		body, _ = json.Marshal(m)
		path = "/api/workflow-runs/" + run + "/moves"
		if m.Move == "suggest" {
			path = "/api/workflow-runs/" + run + "/suggestions"
			body, _ = json.Marshal(struct {
				Token   string   `json:"token"`
				Exclude []string `json:"exclude"`
			}{m.Token, strings.FieldsFunc(m.Exclude, func(r rune) bool { return r == ',' })})
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s\n", err, CLIUsage)
		return 2
	}
	server, key, err := hostConfig(getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+path, bytes.NewReader(body))
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

func parseAnswerProposalCLI(args []string, stdin io.Reader) (string, []byte, error) {
	fs := flag.NewFlagSet("answer-proposal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	proposal := fs.String("proposal", "", "")
	question := fs.Int64("question", 0, "")
	note := fs.String("note", "", "")
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	if fs.NArg() != 0 || !uuidRe.MatchString(*proposal) || *question <= 0 || *note == "" {
		return "", nil, errors.New("answer-proposal needs --proposal <UUID> --question <positive id> --note <file>|-")
	}
	source := stdin
	if *note != "-" {
		f, err := os.Open(*note)
		if err != nil {
			return "", nil, err
		}
		defer f.Close()
		source = f
	}
	text, err := io.ReadAll(io.LimitReader(source, 20001))
	if err != nil {
		return "", nil, err
	}
	if len(text) > 20000 || strings.TrimSpace(string(text)) == "" {
		return "", nil, errors.New("answer must contain 1 to 20000 bytes")
	}
	body, _ := json.Marshal(map[string]any{"questionId": *question, "text": string(text)})
	return "/api/workflow-proposals/" + *proposal + "/comments/agent", body, nil
}

func parseProposalCLI(args []string, stdin io.Reader) (string, []byte, error) {
	if args[0] == "propose" {
		if len(args) != 2 {
			return "", nil, errors.New("propose needs <file.json> (or - for standard input)")
		}
		var b []byte
		var err error
		if args[1] == "-" {
			b, err = io.ReadAll(io.LimitReader(stdin, 1<<20+1))
		} else {
			var f *os.File
			f, err = os.Open(args[1])
			if err == nil {
				defer f.Close()
				b, err = io.ReadAll(io.LimitReader(f, 1<<20+1))
			}
		}
		if err != nil {
			return "", nil, err
		}
		if len(b) > 1<<20 || !json.Valid(b) {
			return "", nil, errors.New("proposal must be JSON, at most 1 MiB")
		}
		return "/api/workflow-proposals", b, nil
	}
	var id, reason string
	fs := flag.NewFlagSet("withdraw", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&reason, "reason", "", "")
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return "", nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		if id != "" {
			return "", nil, errors.New("withdraw takes one proposal id")
		}
		id = fs.Arg(0)
		rest = fs.Args()[1:]
	}
	if id == "" || strings.ContainsAny(id, "/?#") || strings.TrimSpace(reason) == "" {
		return "", nil, errors.New("withdraw needs <id> --reason <text>")
	}
	b, _ := json.Marshal(map[string]string{"reason": reason})
	return "/api/workflow-proposals/" + id + "/withdraw", b, nil
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
	fs.StringVar(&m.Exclude, "exclude", "", "comma-separated provider/model routes to skip")
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
	case "suggest":
		m.Token, err = one("a model group’s token key, without braces")
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
	if err == nil && len(pos) > 0 && m.Move != "project" && m.Move != "back" && m.Move != "suggest" {
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
