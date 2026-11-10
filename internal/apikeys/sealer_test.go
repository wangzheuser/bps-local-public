package apikeys

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSealerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSealer(dir)
	if err != nil {
		t.Fatal(err)
	}
	plain := Prefix + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	sealed, err := s.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, plain) {
		t.Fatal("sealed value must not contain the plaintext")
	}
	got, err := s.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Fatalf("round trip = %q, want %q", got, plain)
	}
}

func TestSealerSecretFilePermissionsAndReuse(t *testing.T) {
	dir := t.TempDir()
	s1, err := OpenSealer(dir)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s1.Seal("value")
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, keyFileName)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows does not model POSIX permission bits; the mode is enforced on
	// the Linux deployment target.
	if runtime.GOOS != "windows" {
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("secret permissions = %o, want 600", perm)
		}
	}

	// A second sealer must load the same secret and decrypt earlier values.
	s2, err := OpenSealer(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "value" {
		t.Fatalf("reloaded sealer = %q", got)
	}
}

func TestSealerRejectsForeignCiphertext(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	a, err := OpenSealer(dirA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenSealer(dirB)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.Seal("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("a different secret must not decrypt")
	}
}

func TestSealerEmptyAndMissing(t *testing.T) {
	s, err := OpenSealer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(""); err == nil {
		t.Fatal("empty blob must not decrypt")
	}
	var nilSealer *Sealer
	if _, err := nilSealer.Seal("x"); err == nil {
		t.Fatal("nil sealer must fail closed")
	}
	if _, err := nilSealer.Open("x"); err == nil {
		t.Fatal("nil sealer must fail closed")
	}
}
