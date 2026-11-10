package apikeys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Sealer encrypts API keys at rest so the console can show a saved key again
// without keeping plaintext in the database.
//
// The AES-256 key lives in its own file inside the data directory, never in the
// database and never in an environment variable. A database dump on its own is
// therefore not enough to recover a key.
type Sealer struct {
	aead cipher.AEAD
}

// ErrSealedUnavailable reports a key that was stored before encryption existed
// (or whose ciphertext was cleared); its plaintext cannot be recovered.
var ErrSealedUnavailable = errors.New("api key plaintext is not recoverable")

const keyFileName = "api_key_secret"

// OpenSealer loads the sealing key from dir, creating it on first use with
// owner-only permissions.
func OpenSealer(dir string) (*Sealer, error) {
	path := filepath.Join(dir, keyFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		raw = make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generate api key secret: %w", err)
		}
		// O_EXCL keeps two racing processes from overwriting a live key.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return OpenSealer(dir)
			}
			return nil, fmt.Errorf("create api key secret: %w", err)
		}
		if _, err := f.Write(raw); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("write api key secret: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close api key secret: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read api key secret: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("api key secret must be 32 bytes, got %d", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("init api key cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init api key aead: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext and returns base64(nonce || ciphertext).
func (s *Sealer) Seal(plaintext string) (string, error) {
	if s == nil {
		return "", ErrSealedUnavailable
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	out := s.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// Open decrypts a value produced by Seal.
func (s *Sealer) Open(blob string) (string, error) {
	if s == nil || blob == "" {
		return "", ErrSealedUnavailable
	}
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		return "", fmt.Errorf("decode sealed key: %w", err)
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("sealed key is truncated")
	}
	plain, err := s.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt sealed key: %w", err)
	}
	return string(plain), nil
}
