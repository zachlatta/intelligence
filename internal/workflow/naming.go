package workflow

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A run's names: its workspace title "<workflow>: <task> · <run>" and its worktree branch
// "wf-<workflow>-<task>-<run>". <task> is the task's first real sentence as written: lines and
// sentences that are only a heading, a greeting ("Hi!", "Hey Claude,") or a pleasantry
// from the closed list in pleasantries ("Thanks!", "Hope you're well.") are skipped, and one that
// opens with a listed pleasantry before a clause break loses only that ("Thanks for the fix, but
// …"); anything not on the list is kept. Then only a polite opening ("could you please")
// and the dash after a greeting, quotes around the whole sentence, and a . , ; : ! or ? stuck to
// the end of its last word outside code go; every other word stays as written (articles,
// negations, code, operators, paths, URLs, quotes), so the name never says less or the opposite
// of the task. It is cut at whole words to titleWords words and titleMax characters, never
// ending a cut on a connective; a cut inside a code span closes it with "…`", and a longer first
// word, or a script written without spaces, is cut to titleMax characters ending in "…". A first
// word of plain letters gets a capital ("fix" -> "Fix", never "go.mod").
//
// The title keeps any script. The branch keeps lowercase ASCII only, because the Paseo daemon
// refuses any other worktree branch (validateBranchSlug in @getpaseo/protocol: [a-z0-9-/], no
// "--", at most 100 characters); accents are folded (é -> e) and words with no ASCII rendering
// (a Chinese task) leave the branch with just the workflow and the run, while the title, stored on
// the run, keeps the original.
//
// <run> is the run id's first 12 hex digits (48 random bits of a version 4 UUID) in both, so
// two runs sharing a name is vanishingly unlikely.
const (
	titleWords  = 8  // the task's words in a title, at most
	titleMax    = 60 // and its characters
	workflowMax = 24 // a branch's slug of the workflow's name, at most
	taskMax     = 40 // and of the task
)

// runNames are a run's workspace title and worktree branch.
//
//	"Factory Task", "Please fix the login page" -> "Factory Task: Fix the login page · 1bf01c44a2b3", "wf-factory-task-fix-the-login-page-1bf01c44a2b3"
func runNames(id, workflow, input string) (wsTitle, branch string) {
	hex := strings.ReplaceAll(id, "-", "")
	words := taskWords(input)
	task := strings.Join(words, " ")
	if len(words) > 0 && plainWord(words[0]) {
		r, n := utf8.DecodeRuneInString(task)
		task = string(unicode.ToUpper(r)) + task[n:]
	}
	if task == "" {
		task = title(input)
	}
	wsTitle = task + " · " + hex[:12]
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

// taskWords are the words naming input's task, as written: its first sentence that is more
// than a greeting or pleasantry, on its first line that is more than a heading or a greeting,
// without a polite opening, cut to titleWords words and titleMax characters at whole
// words. A first word longer than titleMax (or a script written without spaces) is cut and
// marked with "…".
func taskWords(input string) []string {
	var sentence, fallback []string
	for line := range strings.Lines(input) {
		line = taskLine(line)
		if line == "" {
			continue
		}
		sentences := splitSentences(strings.Fields(line))
		if fallback == nil {
			fallback = sentences[0]
		}
		for len(sentences) > 0 && social(sentences[0]) {
			sentences = sentences[1:]
		}
		if len(sentences) > 0 {
			sentence = unsocial(sentences[0])
			break
		}
	}
	if sentence == nil {
		sentence = fallback // nothing but greetings: the first one stands in
	}
	words := unquote(dropDashes(dropOpening(sentence)))
	if len(words) == 0 {
		return nil
	}
	if r := []rune(words[0]); len(r) > titleMax {
		return []string{string(r[:titleMax-1]) + "…"}
	}
	var out []string
	n := 0
	for _, w := range words {
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
	if len(out) < len(words) {
		if openSpan(out) {
			// cut inside code: close the span, marking the cut
			for len(out) > 1 && n+2 > titleMax {
				n -= utf8.RuneCountInString(out[len(out)-1]) + 1
				out = out[:len(out)-1]
			}
			out[len(out)-1] += "…`"
			return out
		}
		if t := trimConnectives(out); len(t) > 0 {
			out = t
		}
	}
	// a mark ending the sentence goes, when it is on a word and not in code
	if last := strings.TrimRight(out[len(out)-1], ".,;:!?"); last != "" && !openSpan(out) {
		out[len(out)-1] = last
	}
	return out
}

// openSpan reports whether words leave a code span (`…`) open.
func openSpan(words []string) bool {
	n := 0
	for _, w := range words {
		n += strings.Count(w, "`")
	}
	return n%2 == 1
}

// dropDashes is words without the dashes or bullets they open with ("Good morning — fix…").
func dropDashes(words []string) []string {
	for len(words) > 0 && strings.Trim(words[0], "-–—•·:,;") == "" {
		words = words[1:]
	}
	return words
}

// unquote is words without the quotes around all of them (`"Show each agent's model."`).
func unquote(words []string) []string {
	if len(words) == 0 {
		return words
	}
	quotes := 0
	for _, w := range words {
		quotes += strings.Count(w, `"`) + strings.Count(w, "“") + strings.Count(w, "”")
	}
	first, last := words[0], words[len(words)-1]
	open := strings.HasPrefix(first, `"`) || strings.HasPrefix(first, "“")
	end := strings.TrimRight(last, ".,;:!?")
	closed := strings.HasSuffix(end, `"`) || strings.HasSuffix(end, "”")
	if quotes != 2 || !open || !closed || (len(words) == 1 && utf8.RuneCountInString(first) < 3) {
		return words
	}
	words = slices.Clone(words)
	_, n := utf8.DecodeRuneInString(first)
	words[0] = first[n:]
	_, m := utf8.DecodeLastRuneInString(end)
	words[len(words)-1] = end[:len(end)-m] + last[len(end):]
	if words[0] == "" {
		words = words[1:]
	}
	return words
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

// plainWord reports whether w is letters only (with apostrophes, as in "don't"), not code like "go.mod".
func plainWord(w string) bool {
	return !strings.ContainsFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' && r != '’' })
}

func notWordRune(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }

// abbreviations end with a full stop but not a sentence.
var abbreviations = map[string]bool{"e.g.": true, "i.e.": true, "etc.": true, "vs.": true, "cf.": true, "approx.": true}

// splitSentences splits words after each that ends a sentence (with ., ! or ?, before any
// closing quote or bracket), but not inside a code span or quotes, nor after an abbreviation.
func splitSentences(words []string) [][]string {
	var out [][]string
	start, ticks, quotes := 0, 0, 0
	for i, w := range words {
		ticks += strings.Count(w, "`")
		quotes += strings.Count(w, `"`) + strings.Count(w, "“") + strings.Count(w, "”")
		t := strings.TrimRight(w, `"'”’)]}*_`)
		ends := strings.HasSuffix(t, ".") || strings.HasSuffix(t, "!") || strings.HasSuffix(t, "?")
		if i == len(words)-1 || (ends && ticks%2 == 0 && quotes%2 == 0 && !abbreviations[strings.ToLower(w)]) {
			out = append(out, words[start:i+1])
			start = i + 1
		}
	}
	return out
}

// openingWord is w as a polite opening would be written: lowercase, without the marks after it
// ("Hey,", "Hi!"). Code ("`please`") is never one.
func openingWord(w string) string {
	return strings.ToLower(strings.TrimRight(strings.ReplaceAll(w, "’", "'"), ",.!?:;—–-"))
}

// hasPrefix reports whether words open with phrase (lowercase words).
func hasPrefix(words []string, phrase []string) bool {
	if len(words) < len(phrase) {
		return false
	}
	for i, p := range phrase {
		if openingWord(words[i]) != p {
			return false
		}
	}
	return true
}

// greetings open a message; a sentence of a greeting and at most a name ("Hey Claude,") is only
// social.
var greetings = phrases("hi", "hi there", "hey", "hey there", "hello", "hello there", "dear", "greetings",
	"good morning", "good afternoon", "good evening")

// pleasantries is the closed list of social phrases: a sentence made only of these (and of
// "and", "but", "so") is skipped, and one opening a sentence before a clause break is dropped.
// Anything not on the list is kept: the list says what is social, never what might be.
var pleasantries = phrases(
	"thanks", "thank you", "thanks so much", "thank you so much", "thanks a lot", "thank you very much",
	"many thanks", "much appreciated", "appreciate it", "i appreciate it", "cheers", "ty",
	"thanks for the fix", "thanks for the help", "thanks for your help", "thanks for all your help",
	"thanks so much for all your help", "thank you for your help", "thank you for all your help",
	"thanks for the update", "thank you for the update",
	"hope you're well", "hope you are well", "i hope you're well", "i hope you are well",
	"hope you're doing well", "hope you are doing well", "i hope you're doing well", "i hope you are doing well",
	"hope all is well", "i hope all is well", "hope your week is going well", "i hope your week is going well",
	"hope you had a good weekend", "i hope you had a good weekend", "hope this helps", "i hope this helps",
	"sorry to bother you", "sorry for the noise", "sorry for the delay", "sorry for the late reply",
	"apologies for the delay", "apologies for the noise")

// hopeYouCan opens a request worded as a hope ("I hope you can fix…"): the request follows.
var hopeYouCan = func() [][]string {
	var out [][]string
	for _, h := range []string{"hope", "i hope", "hoping", "i'm hoping"} {
		for _, v := range []string{"can", "could", "will", "would"} {
			out = append(out, strings.Fields(h+" you "+v))
		}
	}
	return out
}()

// conjunctions join a social clause to the task after it ("…, but login still crashes").
var conjunctions = map[string]bool{"but": true, "and": true, "so": true, "also": true}

func phrases(ps ...string) [][]string {
	var out [][]string
	for _, p := range ps {
		out = append(out, strings.Fields(p))
	}
	return out
}

// social reports whether a sentence is only social: a greeting with at most a name ("Hey
// Claude,"), only listed pleasantries ("Thanks so much!", "Thanks, hope you're well."), or no
// words at all ("🙏").
func social(s []string) bool {
	if !slices.ContainsFunc(s, func(w string) bool { return socialWord(w) != "" }) {
		return true
	}
	for _, p := range greetings {
		if hasPrefix(s, p) && len(s) <= len(p)+1 {
			return true
		}
	}
	return len(unsocial(s)) == 0
}

// socialWord is w compared with the pleasantries: lowercase, with ’ as ', without the marks
// around it; "" for a token of no letters or digits (a dash, an emoji).
func socialWord(w string) string {
	return strings.ToLower(strings.TrimFunc(strings.ReplaceAll(w, "’", "'"), func(r rune) bool { return notWordRune(r) && r != '\'' }))
}

// pleasantryAt is how many words of s, from its start, the longest listed pleasantry takes; 0
// for none.
func pleasantryAt(s []string) int {
	best := 0
	for _, p := range pleasantries {
		if len(p) > best && socialPrefix(s, p) {
			best = len(p)
		}
	}
	return best
}

// socialPrefix reports whether s opens with phrase, compared as socialWords.
func socialPrefix(s, phrase []string) bool {
	return len(s) >= len(phrase) && slices.EqualFunc(phrase, s[:len(phrase)], func(p, w string) bool { return p == socialWord(w) })
}

// unsocial is s without the listed pleasantries it opens with, each only where a clause break
// (a comma, semicolon, colon or dash), a conjunction or the sentence's end follows it, and
// without a hope worded as a request ("I hope you can"); nil when nothing else is in s.
func unsocial(s []string) []string {
	for len(s) > 0 {
		for _, p := range hopeYouCan {
			if len(s) > len(p) && socialPrefix(s, p) {
				return s[len(p):]
			}
		}
		n := pleasantryAt(s)
		if n == 0 {
			return s
		}
		if n < len(s) && !strings.ContainsAny(s[n-1][len(s[n-1])-1:], ",;:—–-") && socialWord(s[n]) != "" && !conjunctions[socialWord(s[n])] {
			return s // a listed phrase going on into more ("Thanks page shows…")
		}
		s = s[n:]
		for len(s) > 0 && (socialWord(s[0]) == "" || conjunctions[socialWord(s[0])]) {
			s = s[1:]
		}
	}
	return nil
}

// openings are what a request may open with that says nothing of the task.
var openings = func() [][]string {
	var out [][]string
	for _, p := range []string{"hi there", "hey there", "hello there", "hi", "hey", "hello", "dear", "greetings",
		"good morning", "good afternoon", "good evening", "ok", "okay", "so",
		"please", "pls", "kindly", "can you", "could you", "would you", "will you", "can we", "could we",
		"i want you to", "i'd like you to", "i would like you to", "i need you to", "go ahead and", "make sure to",
		"when you get a chance", "when you have a moment", "if you can", "if possible",
		"thanks in advance for", "thank you in advance for"} {
		out = append(out, strings.Fields(p))
	}
	return out
}()

// dropOpening is s without the polite openings it starts with, and the name a greeting
// addresses ("Hey Claude, can you…").
func dropOpening(s []string) []string {
	for again := true; again; {
		again = false
		for _, p := range openings {
			if len(s) > len(p) && hasPrefix(s, p) {
				greeted := slices.ContainsFunc(greetings, func(g []string) bool { return slices.Equal(g, p) })
				named := !strings.HasSuffix(s[len(p)-1], ",") && strings.HasSuffix(s[len(p)], ",")
				s, again = s[len(p):], true
				if greeted && named && len(s) > 1 {
					s = s[1:]
				}
				break
			}
		}
	}
	return s
}

// connectives join words, and articles lead to one: a name cut short never ends on either. Words
// in code ("`a`") and negations are not among them.
var connectives = map[string]bool{"a": true, "an": true, "the": true, "and": true, "or": true, "but": true, "with": true, "to": true, "for": true, "of": true,
	"in": true, "on": true, "at": true, "by": true, "from": true, "into": true, "that": true, "which": true, "who": true,
	"as": true, "before": true, "after": true, "than": true, "then": true, "so": true, "if": true, "when": true, "while": true, "because": true}

func trimConnectives(words []string) []string {
	for len(words) > 0 && connectives[strings.ToLower(strings.TrimRight(words[len(words)-1], ",;:"))] {
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

// slug is words as lowercase ASCII joined by hyphens: as many of their parts (split at anything
// but a letter or digit, so "dependency-vulnerability-review" is three) as fit in max bytes, and,
// when that cuts it short, not ending on a connective outside code. A first part longer than max
// is truncated to max, so the slug is never empty while there is ASCII to name it by.
func slug(words []string, max int) string {
	type part struct {
		s    string
		code bool
	}
	var parts []part
	for _, w := range words {
		for _, s := range strings.Split(nonSlug.ReplaceAllString(fold.Replace(strings.ToLower(w)), "-"), "-") {
			if s != "" {
				parts = append(parts, part{s, strings.Contains(w, "`")})
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts[0].s) > max {
		return parts[0].s[:max]
	}
	var kept []string
	n, cut := 0, false
	for _, p := range parts {
		size := len(p.s)
		if len(kept) > 0 {
			size++
		}
		if n+size > max {
			cut = true
			break
		}
		kept = append(kept, p.s)
		n += size
	}
	for cut && len(kept) > 1 && !parts[len(kept)-1].code && connectives[kept[len(kept)-1]] {
		kept = kept[:len(kept)-1]
	}
	return strings.Join(kept, "-")
}
