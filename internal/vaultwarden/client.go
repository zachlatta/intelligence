// Package vaultwarden stores secrets in a Bitwarden/Vaultwarden server's shared
// organization collection using the standard Bitwarden client encryption protocol.
package vaultwarden

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

const (
	DefaultOrganization = "AI Agents"
	DefaultCollection   = "AI Agents Shared Passwords"
)

// Config holds the Vaultwarden connection and unlock settings.
type Config struct {
	URL            string
	ClientID       string
	ClientSecret   string
	MasterPassword string
	Organization   string // organization name or UUID; default DefaultOrganization
	Collection     string // collection name or UUID; default DefaultCollection
}

// StoreRequest describes a secret item to create in Vaultwarden.
type StoreRequest struct {
	Name     string
	Username string
	Password string
	Notes    string
}

// StoredItem is the non-secret reference returned after storing a secret.
type StoredItem struct {
	ID         string
	Name       string
	Collection string
}

// Client writes encrypted login items into a Vaultwarden organization collection.
type Client struct {
	cfg  Config
	HTTP *http.Client
}

// NewClient constructs a Vaultwarden API client with defaults applied.
func NewClient(cfg Config) *Client {
	cfg.URL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if strings.TrimSpace(cfg.Organization) == "" {
		cfg.Organization = DefaultOrganization
	}
	if strings.TrimSpace(cfg.Collection) == "" {
		cfg.Collection = DefaultCollection
	}
	return &Client{
		cfg:  cfg,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// TargetCollection returns the configured collection name or ID.
func (c *Client) TargetCollection() string {
	return c.cfg.Collection
}

// StoreSecret authenticates with Vaultwarden, unlocks the organization key,
// encrypts the item locally, and creates it in the target collection.
// Returned errors never contain secret values or Vaultwarden credentials.
func (c *Client) StoreSecret(ctx context.Context, req StoreRequest) (StoredItem, error) {
	if strings.TrimSpace(req.Name) == "" {
		return StoredItem{}, errors.New("secret item name is required")
	}
	if req.Password == "" {
		return StoredItem{}, errors.New("secret value is required")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	tok, err := c.authenticate(ctx, hc)
	if err != nil {
		return StoredItem{}, err
	}
	syncResp, err := c.sync(ctx, hc, tok.AccessToken)
	if err != nil {
		return StoredItem{}, err
	}

	userKeyEnc := tok.Key
	if userKeyEnc == "" {
		userKeyEnc = syncResp.Profile.Key
	}
	privKeyEnc := tok.PrivateKey
	if privKeyEnc == "" {
		privKeyEnc = syncResp.Profile.PrivateKey
	}
	if syncResp.Profile.Email == "" || userKeyEnc == "" || privKeyEnc == "" {
		return StoredItem{}, errors.New("vaultwarden sync response missing account key material")
	}

	masterKey, err := deriveMasterKey(c.cfg.MasterPassword, syncResp.Profile.Email, tok.Kdf, tok.KdfIterations, tok.KdfMemory, tok.KdfParallelism)
	if err != nil {
		return StoredItem{}, err
	}
	stEnc, stMac, err := stretchKey(masterKey)
	if err != nil {
		return StoredItem{}, err
	}
	userKey, err := decryptCipherString(userKeyEnc, stEnc, stMac)
	if err != nil || len(userKey) != 64 {
		return StoredItem{}, errors.New("vaultwarden unlock failed: check master password")
	}
	userEnc, userMac := userKey[:32], userKey[32:]

	privDER, err := decryptCipherString(privKeyEnc, userEnc, userMac)
	if err != nil {
		return StoredItem{}, errors.New("vaultwarden unlock failed: could not decrypt account private key")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(privDER)
	if err != nil {
		return StoredItem{}, errors.New("vaultwarden unlock failed: invalid account private key")
	}
	rsaPriv, ok := parsedKey.(*rsa.PrivateKey)
	if !ok {
		return StoredItem{}, errors.New("vaultwarden unlock failed: account private key is not RSA")
	}

	var targetOrg *syncOrg
	for i := range syncResp.Profile.Organizations {
		org := &syncResp.Profile.Organizations[i]
		if org.ID == c.cfg.Organization || strings.EqualFold(org.Name, c.cfg.Organization) {
			targetOrg = org
			break
		}
	}
	if targetOrg == nil {
		return StoredItem{}, fmt.Errorf("vaultwarden organization %q not found", c.cfg.Organization)
	}
	orgKey, err := decryptRSAKey(targetOrg.Key, rsaPriv)
	if err != nil || len(orgKey) != 64 {
		return StoredItem{}, errors.New("vaultwarden unlock failed: could not decrypt organization key")
	}
	orgEnc, orgMac := orgKey[:32], orgKey[32:]

	var targetColID, targetColName string
	for _, col := range syncResp.Collections {
		if col.OrganizationID != targetOrg.ID {
			continue
		}
		decName := ""
		if pt, derr := decryptCipherString(col.Name, orgEnc, orgMac); derr == nil {
			decName = string(pt)
		}
		if col.ID == c.cfg.Collection || strings.EqualFold(decName, c.cfg.Collection) {
			targetColID = col.ID
			targetColName = decName
			if targetColName == "" {
				targetColName = c.cfg.Collection
			}
			break
		}
	}
	if targetColID == "" {
		return StoredItem{}, fmt.Errorf("vaultwarden collection %q not found in organization %q", c.cfg.Collection, targetOrg.Name)
	}

	encName, err := encryptCipherString([]byte(req.Name), orgEnc, orgMac)
	if err != nil {
		return StoredItem{}, errors.New("vaultwarden encrypt failed")
	}
	encPassword, err := encryptCipherString([]byte(req.Password), orgEnc, orgMac)
	if err != nil {
		return StoredItem{}, errors.New("vaultwarden encrypt failed")
	}
	var encNotes *string
	if strings.TrimSpace(req.Notes) != "" {
		n, err := encryptCipherString([]byte(req.Notes), orgEnc, orgMac)
		if err != nil {
			return StoredItem{}, errors.New("vaultwarden encrypt failed")
		}
		encNotes = &n
	}
	var encUsername *string
	if strings.TrimSpace(req.Username) != "" {
		u, err := encryptCipherString([]byte(req.Username), orgEnc, orgMac)
		if err != nil {
			return StoredItem{}, errors.New("vaultwarden encrypt failed")
		}
		encUsername = &u
	}

	payload := createCipherRequest{
		Cipher: cipherBody{
			Type:           1, // Login
			OrganizationID: targetOrg.ID,
			Name:           encName,
			Notes:          encNotes,
			Login: cipherLogin{
				Username: encUsername,
				Password: encPassword,
			},
		},
		CollectionIDs: []string{targetColID},
	}
	itemID, err := c.createCipher(ctx, hc, tok.AccessToken, payload)
	if err != nil {
		return StoredItem{}, err
	}
	return StoredItem{
		ID:         itemID,
		Name:       req.Name,
		Collection: targetColName,
	}, nil
}

type tokenResponse struct {
	AccessToken    string `json:"access_token"`
	Key            string `json:"Key"`
	PrivateKey     string `json:"PrivateKey"`
	Kdf            int    `json:"Kdf"`
	KdfIterations  int    `json:"KdfIterations"`
	KdfMemory      int    `json:"KdfMemory"`
	KdfParallelism int    `json:"KdfParallelism"`
}

func (c *Client) authenticate(ctx context.Context, hc *http.Client) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("scope", "api")
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	form.Set("device_type", "21")
	form.Set("device_identifier", deviceUUID(c.cfg.ClientID))
	form.Set("device_name", "intelligence")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+"/identity/connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, errors.New("vaultwarden request build failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return tokenResponse{}, errors.New("vaultwarden authentication request failed")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("vaultwarden authentication failed (HTTP %d)", resp.StatusCode)
	}
	var out tokenResponse
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return tokenResponse{}, errors.New("vaultwarden authentication returned invalid token response")
	}
	return out, nil
}

type syncOrg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

type syncCollection struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organizationId"`
	Name           string `json:"name"`
}

type syncResponse struct {
	Profile struct {
		Email         string    `json:"email"`
		Key           string    `json:"key"`
		PrivateKey    string    `json:"privateKey"`
		Organizations []syncOrg `json:"organizations"`
	} `json:"profile"`
	Collections []syncCollection `json:"collections"`
}

func (c *Client) sync(ctx context.Context, hc *http.Client, accessToken string) (syncResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+"/api/sync", nil)
	if err != nil {
		return syncResponse{}, errors.New("vaultwarden sync request build failed")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := hc.Do(req)
	if err != nil {
		return syncResponse{}, errors.New("vaultwarden sync request failed")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return syncResponse{}, fmt.Errorf("vaultwarden sync failed (HTTP %d)", resp.StatusCode)
	}
	var out syncResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return syncResponse{}, errors.New("vaultwarden sync returned invalid JSON")
	}
	return out, nil
}

type createCipherRequest struct {
	Cipher        cipherBody `json:"cipher"`
	CollectionIDs []string   `json:"collectionIds"`
}

type cipherBody struct {
	Type           int         `json:"type"`
	OrganizationID string      `json:"organizationId"`
	Name           string      `json:"name"`
	Notes          *string     `json:"notes,omitempty"`
	Login          cipherLogin `json:"login"`
}

type cipherLogin struct {
	Username *string `json:"username,omitempty"`
	Password string  `json:"password"`
}

func (c *Client) createCipher(ctx context.Context, hc *http.Client, accessToken string, payload createCipherRequest) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", errors.New("vaultwarden encode failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+"/api/ciphers/create", bytes.NewReader(b))
	if err != nil {
		return "", errors.New("vaultwarden create request build failed")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", errors.New("vaultwarden create request failed")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("vaultwarden create item failed (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		return "", errors.New("vaultwarden create item returned no item id")
	}
	return out.ID, nil
}

func deviceUUID(seed string) string {
	h := sha256.Sum256([]byte("intelligence-vaultwarden:" + seed))
	h[6] = (h[6] & 0x0f) | 0x40
	h[8] = (h[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func deriveMasterKey(password, email string, kdf, iterations, memoryMiB, parallelism int) ([]byte, error) {
	saltStr := strings.ToLower(strings.TrimSpace(email))
	switch kdf {
	case 0: // PBKDF2-SHA256
		if iterations <= 0 {
			iterations = 600000
		}
		return pbkdf2.Key([]byte(password), []byte(saltStr), iterations, 32, sha256.New), nil
	case 1: // Argon2id
		if iterations <= 0 || memoryMiB <= 0 || parallelism <= 0 {
			return nil, errors.New("vaultwarden invalid argon2id parameters")
		}
		salt := sha256.Sum256([]byte(saltStr))
		return argon2.IDKey([]byte(password), salt[:], uint32(iterations), uint32(memoryMiB)*1024, uint8(parallelism), 32), nil
	default:
		return nil, fmt.Errorf("vaultwarden unsupported KDF type %d", kdf)
	}
}

func stretchKey(masterKey []byte) (encKey, macKey []byte, err error) {
	encKey = make([]byte, 32)
	macKey = make([]byte, 32)
	if _, err := io.ReadFull(hkdf.Expand(sha256.New, masterKey, []byte("enc")), encKey); err != nil {
		return nil, nil, err
	}
	if _, err := io.ReadFull(hkdf.Expand(sha256.New, masterKey, []byte("mac")), macKey); err != nil {
		return nil, nil, err
	}
	return encKey, macKey, nil
}

func encryptCipherString(plaintext, encKey, macKey []byte) (string, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	pad := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := make([]byte, len(plaintext)+pad)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", err
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	m := hmac.New(sha256.New, macKey)
	m.Write(iv)
	m.Write(ct)
	mac := m.Sum(nil)
	return "2." + base64.StdEncoding.EncodeToString(iv) + "|" +
		base64.StdEncoding.EncodeToString(ct) + "|" +
		base64.StdEncoding.EncodeToString(mac), nil
}

func decryptCipherString(cs string, encKey, macKey []byte) ([]byte, error) {
	typ, rest, ok := strings.Cut(cs, ".")
	if !ok || typ != "2" {
		return nil, errors.New("unsupported cipher string type")
	}
	parts := strings.Split(rest, "|")
	if len(parts) != 3 {
		return nil, errors.New("malformed cipher string")
	}
	iv, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil || len(iv) != aes.BlockSize {
		return nil, errors.New("invalid cipher iv")
	}
	ct, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("invalid ciphertext")
	}
	gotMac, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("invalid mac")
	}
	m := hmac.New(sha256.New, macKey)
	m.Write(iv)
	m.Write(ct)
	if !hmac.Equal(gotMac, m.Sum(nil)) {
		return nil, errors.New("mac mismatch")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(pt) {
		return nil, errors.New("invalid padding")
	}
	for i := len(pt) - pad; i < len(pt); i++ {
		if pt[i] != byte(pad) {
			return nil, errors.New("invalid padding")
		}
	}
	return pt[:len(pt)-pad], nil
}

func decryptRSAKey(cs string, priv *rsa.PrivateKey) ([]byte, error) {
	typ, rest, ok := strings.Cut(cs, ".")
	if !ok {
		return nil, errors.New("malformed rsa cipher string")
	}
	ctB64, _, _ := strings.Cut(rest, "|")
	ct, err := base64.StdEncoding.DecodeString(ctB64)
	if err != nil {
		return nil, errors.New("invalid rsa ciphertext")
	}
	switch typ {
	case "3": // Rsa2048_OaepSha256_B64
		return rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, ct, nil)
	case "4": // Rsa2048_OaepSha1_B64
		return rsa.DecryptOAEP(sha1.New(), rand.Reader, priv, ct, nil)
	default:
		return nil, fmt.Errorf("unsupported rsa cipher type %q", typ)
	}
}
