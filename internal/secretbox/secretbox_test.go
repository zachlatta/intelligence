package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestNewNeeds32Bytes(t *testing.T) {
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) accepted", bad)
		}
	}
	if _, err := New(" " + testKey(t) + "\n"); err != nil {
		t.Fatalf("New(valid): %v", err)
	}
}

func TestSealOpenBindsAAD(t *testing.T) {
	b, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("sk_live_plaintext_value")
	ct, err := b.Seal(plain, "row-1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext contains the plaintext")
	}
	ct2, _ := b.Seal(plain, "row-1")
	if bytes.Equal(ct, ct2) {
		t.Fatal("two seals of the same value are equal: nonce reused")
	}
	got, err := b.Open(ct, "row-1")
	if err != nil || string(got) != string(plain) {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := b.Open(ct, "row-2"); err == nil {
		t.Fatal("ciphertext opened under another row's AAD (swappable)")
	}
	ct[len(ct)-1] ^= 1
	if _, err := b.Open(ct, "row-1"); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	if _, err := b.Open([]byte{1, 2}, "row-1"); err == nil {
		t.Fatal("short ciphertext opened")
	}
	other, _ := New(testKey(t))
	if _, err := other.Open(ct2, "row-1"); err == nil {
		t.Fatal("opened with another key")
	}
}
