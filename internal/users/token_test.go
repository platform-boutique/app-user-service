package users

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestNewToken(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != tokenLen || !wellFormedToken(tok) {
			t.Fatalf("token %q: length %d", tok, len(tok))
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(raw) != 32 {
			t.Fatalf("token %q is not 32 bytes of base64url without padding: %v", tok, err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

func TestHashToken(t *testing.T) {
	// SHA-256 of the token text (known vector: sha256("abc"))
	if got := hex.EncodeToString(HashToken("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("HashToken(abc) = %s", got)
	}
	tok, _ := NewToken()
	want := sha256.Sum256([]byte(tok))
	h := HashToken(tok)
	if len(h) != 32 || !bytes.Equal(h, want[:]) {
		t.Errorf("hash %x, want %x", h, want)
	}
	if bytes.Contains(h, []byte(tok)) {
		t.Error("hash contains the token")
	}
	other, _ := NewToken()
	if bytes.Equal(HashToken(other), h) {
		t.Error("different tokens, same hash")
	}
}

func TestWellFormedToken(t *testing.T) {
	good, _ := NewToken()
	if !wellFormedToken(good) {
		t.Fatal("fresh token not well formed")
	}
	bad := []string{"", "short", good + "A", good[:42], good[:42] + "=", good[:42] + "+", good[:42] + "/", good[:42] + " ", good[:42] + "\x00", good[:42] + "é"}
	for _, b := range bad {
		if wellFormedToken(b) {
			t.Errorf("%q accepted", b)
		}
	}
}
