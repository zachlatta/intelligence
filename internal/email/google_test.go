package email

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) *Keyring {
	t.Helper()
	k, err := NewKeyring(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestATokenIsSealedSoOnlyTheKeyOpensIt(t *testing.T) {
	k := testKey(t)
	sealed, err := k.Seal("a refresh token")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("refresh")) {
		t.Error("the sealed token shows the token")
	}
	if got, err := k.Open(sealed); err != nil || got != "a refresh token" {
		t.Errorf("Open = %q, %v", got, err)
	}
	other, _ := NewKeyring(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	if _, err := other.Open(sealed); err == nil {
		t.Error("another key opened it")
	}
	for _, bad := range []string{"", "short", base64.StdEncoding.EncodeToString([]byte("sixteen bytes!!!"))} {
		if _, err := NewKeyring(bad); err == nil {
			t.Errorf("key %q: no error", bad)
		}
	}
}

func TestTheSignInStateNamesItsAccountAndExpires(t *testing.T) {
	k := testKey(t)
	now := time.Unix(1762632000, 0)
	state := k.State(42, now)
	if acct, err := k.CheckState(state, now.Add(5*time.Minute)); err != nil || acct != 42 {
		t.Errorf("CheckState = %d, %v", acct, err)
	}
	if _, err := k.CheckState(state, now.Add(time.Hour)); err == nil {
		t.Error("an hour-old state was accepted")
	}
	tampered := strings.Replace(state, "42.", "43.", 1)
	if _, err := k.CheckState(tampered, now); err == nil {
		t.Error("a state for another account was accepted")
	}
	if v := k.Verifier(state); len(v) < 43 || v == k.Verifier(k.State(42, now.Add(time.Second))) {
		t.Errorf("verifier %q: too short, or the same for another state", v)
	}
}

func TestAGoogleClientIsReadFromItsJSON(t *testing.T) {
	installed := `{"installed":{"client_id":"id-1","client_secret":"s-1","auth_uri":"https://accounts.example.com/auth","token_uri":"https://oauth.example.com/token"}}`
	for _, raw := range []string{installed, base64.StdEncoding.EncodeToString([]byte(installed))} {
		g, err := ParseClient(raw)
		if err != nil {
			t.Fatal(err)
		}
		if g.ClientID != "id-1" || g.ClientSecret != "s-1" || !g.Installed || g.AuthURL != "https://accounts.example.com/auth" ||
			g.TokenURL != "https://oauth.example.com/token" || g.APIURL != "https://gmail.googleapis.com" {
			t.Errorf("client = %+v", g)
		}
	}
	g, err := ParseClient(`{"web":{"client_id":"id-2","client_secret":"s-2"}}`)
	if err != nil || g.Installed || g.AuthURL != "https://accounts.google.com/o/oauth2/v2/auth" {
		t.Errorf("web client = %+v, %v", g, err)
	}
	if _, err := ParseClient(`{"other":{}}`); err == nil {
		t.Error("no client: no error")
	}
}

func TestTheSignInLinkAsksOnlyToReadMail(t *testing.T) {
	g := &Google{ClientID: "id-1", AuthURL: "https://accounts.example.com/auth"}
	u, err := url.Parse(g.SignInURL("st", "challenge", "https://app.example.com/gmail/callback"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("scope") != "https://www.googleapis.com/auth/gmail.readonly" || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != "challenge" || q.Get("state") != "st" ||
		q.Get("redirect_uri") != "https://app.example.com/gmail/callback" || q.Get("client_id") != "id-1" {
		t.Errorf("sign-in URL = %s", u)
	}
	if g.Redirect("https://app.example.com") != "https://app.example.com/gmail/callback" {
		t.Errorf("web redirect = %s", g.Redirect("https://app.example.com"))
	}
	g.Installed = true
	if g.Redirect("https://app.example.com") != "http://localhost" {
		t.Errorf("installed redirect = %s", g.Redirect("https://app.example.com"))
	}
}
