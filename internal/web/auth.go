package web

import (
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/fleet"
)

// sessionCookie holds a signed-in browser's session token (accounts.Store.StartSession).
const sessionCookie = "intelligence_session"

// viewer is who a request is from: a signed-in account, and what it reads through. Everything
// it reads goes through its own Postgres role, so it sees only its own rows.
type viewer struct {
	acct accounts.Account
	pool *pgxpool.Pool   // the account's read-only pool
	reg  *fleet.Registry // reads through pool
}

// hostCaller is shared by run moves and proposal-only CLI writes. A host key
// identifies its account but never authorizes approval or workflow submission.
func (a *app) hostCaller(w http.ResponseWriter, r *http.Request) (accounts.Account, string, bool) {
	key, _ := bearerOf(r)
	host, acct, err := a.store.HostIdentity(r.Context(), key)
	if err != nil {
		workflowResult(w, "", err)
		return accounts.Account{}, "", false
	}
	if host == "" {
		workflowMessage(w, http.StatusUnauthorized, "bad or missing host key")
		return accounts.Account{}, "", false
	}
	return acct, host, true
}

func (a *app) hostWrite(h func(accounts.Account, string) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		acct, host, ok := a.hostCaller(w, r)
		if ok {
			h(acct, host)(w, r)
		}
	}
}

// viewerOf is the account r is signed in as, or accounts.ErrNoSession.
func (a *app) viewerOf(r *http.Request) (viewer, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return viewer{}, accounts.ErrNoSession
	}
	acct, err := a.store.Session(r.Context(), c.Value)
	if err != nil {
		return viewer{}, err
	}
	p, err := a.store.Reader(r.Context(), acct)
	if err != nil {
		return viewer{}, err
	}
	return viewer{acct: acct, pool: p, reg: fleet.New(p)}, nil
}

// page serves h to a signed-in browser, and sends any other to sign in, then back.
func (a *app) page(h func(v viewer) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := a.viewerOf(r)
		if errors.Is(err, accounts.ErrNoSession) {
			next := r.URL.Path
			if r.URL.RawQuery != "" {
				next += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h(v)(w, r)
	}
}

// read serves h to a signed-in account, and refuses any other with 401.
func (a *app) read(h func(v viewer) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := a.signedIn(w, r)
		if ok {
			h(v)(w, r)
		}
	}
}

// write serves h to a signed-in account's same-origin JSON request (or bodiless one, for a
// DELETE), and refuses any other. JSON is required because another site cannot send it without
// the server's leave; the cookie is SameSite=Strict as well.
func (a *app) write(h func(v viewer) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		v, ok := a.signedIn(w, r)
		if !ok {
			return
		}
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" && (r.Method != http.MethodDelete || r.ContentLength > 0) {
			http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		h(v)(w, r)
	}
}

func (a *app) signedIn(w http.ResponseWriter, r *http.Request) (viewer, bool) {
	v, err := a.viewerOf(r)
	if errors.Is(err, accounts.ErrNoSession) {
		http.Error(w, "sign in first: POST /api/session", http.StatusUnauthorized)
		return v, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return v, false
	}
	return v, true
}

// sameOrigin refuses a browser request another site started. The cookie is SameSite=Strict as
// well; this also stops a sibling site (same-site, another origin). Sec-Fetch-Site, which the
// browser sets, decides when present: behind a proxy the Host seen here need not be the one the
// browser used. Without it, Origin (if any) must name this host.
func sameOrigin(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin" || s == "none"
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host != "" && (u.Host == r.Host || u.Host == r.Header.Get("X-Forwarded-Host"))
	}
	return true
}

func setSession(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

// decode reads a small JSON body into v, answering the request itself when it cannot.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// accountError answers with err: a person's mistake as 4xx, anything else as 500.
func accountError(w http.ResponseWriter, err error) {
	var bad *accounts.BadInputError
	switch {
	case errors.As(err, &bad):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, accounts.ErrBadCredentials), errors.Is(err, accounts.ErrNoSession):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, accounts.ErrTaken), errors.Is(err, accounts.ErrHostTaken):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, accounts.ErrNoSuchCode), errors.Is(err, accounts.ErrNoSuchEnrollment), errors.Is(err, accounts.ErrNoSuchHost):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		log.Printf("accounts: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// signUp is POST /api/accounts {"username", "password"}: it makes an account and signs the browser in.
func (a *app) signUp(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	var req struct{ Username, Password string }
	if !decode(w, r, &req) {
		return
	}
	acct, err := a.store.Create(r.Context(), req.Username, req.Password)
	if err != nil {
		accountError(w, err)
		return
	}
	tok, exp, err := a.store.StartSession(r.Context(), acct)
	if err != nil {
		accountError(w, err)
		return
	}
	setSession(w, tok, exp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(acct)
}

// signIn is POST /api/session {"username", "password"}.
func (a *app) signIn(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	var req struct{ Username, Password string }
	if !decode(w, r, &req) {
		return
	}
	acct, err := a.store.Authenticate(r.Context(), req.Username, req.Password)
	if err != nil {
		accountError(w, err)
		return
	}
	tok, exp, err := a.store.StartSession(r.Context(), acct)
	if err != nil {
		accountError(w, err)
		return
	}
	setSession(w, tok, exp)
	w.WriteHeader(http.StatusNoContent)
}

// signOut is DELETE /api/session.
func (a *app) signOut(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := a.store.EndSession(r.Context(), c.Value); err != nil {
			accountError(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// changePassword is PUT /api/account/password {"current", "password"}. Every session of the
// account ends; this browser gets a new one.
func (a *app) changePassword(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Current, Password string }
		if !decode(w, r, &req) {
			return
		}
		if err := a.store.ChangePassword(r.Context(), v.acct.ID, req.Current, req.Password); err != nil {
			accountError(w, err)
			return
		}
		tok, exp, err := a.store.StartSession(r.Context(), v.acct)
		if err != nil {
			accountError(w, err)
			return
		}
		setSession(w, tok, exp)
		w.WriteHeader(http.StatusNoContent)
	}
}
