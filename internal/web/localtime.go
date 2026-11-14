package web

import (
	_ "embed"
	"html/template"
	"regexp"
	"strings"
	"time"
)

// Times on every page show in the reader's own time zone and locale. The server never formats a
// clock time for a page: it writes the instant (RFC 3339, UTC) in a <time> and localtime.js, which
// every page loads, rewrites the text in the browser's zone and locale and gives it the full local
// instant as its title. The text the server writes is the instant in UTC, for a browser that runs
// no script. TestNoServerFormattedClockTimes keeps it so.

//go:embed localtime.js
var localTimeJS []byte

var localTimeScript = asset("localtime.js", "text/javascript; charset=utf-8", localTimeJS)

// localFormats are the formats a page can ask for (data-format), each with its UTC fallback.
// localtime.js has the same names, with what each shows in the browser.
var localFormats = map[string]string{
	"time":    "15:04 MST",                // a clock time, in a list of one day's events
	"day":     "Jan 2",                    // a day, on a chart's axis
	"short":   "Jan 2 15:04 MST",          // a day and time
	"seconds": "Jan 2 15:04:05 MST",       // a day and time to the second
	"weekday": "Mon Jan 2 15:04 MST",      // a day of the week, day and time
	"minute":  "Jan 2, 2006 15:04 MST",    // a date and time
	"full":    "Jan 2, 2006 15:04:05 MST", // a date and time to the second
}

// localFuncs are the template functions that show times; every page template has them.
var localFuncs = template.FuncMap{"at": localTime, "agoAt": localAgo, "localText": localText, "localMarkdown": localMarkdown}

// localTime is t as a <time> the browser shows in its own zone, in format (one of localFormats).
func localTime(t time.Time, format string) template.HTML {
	if t.IsZero() {
		return ""
	}
	layout, ok := localFormats[format]
	if !ok {
		format, layout = "full", localFormats["full"]
	}
	iso := t.UTC().Format(time.RFC3339)
	return template.HTML(`<time datetime="` + iso + `" data-format="` + format + `" title="` + iso + `">` +
		template.HTMLEscapeString(t.UTC().Format(layout)) + `</time>`)
}

// localAgo is how long before now t was (ago), in a <time> whose title the browser sets to the
// local instant.
func localAgo(now, t time.Time) template.HTML {
	if t.IsZero() {
		return template.HTML(ago(now, t))
	}
	iso := t.UTC().Format(time.RFC3339)
	return template.HTML(`<time datetime="` + iso + `" title="` + iso + `">` + template.HTMLEscapeString(ago(now, t)) + `</time>`)
}

var rfc3339 = regexp.MustCompile(`\b\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)`)

// localText is s, escaped, with each RFC 3339 instant in it shown as a local time.
func localText(s string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range rfc3339.FindAllStringIndex(s, -1) {
		t, err := time.Parse(time.RFC3339, s[m[0]:m[1]])
		if err != nil {
			continue
		}
		b.WriteString(template.HTMLEscapeString(s[last:m[0]]))
		b.WriteString(string(localTime(t, "short")))
		last = m[1]
	}
	b.WriteString(template.HTMLEscapeString(s[last:]))
	return template.HTML(b.String())
}

var tagOrInstant = regexp.MustCompile(`<[^>]*>|` + rfc3339.String())

// localMarkdown is text as Markdown (markdown), with each RFC 3339 instant in its text, not in a
// tag, shown as a local time.
func localMarkdown(text string) template.HTML {
	return template.HTML(tagOrInstant.ReplaceAllStringFunc(string(markdown(text)), func(m string) string {
		if strings.HasPrefix(m, "<") {
			return m
		}
		t, err := time.Parse(time.RFC3339, m)
		if err != nil {
			return m
		}
		return string(localTime(t, "short"))
	}))
}

// chartLabel is a label at one end of a chart's axis: a time the browser localizes, or plain text.
type chartLabel struct {
	At     time.Time
	Format string
	Text   string
}

func labelAt(t time.Time, format string) chartLabel { return chartLabel{At: t, Format: format} }

func labelText(s string) chartLabel { return chartLabel{Text: s} }

// svg is the label as the attributes and text of an SVG <text> (which takes no <time>).
func (l chartLabel) svg() (attrs, text string) {
	if l.At.IsZero() {
		return "", template.HTMLEscapeString(l.Text)
	}
	layout, ok := localFormats[l.Format]
	if !ok {
		l.Format, layout = "full", localFormats["full"]
	}
	return ` data-datetime="` + l.At.UTC().Format(time.RFC3339) + `" data-format="` + l.Format + `"`,
		template.HTMLEscapeString(l.At.UTC().Format(layout))
}
