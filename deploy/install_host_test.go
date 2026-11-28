// Package deploy holds tests for the install scripts in this directory. They run each script with
// stub commands (uname, go, launchctl, systemctl, journalctl) first on PATH and a throwaway HOME,
// so nothing is installed on the machine running the tests.
package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
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
	// the script finds the repo with git rev-parse
	if err := exec.Command("git", "rev-parse", "--show-toplevel").Run(); err != nil {
		t.Skip("no git checkout here (the image build): CI and make test run these tests")
	}
	h := &installHost{t: t, home: t.TempDir(), stubs: t.TempDir()}
	h.calls = filepath.Join(h.stubs, "calls")
	log := `printf '%s\n' "$(basename "$0") $*" >> "` + h.calls + `"` + "\n"
	h.stub("uname", log+`case "${1:-}" in -m) echo "${STUB_ARCH:-x86_64}";; *) echo `+system+`;; esac`)
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
	gotUnit, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("unit not installed: %v", err)
	}
	checkHostUnit(t, gotUnit)
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

// runPiped runs deploy/install-host piped to bash from a directory outside the repo,
// without go on PATH, as on a host that does not have the repository cloned.
func (h *installHost) runPiped(env ...string) (string, error) {
	h.t.Helper()
	script, err := os.ReadFile("install-host")
	if err != nil {
		h.t.Fatal(err)
	}
	// shadow go with a failing stub so tests catch any accidental go build in standalone mode
	h.stub("go", `echo "go should not be called in standalone mode" >&2; exit 99`)
	cmd := exec.Command("bash")
	cmd.Dir = h.t.TempDir()
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Env = append([]string{"HOME=" + h.home, "PATH=" + h.stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"XDG_RUNTIME_DIR=" + filepath.Join(h.home, "run")}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func fakeBinaryServer(t *testing.T, wantPath string, body []byte, shaOverride string) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	if shaOverride != "" {
		sha = shaOverride
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			http.NotFound(w, r)
			return
		}
		if sha != "NONE" {
			w.Header().Set("X-Intelligence-Sha256", sha)
		}
		w.Header().Set("X-Intelligence-Version", "c0ffee")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInstallHostLinuxStandalonePipedFromServer(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	h := newInstallHost(t, "Linux")
	binBody := []byte("#!/bin/sh\necho c0ffee\n")
	srv := fakeBinaryServer(t, "/api/host/binary/linux/amd64", binBody, "")

	out, err := h.runPiped("HOST_SERVER="+srv.URL, "STUB_CODE=ABCD-1234", "STUB_ARCH=x86_64")
	if err != nil {
		t.Fatalf("standalone install-host failed: %v\n%s", err, out)
	}

	envFile := filepath.Join(h.home, ".config/intelligence/host.env")
	if b, err := os.ReadFile(envFile); err != nil || string(b) != "HOST_SERVER="+srv.URL+"\n" {
		t.Fatalf("host.env = %q, %v", b, err)
	}
	binPath := filepath.Join(h.home, ".local/bin/intelligence")
	if b, err := os.ReadFile(binPath); err != nil || string(b) != string(binBody) {
		t.Fatalf("installed binary = %q, %v", b, err)
	}
	if _, err := os.Stat(binPath + ".new"); !os.IsNotExist(err) {
		t.Errorf("temp file %s.new was left behind", binPath)
	}
	if _, err := os.Stat(binPath + ".headers"); !os.IsNotExist(err) {
		t.Errorf("temp file %s.headers was left behind", binPath)
	}
	gotUnit, err := os.ReadFile(filepath.Join(h.home, ".config/systemd/user/intelligence-host.service"))
	if err != nil {
		t.Fatalf("unit not installed: %v", err)
	}
	checkHostUnit(t, gotUnit)
	if !strings.Contains(out, "enter the code ABCD-1234") {
		t.Errorf("output lacks code:\n%s", out)
	}
}

func TestInstallHostMacOSStandalonePipedFromServer(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	h := newInstallHost(t, "Darwin")
	binBody := []byte("#!/bin/sh\necho c0ffee\n")
	srv := fakeBinaryServer(t, "/api/host/binary/darwin/arm64", binBody, "")

	out, err := h.runPiped("HOST_SERVER="+srv.URL, "STUB_CODE=EFGH-5678", "STUB_ARCH=arm64")
	if err != nil {
		t.Fatalf("standalone macOS install-host failed: %v\n%s", err, out)
	}
	binPath := filepath.Join(h.home, ".local/bin/intelligence")
	if b, err := os.ReadFile(binPath); err != nil || string(b) != string(binBody) {
		t.Fatalf("installed binary = %q, %v", b, err)
	}
	if !h.called("launchctl bootstrap gui/") || !h.called("launchctl kickstart -k gui/") {
		t.Errorf("launchctl calls: %q", h.callLines())
	}
	if !strings.Contains(out, "enter the code EFGH-5678") {
		t.Errorf("output lacks code:\n%s", out)
	}
}

func TestInstallHostStandaloneRerunReadsServerFromEnvFile(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	h := newInstallHost(t, "Linux")
	binBody := []byte("#!/bin/sh\necho c0ffee\n")
	srv := fakeBinaryServer(t, "/api/host/binary/linux/amd64", binBody, "")

	if out, err := h.runPiped("HOST_SERVER="+srv.URL, "STUB_CODE=FIRST-111"); err != nil {
		t.Fatalf("first run failed: %v\n%s", err, out)
	}
	// rerun without HOST_SERVER in environment: should read HOST_SERVER from host.env
	out, err := h.runPiped("STUB_CODE=SECOND-22")
	if err != nil {
		t.Fatalf("rerun failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "enter the code SECOND-22") {
		t.Errorf("rerun output lacks code:\n%s", out)
	}
}

func TestInstallHostStandaloneRejectsBadChecksum(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	h := newInstallHost(t, "Linux")
	srv := fakeBinaryServer(t, "/api/host/binary/linux/amd64", []byte("#!/bin/sh\necho c0ffee\n"), strings.Repeat("0", 64))

	out, err := h.runPiped("HOST_SERVER=" + srv.URL)
	if err == nil {
		t.Fatalf("expected checksum mismatch failure, got success:\n%s", out)
	}
	if !strings.Contains(out, "sha256") {
		t.Errorf("expected sha256 error message, got:\n%s", out)
	}
	binPath := filepath.Join(h.home, ".local/bin/intelligence")
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Errorf("binary should not be installed after checksum failure")
	}
	if _, err := os.Stat(binPath + ".new"); !os.IsNotExist(err) {
		t.Errorf("temp file %s.new should be cleaned up", binPath)
	}
}

// checkHostUnit checks the systemd unit install-host writes runs the installed client with host.env.
func checkHostUnit(t *testing.T, unit []byte) {
	t.Helper()
	for _, want := range []string{"EnvironmentFile=%h/.config/intelligence/host.env",
		"ExecStart=%h/.local/bin/intelligence host", "Restart=always", "WantedBy=default.target"} {
		if !strings.Contains(string(unit), want+"\n") {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
}
