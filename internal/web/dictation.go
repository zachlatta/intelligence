package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Dictation in the chat box: the browser streams the microphone to AssemblyAI's streaming
// speech-to-text and writes what comes back into the box. The browser never holds the server's
// key: POST /api/dictation/token mints a short-lived token for one streaming session with it.
// Nothing is stored, so the route has no state to read back through SQL.

// dictationModel is AssemblyAI's most accurate streaming model.
const dictationModel = "universal-3-6-pro"

const (
	assemblyAITokenURL = "https://token.assemblyai.com/v1/token"
	assemblyAIStream   = "wss://streaming.assemblyai.com/v3/ws"
	dictationTokenPath = "/api/dictation/token"
	// the token must be used to open a stream within this, and the stream lasts at most maxDictation
	dictationTokenTTL = 60
	maxDictation      = 30 * time.Minute
)

// dictationURL is the stream to open with the token; the browser adds the token and its own
// sample rate.
func dictationURL() string {
	q := url.Values{"speech_model": {dictationModel}, "encoding": {"pcm_s16le"}}
	return assemblyAIStream + "?" + q.Encode()
}

func dictationToken(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.AssemblyAIKey == "" {
			http.Error(w, "dictation is off: the server has no ASSEMBLYAI_API_KEY", http.StatusServiceUnavailable)
			return
		}
		body, _ := json.Marshal(map[string]any{"product": "streaming", "expires_in_seconds": dictationTokenTTL,
			"max_session_duration_seconds": int(maxDictation / time.Second)})
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cfg.AssemblyAITokenURL, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		req.Header.Set("Authorization", cfg.AssemblyAIKey)
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 10 * time.Second}
		res, err := client.Do(req)
		if err != nil {
			http.Error(w, "AssemblyAI: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		var got struct {
			Token string `json:"token"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &got)
		if res.StatusCode != http.StatusOK || got.Token == "" {
			reason := got.Error
			if reason == "" {
				reason = strings.TrimSpace(string(raw))
			}
			http.Error(w, fmt.Sprintf("AssemblyAI refused a token (%d): %s", res.StatusCode, reason), http.StatusBadGateway)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"token": got.Token, "url": dictationURL(), "expiresIn": dictationTokenTTL})
	}
}

// dictationPath is where the chat box gets its tokens, or "" when dictation is off.
func dictationPath(cfg Config) string {
	if cfg.AssemblyAIKey == "" {
		return ""
	}
	return dictationTokenPath
}
