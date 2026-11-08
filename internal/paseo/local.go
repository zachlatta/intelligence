package paseo

import (
	"encoding/json"
	"fmt"
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
