package workflow

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A run's names: its workspace title "<workflow>: <task> · <run>" and its worktree branch
// "wf-<workflow>-<task>-<run>". <task> is the task's first sentence nearly as written (only
// greetings, a polite opening and articles go, so "not", "no", "never" and "without" always stay
// and the name never says the opposite of the task), cut at whole words.
//
// The title keeps any script. The branch keeps lowercase ASCII only, because the Paseo daemon
// refuses any other worktree branch (validateBranchSlug in @getpaseo/protocol: [a-z0-9-/], no
// "--", at most 100 characters); accents are folded (é -> e) and words with no ASCII rendering
// (a Chinese task) leave the branch with just the workflow and the run, while the title, stored on
// the run, keeps the original.
//
// <run> is the run id's first 8 hex digits in the title, enough to tell runs apart by eye, and
// its first 12 (48 random bits of a version 4 UUID) in the branch, so two runs sharing a branch is
// vanishingly unlikely.
const (
	titleWords  = 8  // the task's words in a title, at most
	titleMax    = 60 // and its characters
	workflowMax = 24 // a branch's slug of the workflow's name, at most
	taskMax     = 40 // and of the task
)

// runNames are a run's workspace title and worktree branch.
//
//	"Factory Task", "Please fix the login page" -> "Factory Task: Fix login page · 1bf01c44", "wf-factory-task-fix-login-page-1bf01c44a2b3"
func runNames(id, workflow, input string) (wsTitle, branch string) {
	hex := strings.ReplaceAll(id, "-", "")
	words := taskWords(input)
	task := strings.Join(words, " ")
	if r, n := utf8.DecodeRuneInString(task); n > 0 {
		task = string(unicode.ToUpper(r)) + task[n:]
	}
	if task == "" {
		task = title(input)
	}
	wsTitle = task + " · " + hex[:8]
	if workflow = strings.TrimSpace(workflow); workflow != "" {
		wsTitle = workflow + ": " + wsTitle
	}
	branch = "wf"
	for _, s := range []string{slug(strings.Fields(workflow), workflowMax), slug(words, taskMax), hex[:12]} {
		if s != "" {
			branch += "-" + s
		}
	}
	return wsTitle, branch
}

// taskWords are the words naming input's task: its first sentence that is more than a
// greeting, on its first line that is more than a heading, without a polite opening or articles,
// cut to titleWords words and titleMax characters at whole words.
func taskWords(input string) []string {
	var sentence []string
	for line := range strings.Lines(input) {
		line = taskLine(line)
		if line == "" {
			continue
		}
		sentences := splitSentences(strings.Fields(line))
		for len(sentences) > 1 && pleasantry(sentences[0]) {
			sentences = sentences[1:]
		}
		sentence = sentences[0]
		break
	}
	sentence = dropOpening(sentence)
	var out []string
	n := 0
	for _, w := range sentence {
		w = strings.TrimFunc(w, notWordRune)
		if w == "" || article[strings.ToLower(w)] {
			continue
		}
		size := utf8.RuneCountInString(w)
		if len(out) > 0 {
			size++
		}
		if len(out) == titleWords || (len(out) > 0 && n+size > titleMax) {
			break
		}
		out = append(out, w)
		n += size
	}
	return trimConnectives(out)
}

var (
	// listMarker opens a Markdown heading, list item or quote.
	listMarker = regexp.MustCompile(`^(?:#+|[-*+>]|\d+[.)])\s+`)
	// label is a generic label at a line's start ("Task:", "TASK (from Zach):", "## Goal").
	label = regexp.MustCompile(`(?i)^(?:task|tasks|goal|request|ask|todo|to-do|context|summary|description|title|prompt|issue|problem|objective|background|details|note|subject)\b(?:\s*\([^)]*\))?\s*(?::\s*|$)`)
)

// taskLine is line without Markdown markers or a generic label; "" when nothing else is on it.
func taskLine(line string) string {
	line = strings.TrimSpace(line)
	for {
		m := listMarker.FindString(line + " ")
		if m == "" {
			break
		}
		line = strings.TrimSpace(line[min(len(m), len(line)):])
	}
	line = strings.TrimSpace(label.ReplaceAllString(line, ""))
	if strings.TrimFunc(line, notWordRune) == "" {
		return ""
	}
	return line
}

func notWordRune(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }

// splitSentences splits words after each that ends a sentence (with ., ! or ?, before any
// closing quote or bracket).
func splitSentences(words []string) [][]string {
	var out [][]string
	start := 0
	for i, w := range words {
		w = strings.TrimRight(w, `"'”’)]}*_`)
		if strings.HasSuffix(w, ".") || strings.HasSuffix(w, "!") || strings.HasSuffix(w, "?") || i == len(words)-1 {
			out = append(out, words[start:i+1])
			start = i + 1
		}
	}
	return out
}

// lowerWord is w in lowercase without surrounding punctuation, with ’ as '.
func lowerWord(w string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimFunc(w, notWordRune), "’", "'"))
}

// hasPrefix reports whether words open with phrase (lowercase words).
func hasPrefix(words []string, phrase []string) bool {
	if len(words) < len(phrase) {
		return false
	}
	for i, p := range phrase {
		if lowerWord(words[i]) != p {
			return false
		}
	}
	return true
}

// pleasantry reports whether a sentence is only social: a greeting, a hope, thanks.
func pleasantry(s []string) bool {
	for _, p := range [][]string{{"hi"}, {"hey"}, {"hello"}, {"dear"}, {"greetings"}, {"good", "morning"}, {"good", "afternoon"},
		{"good", "evening"}, {"hope"}, {"i", "hope"}, {"thanks"}, {"thank", "you"}, {"i", "appreciate"}} {
		if hasPrefix(s, p) {
			return true
		}
	}
	return false
}

// openings are what a request may open with that says nothing of the task.
var openings = func() [][]string {
	var out [][]string
	for _, p := range []string{"hi there", "hey there", "hello there", "hi", "hey", "hello", "ok", "okay", "so",
		"please", "pls", "kindly", "can you", "could you", "would you", "will you", "can we", "could we",
		"i want you to", "i'd like you to", "i would like you to", "i need you to", "go ahead and", "make sure to",
		"when you get a chance", "when you have a moment", "if you can", "if possible"} {
		out = append(out, strings.Fields(p))
	}
	return out
}()

// dropOpening is s without the polite openings it starts with.
func dropOpening(s []string) []string {
	for again := true; again; {
		again = false
		for _, p := range openings {
			if len(s) > len(p) && hasPrefix(s, p) {
				s, again = s[len(p):], true
				break
			}
		}
	}
	return s
}

var article = map[string]bool{"a": true, "an": true, "the": true}

// connectives join words; a name cut short never ends on one. Negations are not among them.
var connectives = map[string]bool{"and": true, "or": true, "but": true, "with": true, "to": true, "for": true, "of": true,
	"in": true, "on": true, "at": true, "by": true, "from": true, "into": true, "that": true, "which": true, "who": true,
	"as": true, "than": true, "then": true, "so": true, "if": true, "when": true, "while": true, "because": true}

func trimConnectives(words []string) []string {
	for len(words) > 0 && connectives[lowerWord(words[len(words)-1])] {
		words = words[:len(words)-1]
	}
	return words
}

// fold spells common accented Latin letters in ASCII.
var fold = strings.NewReplacer("à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a", "ā", "a", "ą", "a",
	"ç", "c", "ć", "c", "č", "c", "ď", "d", "đ", "d", "è", "e", "é", "e", "ê", "e", "ë", "e", "ē", "e", "ę", "e", "ě", "e",
	"ì", "i", "í", "i", "î", "i", "ï", "i", "ī", "i", "ł", "l", "ñ", "n", "ń", "n", "ň", "n",
	"ò", "o", "ó", "o", "ô", "o", "õ", "o", "ö", "o", "ø", "o", "ō", "o", "ő", "o", "ř", "r", "ś", "s", "š", "s", "ß", "ss",
	"ť", "t", "ù", "u", "ú", "u", "û", "u", "ü", "u", "ū", "u", "ů", "u", "ű", "u", "ý", "y", "ÿ", "y",
	"ź", "z", "ż", "z", "ž", "z", "æ", "ae", "œ", "oe", "þ", "th", "ð", "d", "'", "", "’", "")

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug is words as lowercase ASCII joined by hyphens, as many whole words as fit in max bytes,
// not ending on a connective.
func slug(words []string, max int) string {
	var parts, kept []string
	n := 0
	for _, w := range words {
		s := strings.Trim(nonSlug.ReplaceAllString(fold.Replace(strings.ToLower(w)), "-"), "-")
		if s == "" {
			continue
		}
		size := len(s)
		if len(parts) > 0 {
			size++
		}
		if n+size > max {
			break
		}
		parts, kept = append(parts, s), append(kept, w)
		n += size
	}
	return strings.Join(parts[:len(trimConnectives(kept))], "-")
}
