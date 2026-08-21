package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordRoundTripAndNFC(t *testing.T) {
	p := DefaultParams()
	hash, err := HashPassword("correct horse battery staple", p)
	if err != nil {
		t.Fatal(err)
	}
	if ok, rehash, err := VerifyPassword("correct horse battery staple", hash, p); err != nil || !ok || rehash {
		t.Fatalf("verify=%v rehash=%v err=%v", ok, rehash, err)
	}
	if ok, _, _ := VerifyPassword("wrong password value", hash, p); ok {
		t.Fatal("wrong password matched")
	}
	composed := strings.Repeat("é", 15)
	decomposed := strings.Repeat("e\u0301", 15)
	hash, err = HashPassword(composed, p)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := VerifyPassword(decomposed, hash, p); err != nil || !ok {
		t.Fatalf("NFC match failed: %v", err)
	}
}

func TestPasswordPolicy(t *testing.T) {
	policy := Policy{Username: "n0call", Callsign: "N0CALL", ServiceTerms: []string{"OpusRef"}}
	for _, tc := range []struct {
		password string
		code     PolicyCode
	}{
		{"short", PolicyLength}, {"passwordpassword", PolicyCommon}, {"this passphrase has n0call inside", PolicyContext},
	} {
		if err := policy.Check(tc.password); err == nil || err.Code != tc.code {
			t.Fatalf("%q: %#v", tc.password, err)
		}
	}
	if err := policy.Check("quiet marble nebula orchard"); err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddedSecListsCorpusIsLoaded(t *testing.T) {
	if len(builtInCommon) < 9900 {
		t.Fatalf("embedded common-password corpus has only %d normalized entries", len(builtInCommon))
	}
	if err := (Policy{}).Check("films+pic+galeries"); err == nil || err.Code != PolicyCommon {
		t.Fatalf("SecLists password was not rejected: %#v", err)
	}
}

func TestUsernameValidation(t *testing.T) {
	if got, err := NormalizeUsername("Alice.Name"); err != nil || got != "alice.name" {
		t.Fatalf("got %q: %v", got, err)
	}
	if _, err := NormalizeUsername("bad name"); err == nil {
		t.Fatal("expected invalid username")
	}
}
func TestAdditionalBlocklistIsAdditive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	if err := os.WriteFile(path, []byte("Special-Phrase!\n"), 0600); err != nil {
		t.Fatal(err)
	}
	items, err := LoadAdditionalBlocklist(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := Policy{Additional: items}
	if got := policy.Check("Special-Phrase!"); got == nil {
		t.Fatal("additional blocklist was not applied")
	}
}
