package email

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Keyring seals what must not be readable through SQL (each mailbox's Google refresh token) and
// signs the Google sign-in's state, with the server's CONNECTIONS_KEY: 32 random bytes, base64.
type Keyring struct {
	aead          cipher.AEAD
	signing, pkce []byte
}

// NewKeyring reads a base64 key of 32 bytes.
func NewKeyring(b64 string) (*Keyring, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(key) != 32 {
		return nil, errors.New("CONNECTIONS_KEY must be 32 random bytes in base64 (openssl rand -base64 32)")
	}
	derive := func(label string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(label))
		return m.Sum(nil)
	}
	block, err := aes.NewCipher(derive("seal"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keyring{aead: aead, signing: derive("state"), pkce: derive("pkce")}, nil
}

// Seal encrypts s.
func (k *Keyring) Seal(s string) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, []byte(s), nil), nil
}

// Open decrypts what Seal made.
func (k *Keyring) Open(b []byte) (string, error) {
	n := k.aead.NonceSize()
	if len(b) < n {
		return "", errors.New("sealed value too short")
	}
	plain, err := k.aead.Open(nil, b[:n], b[n:], nil)
	if err != nil {
		return "", errors.New("cannot open the sealed token: CONNECTIONS_KEY changed? Connect the mailbox again")
	}
	return string(plain), nil
}

func (k *Keyring) mac(key []byte, s string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// stateTTL is how long a Google sign-in may take.
const stateTTL = 30 * time.Minute

// State is the Google sign-in's state for account, started at now: account.unix.nonce.mac.
func (k *Keyring) State(account int64, now time.Time) string {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	s := fmt.Sprintf("%d.%d.%s", account, now.Unix(), base64.RawURLEncoding.EncodeToString(nonce))
	return s + "." + k.mac(k.signing, s)
}

// CheckState is the account a state names, if this server signed it in the last half hour.
func (k *Keyring) CheckState(state string, now time.Time) (int64, error) {
	i := strings.LastIndex(state, ".")
	if i < 0 || !hmac.Equal([]byte(k.mac(k.signing, state[:i])), []byte(state[i+1:])) {
		return 0, errors.New("the sign-in state is not this server's: start again")
	}
	parts := strings.Split(state[:i], ".")
	acct, err1 := strconv.ParseInt(parts[0], 10, 64)
	at, err2 := strconv.ParseInt(parts[1], 10, 64)
	if len(parts) != 3 || err1 != nil || err2 != nil {
		return 0, errors.New("bad sign-in state")
	}
	if now.Sub(time.Unix(at, 0)) > stateTTL {
		return 0, errors.New("the sign-in took over 30 minutes: start again")
	}
	return acct, nil
}

// Verifier is the PKCE code verifier for state: only this server can make it again.
func (k *Keyring) Verifier(state string) string { return k.mac(k.pkce, state) }

// Challenge is the S256 code challenge for verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Google is an OAuth client of Google's (GOOGLE_OAUTH_CLIENT_JSON) and Gmail's API. An installed
// (Desktop) client redirects to http://localhost, which does not load: its URL is pasted back. A
// web client redirects to the server's /gmail/callback.
type Google struct {
	ClientID, ClientSecret string
	Installed              bool
	AuthURL, TokenURL      string
	APIURL                 string // Gmail's API; a fake in tests
	HTTP                   *http.Client
}

// Scope is all a mailbox grants: reading mail.
const Scope = "https://www.googleapis.com/auth/gmail.readonly"

// ParseClient reads the OAuth client JSON Google Cloud gives, raw or in base64.
func ParseClient(raw string) (*Google, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
			raw = string(b)
		}
	}
	var f map[string]struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		AuthURI      string `json:"auth_uri"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil, fmt.Errorf("GOOGLE_OAUTH_CLIENT_JSON: %w", err)
	}
	g := &Google{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
		APIURL: "https://gmail.googleapis.com"}
	c, ok := f["installed"]
	g.Installed = ok
	if !ok {
		if c, ok = f["web"]; !ok {
			return nil, errors.New(`GOOGLE_OAUTH_CLIENT_JSON: want an "installed" or a "web" client`)
		}
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return nil, errors.New("GOOGLE_OAUTH_CLIENT_JSON: no client_id or client_secret")
	}
	g.ClientID, g.ClientSecret = c.ClientID, c.ClientSecret
	if c.AuthURI != "" {
		g.AuthURL = c.AuthURI
	}
	if c.TokenURI != "" {
		g.TokenURL = c.TokenURI
	}
	return g, nil
}

// Redirect is where Google sends the browser after sign-in, for the server at base.
func (g *Google) Redirect(base string) string {
	if g.Installed {
		return "http://localhost"
	}
	return strings.TrimRight(base, "/") + "/gmail/callback"
}

// SignInURL is Google's consent page, asking to read mail, offline (for a refresh token).
func (g *Google) SignInURL(state, challenge, redirect string) string {
	q := url.Values{"client_id": {g.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {Scope},
		"access_type": {"offline"}, "prompt": {"consent"}, "state": {state}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}}
	return g.AuthURL + "?" + q.Encode()
}

func (g *Google) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func (g *Google) token(ctx context.Context, form url.Values) (tokenResponse, error) {
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := g.client().Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer res.Body.Close()
	var t tokenResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&t); err != nil {
		return t, responseError(res, fmt.Sprintf("Google's token endpoint: %s", res.Status), false)
	}
	if res.StatusCode != http.StatusOK || t.AccessToken == "" {
		return t, responseError(res, fmt.Sprintf("Google refused: %s %s", t.Error, t.Description), false)
	}
	return t, nil
}

// Exchange trades the sign-in's code for a refresh token.
func (g *Google) Exchange(ctx context.Context, code, verifier, redirect string) (string, error) {
	t, err := g.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"redirect_uri": {redirect}})
	if err != nil {
		return "", err
	}
	if t.RefreshToken == "" {
		return "", errors.New("Google gave no refresh token: remove this app's access in the Google account's security settings, then connect again")
	}
	return t.RefreshToken, nil
}

// Mailbox is a Gmail mailbox, read with a refresh token.
func (g *Google) Mailbox(refresh string) *Mailbox { return &Mailbox{g: g, refresh: refresh} }

// Mailbox reads one Gmail mailbox, refreshing its access token as it expires.
type Mailbox struct {
	g       *Google
	refresh string
	mu      sync.Mutex
	access  string
	expiry  time.Time
}

// ErrHistoryGone: Gmail no longer has the history asked for (it keeps about a week).
var ErrHistoryGone = errors.New("Gmail no longer has history that old")

func (m *Mailbox) accessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.access != "" && time.Until(m.expiry) > time.Minute {
		return m.access, nil
	}
	t, err := m.g.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {m.refresh}})
	if err != nil {
		return "", err
	}
	m.access, m.expiry = t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn)*time.Second)
	return m.access, nil
}

func (m *Mailbox) get(ctx context.Context, path string, q url.Values, out any) error {
	if p, ok := ctx.Value(paceKey{}).(*pacer); ok {
		units := 1
		switch {
		case path == "history":
			units = 2
		case path == "messages":
			units = 5
		case strings.HasPrefix(path, "messages/"):
			units = 60
		}
		if err := p.Wait(ctx, units); err != nil {
			return err
		}
	}
	tok, err := m.accessToken(ctx)
	if err != nil {
		return err
	}
	u := strings.TrimRight(m.g.APIURL, "/") + "/gmail/v1/users/me/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := m.g.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound && path == "history" {
		return ErrHistoryGone
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		var detail struct {
			Error struct {
				Errors []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &detail)
		limited := false
		for _, e := range detail.Error.Errors {
			if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
				limited = true
			}
		}
		return responseError(res, fmt.Sprintf("Gmail %s: %s %s", path, res.Status, strings.TrimSpace(string(b))), limited)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(out)
}

// Profile is the mailbox's address and its latest history id.
func (m *Mailbox) Profile(ctx context.Context) (address, historyID string, err error) {
	var p struct {
		EmailAddress string `json:"emailAddress"`
		HistoryID    string `json:"historyId"`
	}
	err = m.get(ctx, "profile", nil, &p)
	return strings.ToLower(p.EmailAddress), p.HistoryID, err
}

// Search is the ids of up to max messages Gmail's search q finds, newest first.
func (m *Mailbox) Search(ctx context.Context, q string, max int) ([]string, error) {
	var ids []string
	page := ""
	for len(ids) < max {
		var r struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			NextPageToken string `json:"nextPageToken"`
		}
		v := url.Values{"q": {q}, "maxResults": {strconv.Itoa(min(100, max-len(ids)))}}
		if page != "" {
			v.Set("pageToken", page)
		}
		if err := m.get(ctx, "messages", v, &r); err != nil {
			return ids, err
		}
		for _, x := range r.Messages {
			ids = append(ids, x.ID)
		}
		if page = r.NextPageToken; page == "" {
			break
		}
	}
	return ids, nil
}

// Message is one message, in full.
func (m *Mailbox) Message(ctx context.Context, id string) (GmailMessage, error) {
	var g GmailMessage
	err := m.get(ctx, "messages/"+url.PathEscape(id), url.Values{"format": {"full"}}, &g)
	return g, err
}

// Added is the ids of the messages added to the mailbox since history id start, oldest first, and
// the history id they reach.
func (m *Mailbox) Added(ctx context.Context, start string) ([]string, string, error) {
	var ids []string
	seen := map[string]bool{}
	page, latest := "", start
	for {
		var r struct {
			History []struct {
				MessagesAdded []struct {
					Message struct {
						ID string `json:"id"`
					} `json:"message"`
				} `json:"messagesAdded"`
			} `json:"history"`
			HistoryID     string `json:"historyId"`
			NextPageToken string `json:"nextPageToken"`
		}
		v := url.Values{"startHistoryId": {start}, "historyTypes": {"messageAdded"}, "maxResults": {"500"}}
		if page != "" {
			v.Set("pageToken", page)
		}
		if err := m.get(ctx, "history", v, &r); err != nil {
			return nil, start, err
		}
		for _, h := range r.History {
			for _, a := range h.MessagesAdded {
				if !seen[a.Message.ID] {
					seen[a.Message.ID] = true
					ids = append(ids, a.Message.ID)
				}
			}
		}
		if r.HistoryID != "" {
			latest = r.HistoryID
		}
		if page = r.NextPageToken; page == "" {
			return ids, latest, nil
		}
	}
}
