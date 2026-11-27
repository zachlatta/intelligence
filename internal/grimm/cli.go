package grimm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zachlatta/intelligence/internal/workflow"
)

// AgentPath is the route the CLI calls; AgentHeader names the calling agent (PASEO_AGENT_ID).
const (
	AgentPath   = "/api/grimm/agent"
	AgentHeader = "X-Paseo-Agent-Id"
)

// CLIUsage is `intelligence grimm`'s help.
const CLIUsage = `usage: intelligence grimm ctl|bus <command> [...]

Grimm's agents run these on their host; each acts as the agent that runs it (PASEO_AGENT_ID).
A message, task or rule is the rest of the line, - for stdin, or --file PATH.

  ctl quota                              plan headroom and where the next hire goes
  ctl roster                             who is hired, their tasks, statuses and last replies
  ctl hire --role <role> [--name <name>] [--provider claude|codex|antigravity] <task>|-
  ctl say <name> <message>|-             a follow-up to a worker (the manager only)
  ctl peek <name> [--lines N]            the tail of a worker's activity
  ctl result <name>                      a worker's latest full reply
  ctl wait <name...> [--timeout S]       block until they finish (at most 10 minutes)
  ctl stop <name>                        interrupt a worker's turn
  ctl archive <name|all>                 archive finished workers (files stay)
  ctl directive list | add <rule>|- | remove <n>
  ctl workflow request --workflow <id|name> [--summary S] [--host H] <input>|-
  ctl workflow list | status <proposal-or-run-id>
  bus read [--limit N] [--mine]          the shared message board
  bus post --to <name|grimm|all> [--kind chat|handoff|question|result] <message>|-

It reaches the server with this host's key (HOST_SERVER and the key, as the host client has them).`

// CLI runs `intelligence grimm args...` and returns its exit code.
func CLI(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, client *http.Client) int {
	call, err := ParseCLI(args, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s\n", err, CLIUsage)
		return 2
	}
	agent := getenv("PASEO_AGENT_ID")
	if agent == "" {
		fmt.Fprintln(stderr, "PASEO_AGENT_ID is not set: run this from inside a Grimm agent")
		return 1
	}
	server, key, err := workflow.HostConfig(getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	timeout := time.Minute
	if call.Verb == "wait" {
		timeout = MaxWait + time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(call)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+AgentPath, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(AgentHeader, agent)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
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

// ParseCLI reads a command line into the call the server runs.
func ParseCLI(args []string, stdin io.Reader) (Call, error) {
	var c Call
	if len(args) < 2 || (args[0] != "ctl" && args[0] != "bus") {
		return c, errors.New("want ctl or bus, then a command")
	}
	group, verb := args[0], args[1]
	var pos []string
	file, dash := "", false
	flags := map[string]*string{"--role": &c.Role, "--name": &c.Name, "--provider": &c.Provider, "--to": &c.To, "--kind": &c.Kind, "--file": &file, "--workflow": &c.Workflow, "--summary": &c.Summary, "--host": &c.Host}
	var ignored string
	for _, f := range []string{"--from", "--state-dir"} {
		flags[f] = &ignored // kept from the standalone CLI: the server knows who and where
	}
	ints := map[string]*int{"--lines": &c.Lines, "--limit": &c.Lines, "--timeout": &c.Timeout}
	mine := false
	rest := args[2:]
	if group == "ctl" && verb == "--state-dir" && len(args) > 3 {
		verb, rest = args[3], args[4:]
	}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		name, val, hasVal := strings.Cut(a, "=")
		if p, ok := flags[name]; ok && strings.HasPrefix(a, "--") {
			if !hasVal {
				if i+1 >= len(rest) {
					return c, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = rest[i]
			}
			*p = val
			continue
		}
		if p, ok := ints[name]; ok && strings.HasPrefix(a, "--") {
			if !hasVal {
				if i+1 >= len(rest) {
					return c, fmt.Errorf("%s needs a number", name)
				}
				i++
				val = rest[i]
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return c, fmt.Errorf("%s needs a number", name)
			}
			*p = n
			continue
		}
		switch {
		case a == "--mine":
			mine = true
		case a == "-":
			dash = true
		case strings.HasPrefix(a, "--"):
			return c, fmt.Errorf("unknown flag %s", a)
		default:
			pos = append(pos, a)
		}
	}
	text := func(from int) (string, error) {
		switch {
		case file != "":
			b, err := os.ReadFile(file)
			return strings.TrimSpace(string(b)), err
		case dash:
			b, err := io.ReadAll(io.LimitReader(stdin, MaxText+1))
			return strings.TrimSpace(string(b)), err
		case len(pos) > from:
			return strings.Join(pos[from:], " "), nil
		}
		return "", nil
	}
	var err error
	switch group + " " + verb {
	case "ctl quota", "ctl roster":
		c.Verb = verb
	case "ctl hire":
		c.Verb = verb
		if c.Text, err = text(0); err == nil && c.Text == "" {
			err = errors.New("hire needs a task")
		}
	case "ctl say":
		c.Verb = verb
		if len(pos) == 0 {
			return c, errors.New("say needs a worker's name")
		}
		c.Args = pos[:1]
		if c.Text, err = text(1); err == nil && c.Text == "" {
			err = errors.New("say needs a message")
		}
	case "ctl peek", "ctl result", "ctl stop", "ctl archive":
		c.Verb = verb
		if len(pos) != 1 {
			return c, fmt.Errorf("%s needs one worker's name", verb)
		}
		c.Args = pos
	case "ctl wait":
		c.Verb, c.Args = verb, pos
	case "ctl directive":
		c.Verb = verb
		if len(pos) == 0 {
			pos = []string{"list"}
		}
		c.Args = pos[:1]
		switch pos[0] {
		case "list":
		case "add":
			if c.Text, err = text(1); err == nil && c.Text == "" {
				err = errors.New("directive add needs a rule")
			}
		case "remove":
			if len(pos) != 2 {
				return c, errors.New("directive remove <n>")
			}
			c.Args = pos
		default:
			return c, errors.New("directive list | add <rule> | remove <n>")
		}
	case "ctl workflow":
		c.Verb = verb
		if len(pos) == 0 {
			pos = []string{"list"}
		}
		c.Args = pos[:1]
		switch pos[0] {
		case "list":
		case "request":
			if strings.TrimSpace(c.Workflow) == "" {
				return c, errors.New("workflow request needs --workflow <id|name>")
			}
			if c.Text, err = text(1); err == nil && c.Text == "" {
				err = errors.New("workflow request needs task input")
			}
		case "status":
			if len(pos) != 2 {
				return c, errors.New("workflow status <proposal-or-run-id>")
			}
			c.Args = pos
		default:
			return c, errors.New("workflow request --workflow <id|name> <input>|- | list | status <id>")
		}
	case "bus read":
		c.Verb = "bus-read"
		if mine {
			c.Args = []string{"mine"}
		}
	case "bus post":
		c.Verb = "bus-post"
		if c.To == "" {
			return c, errors.New("post needs --to")
		}
		if c.Text, err = text(0); err == nil && c.Text == "" {
			err = errors.New("post needs a message")
		}
	default:
		return c, fmt.Errorf("unknown command %s %s", group, verb)
	}
	return c, err
}
