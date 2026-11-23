// Package webpush implements standard Web Push notifications (RFC 8291 aes128gcm payload
// encryption and RFC 8292 VAPID authentication) using the Go standard library.
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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrSubscriptionExpired is returned when the push service answers 404 or 410,
// meaning the browser subscription is no longer valid and should be deleted.
var ErrSubscriptionExpired = errors.New("push subscription expired")

// ErrBadSubscription is returned when a browser subscription payload fails validation.
var ErrBadSubscription = errors.New("invalid push subscription")

// Message is one push notification sent to a subscribed device's service worker.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

// Sender delivers push notifications to all of an account's subscribed devices.
type Sender interface {
	SendAccount(ctx context.Context, accountID int64, msg Message) (sent, expired int, err error)
}

// Subscription is a browser PushSubscription's endpoint and encryption keys.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

// Validate checks that s has an HTTPS endpoint and valid P-256 / auth keys.
func (s Subscription) Validate() (endpointHost string, uaPub, authSecret []byte, err error) {
	ep := strings.TrimSpace(s.Endpoint)
	if ep == "" || len(ep) > 2048 {
		return "", nil, nil, fmt.Errorf("%w: endpoint required (max 2048 bytes)", ErrBadSubscription)
	}
	u, err := url.Parse(ep)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", nil, nil, fmt.Errorf("%w: endpoint must be an https URL", ErrBadSubscription)
	}
	uaPub, err = decodeBase64URL(s.P256dh)
	if err != nil || len(uaPub) != 65 || uaPub[0] != 0x04 {
		return "", nil, nil, fmt.Errorf("%w: p256dh must be a 65-byte uncompressed P-256 public key", ErrBadSubscription)
	}
	if _, err := ecdh.P256().NewPublicKey(uaPub); err != nil {
		return "", nil, nil, fmt.Errorf("%w: p256dh is not on P-256", ErrBadSubscription)
	}
	authSecret, err = decodeBase64URL(s.Auth)
	if err != nil || len(authSecret) != 16 {
		return "", nil, nil, fmt.Errorf("%w: auth must be 16 bytes", ErrBadSubscription)
	}
	return u.Hostname(), uaPub, authSecret, nil
}

// EndpointHash returns the SHA-256 hex digest of an endpoint URL for indexing without exposing it.
func EndpointHash(endpoint string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(endpoint)))
	return hex.EncodeToString(sum[:])
}

// VAPID holds a parsed P-256 VAPID key pair and subject URI (RFC 8292).
type VAPID struct {
	PublicKey string // unpadded base64url 65-byte uncompressed P-256 point
	Subject   string // mailto:... or https://...
	priv      *ecdsa.PrivateKey
	sealKey   []byte // 32-byte AES-256-GCM key derived from priv for sealing stored subscriptions
}

// GenerateVAPIDKeys creates a fresh P-256 VAPID key pair encoded as unpadded base64url.
func GenerateVAPIDKeys() (publicKey, privateKey string, err error) {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(k.Bytes()), nil
}

// ParseVAPID parses and validates VAPID_PUBLIC_KEY, VAPID_PRIVATE_KEY and VAPID_SUBJECT.
func ParseVAPID(publicKey, privateKey, subject string) (*VAPID, error) {
	publicKey = strings.TrimSpace(publicKey)
	privateKey = strings.TrimSpace(privateKey)
	subject = strings.TrimSpace(subject)
	if !strings.HasPrefix(subject, "mailto:") && !strings.HasPrefix(subject, "https://") {
		return nil, errors.New("VAPID_SUBJECT must start with mailto: or https://")
	}
	privBytes, err := decodeBase64URL(privateKey)
	if err != nil || len(privBytes) != 32 {
		return nil, errors.New("VAPID_PRIVATE_KEY must be a 32-byte base64url P-256 scalar")
	}
	ecdhPriv, err := ecdh.P256().NewPrivateKey(privBytes)
	if err != nil {
		return nil, fmt.Errorf("VAPID_PRIVATE_KEY: %w", err)
	}
	pubBytes, err := decodeBase64URL(publicKey)
	if err != nil || len(pubBytes) != 65 || pubBytes[0] != 0x04 {
		return nil, errors.New("VAPID_PUBLIC_KEY must be a 65-byte base64url uncompressed P-256 public key")
	}
	if !bytes.Equal(ecdhPriv.PublicKey().Bytes(), pubBytes) {
		return nil, errors.New("VAPID_PUBLIC_KEY does not match VAPID_PRIVATE_KEY")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), pubBytes)
	if x == nil {
		return nil, errors.New("VAPID_PUBLIC_KEY is not a valid P-256 point")
	}
	ecdsaPriv := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y},
		D:         new(big.Int).SetBytes(privBytes),
	}
	sealKey, err := hkdf.Key(sha256.New, privBytes, nil, "intelligence-push-subscription-seal-v1", 32)
	if err != nil {
		return nil, err
	}
	return &VAPID{
		PublicKey: base64.RawURLEncoding.EncodeToString(pubBytes),
		Subject:   subject,
		priv:      ecdsaPriv,
		sealKey:   sealKey,
	}, nil
}

// SealSubscription encrypts s with AES-256-GCM using a key derived from the VAPID private key,
// so storing it in a public Postgres table exposes no endpoint URL or push keys to /api/sql.
func (v *VAPID) SealSubscription(s Subscription) ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(v.sealKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, raw, nil), nil
}

// UnsealSubscription decrypts a subscription sealed by SealSubscription.
func (v *VAPID) UnsealSubscription(sealed []byte) (Subscription, error) {
	var s Subscription
	block, err := aes.NewCipher(v.sealKey)
	if err != nil {
		return s, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return s, err
	}
	if len(sealed) < gcm.NonceSize() {
		return s, errors.New("sealed subscription is too short")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(plain, &s)
	return s, err
}

// AuthHeader returns the RFC 8292 Authorization header ("vapid t=<jwt>, k=<pub>") for endpoint.
func (v *VAPID) AuthHeader(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid push endpoint")
	}
	aud := u.Scheme + "://" + u.Host
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claimsJSON, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{
		Aud: aud,
		Exp: now.Add(12 * time.Hour).Unix(),
		Sub: v.Subject,
	})
	if err != nil {
		return "", err
	}
	signingInput := hdr + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, v.priv, digest[:])
	if err != nil {
		return "", err
	}
	var sig [64]byte
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	jwt := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig[:])
	return "vapid t=" + jwt + ", k=" + v.PublicKey, nil
}

// encrypt encrypts plaintext for a push subscription using RFC 8291 (aes128gcm).
func encrypt(plaintext, uaPublic, authSecret []byte) ([]byte, error) {
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return encryptWith(plaintext, uaPublic, authSecret, asPriv, salt)
}

// encryptWith performs RFC 8291 aes128gcm encryption with an explicit ephemeral key and salt
// so tests can verify the RFC 8291 Section 5 test vector deterministically.
func encryptWith(plaintext, uaPublic, authSecret []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(salt) != 16 {
		return nil, errors.New("salt must be 16 bytes")
	}
	if len(authSecret) != 16 {
		return nil, errors.New("auth secret must be 16 bytes")
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("invalid ua public key: %w", err)
	}
	ecdhSecret, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPublic := asPriv.PublicKey().Bytes()

	// RFC 8291 Section 3.3: IKM = HKDF-Expand(HKDF-Extract(auth_secret, ecdh_secret), "WebPush: info\0" || ua_public || as_public, 32)
	prkKey, err := hkdf.Extract(sha256.New, ecdhSecret, authSecret)
	if err != nil {
		return nil, err
	}
	keyInfo := make([]byte, 0, len("WebPush: info\x00")+len(uaPublic)+len(asPublic))
	keyInfo = append(keyInfo, "WebPush: info\x00"...)
	keyInfo = append(keyInfo, uaPublic...)
	keyInfo = append(keyInfo, asPublic...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}

	// RFC 8291 Section 3.4 / RFC 8188: PRK = HKDF-Extract(salt, IKM)
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// Single final record: plaintext || 0x02 (RFC 8188 Section 2).
	const recordSize = 4096
	if len(plaintext)+1+gcm.Overhead() > recordSize {
		return nil, errors.New("push payload exceeds single aes128gcm record size")
	}
	padded := make([]byte, len(plaintext)+1)
	copy(padded, plaintext)
	padded[len(plaintext)] = 0x02

	// Header: salt (16) || rs (4) || idlen (1) || as_public (65) = 86 bytes.
	headerLen := 16 + 4 + 1 + len(asPublic)
	out := make([]byte, headerLen, headerLen+len(padded)+gcm.Overhead())
	copy(out[:16], salt)
	binary.BigEndian.PutUint32(out[16:20], recordSize)
	out[20] = byte(len(asPublic))
	copy(out[21:headerLen], asPublic)
	return gcm.Seal(out, nonce, padded, nil), nil
}

// Client sends encrypted Web Push messages using VAPID authentication.
type Client struct {
	VAPID *VAPID
	HTTP  *http.Client
	Now   func() time.Time
}

// Send encrypts msg and delivers it to sub.Endpoint.
// Returns ErrSubscriptionExpired on 404 or 410 from the push service.
func (c *Client) Send(ctx context.Context, sub Subscription, msg Message) error {
	if c.VAPID == nil {
		return errors.New("web push is not configured")
	}
	_, uaPub, authSecret, err := sub.Validate()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	body, err := encrypt(payload, uaPub, authSecret)
	if err != nil {
		return err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	authHeader, err := c.VAPID.AuthHeader(sub.Endpoint, now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w: HTTP %d", ErrSubscriptionExpired, resp.StatusCode)
	default:
		detail := strings.TrimSpace(string(raw))
		if len(detail) > 200 {
			detail = detail[:200]
		}
		if detail != "" {
			return fmt.Errorf("push service HTTP %d: %s", resp.StatusCode, detail)
		}
		return fmt.Errorf("push service HTTP %d", resp.StatusCode)
	}
}

// decodeBase64URL accepts both unpadded and standard padded base64url strings.
func decodeBase64URL(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	return base64.RawURLEncoding.DecodeString(s)
}
