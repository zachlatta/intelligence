// Package interrupt recognises an agent turn that never ran because the provider rejected the
// host's login: an expired or revoked Claude or Codex sign-in. The provider's CLI reports it as the
// whole of the turn's message ("Failed to authenticate: this login reached its time limit…"), and
// the agent then sits idle as if it had finished. Agents also talk about logins and 401s all the
// time, so only that exact shape counts: the provider's own wording, opening a short message of the
// agent's (never a user's message, a tool call or reasoning).
package interrupt

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Reason codes, kept in agent_interruptions.reason and login_incidents.reason.
const (
	LoginExpired = "login_expired" // the sign-in reached its time limit, or its token could not be refreshed
	LoggedOut    = "logged_out"    // no sign-in, or it was revoked
	AuthRejected = "auth_rejected" // the provider refused the credential (401) for another reason
)

// Reason is why a turn failed.
type Reason struct {
	Code    string // LoginExpired, LoggedOut or AuthRejected
	Summary string // in plain words, for the interface
	Message string // what the provider said: its first line, clipped
}

// maxLen bounds a failure message: the providers' are a sentence or two. A longer message is an
// agent's own writing that happens to start the same way.
const maxLen = 700

type pattern struct {
	provider string
	start    *regexp.Regexp // must match at the start of the trimmed message
	also     *regexp.Regexp // and, when set, somewhere in it
	code     string
}

var patterns = []pattern{
	// Claude Code. "Failed to authenticate" alone could be an agent's sentence about GitHub, so it
	// needs Claude's own follow-up: its login command, a 401 or its OAuth wording.
	{"claude", regexp.MustCompile(`^Failed to authenticate\b`), regexp.MustCompile(`time limit|cannot be renewed|OAuth token has expired`), LoginExpired},
	{"claude", regexp.MustCompile(`^Failed to authenticate\b`), regexp.MustCompile("claude auth login|/login|API Error: 401|authentication_error|OAuth"), AuthRejected},
	{"claude", regexp.MustCompile(`^API Error: 401\b`), regexp.MustCompile(`(?i)token has expired`), LoginExpired},
	{"claude", regexp.MustCompile(`^API Error: 401\b`), nil, AuthRejected},
	{"claude", regexp.MustCompile(`^OAuth token has expired\b`), nil, LoginExpired},
	{"claude", regexp.MustCompile(`^(Invalid API key|Not logged in|OAuth token revoked)\b`), regexp.MustCompile(`/login`), LoggedOut},
	// Codex.
	{"codex", regexp.MustCompile(`^Your access token could not be refreshed\b`), nil, LoginExpired},
	{"codex", regexp.MustCompile(`^(stream error: )?unexpected status 401\b`), regexp.MustCompile(`(?i)token is expired|expired`), LoginExpired},
	{"codex", regexp.MustCompile(`^(stream error: )?unexpected status 401\b`), regexp.MustCompile(`(?i)sign(ing)? in again|log ?in again|authentication token`), AuthRejected},
}

// Classify says whether a transcript entry (its provider, kind and text) is a turn the provider
// refused because of the host's login.
func Classify(provider, kind, text string) (Reason, bool) {
	if kind != "assistant_message" && kind != "error" {
		return Reason{}, false
	}
	t := strings.TrimSpace(text)
	if t == "" || len(t) > maxLen {
		return Reason{}, false
	}
	for _, p := range patterns {
		if p.provider != provider || !p.start.MatchString(t) || (p.also != nil && !p.also.MatchString(t)) {
			continue
		}
		return Reason{Code: p.code, Summary: summary(p.code), Message: firstLine(t)}, true
	}
	return Reason{}, false
}

// MeterStatus says whether a login's usage read error (usage_sources.error) means its sign-in is
// gone, as cswap reports it: relogin_required once the refresh token is past its limit,
// no_credentials once Claude Code has wiped it. token_expired is cswap deferring a refresh (it
// retries on its own) and unavailable a failed fetch: neither says the login is broken.
func MeterStatus(provider, readError string) (Reason, bool) {
	if provider != "claude" {
		return Reason{}, false
	}
	if readError == "claude auth status: not logged in" {
		return Reason{Code: LoggedOut, Summary: summary(LoggedOut), Message: "claude auth status: this host is not signed in to Claude"}, true
	}
	status, ok := strings.CutPrefix(readError, "cswap usage status: ")
	if !ok {
		return Reason{}, false
	}
	switch status {
	case "relogin_required":
		return Reason{Code: LoginExpired, Summary: summary(LoginExpired), Message: "cswap: this login needs a new sign-in (relogin_required)"}, true
	case "no_credentials":
		return Reason{Code: LoggedOut, Summary: summary(LoggedOut), Message: "cswap: this login has no credentials (no_credentials)"}, true
	}
	return Reason{}, false
}

func summary(code string) string {
	switch code {
	case LoginExpired:
		return "the login reached its time limit or could not be renewed"
	case LoggedOut:
		return "the host is not signed in to this login"
	}
	return "the provider refused the login"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) <= 300 {
		return s
	}
	for len(s) > 297 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return strings.TrimRight(s, " ") + "…"
}
