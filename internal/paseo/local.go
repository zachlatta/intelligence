package paseo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// LocalTarget is the daemon running from Paseo home dir (~/.paseo): its listen address from
// paseo.pid and its local credential, which only the daemon's user can read.
func LocalTarget(home, clientID string) (Target, error) {
	b, err := os.ReadFile(filepath.Join(home, "paseo.pid"))
	if err != nil {
		return Target{}, fmt.Errorf("no running Paseo daemon found (reading paseo.pid): %w", err)
	}
	var lock struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(b, &lock); err != nil || lock.Listen == "" {
		return Target{}, fmt.Errorf("paseo.pid has no listen address")
	}
	host, port, err := net.SplitHostPort(lock.Listen)
	if err != nil || strings.HasPrefix(lock.Listen, "/") {
		return Target{}, fmt.Errorf("the daemon listens on %q: only TCP listen addresses are supported", lock.Listen)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1" // listening on every interface: dial loopback
	}
	tok, err := os.ReadFile(filepath.Join(home, "local-credential"))
	if err != nil {
		return Target{}, fmt.Errorf("reading the daemon's local credential: %w", err)
	}
	return Target{URL: "ws://" + net.JoinHostPort(host, port) + "/ws", Token: strings.TrimSpace(string(tok)), ClientID: clientID}, nil
}

// CreationKey is the idempotency key a workspace was created with, from the receipt the daemon
// keeps in home/creations (the same directory as LocalTarget's). The daemon writes the receipt and
// its claim on the workspace id before the workspace exists, so a workspace with no receipt never
// gets one: it was made before the daemon kept receipts, or opened rather than created. That is
// "" with no error.
func CreationKey(home, workspaceID string) (string, error) {
	dir := filepath.Join(home, "creations")
	var id bytes.Buffer
	enc := json.NewEncoder(&id) // as the daemon's JSON.stringify: no HTML escaping
	enc.SetEscapeHTML(false)
	if err := enc.Encode([]string{"workspace", workspaceID}); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(id.Bytes(), []byte("\n")))
	identity, err := os.ReadFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".claim"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(identity))
	if name == "" || strings.ContainsAny(name, `/\.`) {
		return "", fmt.Errorf("workspace %s: a bad creation claim", workspaceID)
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return "", fmt.Errorf("workspace %s: its creation receipt: %w", workspaceID, err)
	}
	var receipt struct {
		Snapshot struct {
			IdempotencyKey string `json:"idempotencyKey"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(b, &receipt); err != nil {
		return "", fmt.Errorf("workspace %s: its creation receipt: %w", workspaceID, err)
	}
	return receipt.Snapshot.IdempotencyKey, nil
}

// CreatedInApp reports whether a creation's idempotency key is one the Paseo app (desktop, web or
// mobile) makes: its composer keys each draft "draft_<message id>". The CLI, MCP tools and
// Intelligence's commands use other keys.
func CreatedInApp(key string) bool { return strings.HasPrefix(key, "draft_") }
