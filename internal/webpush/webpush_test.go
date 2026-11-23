package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func b64url(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// TestEncryptRFC8291Vector checks our aes128gcm encryption against RFC 8291 Section 5's test vector
// (both intermediate values and full UA decryption with ua_private).
func TestEncryptRFC8291Vector(t *testing.T) {
	uaPrivBytes := b64url(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94")
	uaPubBytes := b64url(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	asPrivBytes := b64url(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	asPubBytes := b64url(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	authSecret := b64url(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := b64url(t, "DGv6ra1nlYgDCS1FRnbzlw")
	plaintext := []byte("When I grow up, I want to be a watermelon")

	asPriv, err := ecdh.P256().NewPrivateKey(asPrivBytes)
	if err != nil {
		t.Fatalf("asPriv: %v", err)
	}
	if !bytes.Equal(asPriv.PublicKey().Bytes(), asPubBytes) {
		t.Fatalf("asPub mismatch")
	}

	body, err := encryptWith(plaintext, uaPubBytes, authSecret, asPriv, salt)
	if err != nil {
		t.Fatalf("encryptWith: %v", err)
	}

	// RFC 8188 header: 16B salt + 4B rs (4096) + 1B idlen (65) + 65B as_public = 86 bytes.
	// Plaintext (41) + delimiter 0x02 (1) + GCM tag (16) = 58 bytes. Total = 144 bytes.
	if len(body) != 144 {
		t.Fatalf("body len = %d, want 144", len(body))
	}
	if !bytes.Equal(body[:16], salt) {
		t.Fatalf("salt mismatch")
	}
	if rs := binary.BigEndian.Uint32(body[16:20]); rs != 4096 {
		t.Fatalf("rs = %d, want 4096", rs)
	}
	if body[20] != 65 {
		t.Fatalf("idlen = %d, want 65", body[20])
	}
	if !bytes.Equal(body[21:86], asPubBytes) {
		t.Fatalf("header as_public mismatch")
	}

	// Verify intermediate RFC 8291 Section 5 values and decrypt as the User Agent.
	uaPriv, err := ecdh.P256().NewPrivateKey(uaPrivBytes)
	if err != nil {
		t.Fatalf("uaPriv: %v", err)
	}
	asPub, err := ecdh.P256().NewPublicKey(body[21:86])
	if err != nil {
		t.Fatalf("asPub: %v", err)
	}
	ecdhSecret, err := uaPriv.ECDH(asPub)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	if got, want := base64.RawURLEncoding.EncodeToString(ecdhSecret), "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs"; got != want {
		t.Fatalf("ecdh_secret = %s, want %s", got, want)
	}
	prkKey, err := hkdf.Extract(sha256.New, ecdhSecret, authSecret)
	if err != nil {
		t.Fatal(err)
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPubBytes...), asPubBytes...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := base64.RawURLEncoding.EncodeToString(ikm), "S4lYMb_L0FxCeq0WhDx813KgSYqU26kOyzWUdsXYyrg"; got != want {
		t.Fatalf("ikm = %s, want %s", got, want)
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		t.Fatal(err)
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := base64.RawURLEncoding.EncodeToString(cek), "oIhVW04MRdy2XN9CiKLxTg"; got != want {
		t.Fatalf("cek = %s, want %s", got, want)
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := gcm.Open(nil, nonce, body[86:], nil)
	if err != nil {
		t.Fatalf("UA decrypt failed: %v", err)
	}
	if len(dec) == 0 || dec[len(dec)-1] != 0x02 {
		t.Fatalf("missing final record delimiter 0x02: %x", dec)
	}
	if !bytes.Equal(dec[:len(dec)-1], plaintext) {
		t.Fatalf("decrypted = %q, want %q", dec[:len(dec)-1], plaintext)
	}
}

func TestVAPIDConfigAndJWTVerification(t *testing.T) {
	pubB64, privB64, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	cfg, err := ParseVAPID(pubB64, privB64, "mailto:alerts@example.com")
	if err != nil {
		t.Fatalf("ParseVAPID: %v", err)
	}
	if cfg.PublicKey != pubB64 {
		t.Fatalf("PublicKey = %q, want %q", cfg.PublicKey, pubB64)
	}

	now := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	hdr, err := cfg.AuthHeader("https://fcm.googleapis.com/fcm/send/abc123", now)
	if err != nil {
		t.Fatalf("AuthHeader: %v", err)
	}
	if !strings.HasPrefix(hdr, "vapid t=") || !strings.Contains(hdr, ", k="+pubB64) {
		t.Fatalf("bad AuthHeader: %q", hdr)
	}
	tokPart, _, _ := strings.Cut(strings.TrimPrefix(hdr, "vapid t="), ", k=")
	parts := strings.Split(tokPart, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(b64url(t, parts[1]), &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Aud != "https://fcm.googleapis.com" || claims.Sub != "mailto:alerts@example.com" || claims.Exp != now.Add(12*time.Hour).Unix() {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	sig := b64url(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("sig len = %d, want 64", len(sig))
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	pubBytes := b64url(t, pubB64)
	x, y := elliptic.Unmarshal(elliptic.P256(), pubBytes)
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Fatalf("ECDSA signature did not verify")
	}
}

func TestParseVAPIDRejectsBadInputs(t *testing.T) {
	pubB64, privB64, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	pub2, _, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name           string
		pub, priv, sub string
	}{
		{"mismatched pair", pub2, privB64, "mailto:a@example.com"},
		{"bad subject", pubB64, privB64, "http://example.com"},
		{"bad priv", pubB64, "not-a-key", "mailto:a@example.com"},
		{"bad pub", "not-a-key", privB64, "mailto:a@example.com"},
	}
	for _, tc := range cases {
		if _, err := ParseVAPID(tc.pub, tc.priv, tc.sub); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestSendPostsEncryptedPayloadAndDetectsExpired(t *testing.T) {
	pubB64, privB64, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseVAPID(pubB64, privB64, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}

	uaPriv, err := ecdh.P256().GenerateKey(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	authSecret := bytes.Repeat([]byte{9}, 16)

	var gotHeaders http.Header
	var gotBody []byte
	status := http.StatusCreated
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()

	sub := Subscription{
		Endpoint: srv.URL + "/push/123",
		P256dh:   base64.RawURLEncoding.EncodeToString(uaPriv.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(authSecret),
	}
	client := &Client{VAPID: cfg, HTTP: srv.Client()}
	msg := Message{Title: "Needs your reply", Body: "Approve deploy?", URL: "/workflow-runs/abc", Tag: "run-ask-abc"}

	if err := client.Send(context.Background(), sub, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotHeaders.Get("Content-Encoding") != "aes128gcm" {
		t.Errorf("Content-Encoding = %q", gotHeaders.Get("Content-Encoding"))
	}
	if !strings.HasPrefix(gotHeaders.Get("Authorization"), "vapid t=") {
		t.Errorf("Authorization = %q", gotHeaders.Get("Authorization"))
	}
	if gotHeaders.Get("TTL") == "" {
		t.Errorf("missing TTL header")
	}
	if len(gotBody) < 86 {
		t.Fatalf("encrypted body too short: %d", len(gotBody))
	}

	// 410 Gone and 404 Not Found must return ErrSubscriptionExpired.
	for _, code := range []int{http.StatusGone, http.StatusNotFound} {
		status = code
		err := client.Send(context.Background(), sub, msg)
		if !errors.Is(err, ErrSubscriptionExpired) {
			t.Errorf("status %d: err = %v, want ErrSubscriptionExpired", code, err)
		}
	}
}

func TestSealAndUnsealSubscription(t *testing.T) {
	pubB64, privB64, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseVAPID(pubB64, privB64, "mailto:alerts@example.com")
	if err != nil {
		t.Fatal(err)
	}
	sub := Subscription{
		Endpoint: "https://fcm.googleapis.com/fcm/send/secret-token-123",
		P256dh:   pubB64,
		Auth:     base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 16)),
	}
	sealed, err := cfg.SealSubscription(sub)
	if err != nil {
		t.Fatalf("SealSubscription: %v", err)
	}
	if bytes.Contains(sealed, []byte("secret-token-123")) {
		t.Fatalf("sealed bytes leaked endpoint in plaintext")
	}
	got, err := cfg.UnsealSubscription(sealed)
	if err != nil {
		t.Fatalf("UnsealSubscription: %v", err)
	}
	if got != sub {
		t.Fatalf("unsealed = %+v, want %+v", got, sub)
	}
}
