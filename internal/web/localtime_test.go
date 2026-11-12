package web

import (
	"encoding/json"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A time the server shows is a <time> the browser rewrites in its own zone and locale (localtime.js),
// with the instant in UTC as its fallback text for a browser that runs no script.
func TestLocalTimeMarkup(t *testing.T) {
	at := time.Date(2026, 11, 11, 23, 57, 5, 0, time.UTC)
	for _, tc := range []struct{ format, want string }{
		{"time", `<time datetime="2026-11-11T23:57:05Z" data-format="time" title="2026-11-11T23:57:05Z">23:57 UTC</time>`},
		{"day", `<time datetime="2026-11-11T23:57:05Z" data-format="day" title="2026-11-11T23:57:05Z">Nov 11</time>`},
		{"short", `<time datetime="2026-11-11T23:57:05Z" data-format="short" title="2026-11-11T23:57:05Z">Nov 11 23:57 UTC</time>`},
		{"seconds", `<time datetime="2026-11-11T23:57:05Z" data-format="seconds" title="2026-11-11T23:57:05Z">Nov 11 23:57:05 UTC</time>`},
		{"weekday", `<time datetime="2026-11-11T23:57:05Z" data-format="weekday" title="2026-11-11T23:57:05Z">Wed Nov 11 23:57 UTC</time>`},
		{"minute", `<time datetime="2026-11-11T23:57:05Z" data-format="minute" title="2026-11-11T23:57:05Z">Nov 11, 2026 23:57 UTC</time>`},
		{"full", `<time datetime="2026-11-11T23:57:05Z" data-format="full" title="2026-11-11T23:57:05Z">Nov 11, 2026 23:57:05 UTC</time>`},
	} {
		if got := localTime(at, tc.format); got != template.HTML(tc.want) {
			t.Errorf("%s:\n got %s\nwant %s", tc.format, got, tc.want)
		}
	}
	// a time in another zone is the same instant
	if got := localTime(at.In(time.FixedZone("JST", 9*3600)), "time"); !strings.Contains(string(got), `datetime="2026-11-11T23:57:05Z"`) {
		t.Errorf("zoned time: %s", got)
	}
	if got := localTime(time.Time{}, "short"); got != "" {
		t.Errorf("zero time = %q, want nothing", got)
	}
	// a relative time keeps its words; the browser adds the local instant as its title
	now := at.Add(5 * time.Minute)
	if got, want := localAgo(now, at), template.HTML(`<time datetime="2026-11-11T23:57:05Z" title="2026-11-11T23:57:05Z">5m ago</time>`); got != want {
		t.Errorf("localAgo = %s, want %s", got, want)
	}
	if got := localAgo(now, time.Time{}); got != "never" {
		t.Errorf("localAgo(zero) = %s", got)
	}
}

// Text the server stored with an instant in it (a queued run's reason) shows the instant locally.
func TestLocalTextWrapsInstants(t *testing.T) {
	got := localText(`host-1 <a> has no room yet (last measured 2026-11-11T23:57:00Z)`)
	want := template.HTML(`host-1 &lt;a&gt; has no room yet (last measured <time datetime="2026-11-11T23:57:00Z" data-format="short" title="2026-11-11T23:57:00Z">Nov 11 23:57 UTC</time>)`)
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

// A run event's Markdown (a queued run's reason) shows its instants locally too, but never inside a
// tag's attributes.
func TestLocalMarkdownWrapsInstantsInText(t *testing.T) {
	got := string(localMarkdown("Queued: waiting (last measured 2026-11-11T23:57:00Z), see [log](https://example.com/?at=2026-11-11T23:57:00Z)"))
	if !strings.Contains(got, `(last measured <time datetime="2026-11-11T23:57:00Z" data-format="short"`) {
		t.Errorf("instant in text not local: %s", got)
	}
	if !strings.Contains(got, `href="https://example.com/?at=2026-11-11T23:57:00Z"`) {
		t.Errorf("instant in an attribute was changed: %s", got)
	}
}

// Chart axes label their ends with times the browser localizes too (an SVG <text>, which takes no <time>).
func TestChartLabelsAreLocal(t *testing.T) {
	from := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	svg := string(usageChart(nil, from, from.Add(7*24*time.Hour)))
	if !strings.Contains(svg, `data-datetime="2026-11-04T03:00:00Z" data-format="day"`) {
		t.Errorf("usage chart start label is not local: %s", svg)
	}
}

// No page formats a clock time on the server: every one goes through localTime, so it shows in the
// reader's zone. A new template or handler that formats one fails here.
func TestNoServerFormattedClockTimes(t *testing.T) {
	clock := regexp.MustCompile(`15:04|"Mon Jan|"Jan 2|\bMST\b|toISOString\(\)\.replace\("T"|toISOString\(\)\.slice\(0, 16\)`)
	files, err := filepath.Glob("*")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "localtime.go" ||
			!(strings.HasSuffix(f, ".go") || strings.HasSuffix(f, ".html") || strings.HasSuffix(f, ".js")) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if clock.MatchString(line) {
				t.Errorf("%s:%d formats a time on the server or in UTC; use localTime (localtime.go): %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// Every page loads localtime.js, and the server serves it.
func TestPagesLoadLocalTimeScript(t *testing.T) {
	h, _ := dbServer(t)
	for _, path := range []string{"/", "/workflows", "/schedules", "/usage", "/resources", "/whats-new", "/account", "/workflow-proposals"} {
		page := get(t, h, path, nil)
		if page.Code != 200 || !strings.Contains(page.Body.String(), `<script src="/localtime.js" defer></script>`) {
			t.Errorf("%s: %d, no localtime.js", path, page.Code)
		}
	}
	if r := call(t, h, "GET", "/localtime.js", "", nil); r.Code != 200 || !strings.Contains(r.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("localtime.js: %d %s", r.Code, r.Header())
	}
}

// localTimeHarness runs localtime.js (argv[1]) against a small fake DOM holding the elements in
// argv[2] (JSON: tag, attributes), then prints each one's text and title.
const localTimeHarness = `
const fs = require("fs");
const [file, specJSON] = process.argv.slice(1);
const els = JSON.parse(specJSON).map(([tag, attrs, text]) => ({
  tagName: tag.toUpperCase(), attrs: { ...attrs }, textContent: text, title: "",
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
  setAttribute(k, v) { this.attrs[k] = String(v); },
  matches() { return false; },
}));
const root = { querySelectorAll: () => els, matches: () => false };
global.document = { readyState: "complete", documentElement: root, querySelectorAll: () => els, addEventListener() {} };
global.window = global;
global.MutationObserver = class { observe() {} };
eval(fs.readFileSync(file, "utf8"));
console.log(JSON.stringify(els.map((e) => [e.textContent, e.title])));
`

// localtime.js shows each time in the browser's own zone: the same instant reads differently in
// New York and Tokyo, a relative time keeps its words, and a date alone is left as written.
func TestLocalTimeScriptUsesBrowserZone(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	spec := `[["time",{"datetime":"2026-11-11T18:57:05Z","data-format":"time"},"18:57 UTC"],
		["time",{"datetime":"2026-11-11T18:57:05Z","data-format":"short"},"Nov 11 18:57 UTC"],
		["time",{"datetime":"2026-11-11T18:57:05Z"},"5m ago"],
		["text",{"data-datetime":"2026-11-11T18:57:05Z","data-format":"day"},"Nov 11"],
		["time",{"datetime":"2026-11-10"},"November 10, 2026"]]`
	run := func(zone string) [][2]string {
		cmd := exec.Command(node, "-e", localTimeHarness, "localtime.js", spec)
		cmd.Env = append(os.Environ(), "TZ="+zone, "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", zone, err, out)
		}
		var got [][2]string
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: %v %s", zone, err, out)
		}
		return got
	}
	ny, tokyo := run("America/New_York"), run("Asia/Tokyo")
	for i, want := range []string{"1:57 PM", "Nov 11, 1:57 PM", "5m ago", "Nov 11", "November 10, 2026"} {
		if ny[i][0] != want {
			t.Errorf("New York %d = %q, want %q", i, ny[i][0], want)
		}
	}
	for i, want := range []string{"3:57 AM", "Nov 12, 3:57 AM", "5m ago", "Nov 12", "November 10, 2026"} {
		if tokyo[i][0] != want {
			t.Errorf("Tokyo %d = %q, want %q", i, tokyo[i][0], want)
		}
	}
	// the title is the full local instant, with its zone
	if !strings.Contains(ny[2][1], "1:57:05 PM") || !strings.Contains(ny[2][1], "EST") || !strings.Contains(tokyo[2][1], "GMT+9") {
		t.Errorf("titles: %q, %q", ny[2][1], tokyo[2][1])
	}
	if ny[4][1] != "" {
		t.Errorf("a date alone got a title: %q", ny[4][1])
	}
}
