package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/webpush"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestVersionPrefersTheStampedCommit(t *testing.T) {
	old := buildCommit
	t.Cleanup(func() { buildCommit = old })
	buildCommit = "stamped"
	if got := version(env(map[string]string{"SOURCE_COMMIT": "deadbeef"})); got != "stamped" {
		t.Fatalf("version = %q, want the commit stamped at build", got)
	}
}

func TestVersionPrefersSourceCommit(t *testing.T) {
	if got := version(env(map[string]string{"SOURCE_COMMIT": "deadbeef"})); got != "deadbeef" {
		t.Fatalf("version = %q, want deadbeef", got)
	}
}

func TestVersionFallsBackWithoutSourceCommit(t *testing.T) {
	if got := version(env(nil)); got == "" {
		t.Fatal("version is empty without SOURCE_COMMIT")
	}
}

func TestAddrDefaultsToPort8080(t *testing.T) {
	if got := addr(env(nil)); got != ":8080" {
		t.Fatalf("addr = %q, want :8080", got)
	}
	if got := addr(env(map[string]string{"PORT": "9000"})); got != ":9000" {
		t.Fatalf("addr = %q, want :9000", got)
	}
}

func TestHostConfigNeedsOnlyTheServer(t *testing.T) {
	hostname := func() (string, error) { return "host-1", nil }
	if _, err := hostConfig(env(map[string]string{}), hostname); err == nil || !strings.Contains(err.Error(), "HOST_SERVER") {
		t.Errorf("err = %v, want HOST_SERVER missing", err)
	}
	// a host installed before accounts still has DEVBOX_TOKEN in its env file: it starts, and joins
	if _, err := hostConfig(env(map[string]string{"HOST_SERVER": "https://x", "DEVBOX_TOKEN": "old"}), hostname); err != nil {
		t.Errorf("an old env file: %v", err)
	}
}

func TestHostConfigDefaults(t *testing.T) {
	c, err := hostConfig(env(map[string]string{"HOST_SERVER": "https://x", "HOME": "/home/someone"}), func() (string, error) { return "Host-1", nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "host-1" || c.Interval != 15*time.Second || c.Server != "https://x" || c.KeyFile != "/home/someone/.config/intelligence/host.key" {
		t.Errorf("config = %+v", c)
	}
	c, _ = hostConfig(env(map[string]string{"HOST_SERVER": "https://x", "HOST_KEY_FILE": "/etc/host.key"}), func() (string, error) { return "h", nil })
	if c.KeyFile != "/etc/host.key" {
		t.Errorf("HOST_KEY_FILE: %q", c.KeyFile)
	}
}

func TestHostConfigOverrides(t *testing.T) {
	c, err := hostConfig(env(map[string]string{"HOST_SERVER": "https://x", "DEVBOX_TOKEN": "t",
		"HOST_NAME": "host-2", "HOST_INTERVAL": "1m"}), func() (string, error) { return "h", nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "host-2" || c.Interval != time.Minute {
		t.Errorf("config = %+v", c)
	}
	if _, err := hostConfig(env(map[string]string{"HOST_SERVER": "https://x", "DEVBOX_TOKEN": "t", "HOST_NAME": "bad name"}),
		func() (string, error) { return "h", nil }); err == nil {
		t.Errorf("an invalid HOST_NAME should be refused")
	}
}

func TestServerConfigNeedsBothDatabaseURLs(t *testing.T) {
	if _, err := serverConfig(env(map[string]string{"READONLY_DATABASE_URL": "postgres://ro:p@db/app"})); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("err = %v, want DATABASE_URL missing", err)
	}
	if _, err := serverConfig(env(map[string]string{"DATABASE_URL": "postgres://app:p@db/app"})); err == nil || !strings.Contains(err.Error(), "READONLY_DATABASE_URL") {
		t.Errorf("err = %v, want READONLY_DATABASE_URL missing", err)
	}
	if _, err := serverConfig(env(map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://db/app"})); err == nil {
		t.Errorf("a read-only URL without a user and password should be refused")
	}
}

func TestServerConfigReadsTheReadonlyRoleFromItsURL(t *testing.T) {
	c, err := serverConfig(env(map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://intelligence_ro:s3cr%40t@db/app?sslmode=disable"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ReadonlyRole != "intelligence_ro" || c.ReadonlyPassword != "s3cr@t" || c.BinDir != "/usr/local/share/intelligence/host" {
		t.Errorf("config = %+v", c)
	}
	if c.VAPID != nil {
		t.Errorf("VAPID should be nil when unset")
	}
}

func TestServerConfigParsesVAPID(t *testing.T) {
	pub, priv, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		"DATABASE_URL":          "postgres://app:p@db/app",
		"READONLY_DATABASE_URL": "postgres://ro:p@db/app",
		"VAPID_PUBLIC_KEY":      pub,
		"VAPID_PRIVATE_KEY":     priv,
		"VAPID_SUBJECT":         "mailto:alerts@example.com",
	}
	c, err := serverConfig(env(base))
	if err != nil {
		t.Fatalf("serverConfig with VAPID: %v", err)
	}
	if c.VAPID == nil || c.VAPID.PublicKey != pub {
		t.Fatalf("VAPID = %+v, want pub %q", c.VAPID, pub)
	}
	delete(base, "VAPID_SUBJECT")
	if _, err := serverConfig(env(base)); err == nil || !strings.Contains(err.Error(), "VAPID") {
		t.Errorf("partial VAPID should fail, got %v", err)
	}
}

func TestRunVAPIDKeysPrintsValidEnvVars(t *testing.T) {
	var buf bytes.Buffer
	if err := runVAPIDKeys([]string{"-subject", "mailto:ops@example.com"}, &buf); err != nil {
		t.Fatalf("runVAPIDKeys: %v", err)
	}
	vals := map[string]string{
		"DATABASE_URL":          "postgres://app:p@db/app",
		"READONLY_DATABASE_URL": "postgres://ro:p@db/app",
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("bad env line %q", line)
		}
		vals[k] = v
	}
	if vals["VAPID_SUBJECT"] != "mailto:ops@example.com" || vals["VAPID_PUBLIC_KEY"] == "" || vals["VAPID_PRIVATE_KEY"] == "" {
		t.Fatalf("unexpected output:\n%s", buf.String())
	}
	c, err := serverConfig(env(vals))
	if err != nil {
		t.Fatalf("serverConfig refused generated keys: %v", err)
	}
	if c.VAPID == nil || c.VAPID.PublicKey != vals["VAPID_PUBLIC_KEY"] {
		t.Fatalf("parsed VAPID mismatch: %+v", c.VAPID)
	}

	buf.Reset()
	if err := runVAPIDKeys([]string{"-subject", "http://bad.example.com"}, &buf); err == nil {
		t.Errorf("invalid subject should fail")
	}
}

func TestHostAutoUpdateCanBeTurnedOff(t *testing.T) {
	base := map[string]string{"HOST_SERVER": "https://x", "DEVBOX_TOKEN": "t"}
	c, _ := hostConfig(env(base), func() (string, error) { return "h", nil })
	if !c.AutoUpdate {
		t.Errorf("auto-update should default on")
	}
	base["HOST_AUTOUPDATE"] = "off"
	c, _ = hostConfig(env(base), func() (string, error) { return "h", nil })
	if c.AutoUpdate {
		t.Errorf("HOST_AUTOUPDATE=off should turn it off")
	}
}

func TestHostPaseoHome(t *testing.T) {
	base := map[string]string{"HOST_SERVER": "https://x", "DEVBOX_TOKEN": "t", "HOME": "/home/someone"}
	c, err := hostConfig(env(base), func() (string, error) { return "h", nil })
	if err != nil || c.PaseoHome != "/home/someone/.paseo" {
		t.Errorf("PaseoHome = %q (err %v), want ~/.paseo", c.PaseoHome, err)
	}
	base["HOST_PASEO_HOME"] = "/srv/paseo"
	if c, _ := hostConfig(env(base), func() (string, error) { return "h", nil }); c.PaseoHome != "/srv/paseo" {
		t.Errorf("PaseoHome = %q, want the override", c.PaseoHome)
	}
}

func TestProbeConfig(t *testing.T) {
	c := probeConfig(env(map[string]string{"PROBE_SERVER": "https://x", "PROBE_USER": "dev"}))
	if c.Server != "https://x" || c.User != "dev" || c.Socket != "/run/intelligence/probe.sock" || !c.AutoUpdate {
		t.Errorf("config = %+v", c)
	}
	if c := probeConfig(env(map[string]string{"PROBE_AUTOUPDATE": "off", "PROBE_SOCKET": "/s"})); c.AutoUpdate || c.Socket != "/s" {
		t.Errorf("config = %+v", c)
	}
}

func TestHostProbeSocketDefault(t *testing.T) {
	c, err := hostConfig(env(map[string]string{"HOST_SERVER": "https://x", "DEVBOX_TOKEN": "t"}), func() (string, error) { return "b", nil })
	if err != nil || c.ProbeSocket != "/run/intelligence/probe.sock" {
		t.Errorf("probe socket = %q, %v", c.ProbeSocket, err)
	}
}

// A host installed before dev boxes were called hosts runs the new binary (self-update) from its
// old unit: `intelligence devbox`, with DEVBOX_* variables and its key in devbox.key. It keeps working.
func TestHostConfigReadsTheDevboxNamesFromBeforeTheRename(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "intelligence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "devbox.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := map[string]string{"HOME": home, "DEVBOX_SERVER": "https://x", "DEVBOX_NAME": "old-1", "DEVBOX_INTERVAL": "1m",
		"DEVBOX_AUTOUPDATE": "off", "DEVBOX_PASEO_HOME": "/p", "DEVBOX_PROBE_SOCKET": "off"}
	hostname := func() (string, error) { return "h", nil }
	c, err := hostConfig(env(old), hostname)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server != "https://x" || c.Name != "old-1" || c.Interval != time.Minute || c.AutoUpdate || c.PaseoHome != "/p" ||
		c.ProbeSocket != "" || c.KeyFile != filepath.Join(dir, "devbox.key") {
		t.Errorf("config = %+v", c)
	}
	old["HOST_NAME"] = "new-1"
	if c, _ := hostConfig(env(old), hostname); c.Name != "new-1" {
		t.Errorf("HOST_NAME should win over DEVBOX_NAME: %q", c.Name)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := hostConfig(env(old), hostname); c.KeyFile != filepath.Join(dir, "host.key") {
		t.Errorf("host.key should win over devbox.key: %q", c.KeyFile)
	}
}

func TestDevboxIsTheHostCommand(t *testing.T) {
	if got := command([]string{"intelligence", "devbox"}); got != "host" {
		t.Errorf("command(devbox) = %q, want host", got)
	}
	if got := command([]string{"intelligence"}); got != "server" {
		t.Errorf("command() = %q, want server", got)
	}
}

func TestServerConfigReadsDevboxBinariesFromBeforeTheRename(t *testing.T) {
	c, err := serverConfig(env(map[string]string{"DATABASE_URL": "postgres://app:p@db/app",
		"READONLY_DATABASE_URL": "postgres://ro:p@db/app", "DEVBOX_BINARIES": "/old"}))
	if err != nil || c.BinDir != "/old" {
		t.Errorf("BinDir = %q, %v; want /old", c.BinDir, err)
	}
}

func TestServerConfigReadsTheWorkflowQueueLimits(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://ro:p@db/app"}
	c, err := serverConfig(env(base))
	if err != nil || c.QueueLimits != workflow.DefaultLimits() {
		t.Fatalf("default limits = %+v, %v", c.QueueLimits, err)
	}
	set := map[string]string{"WORKFLOW_QUEUE_CPU_PCT": "70", "WORKFLOW_QUEUE_MEM_PCT": "off", "WORKFLOW_QUEUE_LOAD_PER_CPU": "2.5",
		"WORKFLOW_QUEUE_MEM_PEAK_PCT": "90", "WORKFLOW_QUEUE_MEM_PEAKS": "2"}
	for k, v := range base {
		set[k] = v
	}
	c, err = serverConfig(env(set))
	if err != nil {
		t.Fatal(err)
	}
	if l := c.QueueLimits; l.CPUPct != 70 || l.MemPct != 0 || l.LoadPerCPU != 2.5 || l.MemPeakPct != 90 || l.MemPeaks != 2 {
		t.Errorf("limits = %+v", l)
	}
	if l := workflow.DefaultLimits(); l.MemPct != 80 || l.MemPeakPct != 95 || l.MemPeaks != 1 {
		t.Errorf("default memory limits = %+v, want 80%% now, 95%% should a new run's peak meet the largest to come", l)
	}
	for k, v := range map[string]string{"WORKFLOW_QUEUE_CPU_PCT": "lots", "WORKFLOW_QUEUE_MEM_PEAKS": "1.5", "WORKFLOW_QUEUE_MEM_PEAK_PCT": "-1"} {
		bad := map[string]string{k: v}
		for k, v := range base {
			bad[k] = v
		}
		if _, err := serverConfig(env(bad)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("a bad %s should be refused: %v", k, err)
		}
	}
}

func TestServerConfigReadsTheAssemblyAIKey(t *testing.T) {
	c, err := serverConfig(env(map[string]string{"DATABASE_URL": "postgres://app:p@db/app",
		"READONLY_DATABASE_URL": "postgres://ro:p@db/app", "ASSEMBLYAI_API_KEY": "k"}))
	if err != nil || c.AssemblyAIKey != "k" {
		t.Errorf("AssemblyAIKey = %q, %v; want k", c.AssemblyAIKey, err)
	}
}

func TestServerConfigTurnsEmailTriggersOnWithBothGoogleSettings(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://ro:p@db/app"}
	if c, err := serverConfig(env(base)); err != nil || c.Google != nil || c.EmailKey != nil {
		t.Errorf("without settings: %+v, %v; want email off", c, err)
	}
	base["GOOGLE_OAUTH_CLIENT_JSON"] = `{"installed":{"client_id":"id","client_secret":"s"}}`
	if _, err := serverConfig(env(base)); err == nil || !strings.Contains(err.Error(), "CONNECTIONS_KEY") {
		t.Errorf("a client without a key: %v", err)
	}
	base["CONNECTIONS_KEY"] = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	base["PUBLIC_URL"] = "https://intelligence.example.com"
	c, err := serverConfig(env(base))
	if err != nil || c.Google == nil || !c.Google.Installed || c.EmailKey == nil || c.PublicURL != "https://intelligence.example.com" {
		t.Errorf("with both: %+v, %v", c, err)
	}
	base["CONNECTIONS_KEY"] = "short"
	if _, err := serverConfig(env(base)); err == nil {
		t.Error("a bad key: no error")
	}
}

func TestVoiceMemoConfigRequiresAnOriginKeyAndAccount(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://ro:p@db/app"}
	base["PDW_URL"] = "https://pdw.example.com"
	if _, err := serverConfig(env(base)); err == nil {
		t.Fatal("unbound PDW connection accepted")
	}
	base["PDW_API_KEY"] = "test-key"
	base["PDW_INTELLIGENCE_ACCOUNT"] = "owner"
	if _, err := serverConfig(env(base)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://pdw.example.com", "https://pdw.example.com/api", "https://pdw.example.com?token=secret"} {
		base["PDW_URL"] = bad
		if _, err := serverConfig(env(base)); err == nil {
			t.Errorf("invalid origin accepted: %s", bad)
		}
	}
}

func TestHostAlertsNeedPDWAndANotifyKey(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://ro:p@db/app"}
	c, err := serverConfig(env(base))
	if err != nil || c.PDWNotifyKey != "" {
		t.Fatalf("off by default: %+v %v", c.PDWNotifyKey, err)
	}
	if c.NotifyLocation == nil || c.NotifyLocation.String() != "America/New_York" {
		t.Fatalf("default zone: %v", c.NotifyLocation)
	}
	base["PDW_NOTIFY_KEY"] = "notify-key"
	if _, err := serverConfig(env(base)); err == nil {
		t.Fatal("a notify key without PDW_URL and PDW_INTELLIGENCE_ACCOUNT accepted")
	}
	base["PDW_URL"], base["PDW_API_KEY"], base["PDW_INTELLIGENCE_ACCOUNT"] = "https://pdw.example.com", "test-key", "owner"
	base["NOTIFY_TIME_ZONE"] = "Europe/Berlin"
	c, err = serverConfig(env(base))
	if err != nil || c.PDWNotifyKey != "notify-key" || c.NotifyLocation.String() != "Europe/Berlin" {
		t.Fatalf("%q %v %v", c.PDWNotifyKey, c.NotifyLocation, err)
	}
	base["NOTIFY_TIME_ZONE"] = "Mars/Olympus"
	if _, err := serverConfig(env(base)); err == nil {
		t.Fatal("a bad zone accepted")
	}
}

func TestVoiceMemoPollInterval(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://ro:p@db/app"}
	for _, tt := range []struct {
		value string
		want  time.Duration
		bad   bool
	}{
		{"", time.Second, false}, {"5s", 5 * time.Second, false}, {"500ms", 0, true}, {"0", 0, true}, {"bad", 0, true},
	} {
		base["VOICE_MEMO_POLL_INTERVAL"] = tt.value
		c, err := serverConfig(env(base))
		if (err != nil) != tt.bad || (!tt.bad && c.VoiceMemoInterval != tt.want) {
			t.Fatalf("%q: interval=%v, error=%v", tt.value, c.VoiceMemoInterval, err)
		}
	}
}

func TestEmailPollInterval(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://ro:p@db/app"}
	for _, tt := range []struct {
		value string
		want  time.Duration
		bad   bool
	}{
		{"", time.Second, false}, {"2s", 2 * time.Second, false}, {"100ms", 0, true}, {"0", 0, true}, {"bad", 0, true},
	} {
		base["EMAIL_POLL_INTERVAL"] = tt.value
		c, err := serverConfig(env(base))
		if (err != nil) != tt.bad || (!tt.bad && c.EmailInterval != tt.want) {
			t.Fatalf("%q: interval=%v, error=%v", tt.value, c.EmailInterval, err)
		}
	}
}

func TestVaultwardenConfig(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://app:p@db/app", "READONLY_DATABASE_URL": "postgres://ro:p@db/app"}
	c, err := serverConfig(env(base))
	if err != nil || c.Vaultwarden != nil || secretStore(c) != nil {
		t.Fatalf("off by default: %+v %v", c.Vaultwarden, err)
	}
	// Partial config refused.
	base["VAULTWARDEN_URL"] = "https://vault.example.com"
	if _, err := serverConfig(env(base)); err == nil {
		t.Fatal("partial Vaultwarden config accepted")
	}
	// Non-HTTPS URL refused.
	base["VAULTWARDEN_URL"] = "http://vault.example.com"
	base["VAULTWARDEN_CLIENT_ID"] = "user.demo"
	base["VAULTWARDEN_CLIENT_SECRET"] = "secret"
	base["VAULTWARDEN_MASTER_PASSWORD"] = "master"
	base["VAULTWARDEN_ACCOUNT"] = "owner"
	if _, err := serverConfig(env(base)); err == nil {
		t.Fatal("non-HTTPS Vaultwarden URL accepted")
	}
	// Missing VAULTWARDEN_ACCOUNT refused.
	base["VAULTWARDEN_URL"] = "https://vault.example.com"
	delete(base, "VAULTWARDEN_ACCOUNT")
	if _, err := serverConfig(env(base)); err == nil {
		t.Fatal("Vaultwarden config without VAULTWARDEN_ACCOUNT accepted")
	}
	// Complete config accepted.
	base["VAULTWARDEN_ACCOUNT"] = "owner"
	base["VAULTWARDEN_ORGANIZATION"] = "Custom Org"
	base["VAULTWARDEN_COLLECTION"] = "Custom Collection"
	c, err = serverConfig(env(base))
	if err != nil || c.Vaultwarden == nil || secretStore(c) == nil || c.VaultwardenAccount != "owner" {
		t.Fatalf("complete config failed: %+v %q %v", c.Vaultwarden, c.VaultwardenAccount, err)
	}
	if c.Vaultwarden.Organization != "Custom Org" || c.Vaultwarden.Collection != "Custom Collection" {
		t.Fatalf("unexpected org/collection: %+v", c.Vaultwarden)
	}
}
