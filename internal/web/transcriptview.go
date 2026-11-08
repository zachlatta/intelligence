package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"

	"github.com/zachlatta/intelligence/internal/fleet"
)

// A transcript is shown as blocks: a user message (which starts a turn), an agent message,
// reasoning, a run of tool calls, or anything else. Blocks are built the same way from any entry
// on, which lets a live page re-fetch from its last block's start and replace it.
type block struct {
	Kind      string // user, assistant, reasoning, tools, other
	Seq       int    // the first entry's seq
	TurnStart bool   // a user message
	Entries   []entryView
}

type entryView struct {
	fleet.Entry
	Tool *toolView // tool calls only
}

func blockKind(e fleet.Entry) string {
	switch e.Kind {
	case "user_message":
		return "user"
	case "assistant_message":
		return "assistant"
	case "reasoning":
		return "reasoning"
	case "tool_call":
		return "tools"
	}
	return "other"
}

// buildBlocks groups entries, in seq order, into blocks: consecutive tool calls share one.
func buildBlocks(entries []fleet.Entry) []block {
	var out []block
	for _, e := range entries {
		v := entryView{Entry: e}
		if e.Kind == "tool_call" {
			t := parseTool(e)
			v.Tool = &t
		}
		k := blockKind(e)
		if k == "tools" && len(out) > 0 && out[len(out)-1].Kind == "tools" {
			out[len(out)-1].Entries = append(out[len(out)-1].Entries, v)
			continue
		}
		out = append(out, block{Kind: k, Seq: e.SeqStart, TurnStart: k == "user", Entries: []entryView{v}})
	}
	return out
}

// toolView is a tool call ready to show: a one-line summary, and the parts its kind has.
type toolView struct {
	Type    string // the detail's type: shell, read, edit, write, search, fetch, sub_agent, plain_text, plan, unknown
	Icon    string
	Summary string
	Status  string // running, completed, failed
	Error   string

	Command, Cwd string
	ExitCode     *int
	Output       cutText

	Path    string
	Lines   string // "lines 10–29" for a partial read
	Content cutText
	Diff    []diffLine
	DiffCut int // diff lines left out

	Query, Counts string
	URL, Code     string
	Label         string
	Markdown      template.HTML // a plan, or plain text
	Input, Result cutText       // an unknown tool's input and output, as JSON
}

type diffLine struct {
	Class string // add, del, hunk, ""
	Text  string
}

// cutText is text that may have been cut for the page; the whole is in the entry.
type cutText struct {
	Text      string
	MoreLines int
	MoreBytes int
}

// Cut reports whether anything was left out.
func (c cutText) Cut() bool { return c.MoreLines > 0 || c.MoreBytes > 0 }

const (
	cutLines = 80
	cutBytes = 6000
)

// cut keeps the first cutLines lines and cutBytes bytes of s.
func cut(s string) cutText {
	s = strings.TrimRight(s, "\n")
	var c cutText
	if lines := strings.Split(s, "\n"); len(lines) > cutLines {
		c.MoreLines = len(lines) - cutLines
		s = strings.Join(lines[:cutLines], "\n")
	}
	if len(s) > cutBytes {
		c.MoreBytes = len(s) - cutBytes
		s = strings.ToValidUTF8(s[:cutBytes], "")
	}
	c.Text = s
	return c
}

func parseTool(e fleet.Entry) toolView {
	var it struct {
		Name   string          `json:"name"`
		Status string          `json:"status"`
		Error  json.RawMessage `json:"error"`
		Detail struct {
			Type        string          `json:"type"`
			Command     string          `json:"command"`
			Cwd         string          `json:"cwd"`
			Output      json.RawMessage `json:"output"` // a shell's text, or an unknown tool's value
			ExitCode    *int            `json:"exitCode"`
			FilePath    string          `json:"filePath"`
			Content     string          `json:"content"`
			Offset      int             `json:"offset"`
			Limit       int             `json:"limit"`
			OldString   *string         `json:"oldString"`
			NewString   *string         `json:"newString"`
			UnifiedDiff string          `json:"unifiedDiff"`
			Query       string          `json:"query"`
			ToolName    string          `json:"toolName"`
			NumFiles    *int            `json:"numFiles"`
			NumMatches  *int            `json:"numMatches"`
			FilePaths   []string        `json:"filePaths"`
			URL         string          `json:"url"`
			Code        int             `json:"code"`
			CodeText    string          `json:"codeText"`
			Result      string          `json:"result"`
			SubAgent    string          `json:"subAgentType"`
			Description string          `json:"description"`
			Label       string          `json:"label"`
			Text        string          `json:"text"`
			Input       json.RawMessage `json:"input"`
			BranchName  string          `json:"branchName"`
			Worktree    string          `json:"worktreePath"`
			Log         string          `json:"log"`
		} `json:"detail"`
	}
	_ = json.Unmarshal(e.Item, &it)
	d := it.Detail
	t := toolView{Type: d.Type, Status: it.Status, Summary: it.Name, Icon: "⚙"}
	if len(it.Error) > 0 && string(it.Error) != "null" {
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(it.Error, &msg) == nil && msg.Message != "" {
			t.Error = msg.Message
		} else {
			t.Error = strings.Trim(string(it.Error), `"`)
		}
	}
	switch d.Type {
	case "shell":
		var out string
		_ = json.Unmarshal(d.Output, &out)
		t.Icon, t.Command, t.Cwd, t.ExitCode, t.Output = "$", d.Command, d.Cwd, d.ExitCode, cut(out)
		t.Summary = firstLine(d.Command)
	case "read":
		t.Icon, t.Path, t.Content, t.Summary = "📄", d.FilePath, cut(d.Content), "Read "+d.FilePath
		if d.Offset > 0 || d.Limit > 0 {
			from := max(d.Offset, 1)
			if d.Limit > 0 {
				t.Lines = fmt.Sprintf("lines %d–%d", from, from+d.Limit-1)
			} else {
				t.Lines = fmt.Sprintf("from line %d", from)
			}
		}
	case "write":
		t.Icon, t.Path, t.Content, t.Summary = "✎", d.FilePath, cut(d.Content), "Write "+d.FilePath
	case "edit":
		t.Icon, t.Path, t.Summary = "✎", d.FilePath, "Edit "+d.FilePath
		t.Diff, t.DiffCut = diffLines(d.UnifiedDiff, d.OldString, d.NewString)
	case "search":
		t.Icon, t.Query = "🔍", d.Query
		t.Summary = strings.TrimSpace(cmpOr(d.ToolName, "search") + " " + d.Query)
		switch {
		case d.NumMatches != nil && d.NumFiles != nil:
			t.Counts = fmt.Sprintf("%s in %s", plural(*d.NumMatches, "match"), plural(*d.NumFiles, "file"))
		case d.NumFiles != nil:
			t.Counts = plural(*d.NumFiles, "file")
		}
		t.Content = cut(cmpOr(d.Content, strings.Join(d.FilePaths, "\n")))
	case "fetch":
		t.Icon, t.URL, t.Summary, t.Content = "🌐", d.URL, "Fetch "+d.URL, cut(d.Result)
		if d.Code > 0 {
			t.Code = strings.TrimSpace(fmt.Sprintf("%d %s", d.Code, d.CodeText))
		}
	case "sub_agent":
		t.Icon, t.Label = "🤖", d.SubAgent
		t.Summary = strings.TrimSpace(cmpOr(d.SubAgent, "Sub-agent") + ": " + d.Description)
		t.Markdown = markdown(d.Description)
	case "plain_text":
		t.Icon, t.Label = "ℹ", d.Label
		t.Summary = firstLine(cmpOr(d.Label, d.Text, it.Name))
		if d.Text != d.Label {
			t.Markdown = markdown(d.Text)
		}
	case "plan":
		t.Icon, t.Summary, t.Markdown = "📋", "Plan", markdown(d.Text)
	case "worktree_setup":
		t.Icon, t.Summary, t.Path, t.Output = "🌱", "Set up worktree "+d.BranchName, d.Worktree, cut(d.Log)
	default: // unknown, and anything newer than this page
		t.Input, t.Result = cut(prettyJSON(d.Input)), cut(prettyJSON(d.Output))
		if s := inputSummary(d.Input); s != "" {
			t.Summary = it.Name + " " + s
		}
	}
	return t
}

// diffLines is a unified diff with a class per line, or one made from old and new strings.
func diffLines(unified string, old, new *string) ([]diffLine, int) {
	var lines []string
	if unified != "" {
		lines = strings.Split(strings.TrimRight(unified, "\n"), "\n")
	} else {
		if old != nil && *old != "" {
			for _, l := range strings.Split(*old, "\n") {
				lines = append(lines, "-"+l)
			}
		}
		if new != nil && *new != "" {
			for _, l := range strings.Split(*new, "\n") {
				lines = append(lines, "+"+l)
			}
		}
	}
	more := 0
	if len(lines) > cutLines*2 {
		more, lines = len(lines)-cutLines*2, lines[:cutLines*2]
	}
	out := make([]diffLine, 0, len(lines))
	for _, l := range lines {
		c := ""
		switch {
		case strings.HasPrefix(l, "@@"):
			c = "hunk"
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
			c = "add"
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
			c = "del"
		}
		out = append(out, diffLine{Class: c, Text: l})
	}
	return out, more
}

// inputSummary is a short line from an unknown tool's input: its command, path, query or prompt.
func inputSummary(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "query", "pattern", "url", "prompt", "description"} {
		if s, ok := m[k].(string); ok && s != "" {
			return firstLine(s)
		}
	}
	return ""
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") != nil {
		return string(raw)
	}
	return buf.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func cmpOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// md renders Markdown with GitHub's tables, strikethrough, task lists and autolinks. Transcripts
// hold whatever agents and people wrote: raw HTML is shown as text (rawHTMLAsText), and goldmark
// drops unsafe links (javascript: and the like).
var md = goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(
	renderer.WithNodeRenderers(util.Prioritized(rawHTMLAsText{}, 0))))

func markdown(text string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(buf.String()) //nolint:gosec // raw HTML is escaped and unsafe links dropped
}

// rawHTMLAsText renders inline and block HTML as escaped text, so the reader sees what was written.
type rawHTMLAsText struct{}

func (rawHTMLAsText) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindRawHTML, func(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			segs := n.(*ast.RawHTML).Segments
			for i := 0; i < segs.Len(); i++ {
				seg := segs.At(i)
				_, _ = w.WriteString(template.HTMLEscapeString(string(seg.Value(src))))
			}
		}
		return ast.WalkSkipChildren, nil
	})
	// images point at files on the dev box (screenshots by path) that the page cannot load: name
	// them, and link those on the web
	reg.Register(ast.KindImage, func(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			img := n.(*ast.Image)
			alt := cmpOr(strings.TrimSpace(string(nodeText(img, src))), "image")
			dest := string(img.Destination)
			_, _ = w.WriteString(`<span class="img">🖼 ` + template.HTMLEscapeString(alt))
			switch {
			case strings.HasPrefix(dest, "https://") || strings.HasPrefix(dest, "http://"):
				_, _ = w.WriteString(` <a href="` + template.HTMLEscapeString(dest) + `" rel="noreferrer noopener" target="_blank">open</a>`)
			case dest != "":
				_, _ = w.WriteString(` <code>` + template.HTMLEscapeString(dest) + `</code>`)
			}
			_, _ = w.WriteString(`</span>`)
		}
		return ast.WalkSkipChildren, nil
	})
	reg.Register(ast.KindHTMLBlock, func(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			b := n.(*ast.HTMLBlock)
			_, _ = w.WriteString("<p>")
			lines := b.Lines()
			for i := 0; i < lines.Len(); i++ {
				seg := lines.At(i)
				_, _ = w.WriteString(template.HTMLEscapeString(string(seg.Value(src))))
			}
			if b.HasClosure() {
				_, _ = w.WriteString(template.HTMLEscapeString(string(b.ClosureLine.Value(src))))
			}
			_, _ = w.WriteString("</p>\n")
		}
		return ast.WalkSkipChildren, nil
	})
}

// blocksData is what the blocks template needs.
type blocksData struct {
	Base   string // the agent's page, for entry links
	Blocks []block
	Now    time.Time
}

// nodeText is the text of n's children (an image's alt text).
func nodeText(n ast.Node, src []byte) []byte {
	var b bytes.Buffer
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			b.Write(t.Segment.Value(src))
		} else {
			b.Write(nodeText(c, src))
		}
	}
	return b.Bytes()
}

// toolLabels names a tool call's kind for people.
var toolLabels = map[string]string{"shell": "shell", "read": "read", "write": "write", "edit": "edit", "search": "search",
	"fetch": "web", "sub_agent": "agent", "plain_text": "note", "plan": "plan", "worktree_setup": "setup"}

// Kind is the tool call's kind in a word.
func (t toolView) Kind() string { return cmpOr(toolLabels[t.Type], "tool") }
