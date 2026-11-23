package web

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/webpush"
)

func TestPWAManifestServiceWorkerAndIcons(t *testing.T) {
	h, _ := dbServer(t)

	// Manifest is open (no sign-in needed) and valid JSON with standalone display and icons.
	rec := get(t, h, "/manifest.webmanifest", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("manifest Content-Type = %q", ct)
	}
	var m struct {
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		StartURL        string `json:"start_url"`
		Scope           string `json:"scope"`
		Display         string `json:"display"`
		ThemeColor      string `json:"theme_color"`
		BackgroundColor string `json:"background_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.Name != "Intelligence" || m.ShortName != "Intelligence" || m.StartURL != "/" || m.Scope != "/" || m.Display != "standalone" {
		t.Fatalf("manifest fields: %+v", m)
	}
	if m.ThemeColor == "" || m.BackgroundColor == "" {
		t.Fatalf("manifest missing colors: %+v", m)
	}
	var has192, has512, hasMaskable bool
	for _, ic := range m.Icons {
		if ic.Sizes == "192x192" && ic.Purpose == "any" {
			has192 = true
		}
		if ic.Sizes == "512x512" && ic.Purpose == "any" {
			has512 = true
		}
		if strings.Contains(ic.Purpose, "maskable") {
			hasMaskable = true
		}
		r := get(t, h, ic.Src, nil)
		if r.Code != http.StatusOK || !strings.HasPrefix(r.Header().Get("Content-Type"), ic.Type) {
			t.Errorf("icon %s: %d %q", ic.Src, r.Code, r.Header().Get("Content-Type"))
		}
	}
	if !has192 || !has512 || !hasMaskable {
		t.Errorf("manifest icons missing required sizes/maskable: 192=%v 512=%v maskable=%v", has192, has512, hasMaskable)
	}

	// Service worker served at root scope with JS Content-Type and Service-Worker-Allowed.
	sw := get(t, h, "/sw.js", nil)
	if sw.Code != http.StatusOK || !strings.Contains(sw.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("sw.js: %d %q", sw.Code, sw.Header().Get("Content-Type"))
	}
	if sw.Header().Get("Service-Worker-Allowed") != "/" {
		t.Errorf("Service-Worker-Allowed = %q, want /", sw.Header().Get("Service-Worker-Allowed"))
	}
	for _, want := range []string{"addEventListener(\"push\"", "showNotification", "notificationclick"} {
		if !strings.Contains(sw.Body.String(), want) {
			t.Errorf("sw.js misses %q", want)
		}
	}

	// Layout links manifest, theme-color, apple-touch-icon and pwa.js.
	login := get(t, h, "/login", nil)
	for _, want := range []string{
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<meta name="theme-color"`,
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
		`<script src="/pwa.js" defer></script>`,
	} {
		if !strings.Contains(login.Body.String(), want) {
			t.Errorf("/login misses %q", want)
		}
	}
}

func TestPushSubscriptionsDegradeGracefullyWhenVAPIDUnset(t *testing.T) {
	h, _ := dbServer(t)
	c := signInAs(t, h)

	// Account page renders cleanly with notifications disabled when VAPID is unset.
	acc := call(t, h, http.MethodGet, "/account", "", nil, c)
	if acc.Code != http.StatusOK {
		t.Fatalf("/account: %d", acc.Code)
	}
	if !strings.Contains(acc.Body.String(), "Push notifications are not configured") {
		t.Errorf("/account should say push notifications are not configured when VAPID is unset")
	}

	// API returns 503 when VAPID is unset.
	rec := call(t, h, http.MethodPost, "/api/push-subscriptions",
		`{"endpoint":"https://fcm.googleapis.com/fcm/send/x","keys":{"p256dh":"a","auth":"b"}}`,
		map[string]string{"Content-Type": "application/json"}, c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/push-subscriptions without VAPID = %d, want 503", rec.Code)
	}
	rec = call(t, h, http.MethodPost, "/api/push-subscriptions/test", `{}`,
		map[string]string{"Content-Type": "application/json"}, c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/push-subscriptions/test without VAPID = %d, want 503", rec.Code)
	}
}

type fakePushRecorder struct {
	mu   sync.Mutex
	msgs []webpush.Message
}

func (f *fakePushRecorder) SendAccount(_ context.Context, _ int64, msg webpush.Message) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, msg)
	return 1, 0, nil
}

func signUpTestAccount(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := call(t, h, http.MethodPost, "/api/accounts", `{"username":"alice","password":"password-twelve-chars"}`,
		map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("sign up: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("sign up returned no session cookie")
	return nil
}

func TestPushSubscriptionsAPIAndTestNotification(t *testing.T) {
	pubB64, privB64, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	vapid, err := webpush.ParseVAPID(pubB64, privB64, "mailto:alerts@example.com")
	if err != nil {
		t.Fatal(err)
	}
	var hits int
	pushSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushSrv.Close()

	d := dbtest.New(t)
	h := New(Config{
		Context:  t.Context(),
		Version:  "test",
		DB:       d.Owner,
		Readonly: d.Readonly,
		VAPID:    vapid,
		PushHTTP: pushSrv.Client(),
	})
	c := signUpTestAccount(t, h)

	// Account page includes the VAPID public key for PushManager.subscribe.
	acc := call(t, h, http.MethodGet, "/account", "", nil, c)
	if acc.Code != http.StatusOK || !strings.Contains(acc.Body.String(), pubB64) {
		t.Fatalf("/account missing VAPID public key (status %d)", acc.Code)
	}

	uaPriv, err := ecdh.P256().GenerateKey(bytes.NewReader(bytes.Repeat([]byte{11}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"endpoint": pushSrv.URL + "/push/dev1",
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(uaPriv.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{12}, 16)),
		},
	})
	rec := call(t, h, http.MethodPost, "/api/push-subscriptions", string(body),
		map[string]string{"Content-Type": "application/json", "User-Agent": "Android Chrome"}, c)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/push-subscriptions: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("created = %+v, err=%v", created, err)
	}

	// Readable through /api/sql without exposing the endpoint URL.
	sqlRec := call(t, h, http.MethodPost, "/api/sql",
		`{"query":"SELECT id, endpoint_host, user_agent FROM push_subscriptions"}`,
		map[string]string{"Content-Type": "application/json"}, c)
	if sqlRec.Code != http.StatusOK || !strings.Contains(sqlRec.Body.String(), "Android Chrome") {
		t.Fatalf("/api/sql: %d %s", sqlRec.Code, sqlRec.Body.String())
	}

	// Test notification endpoint delivers to the subscribed device.
	testRec := call(t, h, http.MethodPost, "/api/push-subscriptions/test", `{}`,
		map[string]string{"Content-Type": "application/json"}, c)
	if testRec.Code != http.StatusOK || hits != 1 {
		t.Fatalf("POST /api/push-subscriptions/test: %d %s (hits=%d)", testRec.Code, testRec.Body.String(), hits)
	}

	// Delete by ID removes the subscription.
	delRec := call(t, h, http.MethodDelete, "/api/push-subscriptions/"+created.ID, "", nil, c)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/push-subscriptions/{id}: %d %s", delRec.Code, delRec.Body.String())
	}
	delAgain := call(t, h, http.MethodDelete, "/api/push-subscriptions/"+created.ID, "", nil, c)
	if delAgain.Code != http.StatusNotFound {
		t.Fatalf("DELETE again: %d, want 404", delAgain.Code)
	}
}

func enrollPushTestHost(t *testing.T, h http.Handler, c *http.Cookie, host string) string {
	t.Helper()
	rec := call(t, h, http.MethodPost, "/api/host-enrollments", `{"host":"`+host+`"}`,
		map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("enroll %s: %d %s", host, rec.Code, rec.Body.String())
	}
	var e struct{ ID, Code, Secret string }
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	claim := call(t, h, http.MethodPost, "/api/host-enrollments/claim", `{"code":"`+e.Code+`"}`,
		map[string]string{"Content-Type": "application/json"}, c)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim %s: %d %s", host, claim.Code, claim.Body.String())
	}
	keyRec := call(t, h, http.MethodPost, "/api/host-enrollments/"+e.ID+"/key", "",
		map[string]string{"Authorization": "Bearer " + e.Secret})
	if keyRec.Code != http.StatusOK {
		t.Fatalf("collect key %s: %d %s", host, keyRec.Code, keyRec.Body.String())
	}
	var k struct{ Key string }
	if err := json.Unmarshal(keyRec.Body.Bytes(), &k); err != nil || k.Key == "" {
		t.Fatalf("key %s: %+v err=%v", host, k, err)
	}
	return k.Key
}

func TestWorkflowAskAndFailSendWebPushNotifications(t *testing.T) {
	pubB64, privB64, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	vapid, err := webpush.ParseVAPID(pubB64, privB64, "mailto:alerts@example.com")
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var pushCount int
	pushSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pushCount++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushSrv.Close()

	d := dbtest.New(t)
	h := New(Config{
		Context:  t.Context(),
		Version:  "test",
		DB:       d.Owner,
		Readonly: d.Readonly,
		VAPID:    vapid,
		PushHTTP: pushSrv.Client(),
	})
	c := signUpTestAccount(t, h)
	key := enrollPushTestHost(t, h, c, "host-1")
	if _, err := d.Owner.Exec(context.Background(), `INSERT INTO hosts (name, last_seen, connected) VALUES ('host-1', now(), true)`); err != nil {
		t.Fatal(err)
	}

	// Subscribe one device for this account.
	uaPriv, err := ecdh.P256().GenerateKey(bytes.NewReader(bytes.Repeat([]byte{21}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	subBody, _ := json.Marshal(map[string]any{
		"endpoint": pushSrv.URL + "/push/android",
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(uaPriv.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{22}, 16)),
		},
	})
	if rec := call(t, h, http.MethodPost, "/api/push-subscriptions", string(subBody),
		map[string]string{"Content-Type": "application/json"}, c); rec.Code != http.StatusCreated {
		t.Fatalf("subscribe: %d %s", rec.Code, rec.Body.String())
	}

	// Create a simple workflow and start a run, then mark it running so the orchestrator can ask & fail.
	wfBody := `{"name":"Ship","description":"d","definition":{"host":"host-1","trigger":"task","project":{"mode":"fixed","directory":"/w/app"},"orchestrator":{"provider":"claude","model":"claude-opus-5-5","thinking":"medium","instructions":"orchestrate"},"steps":[{"id":"s1","title":"Ship","kind":"signoff","instructions":"ask"}]}}`
	wfRec := call(t, h, http.MethodPost, "/api/workflows", wfBody, map[string]string{"Content-Type": "application/json"}, c)
	if wfRec.Code != http.StatusCreated {
		t.Fatalf("create workflow: %d %s", wfRec.Code, wfRec.Body.String())
	}
	var wf struct{ ID string }
	_ = json.Unmarshal(wfRec.Body.Bytes(), &wf)

	runRec := call(t, h, http.MethodPost, "/api/workflow-runs", `{"workflowId":"`+wf.ID+`","input":"Deploy release"}`,
		map[string]string{"Content-Type": "application/json"}, c)
	if runRec.Code != http.StatusCreated {
		t.Fatalf("start run: %d %s", runRec.Code, runRec.Body.String())
	}
	var runOut struct{ ID string }
	_ = json.Unmarshal(runRec.Body.Bytes(), &runOut)

	// Move run into running state directly so we can exercise ask and fail moves.
	if _, err := d.Owner.Exec(context.Background(), `UPDATE workflow_runs SET status='running', step=0, orchestrator_id='ag-1', workspace_id='ws-1' WHERE id=$1`, runOut.ID); err != nil {
		t.Fatal(err)
	}

	// 1. Ask move triggers a Web Push notification.
	askRec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runOut.ID+"/moves",
		`{"move":"ask","note":"Ready to deploy?"}`,
		map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + key})
	if askRec.Code != http.StatusOK {
		t.Fatalf("ask move: %d %s", askRec.Code, askRec.Body.String())
	}
	mu.Lock()
	afterAsk := pushCount
	mu.Unlock()
	if afterAsk != 1 {
		t.Fatalf("pushCount after ask = %d, want 1", afterAsk)
	}

	// 2. Reply to resume, then fail move triggers a second Web Push notification.
	if _, err := d.Owner.Exec(context.Background(), `UPDATE workflow_runs SET status='running' WHERE id=$1`, runOut.ID); err != nil {
		t.Fatal(err)
	}
	failRec := call(t, h, http.MethodPost, "/api/workflow-runs/"+runOut.ID+"/moves",
		`{"move":"fail","why":"build broke"}`,
		map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + key})
	if failRec.Code != http.StatusOK {
		t.Fatalf("fail move: %d %s", failRec.Code, failRec.Body.String())
	}
	mu.Lock()
	afterFail := pushCount
	mu.Unlock()
	if afterFail != 2 {
		t.Fatalf("pushCount after fail = %d, want 2", afterFail)
	}
}
