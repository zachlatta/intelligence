package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cliSecretValue = "s3cr3t-value-for-cli-tests"

// secretServer answers fetches for "Stripe API key" on run r1 with the host key "host-key".
func secretServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/workflow-runs/r1/secrets/fetch" || r.Header.Get("Authorization") != "Bearer host-key" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "no such run on this host"})
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked = append(asked, body.Name)
		if body.Name != "Stripe API key" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Zach has not given it yet"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"name": body.Name, "value": cliSecretValue})
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func secretEnv(t *testing.T, server string) func(string) string {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "host.key")
	if err := os.WriteFile(keyFile, []byte("host-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOST_SERVER": server, "HOST_KEY_FILE": keyFile, "HOME": t.TempDir()}
	return func(k string) string { return env[k] }
}

func runSecretCLI(t *testing.T, getenv func(string) string, terminal bool, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := SecretCLI(context.Background(), args, getenv, strings.NewReader(""), &out, &errb, http.DefaultClient,
		func(io.Writer) bool { return terminal })
	return code, out.String(), errb.String()
}

func TestSecretGetPrintsOnlyToAPipe(t *testing.T) {
	srv, asked := secretServer(t)
	env := secretEnv(t, srv.URL)
	code, out, errs := runSecretCLI(t, env, false, "get", "--run", "r1", "Stripe API key")
	if code != 0 || out != cliSecretValue || errs != "" {
		t.Fatalf("get to a pipe = %d %q %q", code, out, errs)
	}
	code, out, errs = runSecretCLI(t, env, true, "get", "--run", "r1", "Stripe API key")
	if code == 0 || strings.Contains(out+errs, cliSecretValue) || !strings.Contains(errs, "terminal") {
		t.Fatalf("get to a terminal = %d %q %q: want refused, value unprinted", code, out, errs)
	}
	if len(*asked) != 1 {
		t.Fatalf("a refused terminal get still fetched: %v", *asked)
	}
	code, _, errs = runSecretCLI(t, env, false, "get", "--run", "r1", "Other")
	if code == 0 || !strings.Contains(errs, "not given it yet") {
		t.Fatalf("server refusal = %d %q", code, errs)
	}
}

func TestSecretGetToFileIs0600(t *testing.T) {
	srv, _ := secretServer(t)
	env := secretEnv(t, srv.URL)
	path := filepath.Join(t.TempDir(), "stripe.key")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runSecretCLI(t, env, true, "get", "--run", "r1", "--to-file", path, "Stripe API key")
	if code != 0 || strings.Contains(out+errs, cliSecretValue) {
		t.Fatalf("get --to-file = %d %q %q", code, out, errs)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != cliSecretValue {
		t.Fatalf("file = %q %v", b, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runSecretCLI(t, env, false, "get", "--run", "r1", "--to-file", link, "Stripe API key"); code == 0 {
		t.Fatal("wrote a secret through a symlink")
	}
}

func TestSecretEnvRunsTheCommandWithTheValue(t *testing.T) {
	srv, _ := secretServer(t)
	env := secretEnv(t, srv.URL)
	out := filepath.Join(t.TempDir(), "out")
	code, stdout, errs := runSecretCLI(t, env, true, "env", "--run", "r1", "STRIPE_KEY=Stripe API key", "--",
		"sh", "-c", `test "$STRIPE_KEY" = "`+cliSecretValue+`" && echo ok > "$0"`, out)
	if code != 0 || strings.Contains(stdout+errs, cliSecretValue) {
		t.Fatalf("env = %d %q %q", code, stdout, errs)
	}
	if b, _ := os.ReadFile(out); strings.TrimSpace(string(b)) != "ok" {
		t.Fatalf("command did not see the value in its environment: %q", b)
	}
	if code, _, _ := runSecretCLI(t, env, true, "env", "--run", "r1", "STRIPE_KEY=Stripe API key", "--", "sh", "-c", "exit 3"); code != 3 {
		t.Fatalf("env exit code = %d, want the command's 3", code)
	}
	for _, bad := range [][]string{
		{"env", "--run", "r1", "STRIPE_KEY=Stripe API key"},         // no command
		{"env", "--run", "r1", "--", "true"},                        // no binding
		{"env", "--run", "r1", "1BAD=Stripe API key", "--", "true"}, // bad variable name
		{"env", "--run", "r1", "STRIPE_KEY=Other", "--", "true"},    // server refuses
		{"get", "Stripe API key"},                                   // no run
		{"get", "--run", "r1"},                                      // no name
		{"nope", "--run", "r1", "Stripe API key"},                   // unknown subcommand
	} {
		if code, _, _ := runSecretCLI(t, env, false, bad...); code == 0 {
			t.Errorf("%v succeeded", bad)
		}
	}
}
