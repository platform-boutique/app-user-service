package users

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

const (
	tokenBytes = 32
	// tokenLen is the length of a token as text: 32 bytes in base64url without padding.
	tokenLen = 43
)

// NewToken returns a fresh session token: 32 bytes from crypto/rand, base64url without padding.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is what the database stores for a token: SHA-256 of its text. A token is 256 random bits, so a
// fast hash is enough (there is nothing to brute-force); a leaked table cannot be replayed as cookies.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// wellFormedToken reports whether s can be a token we issued (43 characters of the base64url alphabet).
// Anything else is unknown without asking the database.
func wellFormedToken(s string) bool {
	if len(s) != tokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
