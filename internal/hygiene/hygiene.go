// Package hygiene checks that the repository's files carry nothing private: the repository is
// public, so a private address, tailnet name, home-directory path, personal email or credential
// in any file is published. Its test runs in `make test`, CI and the image build.
package hygiene

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A Finding is one match of a rule in a file.
type Finding struct {
	Path  string
	Line  int
	Rule  string
	Match string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %q", f.Path, f.Line, f.Rule, f.Match)
}

var (
	ipv4 = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
	// private ranges, the tailnet's (CGNAT) included; loopback, 0.0.0.0 and the documentation
	// ranges (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24) are what tests should use
	private = []netip.Prefix{
		netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, 0, 0}), 8),
		netip.PrefixFrom(netip.AddrFrom4([4]byte{172, 16, 0, 0}), 12),
		netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, 0, 0}), 16),
		netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, 0, 0}), 10),
	}
	homeAllowed = map[string]bool{"someone": true, "runner": true, "linuxbrew": true}
	emailOK     = regexp.MustCompile(`(?i)@(example\.(com|org|net)|users\.noreply\.github\.com)$`)

	rules = []struct {
		name string
		re   *regexp.Regexp
		ok   func(match []string) bool // nil: every match is a finding
	}{
		{"private-ip", regexp.MustCompile(`(?i)\bfd7a:115c:a1e0:[0-9a-f:]*`), nil},
		{"tailnet-name", regexp.MustCompile(`(?i)[a-z0-9-]+\.ts\.net\b`), nil},
		{"home-path", regexp.MustCompile(`/(?:home|Users)/([A-Za-z0-9._-]+)`),
			func(m []string) bool { return homeAllowed[m[1]] }},
		{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`),
			func(m []string) bool { return emailOK.MatchString(m[0]) }},
		{"private-key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), nil},
		{"token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-ant-[A-Za-z0-9_-]{20,}|xox[abpr]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,})`), nil},
	}
)

// Scan returns the findings in one file's contents.
func Scan(path string, data []byte) []Finding {
	var out []Finding
	for i, line := range strings.Split(string(data), "\n") {
		add := func(rule, match string) { out = append(out, Finding{path, i + 1, rule, match}) }
		for _, loc := range ipv4.FindAllStringIndex(line, -1) {
			// skip a run of four numbers inside a longer dotted run (a version, an OID)
			if loc[0] > 0 && (line[loc[0]-1] == '.' || isDigit(line[loc[0]-1])) ||
				loc[1] < len(line) && (isDigit(line[loc[1]]) || line[loc[1]] == '.' && loc[1]+1 < len(line) && isDigit(line[loc[1]+1])) {
				continue
			}
			addr, err := netip.ParseAddr(line[loc[0]:loc[1]])
			if err != nil {
				continue
			}
			for _, p := range private {
				if p.Contains(addr) {
					add("private-ip", line[loc[0]:loc[1]])
					break
				}
			}
		}
		for _, r := range rules {
			for _, m := range r.re.FindAllStringSubmatch(line, -1) {
				if r.ok == nil || !r.ok(m) {
					add(r.name, m[0])
				}
			}
		}
	}
	return out
}

func isDigit(b byte) bool { return '0' <= b && b <= '9' }

// outputDirs are top-level folders agents keep their output in; dataExt are data files. Neither
// belongs in a public repository: reports, metrics and exports would be published.
var (
	outputDirs = map[string]bool{"work": true, "scratch": true, ".factory-scratch": true, ".factory-evidence": true}
	dataExt    = map[string]bool{".csv": true, ".tsv": true, ".xlsx": true, ".xls": true, ".parquet": true,
		".duckdb": true, ".sqlite": true, ".sqlite3": true, ".jsonl": true, ".ndjson": true}
)

// TrackedOutput lists the files git tracks under root that are agent output (work/, scratch/,
// .factory-scratch/, .factory-evidence/) or data files (CSV, spreadsheets, Parquet, DuckDB,
// SQLite, JSON Lines), sorted. Ignored, untracked files there are fine: they are never pushed.
func TrackedOutput(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	return outputPaths(strings.Split(string(out), "\x00")), nil
}

// OutgoingOutput lists the agent output and data files that any commit in base..HEAD adds, sorted
// and once each. A file added and deleted again before a push is still in the pushed history, so
// the final tree alone (TrackedOutput) does not show it.
func OutgoingOutput(root, base string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "log", "--no-renames", "--diff-filter=A", "--name-only",
		"--format=", "-z", base+"..HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s..HEAD: %w", base, err)
	}
	return outputPaths(strings.FieldsFunc(string(out), func(r rune) bool { return r == 0 || r == '\n' })), nil
}

// outputPaths keeps the paths that are agent output or data files, sorted and once each.
func outputPaths(paths []string) []string {
	seen := map[string]bool{}
	var found []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		first, _, _ := strings.Cut(p, "/")
		if (outputDirs[first] && first != p) || dataExt[strings.ToLower(path.Ext(p))] {
			found = append(found, p)
		}
	}
	sort.Strings(found)
	return found
}

// skipped holds directories that are not part of the repository's contents: git's own, the
// git-ignored scratch and work directories (TrackedOutput checks nothing in them is tracked).
var skipped = map[string]bool{".git": true, ".factory-scratch": true, "work": true}

// ScanTree scans every text file under root, except skipped directories and the built binary.
func ScanTree(root string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if p != root && skipped[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil // a worktree's .git file
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() || rel == "intelligence" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 { // binary
			return nil
		}
		out = append(out, Scan(rel, data)...)
		return nil
	})
	return out, err
}
