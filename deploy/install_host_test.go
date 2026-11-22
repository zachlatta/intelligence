// Package deploy holds tests for the install scripts in this directory. They run each script with
// stub commands (uname, go, launchctl, systemctl, journalctl) first on PATH and a throwaway HOME,
// so nothing is installed on the machine running the tests.
package deploy

import (
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type installHost struct {
	t     *testing.T
	home  string
	stubs string
	calls string // every stub call, one "name args" line each
}

// newInstallHost makes a HOME and stub commands for a host whose `uname -s` is system.
func newInstallHost(t *testing.T, system string) *installHost {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	h := &installHost{t: t, home: t.TempDir(), stubs: t.TempDir()}
	h.calls = filepath.Join(h.stubs, "calls")
	log := `printf '%s\n' "$(basename "$0") $*" >> "` + h.calls + `"` + "\n"
	h.stub("uname", log+`echo `+system)
	// go build ... -o <path> ...: write a stand-in binary at <path>
	h.stub("go", log+`while [ $# -gt 0 ]; do if [ "$1" = -o ]; then printf '#!/bin/sh\n' > "$2"; chmod +x "$2"; fi; shift; done`)
	// the host logs its code once it is started: kickstart and bootstrap stand for that
	h.stub("launchctl", log+`case "$1" in
bootstrap|kickstart) [ -n "${STUB_CODE:-}" ] && printf 'join: enter the code %s on the account page\n' "$STUB_CODE" >> "$HOME/Library/Logs/intelligence-host.log";;
esac
exit 0`)
	h.stub("systemctl", log+`exit 0`)
	h.stub("journalctl", log+`[ -n "${STUB_CODE:-}" ] && printf 'join: enter the code %s on the account page\n' "$STUB_CODE"; exit 0`)
	h.stub("sleep", log+`exit 0`)
	return h
}

func (h *installHost) stub(name, body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

// run runs deploy/install-host with env added; it returns its output and fails the test on an error.
func (h *installHost) run(env ...string) string {
	h.t.Helper()
	cmd := exec.Command("bash", "install-host")
	cmd.Env = append([]string{"HOME=" + h.home, "PATH=" + h.stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"XDG_RUNTIME_DIR=" + filepath.Join(h.home, "run")}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("install-host: %v\n%s", err, out)
	}
	return string(out)
}

func (h *installHost) callLines() []string {
	h.t.Helper()
	b, err := os.ReadFile(h.calls)
	if err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (h *installHost) called(prefix string) bool {
	for _, l := range h.callLines() {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestInstallHostMacOSInstallsLaunchAgent(t *testing.T) {
	h := newInstallHost(t, "Darwin")
	out := h.run("HOST_SERVER=https://server.example", "STUB_CODE=ABCD-1234")

	envFile := filepath.Join(h.home, ".config/intelligence/host.env")
	b, err := os.ReadFile(envFile)
	if err != nil || string(b) != "HOST_SERVER=https://server.example\n" {
		t.Fatalf("host.env = %q, %v", b, err)
	}
	if fi, _ := os.Stat(envFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("host.env mode %v, want 600", fi.Mode().Perm())
	}
	if fi, err := os.Stat(filepath.Join(h.home, ".local/bin/intelligence")); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("binary not installed: %v", err)
	}

	plist := filepath.Join(h.home, "Library/LaunchAgents/intelligence-host.plist")
	pb, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	p := string(pb)
	checkXML(t, pb)
	for _, want := range []string{
		"<key>Label</key>", "<string>intelligence-host</string>",
		"<key>RunAtLoad</key>", "<key>KeepAlive</key>",
		"<key>StandardErrorPath</key>", "<key>StandardOutPath</key>",
		filepath.Join(h.home, "Library/Logs/intelligence-host.log"),
		envFile, filepath.Join(h.home, ".local/bin/intelligence"),
		"<key>PATH</key>", filepath.Join(h.home, ".local/bin") + ":", "/opt/homebrew/bin",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q:\n%s", want, p)
		}
	}

	if h.called("systemctl") || h.called("journalctl") {
		t.Errorf("macOS install called systemd: %q", h.callLines())
	}
	if !h.called("launchctl bootstrap gui/") || !h.called("launchctl kickstart -k gui/") {
		t.Errorf("launchctl calls: %q", h.callLines())
	}
	if !strings.Contains(out, "enter the code ABCD-1234") {
		t.Errorf("output lacks the code:\n%s", out)
	}
}

// checkXML fails the test if b is not well-formed XML.
func checkXML(t *testing.T, b []byte) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(string(b)))
	d.Strict = true
	for {
		if _, err := d.Token(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("plist is not XML: %v\n%s", err, b)
		}
	}
}

func TestInstallHostMacOSRerunRestartsAndPrintsOnlyTheNewCode(t *testing.T) {
	h := newInstallHost(t, "Darwin")
	h.run("HOST_SERVER=https://server.example", "STUB_CODE=OLD-0000")
	if err := os.WriteFile(h.calls, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// the second run keeps host.env (no HOST_SERVER needed) and reads only what the log gained
	out := h.run("STUB_CODE=NEW-1111")
	if strings.Contains(out, "OLD-0000") || !strings.Contains(out, "NEW-1111") {
		t.Errorf("rerun output:\n%s", out)
	}
	calls := h.callLines()
	bootout, bootstrap := -1, -1
	for i, l := range calls {
		if strings.HasPrefix(l, "launchctl bootout gui/") {
			bootout = i
		}
		if strings.HasPrefix(l, "launchctl bootstrap gui/") {
			bootstrap = i
		}
	}
	if bootout < 0 || bootstrap < bootout {
		t.Errorf("rerun should boot out the old agent, then bootstrap: %q", calls)
	}
	if !h.called("go build") {
		t.Errorf("rerun did not rebuild: %q", calls)
	}
}

func TestInstallHostMacOSWithKeySkipsTheCode(t *testing.T) {
	h := newInstallHost(t, "Darwin")
	if err := os.MkdirAll(filepath.Join(h.home, ".config/intelligence"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.home, ".config/intelligence/host.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := h.run("HOST_SERVER=https://server.example", "STUB_CODE=ABCD-1234")
	if strings.Contains(out, "waiting") || strings.Contains(out, "ABCD-1234") {
		t.Errorf("a host with a key should not wait for a code:\n%s", out)
	}
}

func TestInstallHostLinuxKeepsSystemdUserService(t *testing.T) {
	h := newInstallHost(t, "Linux")
	out := h.run("HOST_SERVER=https://server.example", "STUB_CODE=ABCD-1234")
	unit := filepath.Join(h.home, ".config/systemd/user/intelligence-host.service")
	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("unit not installed: %v", err)
	}
	for _, want := range []string{"systemctl --user daemon-reload", "systemctl --user enable intelligence-host.service",
		"systemctl --user restart intelligence-host.service"} {
		if !h.called(want) {
			t.Errorf("no %q: %q", want, h.callLines())
		}
	}
	if h.called("launchctl") {
		t.Errorf("Linux install called launchctl: %q", h.callLines())
	}
	if _, err := os.Stat(filepath.Join(h.home, "Library")); err == nil {
		t.Error("Linux install made ~/Library")
	}
	if !strings.Contains(out, "enter the code ABCD-1234") {
		t.Errorf("output lacks the code:\n%s", out)
	}
}
