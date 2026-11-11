package hygiene

import (
	"os"
	"os/exec"
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

// Agent output is not the repository's to publish: a tracked work/ or scratch folder, or a data
// file, would put reports, metrics or exports on a public GitHub. The image build
// has no .git (.dockerignore), so this check runs in make test and CI.
func TestRepositoryTracksNoAgentOutput(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("no .git here (the image build): CI and make test run this check")
	}
	tracked, err := TrackedOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range tracked {
		t.Errorf("%s: agent output or data file tracked in a public repository; keep it in .factory-scratch/ and deliver it privately", p)
	}
}

func TestTrackedOutputFlagsWorkScratchAndDataFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	files := []string{
		"work/2026-01-01-report/README.md", "scratch/q.sql", ".factory-scratch/x.md", ".factory-evidence/a.webp",
		"internal/report/metrics.csv", "exports/people.xlsx", "data.parquet",
		"README.md", "internal/x/x.go", "docs/workflows.md", "internal/web/app.js",
	}
	for _, p := range files {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte("x\n"), 0o644)
	}
	os.WriteFile(filepath.Join(root, "ignored.csv"), []byte("x\n"), 0o644) // untracked: not published
	git(append([]string{"add", "-f", "--"}, files...)...)
	got, err := TrackedOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".factory-evidence/a.webp", ".factory-scratch/x.md", "data.parquet", "exports/people.xlsx",
		"internal/report/metrics.csv", "scratch/q.sql", "work/2026-01-01-report/README.md"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("TrackedOutput = %q, want %q", got, want)
	}
}

// A file committed and deleted again before a push is still published with that push's history.
func TestOutgoingOutputFindsFilesAddedThenDeleted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=someone", "-c", "user.email=someone@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(p string) {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte("x\n"), 0o644)
	}
	git("init", "-q", "-b", "main")
	write("README.md")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	git("branch", "base")
	write("work/2026-01-01-report/README.md")
	write("internal/x/counts.csv")
	write("internal/x/x.go")
	git("add", "-f", ".")
	git("commit", "-q", "-m", "add a report")
	git("rm", "-q", "-r", "work", "internal/x/counts.csv")
	git("commit", "-q", "-m", "remove it again")

	if got, err := TrackedOutput(root); err != nil || len(got) != 0 {
		t.Fatalf("TrackedOutput = %q, %v; want none (the final tree is clean)", got, err)
	}
	got, err := OutgoingOutput(root, "base")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"internal/x/counts.csv", "work/2026-01-01-report/README.md"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("OutgoingOutput = %q, want %q", got, want)
	}
}

// This branch's commits not yet on origin/main add no agent output, even ones deleted later.
func TestOutgoingCommitsAddNoAgentOutput(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "-q", "origin/main").Run(); err != nil {
		t.Skip("no origin/main here (the image build, a shallow CI checkout)")
	}
	got, err := OutgoingOutput(root, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		t.Errorf("%s: added by a commit not yet on origin/main; rewrite that commit before pushing", p)
	}
}

// git log leaves merge commits' diffs out unless asked, so a report added while resolving a merge
// and deleted afterwards would escape a check that reads only ordinary commits.
func TestOutgoingOutputFindsFilesAddedInAMergeThenDeleted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=someone", "-c", "user.email=someone@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(p string) {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte("x\n"), 0o644)
	}
	git("init", "-q", "-b", "main")
	write("README.md")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	git("branch", "base")
	git("checkout", "-q", "-b", "side")
	write("side.go")
	git("add", ".")
	git("commit", "-q", "-m", "side")
	git("checkout", "-q", "main")
	write("main.go")
	git("add", ".")
	git("commit", "-q", "-m", "main")
	git("merge", "-q", "--no-ff", "--no-commit", "side")
	write("work/internal-audit.md")
	git("add", "-f", "work/internal-audit.md")
	git("commit", "-q", "-m", "merge side")
	git("rm", "-q", "-r", "work")
	git("commit", "-q", "-m", "remove the report")

	got, err := OutgoingOutput(root, "base")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != "work/internal-audit.md" {
		t.Errorf("OutgoingOutput = %q, want the report added in the merge", got)
	}
}
