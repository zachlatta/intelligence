package workflow

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// How long a run's names may grow: the task's words, and the slugs of the workflow and the task
// in its branch. A branch is at most "wf-" + 24 + "-" + 40 + "-" + 8 = 77 characters.
const (
	taskWords   = 5
	workflowMax = 24
	taskMax     = 40
)

// word is a word of a task: letters and digits, with inner apostrophes and dots (don't, 5.5).
var word = regexp.MustCompile(`[\p{L}\p{N}]+(?:['’.][\p{L}\p{N}]+)*`)

// filler are words that say little about a task: articles, pronouns, auxiliaries, prepositions,
// question words and the polite or wishful framing of a request.
var filler = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
		a an the this that these those it its it's i i'm i'd i'll i've me my mine we we're our us you you're your
		he she they them their his her him who whom whose what which when where why how
		is am are was were be been being do does did doing done have has had having
		can could would should will shall may might must need needs want wants wanted like
		please pls kindly let lets let's just also really very so then than too maybe perhaps
		and or but nor if else because as of in on at to for from by with without into onto about
		over under up down out off there here all any some each every much many more most such
		not no yes ok okay hey hi hello thanks thank get got go going make sure good nice way thing things
		https http www com`) {
		filler[w] = true
	}
}

// taskKeyWords are the first few words of input's first line that carry its meaning: filler
// dropped, each word once (a plural counts as its singular).
func taskKeyWords(input string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range word.FindAllString(title(input), -1) {
		lower := strings.ToLower(strings.ReplaceAll(w, "’", "'"))
		if filler[lower] {
			continue
		}
		key := lower
		if len(key) > 3 && strings.HasSuffix(key, "s") && !strings.HasSuffix(key, "ss") {
			key = key[:len(key)-1]
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, w)
		if len(out) == taskWords {
			break
		}
	}
	return out
}

// slug is s as lowercase ASCII words joined by hyphens, cut at a word to at most max bytes.
func slug(s string, max int) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	for len(s) > max {
		i := strings.LastIndexByte(s[:max+1], '-')
		if i <= 0 {
			return strings.TrimRight(s[:max], "-")
		}
		s = s[:i]
	}
	return s
}

// runNames are a run's workspace title and worktree branch: they name the workflow and the task
// (its key words, or its first line when it has none), and the branch the run (its id's first 8).
//
//	"Factory Task", "Fix the login page" -> "Factory Task: Fix login page", "wf-factory-task-fix-login-page-1bf01c44"
func runNames(id, workflow, input string) (wsTitle, branch string) {
	task := title(input)
	if words := taskKeyWords(input); len(words) > 0 {
		task = strings.Join(words, " ")
		r, n := utf8.DecodeRuneInString(task)
		task = string(unicode.ToUpper(r)) + task[n:]
	}
	wsTitle = task
	if workflow = strings.TrimSpace(workflow); workflow != "" {
		wsTitle = workflow + ": " + task
	}
	branch = "wf"
	for _, s := range []string{slug(workflow, workflowMax), slug(task, taskMax), id[:8]} {
		if s != "" {
			branch += "-" + s
		}
	}
	return wsTitle, branch
}
