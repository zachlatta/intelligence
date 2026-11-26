package vaultwarden

import (
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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

type fakeVault struct {
	server         *httptest.Server
	orgID          string
	colID          string
	orgEnc, orgMac []byte
	lastRawBody    string
	createdCiphers []createdCipher
	failCreate     bool
}

type createdCipher struct {
	ID             string
	OrganizationID string
	CollectionIDs  []string
	Type           int
	Name           string
	Notes          string
	Username       string
	Password       string
}

func newFakeVault(t *testing.T, kdf int) *fakeVault {
	t.Helper()
	email := "agents@example.com"
	masterPass := "test-master-password-123"
	kdfIter := 5000
	kdfMem := 16
	kdfPar := 1
	if kdf == 1 {
		kdfIter = 1
	}

	var masterKey []byte
	if kdf == 0 {
		masterKey = pbkdf2.Key([]byte(masterPass), []byte(email), kdfIter, 32, sha256.New)
	} else {
		salt := sha256.Sum256([]byte(email))
		masterKey = argon2.IDKey([]byte(masterPass), salt[:], uint32(kdfIter), uint32(kdfMem)*1024, uint8(kdfPar), 32)
	}
	stEnc := make([]byte, 32)
	stMac := make([]byte, 32)
	_, _ = io.ReadFull(hkdf.Expand(sha256.New, masterKey, []byte("enc")), stEnc)
	_, _ = io.ReadFull(hkdf.Expand(sha256.New, masterKey, []byte("mac")), stMac)

	userKey := make([]byte, 64)
	_, _ = rand.Read(userKey)
	userEnc, userMac := userKey[:32], userKey[32:]
	encUserKey := testEncrypt(t, userKey, stEnc, stMac)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	encPrivKey := testEncrypt(t, privDER, userEnc, userMac)

	orgKey := make([]byte, 64)
	_, _ = rand.Read(orgKey)
	orgEnc, orgMac := orgKey[:32], orgKey[32:]
	encOrgKeyBytes, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &rsaKey.PublicKey, orgKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	encOrgKey := "4." + base64.StdEncoding.EncodeToString(encOrgKeyBytes)

	orgID := "11111111-1111-4111-8111-111111111111"
	colID := "22222222-2222-4222-8222-222222222222"
	encColName := testEncrypt(t, []byte(DefaultCollection), orgEnc, orgMac)

	fv := &fakeVault{orgID: orgID, colID: colID, orgEnc: orgEnc, orgMac: orgMac}
	fv.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/identity/connect/token":
			_ = r.ParseForm()
			if r.FormValue("client_id") != "user.test-id" || r.FormValue("client_secret") != "test-client-secret" {
				http.Error(w, "invalid_client", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":   "tok-123",
				"token_type":     "Bearer",
				"expires_in":     3600,
				"Key":            encUserKey,
				"PrivateKey":     encPrivKey,
				"Kdf":            kdf,
				"KdfIterations":  kdfIter,
				"KdfMemory":      kdfMem,
				"KdfParallelism": kdfPar,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/sync":
			if r.Header.Get("Authorization") != "Bearer tok-123" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"profile": map[string]any{
					"email":      email,
					"key":        encUserKey,
					"privateKey": encPrivKey,
					"organizations": []map[string]any{
						{"id": orgID, "name": DefaultOrganization, "key": encOrgKey},
					},
				},
				"collections": []map[string]any{
					{"id": colID, "organizationId": orgID, "name": encColName},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/ciphers/create":
			if r.Header.Get("Authorization") != "Bearer tok-123" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			fv.lastRawBody = string(raw)
			if fv.failCreate {
				http.Error(w, "vault write failed", http.StatusInternalServerError)
				return
			}
			var payload struct {
				Cipher struct {
					Type           int    `json:"type"`
					OrganizationID string `json:"organizationId"`
					Name           string `json:"name"`
					Notes          string `json:"notes"`
					Login          struct {
						Username *string `json:"username"`
						Password string  `json:"password"`
					} `json:"login"`
				} `json:"cipher"`
				CollectionIDs []string `json:"collectionIds"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			itemID := "33333333-3333-4333-8333-333333333333"
			cc := createdCipher{
				ID:             itemID,
				OrganizationID: payload.Cipher.OrganizationID,
				CollectionIDs:  payload.CollectionIDs,
				Type:           payload.Cipher.Type,
				Name:           string(testDecrypt(t, payload.Cipher.Name, orgEnc, orgMac)),
				Password:       string(testDecrypt(t, payload.Cipher.Login.Password, orgEnc, orgMac)),
			}
			if payload.Cipher.Notes != "" {
				cc.Notes = string(testDecrypt(t, payload.Cipher.Notes, orgEnc, orgMac))
			}
			if payload.Cipher.Login.Username != nil && *payload.Cipher.Login.Username != "" {
				cc.Username = string(testDecrypt(t, *payload.Cipher.Login.Username, orgEnc, orgMac))
			}
			fv.createdCiphers = append(fv.createdCiphers, cc)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": itemID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fv.server.Close)
	return fv
}

func testEncrypt(t *testing.T, pt, encKey, macKey []byte) string {
	t.Helper()
	iv := make([]byte, aes.BlockSize)
	_, _ = rand.Read(iv)
	pad := aes.BlockSize - (len(pt) % aes.BlockSize)
	padded := append(append([]byte(nil), pt...), make([]byte, pad)...)
	for i := len(pt); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	m := hmac.New(sha256.New, macKey)
	m.Write(iv)
	m.Write(ct)
	return "2." + base64.StdEncoding.EncodeToString(iv) + "|" +
		base64.StdEncoding.EncodeToString(ct) + "|" +
		base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func testDecrypt(t *testing.T, cs string, encKey, macKey []byte) []byte {
	t.Helper()
	pt, err := decryptCipherString(cs, encKey, macKey)
	if err != nil {
		t.Fatalf("decrypt %q: %v", cs, err)
	}
	return pt
}

func TestStoreSecretPBKDF2AndArgon2id(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kdf  int
	}{
		{"pbkdf2", 0},
		{"argon2id", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fv := newFakeVault(t, tc.kdf)
			client := NewClient(Config{
				URL:            fv.server.URL,
				ClientID:       "user.test-id",
				ClientSecret:   "test-client-secret",
				MasterPassword: "test-master-password-123",
			})
			secretVal := "sk_live_super_secret_value_987654321"
			item, err := client.StoreSecret(context.Background(), StoreRequest{
				Name:     "Stripe API key",
				Username: "billing-bot",
				Password: secretVal,
				Notes:    "Requested by workflow run demo",
			})
			if err != nil {
				t.Fatalf("StoreSecret: %v", err)
			}
			if item.ID != "33333333-3333-4333-8333-333333333333" || item.Name != "Stripe API key" || item.Collection != DefaultCollection {
				t.Fatalf("unexpected item: %+v", item)
			}
			if strings.Contains(fv.lastRawBody, secretVal) {
				t.Fatal("plaintext secret appeared in HTTP body")
			}
			if len(fv.createdCiphers) != 1 {
				t.Fatalf("expected 1 cipher, got %d", len(fv.createdCiphers))
			}
			got := fv.createdCiphers[0]
			if got.Type != 1 || got.OrganizationID != fv.orgID || len(got.CollectionIDs) != 1 || got.CollectionIDs[0] != fv.colID {
				t.Fatalf("unexpected cipher placement: %+v", got)
			}
			if got.Name != "Stripe API key" || got.Username != "billing-bot" || got.Password != secretVal || got.Notes != "Requested by workflow run demo" {
				t.Fatalf("decrypted fields mismatch: %+v", got)
			}
		})
	}
}

func TestStoreSecretErrorsNeverLeakValue(t *testing.T) {
	t.Parallel()
	fv := newFakeVault(t, 0)
	fv.failCreate = true
	client := NewClient(Config{
		URL:            fv.server.URL,
		ClientID:       "user.test-id",
		ClientSecret:   "test-client-secret",
		MasterPassword: "test-master-password-123",
	})
	secretVal := "top-secret-do-not-leak-xyz"
	_, err := client.StoreSecret(context.Background(), StoreRequest{
		Name:     "Deploy token",
		Password: secretVal,
	})
	if err == nil {
		t.Fatal("expected error when create fails")
	}
	if strings.Contains(err.Error(), secretVal) {
		t.Fatalf("error leaked secret value: %v", err)
	}

	// Wrong master password
	badClient := NewClient(Config{
		URL:            fv.server.URL,
		ClientID:       "user.test-id",
		ClientSecret:   "test-client-secret",
		MasterPassword: "wrong-password",
	})
	_, err = badClient.StoreSecret(context.Background(), StoreRequest{
		Name:     "Deploy token",
		Password: secretVal,
	})
	if err == nil {
		t.Fatal("expected error with wrong master password")
	}
	if strings.Contains(err.Error(), secretVal) || strings.Contains(err.Error(), "wrong-password") {
		t.Fatalf("error leaked credential: %v", err)
	}
}
