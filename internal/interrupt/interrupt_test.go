package interrupt

import (
	"strings"
	"testing"
)

func TestClassifyRecognisesProviderLoginFailures(t *testing.T) {
	for _, c := range []struct {
		name, provider, kind, text, code string
	}{
		// What every Claude agent on the host said on 2026-11-19 when its login hit the 30-day limit.
		{"claude time limit", "claude", "assistant_message", "Failed to authenticate: this login reached its time limit and cannot be renewed. Run `claude auth login` to sign in again, or use `claude setup-token` and set CLAUDE_CODE_OAUTH_TOKEN for unattended jobs", LoginExpired},
		{"claude expired oauth", "claude", "assistant_message", `Failed to authenticate. API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired. Please obtain a new token or refresh your existing token."}}`, LoginExpired},
		{"claude api 401", "claude", "assistant_message", `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}} · Please run /login`, AuthRejected},
		{"claude invalid key", "claude", "assistant_message", "Invalid API key · Please run /login", LoggedOut},
		{"claude not logged in", "claude", "assistant_message", "Not logged in · Please run /login", LoggedOut},
		{"claude revoked", "claude", "assistant_message", "OAuth token revoked · Please run /login", LoggedOut},
		{"codex refresh", "codex", "error", "Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.", LoginExpired},
		{"codex 401", "codex", "assistant_message", "unexpected status 401 Unauthorized: Provided authentication token is expired. Please try signing in again.", LoginExpired},
		{"leading space", "claude", "assistant_message", "  \nNot logged in · Please run /login\n", LoggedOut},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, ok := Classify(c.provider, c.kind, c.text)
			if !ok || r.Code != c.code {
				t.Fatalf("Classify = %+v, %v; want %s", r, ok, c.code)
			}
			if r.Summary == "" || r.Message == "" {
				t.Fatalf("no summary or message: %+v", r)
			}
		})
	}
}

// Agents talk about 401s and logins all the time (every file-share check says "anonymous GET 401").
// Only the provider's own failure, as the whole message, counts.
func TestClassifyIgnoresAgentsTalkingAboutAuth(t *testing.T) {
	for _, c := range []struct{ provider, kind, text string }{
		{"claude", "assistant_message", "All 4 uploads are verified: SHA-256 matches the local files, and an anonymous GET returns 401. Now drafting the reply to Sarah."},
		{"claude", "assistant_message", "Access confirmed (anonymous visitors get a 401 \"Sign in with Hack Club\" page)."},
		{"claude", "assistant_message", "Failed to authenticate with GitHub: the gh token lacks repo scope. Trying the SSH remote instead."},
		{"claude", "assistant_message", "The agents got \"Failed to authenticate: this login reached its time limit\" so I renewed both logins."},
		{"claude", "user_message", "Failed to authenticate: this login reached its time limit and cannot be renewed. Run `claude auth login` to sign in again"},
		{"claude", "tool_call", "Not logged in · Please run /login"},
		{"claude", "reasoning", "API Error: 401 authentication_error, please run /login"},
		{"claude", "assistant_message", "Failed to authenticate: " + strings.Repeat("this is a very long message about logins and /login ", 40)},
		{"codex", "assistant_message", "The endpoint returned unexpected status 401 Unauthorized, so the cookie is stale."},
		{"muse", "assistant_message", "Not logged in · Please run /login"},
	} {
		if r, ok := Classify(c.provider, c.kind, c.text); ok {
			t.Errorf("Classify(%q, %q, %q) = %+v; want no match", c.provider, c.kind, c.text, r)
		}
	}
}

func TestMessageIsClippedToOneLine(t *testing.T) {
	r, ok := Classify("claude", "assistant_message", "Not logged in · Please run /login\n\nsecond line")
	if !ok {
		t.Fatal("no match")
	}
	if strings.Contains(r.Message, "\n") || len(r.Message) > 300 {
		t.Fatalf("message %q", r.Message)
	}
}

func TestFixSaysHowToSignInAgain(t *testing.T) {
	if f := Fix("claude", "zach@example.com"); !strings.Contains(f, "claude auth login") || !strings.Contains(f, "zach@example.com") {
		t.Fatal(f)
	}
	if f := Fix("codex", ""); !strings.Contains(f, "codex login") {
		t.Fatal(f)
	}
	if f := Fix("other", ""); f == "" {
		t.Fatal("no fix for an unknown provider")
	}
}

func TestMeterStatusSaysWhenALoginIsBroken(t *testing.T) {
	for status, want := range map[string]string{"relogin_required": LoginExpired, "no_credentials": LoggedOut} {
		r, ok := MeterStatus("claude", "cswap usage status: "+status)
		if !ok || r.Code != want {
			t.Fatalf("%s: %+v %v", status, r, ok)
		}
	}
	// token_expired is cswap deferring a refresh, retried on its own; a failed fetch says nothing
	for _, s := range []string{"cswap usage status: token_expired", "cswap usage status: unavailable", "", "codex meter: 503"} {
		if r, ok := MeterStatus("claude", s); ok {
			t.Fatalf("%q: %+v", s, r)
		}
	}
}
