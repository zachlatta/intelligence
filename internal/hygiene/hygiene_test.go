package hygiene

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The repository is public: none of its files may carry a private address, tailnet name,
// home-directory path, personal email or credential.
func TestRepositoryIsClean(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found at %s: %v", root, err)
	}
	findings, err := ScanTree(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

// Samples are built by concatenation so this file does not trip its own check.
func TestScanFlags(t *testing.T) {
	for _, tc := range []struct{ rule, text string }{
		{"private-ip", "listen on 10.1" + ".2.3:80"},
		{"private-ip", "host 172.20" + ".0.5"},
		{"private-ip", "host 192.168" + ".1.20"},
		{"private-ip", "tailnet peer 100.101" + ".102.103"},
		{"private-ip", "tailnet v6 fd7a:115c" + ":a1e0::1"},
		{"tailnet-name", "https://host.tail1234" + ".ts" + ".net/"},
		{"home-path", "cd /home" + "/zach/dev"},
		{"home-path", "open /Users" + "/zach/Desktop"},
		{"email", "mail zach" + "@hackclub.com"},
		{"private-key", "-----BEGIN OPENSSH " + "PRIVATE KEY-----"},
		{"token", "GH=ghp" + "_0123456789abcdefghijABCDEFGHIJ012345"},
		{"token", "key sk-ant" + "-api03-abcdefghijklmnopqrstuv"},
		{"token", "slack xoxb" + "-1234567890-abcdefghij"},
		{"token", "aws AKIA" + "ABCDEFGHIJKLMNOP"},
		{"token", "jwt eyJhbGciOiJIUzI1NiJ9" + ".eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij"},
	} {
		got := Scan("f", []byte(tc.text))
		if len(got) != 1 || got[0].Rule != tc.rule {
			t.Errorf("Scan(%q) = %v, want one %s finding", tc.text, got, tc.rule)
		}
	}
}

func TestScanAllows(t *testing.T) {
	for _, text := range []string{
		"listen 0.0.0.0:8080 and 127.0.0.1:5432",
		"documentation addresses 192.0.2.9, 198.51.100.7 and 203.0.113.1",
		"version 1.8.15 and 0.11.4",
		"HOME=/home/someone, CI runs in /home/runner, brew in /home/linuxbrew",
		"login someone@example.com",
		"go.sum h1:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG=",
		"postgres://app:p@db:5432/app",
		"200.100.64.1 is public",
	} {
		if got := Scan("f", []byte(text)); len(got) != 0 {
			t.Errorf("Scan(%q) = %v, want none", text, got)
		}
	}
}

func TestScanReportsLine(t *testing.T) {
	got := Scan("a/b.go", []byte("ok\nok\nssh /home"+"/zach\n"))
	if len(got) != 1 || got[0].Line != 3 || !strings.Contains(got[0].String(), "a/b.go:3") {
		t.Fatalf("got %v", got)
	}
}

func TestScanTreeSkipsIgnoredAndBinary(t *testing.T) {
	root := t.TempDir()
	bad := []byte("/home" + "/zach\n")
	for _, p := range []string{".git/config", ".factory-scratch/x.txt", "work/x.txt"} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), bad, 0o644)
	}
	os.WriteFile(filepath.Join(root, "bin.dat"), append([]byte{0}, bad...), 0o644)
	os.WriteFile(filepath.Join(root, "leak.md"), bad, 0o644)
	got, err := ScanTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "leak.md" {
		t.Fatalf("got %v, want only leak.md", got)
	}
}
