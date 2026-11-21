package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/accounts"
)

func TestSessionOnCrossSiteNavigation(t *testing.T) {
	server, _ := cmdServer(t)
	h := anon(server)
	c := signInAs(t, h)
	if c.MaxAge < int((accounts.SessionLife-time.Minute).Seconds()) || c.Expires.Before(time.Now().Add(accounts.SessionLife-time.Minute)) {
		t.Fatal("session cookie should persist for 30 days")
	}
	if !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" {
		t.Fatal("session cookie should be HTTPS-only, HttpOnly and scoped to this host")
	}

	crossSite := map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Mode": "navigate",
		"Sec-Fetch-Dest": "document",
	}
	for _, cookies := range [][]*http.Cookie{nil, {{Name: sessionCookie, Value: "unknown"}}} {
		res := call(t, h, http.MethodGet, "/account", "", crossSite, cookies...)
		if res.Code != http.StatusSeeOther || !strings.HasPrefix(res.Header().Get("Location"), "/login?next=") {
			t.Fatalf("without a valid session: %d %s", res.Code, res.Header().Get("Location"))
		}
		if res.Header().Get("Set-Cookie") != "" {
			t.Fatal("the sign-in redirect replaced or cleared the browser's cookie")
		}
	}
	// net/http doesn't apply SameSite rules. Model which of our cookie policies
	// Chrome sends for a cross-site top-level GET.
	var cookies []*http.Cookie
	if c.SameSite == http.SameSiteLaxMode {
		cookies = append(cookies, c)
	}
	res := call(t, h, http.MethodGet, "/account", "", crossSite, cookies...)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "someone") {
		t.Fatalf("opening an external link with an active session: %d %s", res.Code, res.Header().Get("Location"))
	}
}

func TestCrossOriginSessionWritesAreRefused(t *testing.T) {
	server, _ := cmdServer(t)
	h := anon(server)
	c := signInAs(t, h)
	for _, headers := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Origin": "https://external.example"},
	} {
		// Attach the cookie explicitly, so the server check is exercised even
		// if the browser would withhold the cookie.
		for _, req := range []struct{ method, path, body string }{
			{http.MethodPost, "/api/session", `{"username":"someone","password":"` + testPassword + `"}`},
			{http.MethodPost, "/api/accounts", `{}`},
			{http.MethodPost, "/api/workflow-proposals/ignored/approve", `{}`},
			{http.MethodPut, "/api/account/password", `{}`},
			{http.MethodDelete, "/api/session", ""},
		} {
			res := call(t, h, req.method, req.path, req.body, headers, c)
			if res.Code != http.StatusForbidden || res.Header().Get("Set-Cookie") != "" {
				t.Errorf("%s %s with %v: %d, want 403 without Set-Cookie", req.method, req.path, headers, res.Code)
			}
		}
	}
	if res := call(t, h, http.MethodGet, "/account", "", nil, c); res.Code != http.StatusOK {
		t.Fatalf("session after refused requests: %d", res.Code)
	}
}

func TestSessionCreationRequiresJSON(t *testing.T) {
	server, _ := cmdServer(t)
	h := anon(server)
	for _, req := range []struct{ path, body string }{
		{"/api/session", `{"username":"someone","password":"` + testPassword + `"}`},
		{"/api/accounts", `{"username":"ann","password":"` + testPassword + `"}`},
	} {
		res := call(t, h, http.MethodPost, req.path, req.body, map[string]string{"Content-Type": "text/plain"})
		if res.Code != http.StatusUnsupportedMediaType || res.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s with a form-compatible content type: %d, want 415 without Set-Cookie", req.path, res.Code)
		}
	}
}

func TestExistingSessionCookieUpgradesWithoutExtendingSession(t *testing.T) {
	h, d := cmdServer(t)
	c := h.(*testServer).cookie
	expires := time.Now().Truncate(time.Second).Add(48 * time.Hour)
	if _, err := d.Owner.Exec(t.Context(), "UPDATE sessions SET expires_at = $1, created_from_path = ''", expires); err != nil {
		t.Fatal(err)
	}
	res := call(t, anon(h), http.MethodGet, "/account", "", map[string]string{"Sec-Fetch-Site": "none"}, c)
	if res.Code != http.StatusOK {
		t.Fatalf("existing session: %d", res.Code)
	}
	if res.Header().Get("Set-Cookie") == "" {
		t.Fatal("existing cookie was not upgraded")
	}
	upgraded := cookieOf(t, res)
	if upgraded.SameSite != http.SameSiteLaxMode || !upgraded.Expires.Equal(expires) || upgraded.Value != c.Value {
		t.Fatal("upgrade should keep the original token and expiry")
	}
	var storedExpiry time.Time
	if err := d.Owner.QueryRow(t.Context(), "SELECT expires_at FROM sessions").Scan(&storedExpiry); err != nil || !storedExpiry.Equal(expires) {
		t.Fatalf("stored expiry after upgrade: %v, %v", storedExpiry, err)
	}
}

func TestSessionCreationMetadataIsReadableViaSQL(t *testing.T) {
	server, _ := cmdServer(t)
	h := anon(server)
	const ua = "Mozilla/5.0 (Linux; Android 16; Mobile) Chrome"
	headers := map[string]string{"User-Agent": ua, "Sec-Fetch-Site": "same-origin"}
	const password = "a new test password"
	var cookies []*http.Cookie
	for _, req := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/api/accounts", `{"username":"ann","password":"` + testPassword + `"}`, http.StatusCreated},
		{http.MethodPost, "/api/session", `{"username":"ann","password":"` + testPassword + `"}`, http.StatusNoContent},
		{http.MethodPut, "/api/account/password", `{"current":"` + testPassword + `","password":"` + password + `"}`, http.StatusNoContent},
	} {
		res := call(t, h, req.method, req.path+"?ignored=private", req.body, headers, cookies...)
		if res.Code != req.status {
			t.Fatalf("%s %s: %d %s", req.method, req.path, res.Code, res.Body)
		}
		cookies = []*http.Cookie{cookieOf(t, res)}
		res = call(t, h, http.MethodPost, "/api/sql", q("SELECT user_agent, created_from_path, expires_at - created_at = interval '30 days' AS fixed_life FROM sessions ORDER BY created_at DESC LIMIT 1"), sameOriginFetch, cookies...)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"rows":[["`+ua+`","`+req.path+`",true]]`) {
			t.Errorf("session metadata for %s: %d %s", req.path, res.Code, res.Body)
		}
	}
}
