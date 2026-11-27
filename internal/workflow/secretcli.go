package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// SecretCLIUsage is `intelligence secret`'s help.
const SecretCLIUsage = `usage: intelligence secret <get|env> --run <id> ...

Agents on a workflow run's host fetch the secrets Zach stored for the run, with the host's key:

  env --run <id> VAR=<name> [VAR=<name>]... -- <command> [args]...
                                            run command with each value in its environment
  get --run <id> --to-file <path> <name>    write the value to a 0600 file (not through a symlink)
  get --run <id> <name>                     print the value to a pipe (refused when stdout is a terminal);
                                            not for agents: an agent's shell output is its transcript

<name> is the secret's name as asked (case-insensitive) or its id. Each fetch is recorded on the
run (who, when, which), never the value. Never print, log or paste a value.
`

var envVarRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// StdoutIsTerminal reports whether w is a terminal (a character device).
func StdoutIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// SecretCLI is `intelligence secret`. isTerminal says whether stdout is a terminal.
func SecretCLI(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, client *http.Client, isTerminal func(io.Writer) bool) int {
	usage := func(err error) int {
		fmt.Fprintf(stderr, "%v\n\n%s", err, SecretCLIUsage)
		return 2
	}
	if len(args) == 0 || (args[0] != "get" && args[0] != "env") {
		return usage(errors.New("want get or env"))
	}
	sub := args[0]
	fs := flag.NewFlagSet("secret "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	run := fs.String("run", "", "")
	toFile := fs.String("to-file", "", "")
	rest := args[1:]
	var command []string
	if sub == "env" {
		for i, a := range rest {
			if a == "--" {
				command = rest[i+1:]
				rest = rest[:i]
				break
			}
		}
	}
	// flags and positionals in any order, as `intelligence workflow` takes them
	var pos []string
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return usage(err)
		}
		rest = fs.Args()
		if len(rest) > 0 {
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
	}
	if *run == "" {
		return usage(errors.New("--run is required"))
	}
	fetch := func(name string) (string, error) {
		return fetchSecret(ctx, getenv, client, *run, name)
	}
	switch sub {
	case "get":
		if len(pos) != 1 {
			return usage(errors.New("get takes one secret name (quote a name with spaces)"))
		}
		if *toFile == "" && isTerminal(stdout) {
			fmt.Fprintln(stderr, "refusing to print a secret to a terminal: pipe it into the command that needs it, or use --to-file <path> or `intelligence secret env`")
			return 1
		}
		if *toFile != "" {
			if fi, err := os.Lstat(*toFile); err == nil && !fi.Mode().IsRegular() {
				fmt.Fprintf(stderr, "%s is not a regular file; not writing a secret to it\n", *toFile)
				return 1
			}
		}
		v, err := fetch(pos[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if *toFile == "" {
			_, err = io.WriteString(stdout, v)
		} else {
			err = writeSecretFile(*toFile, v)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if *toFile != "" {
			fmt.Fprintf(stderr, "wrote %s (0600)\n", *toFile)
		}
		return 0
	default: // env
		if *toFile != "" {
			return usage(errors.New("env takes no --to-file"))
		}
		if len(command) == 0 {
			return usage(errors.New("env needs -- <command>"))
		}
		if len(pos) == 0 {
			return usage(errors.New("env needs at least one VAR=<name>"))
		}
		env := os.Environ()
		for _, b := range pos {
			k, name, ok := strings.Cut(b, "=")
			if !ok || !envVarRe.MatchString(k) || strings.TrimSpace(name) == "" {
				return usage(fmt.Errorf("%q is not VAR=<secret name>", b))
			}
			v, err := fetch(name)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			env = append(env, k+"="+v)
		}
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, stdin, stdout, stderr
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return exit.ExitCode()
			}
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
}

func writeSecretFile(path, v string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := io.WriteString(f, v); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// fetchSecret asks the server for one secret of run, with this host's key.
func fetchSecret(ctx context.Context, getenv func(string) string, client *http.Client, run, name string) (string, error) {
	server, key, err := hostConfig(getenv)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+"/api/workflow-runs/"+run+"/secrets/fetch", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Message string `json:"message"`
		Value   string `json:"value"`
	}
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode/100 != 2 {
		if out.Message == "" {
			out.Message = resp.Status
		}
		return "", fmt.Errorf("%s (%s)", out.Message, resp.Status)
	}
	return out.Value, nil
}
