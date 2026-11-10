// Package apikeys generates and hashes bridge API keys. Plaintext keys are
// returned exactly once at creation; only SHA-256 hashes are stored.
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Prefix is the fixed, recognizable key format.
const Prefix = "sk-bps-"

// hashWidth is the display prefix length, e.g. sk-bps-Ab3dEf9x.
const displayWidth = len(Prefix) + 8

// Generated holds one freshly minted key.
type Generated struct {
	Plaintext string
	Hash      string
	DisplayPrefix string
}

// Generate mints a new high-entropy key.
func Generate() (*Generated, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate api key: %w", err)
	}
	plaintext := Prefix + base64.RawURLEncoding.EncodeToString(raw)
	return &Generated{
		Plaintext:     plaintext,
		Hash:          Hash(plaintext),
		DisplayPrefix: plaintext[:displayWidth],
	}, nil
}

// Hash returns the hex SHA-256 of a plaintext key.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Matches reports in constant time whether a stored hash belongs to plaintext.
func Matches(plaintext, storedHash string) bool {
	return subtle.ConstantTimeCompare([]byte(Hash(plaintext)), []byte(storedHash)) == 1
}

// ValidFormat reports whether a presented key could be one of ours. It is a
// cheap pre-filter; the authoritative check is the hash lookup.
func ValidFormat(key string) bool {
	if len(key) != len(Prefix)+43 { // 32 bytes -> 43 base64url chars
		return false
	}
	if key[:len(Prefix)] != Prefix {
		return false
	}
	for _, c := range key[len(Prefix):] {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// NewID returns a random identifier with the given prefix.
func NewID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}
