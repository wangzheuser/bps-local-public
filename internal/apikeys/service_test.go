package apikeys

import "testing"

func TestGenerateFormat(t *testing.T) {
	gen, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !ValidFormat(gen.Plaintext) {
		t.Fatalf("generated key fails format: %s", gen.Plaintext)
	}
	if len(gen.DisplayPrefix) != displayWidth {
		t.Fatalf("prefix length = %d", len(gen.DisplayPrefix))
	}
	if gen.Plaintext[:displayWidth] != gen.DisplayPrefix {
		t.Fatalf("prefix mismatch")
	}
	if Hash(gen.Plaintext) != gen.Hash {
		t.Fatal("hash instability")
	}
}

func TestHashMatches(t *testing.T) {
	gen, _ := Generate()
	if !Matches(gen.Plaintext, gen.Hash) {
		t.Fatal("matching key rejected")
	}
	if Matches(gen.Plaintext+"x", gen.Hash) {
		t.Fatal("wrong key accepted")
	}
}

func TestValidFormatRejects(t *testing.T) {
	gen, _ := Generate()
	bad := []string{
		"",
		gen.Plaintext + "extra",
		"sk-other-" + gen.Plaintext[len(Prefix):],
		gen.Plaintext[:len(gen.Plaintext)-1] + "!", // illegal char
	}
	for _, b := range bad {
		if ValidFormat(b) {
			t.Fatalf("bad key accepted: %q", b)
		}
	}
}

func TestNewID(t *testing.T) {
	id, err := NewID("acct_")
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	if len(id) != len("acct_")+24 {
		t.Fatalf("unexpected id length: %s", id)
	}
}
