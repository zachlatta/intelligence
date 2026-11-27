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

  answer    --run <id> --channel email --message-id <id> [--sent-at <RFC3339 or RFC5322>] [--thread-id <id>] [--checked]
  answer    --run <id> --steered-to <run id> [--reason <text>] [--sent-at <handover time>]   another run took the request; its answer counts
  answer    --run <id> --not-needed <reason>   no answer is due (not a request)
  project   --run <id> <directory>|none   the project finder's answer
  summarize --run <id> --summary <text>   save a short task summary without moving the run
  status    --run <id> --now <sentence> [--eta <RFC3339>] [--waiting-on <who>]   report current activity without advancing
  next      --run <id> [--summary <text>]  this step is done; prints the next step (after the last, finishes the run)
  back      --run <id> <step id> --why <text>
  skip      --run <id> --why <text>
  ask       --run <id> --note <file>|- [--secret "<name>[: <purpose>]"]... [--multiline-secret "<name>[: <purpose>]"]...
                                       ask Zach (Markdown); each --secret is a password field on the ask: Intelligence keeps
                                       the value encrypted, fetchable with the run's host key (never in events or
                                       transcripts); fetch it with intelligence secret env or get --to-file (multiline:
                                       a text area, for a PEM key)
  done      --run <id> [--summary <text>]  finish the run now
  fail      --run <id> --why <text>
  suggest   --run <id> <group_token> [--exclude <provider/model>]  record a fresh suggestion without moving
  claim     --run <id> <link or name>      claim a deliverable before building; refused, naming the run, when a live run holds it
  steer     --run <id> --into <run id> --note <file>|- [--deliverable <link or name>]
                                       hand this request to the live run on the same deliverable; finishes this run

  propose <file.json>|-                propose a workflow change or run; prints its approval URL
  withdraw <id> --reason <text>        withdraw your pending proposal with a reason
  answer-proposal --proposal <id> --question <id> --note <file>|- [--reopen]
                                      post a dispatched question's Markdown answer

It reaches the server with this host's key (HOST_SERVER and the key, as the host client has them).`

// CLI runs `intelligence workflow args...` and returns its exit code.
func CLI(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, client *http.Client) int {
	var body []byte
	var path string
	var err error
	if len(args) > 0 && args[0] == "answer" {
		path, body, err = parseAnswerCLI(args[1:])
	} else if len(args) > 0 && args[0] == "answer-proposal" {
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
	reopen := fs.Bool("reopen", false, "return a rejected proposal to review with this answer")
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
	body, _ := json.Marshal(map[string]any{"questionId": *question, "text": string(text), "reopen": *reopen})
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
	fs.StringVar(&m.Now, "now", "", "")
	fs.StringVar(&m.WaitingOn, "waiting-on", "", "person or party holding up the run")
	fs.StringVar(&m.ETA, "eta", "", "")
	fs.StringVar(&m.Why, "why", "", "")
	fs.StringVar(&m.Exclude, "exclude", "", "comma-separated provider/model routes to skip")
	fs.StringVar(&m.Into, "into", "", "steer: the live run on the same deliverable")
	fs.StringVar(&m.Deliverable, "deliverable", "", "steer: what the two runs share")
	var secretFlags secretFlagSlice
	fs.Var(secretFlag{&secretFlags, false}, "secret", "ask: request a secret (\"Name: Purpose\" or JSON); repeatable")
	fs.Var(secretFlag{&secretFlags, true}, "multiline-secret", "ask: request a multi-line secret (a PEM key); repeatable")
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
	case "status":
		_, _, err = activityValues(m)
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
	case "claim":
		m.Deliverable, err = one("the deliverable: its public link or folder, or a short name for the work item")
	case "ask", "steer":
		if m.Move == "steer" && m.Into == "" {
			err = errors.New("steer needs --into <run id>")
			break
		}
		switch *note {
		case "":
			err = fmt.Errorf("%s needs --note <file> (or - for standard input)", m.Move)
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
	if err == nil && len(secretFlags) > 0 {
		m.Secrets = make([]SecretRequest, 0, len(secretFlags))
		for _, raw := range secretFlags {
			sr, serr := parseSecretFlag(raw.value)
			if serr != nil {
				err = serr
				break
			}
			sr.Multiline = sr.Multiline || raw.multiline
			m.Secrets = append(m.Secrets, sr)
		}
		if err == nil {
			m.Secrets, err = validateSecretRequests(m.Move, m.Secrets)
		}
	}
	if err == nil && len(pos) > 0 && m.Move != "project" && m.Move != "back" && m.Move != "suggest" && m.Move != "claim" {
		err = fmt.Errorf("%s takes no argument %q", m.Move, pos[0])
	}
	return m, *run, err
}

// secretFlagSlice keeps --secret and --multiline-secret in the order given: the ask shows them so.
type secretFlagSlice []struct {
	value     string
	multiline bool
}

type secretFlag struct {
	list      *secretFlagSlice
	multiline bool
}

func (s secretFlag) String() string { return "" }

func (s secretFlag) Set(v string) error {
	*s.list = append(*s.list, struct {
		value     string
		multiline bool
	}{v, s.multiline})
	return nil
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

// HostConfig is the server and this host's key, as the host client has them (for other CLIs
// that reach the server as the host, such as `intelligence grimm`).
func HostConfig(getenv func(string) string) (server, key string, err error) {
	return hostConfig(getenv)
}
