package auth

import (
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

func TestUsernameValidation(t *testing.T) {
	if got, err := NormalizeUsername("Alice.Name"); err != nil || got != "alice.name" {
		t.Fatalf("got %q: %v", got, err)
	}
	if _, err := NormalizeUsername("bad name"); err == nil {
		t.Fatal("expected invalid username")
	}
}
