// Package gmailfake is an in-memory Gmail for tests: Google's token endpoint (with PKCE), its
// consent step, and the Gmail API calls internal/email makes.
package gmailfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/email"
)

// Server is one mailbox, Address.
type Server struct {
	*httptest.Server
	Address string

	mu       sync.Mutex
	history  int
	oldest   int // history before this is gone
	messages []entry
	codes    map[string]consent // sign-in code -> what it was given for
	tokens   map[string]bool    // access tokens handed out
	Fail     bool               // every Gmail call fails
}

type entry struct {
	m    email.GmailMessage
	hist int
}

type consent struct{ challenge, redirect string }

// New starts a fake mailbox for address, stopped when t ends.
func New(t testing.TB, address string) *Server {
	s := &Server{Address: address, history: 1000, codes: map[string]consent{}, tokens: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", s.token)
	mux.HandleFunc("GET /gmail/v1/users/me/{rest...}", s.api)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Google is a web client of the fake.
func (s *Server) Google() *email.Google {
	return &email.Google{ClientID: "fake-client", ClientSecret: "fake-secret", AuthURL: s.URL + "/auth", TokenURL: s.URL + "/token",
		APIURL: s.URL, HTTP: s.Client()}
}

// Consent is Google's consent page, signed in as the mailbox: it answers a sign-in URL with where
// Google sends the browser, with the code and the state.
func (s *Server) Consent(signIn string) string {
	u, err := url.Parse(signIn)
	if err != nil {
		panic(err)
	}
	q := u.Query()
	s.mu.Lock()
	code := fmt.Sprintf("code-%d", len(s.codes)+1)
	s.codes[code] = consent{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	s.mu.Unlock()
	return q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
}

// Add delivers m to the mailbox, as the next history.
func (s *Server) Add(m email.GmailMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history++
	if m.InternalDate == "" {
		m.InternalDate = strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	s.messages = append(s.messages, entry{m: m, hist: s.history})
}

// Forget drops the history before now, as Gmail does after a week.
func (s *Server) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history += 10
	s.oldest = s.history
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Form.Get("client_id") != "fake-client" || r.Form.Get("client_secret") != "fake-secret" {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	access := fmt.Sprintf("access-%d", len(s.tokens)+1)
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		c, ok := s.codes[r.Form.Get("code")]
		delete(s.codes, r.Form.Get("code"))
		if !ok || email.Challenge(r.Form.Get("code_verifier")) != c.challenge || r.Form.Get("redirect_uri") != c.redirect {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_grant"})
			return
		}
		s.tokens[access] = true
		writeJSON(w, map[string]any{"access_token": access, "refresh_token": "refresh-for-" + s.Address, "expires_in": 3600})
	case "refresh_token":
		if r.Form.Get("refresh_token") != "refresh-for-"+s.Address {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_grant"})
			return
		}
		s.tokens[access] = true
		writeJSON(w, map[string]any{"access_token": access, "expires_in": 3600})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.Fail {
		http.Error(w, "backend error", http.StatusInternalServerError)
		return
	}
	rest := r.PathValue("rest")
	switch {
	case rest == "profile":
		writeJSON(w, map[string]string{"emailAddress": s.Address, "historyId": strconv.Itoa(s.history)})
	case rest == "messages": // newest first; the search itself is not modeled
		var list []map[string]string
		for i := len(s.messages) - 1; i >= 0; i-- {
			list = append(list, map[string]string{"id": s.messages[i].m.ID, "threadId": s.messages[i].m.ThreadID})
		}
		writeJSON(w, map[string]any{"messages": list})
	case strings.HasPrefix(rest, "messages/"):
		for _, e := range s.messages {
			if e.m.ID == strings.TrimPrefix(rest, "messages/") {
				writeJSON(w, e.m)
				return
			}
		}
		http.NotFound(w, r)
	case rest == "history":
		start, _ := strconv.Atoi(r.URL.Query().Get("startHistoryId"))
		if start < s.oldest {
			http.NotFound(w, r)
			return
		}
		var added []map[string]any
		for _, e := range s.messages {
			if e.hist > start {
				added = append(added, map[string]any{"messagesAdded": []map[string]any{{"message": map[string]string{"id": e.m.ID}}}})
			}
		}
		writeJSON(w, map[string]any{"history": added, "historyId": strconv.Itoa(s.history)})
	default:
		http.NotFound(w, r)
	}
}
