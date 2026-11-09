package workflow

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A run's names: its workspace title "<workflow>: <task> · <run>" and its worktree branch
// "wf-<workflow>-<task>-<run>". <task> is the task's first real sentence as written, with only
// these removed (the README lists them with examples): heading and label lines and Markdown
// markers (taskLine); sentences that are only a greeting, listed pleasantries or no words
// (social); lead-ins at the start whose end is clear from a mark, a dash or a request after them
// (dropOpening, unsocial); quotes around the whole sentence (unquote); and a . , ; : ! or ? stuck
// to the end of its last word outside code. Anything ambiguous is kept, so the name never says
// less or the opposite of the task. It is cut at whole words to titleWords words and titleMax
// characters, never ending a cut on a connective; a cut inside a code span closes it with "…`",
// and a longer first word, or a script written without spaces, is cut to titleMax characters
// ending in "…". A first word of plain letters gets a capital ("fix" -> "Fix", never "go.mod").
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

// taskName is the name of input's task, the run's title (workflow_runs.title), from which every
// other name of the run is made: its words (taskWords), with a first word of plain letters
// capitalised; or, with none, the input's first line.
func taskName(input string) string {
	words := taskWords(input)
	if len(words) == 0 {
		return firstLine(input)
	}
	task := strings.Join(words, " ")
	if plainWord(words[0]) {
		r, n := utf8.DecodeRuneInString(task)
		task = string(unicode.ToUpper(r)) + task[n:]
	}
	return task
}

// firstLine is input's first line, cut to 80 characters.
func firstLine(input string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(input), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 80 {
		line = string(r[:79]) + "…"
	}
	return line
}

// runNames are a run's workspace title and worktree branch, from its id, its workflow's name and
// its title (taskName).
//
//	"Factory Task", "Fix the login page" -> "Factory Task: Fix the login page · 1bf01c44a2b3", "wf-factory-task-fix-the-login-page-1bf01c44a2b3"
func runNames(id, workflow, task string) (wsTitle, branch string) {
	hex := strings.ReplaceAll(id, "-", "")
	wsTitle = task + " · " + hex[:12]
	if workflow = strings.TrimSpace(workflow); workflow != "" {
		wsTitle = workflow + ": " + wsTitle
	}
	branch = "wf"
	taskSlug := slug(strings.Fields(task), taskMax)
	if taskLine(task) == "" {
		taskSlug = "" // a title that is only a heading ("Task:") says nothing of the task
	}
	for _, s := range []string{slug(strings.Fields(workflow), workflowMax), taskSlug, hex[:12]} {
		if s != "" {
			branch += "-" + s
		}
	}
	return wsTitle, branch
}

// agentTitle is the title of a run's agent: its role, the workflow's name and the run's title.
func agentTitle(role, workflow, task string) string {
	if workflow = strings.TrimSpace(workflow); workflow != "" {
		task = workflow + ": " + task
	}
	return role + " · " + task
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
	// header is an email header line, as an email's run input has under its subject (Mail.Input).
	header = regexp.MustCompile(`(?i)^(?:from|to|cc|bcc|date|sent|attachments):\s`)
)

// taskLine is line without Markdown markers or a generic label; "" when nothing else is on it,
// or when it is an email header ("From: …").
func taskLine(line string) string {
	line = strings.TrimSpace(line)
	if header.MatchString(line) {
		return ""
	}
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
var abbreviations = map[string]bool{"e.g.": true, "i.e.": true, "etc.": true, "vs.": true, "cf.": true, "approx.": true,
	"inc.": true, "ltd.": true, "co.": true, "corp.": true, "mr.": true, "mrs.": true, "ms.": true, "dr.": true, "st.": true,
	"jr.": true, "sr.": true, "no.": true}

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

// pleasantries is the closed list of social phrases: a sentence made only of these (and of
// conjunctions) is skipped, and one opening a sentence before a clause break is dropped.
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

// conjunctions join a social clause to the task after it ("…, but login still crashes").
var conjunctions = map[string]bool{"but": true, "and": true, "so": true, "also": true}

func phrases(ps ...string) [][]string {
	var out [][]string
	for _, p := range ps {
		out = append(out, strings.Fields(p))
	}
	return out
}

// social reports whether a sentence is only social: a greeting alone or with a name and a mark
// after it ("Hi", "Hey Claude,", but not "Hey Jude"), only listed pleasantries ("Thanks so much!", "Thanks, hope you're well."), or no
// words at all ("🙏").
func social(s []string) bool {
	if !slices.ContainsFunc(s, func(w string) bool { return socialWord(w) != "" }) {
		return true
	}
	for _, p := range greetOpenings {
		if hasPrefix(s, p) && (len(s) == len(p) || len(s) == len(p)+1 && punctuated(s[len(p)])) {
			return true
		}
	}
	for len(s) > 0 {
		for len(s) > 0 && (socialWord(s[0]) == "" || conjunctions[socialWord(s[0])]) {
			s = s[1:]
		}
		n := pleasantryAt(s)
		if n == 0 {
			return len(s) == 0
		}
		s = s[n:]
	}
	return true
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

// unsocial is s without the listed pleasantries it opens with, each only where its end is
// clear: a mark after it (a comma, semicolon, colon, dash, "!" or "."), or a request after it
// ("Thanks, but…", "Thanks can you fix…"). nil when s is only social.
func unsocial(s []string) []string {
	if social(s) {
		return nil
	}
	for len(s) > 0 {
		n := pleasantryAt(s)
		if n == 0 || n == len(s) || !(punctuated(s[n-1]) || isDash(s[n]) || request(s[n:])) {
			return s
		}
		s = s[n:]
		for len(s) > 0 && (isDash(s[0]) || conjunctions[socialWord(s[0])]) {
			s = s[1:]
		}
	}
	return s
}

// punctuated reports whether w ends with a mark that ends a clause.
func punctuated(w string) bool {
	r, _ := utf8.DecodeLastRuneInString(w)
	return strings.ContainsRune(",;:.!?—–", r) || isDash(w)
}

func isDash(w string) bool { return w != "" && strings.Trim(w, "-–—") == "" }

// verbs open a request ("fix the login page"): a closed list, so a lead-in before anything else
// ("Please field is mislabeled", "Could you believe…") is kept.
var verbs = func() map[string]bool {
	m := map[string]bool{}
	for _, v := range strings.Fields(`add allow analyze archive audit automate avoid backfill benchmark bisect block book build
		bump buy call cancel change check clean clear close compare configure confirm convert copy create debug delete
		deploy describe design disable do don't document double-check draft drop edit email enable ensure explain export
		extract figure file find finish fix flag follow generate get give go handle help hide implement import improve
		include install investigate keep kill let limit list load log look make measure merge migrate monitor move name
		notify open optimize pay pin ping plan port post prepare prevent print profile publish pull push put read rebuild
		reduce refactor release reload remove rename reorder repair replace reply report rerun research reset resolve
		restart restore retry return revert review rewrite rotate run save schedule search see send set share ship show
		simplify skip sort speed split start stop summarize support swap switch sync tell test tidy track translate trim
		try turn tweak undo unify update upgrade upload use validate verify watch wire write`) {
		m[v] = true
	}
	return m
}()

// request reports whether s opens a request: a listed verb, perhaps after "never", "not",
// "also", "just", "then" or "now", or a polite opening before one ("please fix…").
func request(s []string) bool {
	for len(s) > 0 && slices.Contains([]string{"never", "not", "also", "just", "then", "now"}, socialWord(s[0])) {
		s = s[1:]
	}
	if len(s) == 0 {
		return false
	}
	if verbs[socialWord(s[0])] {
		return true
	}
	t, ok := dropOne(s)
	return ok && len(t) < len(s) && request(t)
}

// openings are what a request may open with that says nothing of the task, each going only
// with clear evidence of where it ends:
//   - greetings and asides ("hi", "ok", "so", "when you get a chance") before a mark or a dash,
//     or before a request; a greeting also takes a name with a mark after it ("Hey Claude,");
//   - polite requests ("please", "could you", "I'd like you to", "I hope you can") before a
//     request;
//   - "thanks in advance for" before a verb in -ing.
var (
	greetOpenings = phrases("hi there", "hey there", "hello there", "hi", "hey", "hello", "dear", "greetings",
		"good morning", "good afternoon", "good evening")
	asideOpenings = phrases("ok", "okay", "so", "when you get a chance", "when you have a moment", "if you can",
		"if possible")
	requestOpenings = phrases("please", "pls", "kindly", "can you", "could you", "would you", "will you", "can we",
		"could we", "i want you to", "i'd like you to", "i would like you to", "i need you to", "go ahead and",
		"make sure to", "hope you can", "hope you could", "hope you will", "hope you would", "i hope you can",
		"i hope you could", "i hope you will", "i hope you would", "hoping you can", "hoping you could",
		"hoping you will", "hoping you would", "i'm hoping you can", "i'm hoping you could", "i'm hoping you will",
		"i'm hoping you would")
	gerundOpenings = phrases("thanks in advance for", "thank you in advance for")
)

// dropOpening is s without the openings it starts with.
func dropOpening(s []string) []string {
	for {
		t, ok := dropOne(s)
		if !ok {
			return s
		}
		s = t
	}
}

// dropOne is s without the one opening it starts with, if that opening's end is clear.
func dropOne(s []string) ([]string, bool) {
	for _, p := range greetOpenings {
		if n := len(p); len(s) > n && hasPrefix(s, p) {
			switch {
			case punctuated(s[n-1]), isDash(s[n]), request(s[n:]):
				return s[n:], true
			case len(s) > n+1 && plainWord(strings.TrimRight(s[n], ",;:!.—–")) && punctuated(s[n]):
				return s[n+1:], true // "Hey Claude, …"
			}
		}
	}
	for _, p := range asideOpenings {
		if n := len(p); len(s) > n && hasPrefix(s, p) && (punctuated(s[n-1]) || isDash(s[n]) || request(s[n:])) {
			return s[n:], true
		}
	}
	for _, p := range requestOpenings {
		if n := len(p); len(s) > n && hasPrefix(s, p) && request(s[n:]) {
			return s[n:], true
		}
	}
	for _, p := range gerundOpenings {
		if n := len(p); len(s) > n && hasPrefix(s, p) && strings.HasSuffix(socialWord(s[n]), "ing") {
			return s[n:], true
		}
	}
	return s, false
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
