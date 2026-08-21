// Package auth implements password, session, and authorization primitives.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type Params struct {
	MemoryKiB, Iterations uint32
	Parallelism           uint8
	SaltBytes, KeyBytes   uint32
}

func DefaultParams() Params { return Params{65536, 3, 4, 16, 32} }
func (p Params) Validate() error {
	if p.MemoryKiB < 19*1024 || p.Iterations < 2 || p.Parallelism < 1 || p.SaltBytes < 16 || p.KeyBytes < 32 {
		return errors.New("Argon2 parameters are below the permitted floor")
	}
	return nil
}

func NormalizePassword(value string) string { return norm.NFC.String(value) }
func HashPassword(password string, p Params) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	password = NormalizePassword(password)
	salt := make([]byte, p.SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, p.KeyBytes)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", p.MemoryKiB, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}
func VerifyPassword(password, encoded string, current Params) (bool, bool, error) {
	p, salt, want, err := parsePHC(encoded)
	if err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(NormalizePassword(password)), salt, p.Iterations, p.MemoryKiB, p.Parallelism, uint32(len(want)))
	ok := subtle.ConstantTimeCompare(got, want) == 1
	weaker := p.MemoryKiB < current.MemoryKiB || p.Iterations < current.Iterations || p.Parallelism < current.Parallelism || uint32(len(salt)) < current.SaltBytes || uint32(len(want)) < current.KeyBytes
	return ok, ok && weaker, nil
}
func parsePHC(value string) (Params, []byte, []byte, error) {
	parts := strings.Split(value, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return Params{}, nil, nil, errors.New("invalid Argon2id PHC string")
	}
	p := Params{SaltBytes: 16, KeyBytes: 32}
	for _, v := range strings.Split(parts[3], ",") {
		pair := strings.SplitN(v, "=", 2)
		if len(pair) != 2 {
			return Params{}, nil, nil, errors.New("invalid Argon2 parameters")
		}
		n, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil {
			return Params{}, nil, nil, errors.New("invalid Argon2 parameters")
		}
		switch pair[0] {
		case "m":
			p.MemoryKiB = uint32(n)
		case "t":
			p.Iterations = uint32(n)
		case "p":
			p.Parallelism = uint8(n)
		default:
			return Params{}, nil, nil, errors.New("invalid Argon2 parameters")
		}
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, errors.New("invalid Argon2 salt")
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, errors.New("invalid Argon2 result")
	}
	p.SaltBytes = uint32(len(salt))
	p.KeyBytes = uint32(len(key))
	if err = p.Validate(); err != nil {
		return Params{}, nil, nil, err
	}
	return p, salt, key, nil
}

var usernameChars = func() [256]bool {
	var a [256]bool
	for c := 'a'; c <= 'z'; c++ {
		a[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		a[c] = true
	}
	for _, c := range "._-" {
		a[c] = true
	}
	return a
}()

func NormalizeUsername(value string) (string, error) {
	value = strings.ToLower(value)
	if len(value) < 3 || len(value) > 32 {
		return "", errors.New("username must contain 3 through 32 characters")
	}
	for i := range value {
		if value[i] >= 128 || !usernameChars[value[i]] {
			return "", errors.New("username contains an invalid character")
		}
	}
	return value, nil
}

type PolicyCode string

const (
	PolicyLength  PolicyCode = "password_length"
	PolicyCommon  PolicyCode = "password_common"
	PolicyContext PolicyCode = "password_context"
)

type PolicyError struct {
	Code    PolicyCode
	Message string
}

func (e *PolicyError) Error() string { return e.Message }

type Policy struct {
	Username, Callsign string
	ServiceTerms       []string
	Additional         map[string]struct{}
}

var builtInCommon = map[string]struct{}{"password": {}, "passwordpassword": {}, "123456789012345": {}, "qwertyuiopasdfgh": {}, "letmeinletmeinletmein": {}}

func (p Policy) Check(value string) *PolicyError {
	value = NormalizePassword(value)
	count := utf8.RuneCountInString(value)
	if count < 15 || count > 128 || len(value) > 1024 {
		return &PolicyError{PolicyLength, "password must contain 15 through 128 Unicode characters"}
	}
	key := comparisonKey(value)
	if _, ok := builtInCommon[key]; ok {
		return &PolicyError{PolicyCommon, "choose a password that is not common"}
	}
	if _, ok := p.Additional[key]; ok {
		return &PolicyError{PolicyCommon, "choose a password that is not common"}
	}
	terms := append([]string{p.Username, p.Callsign, "opusref"}, p.ServiceTerms...)
	for _, term := range terms {
		k := comparisonKey(term)
		if len([]rune(k)) >= 3 && strings.Contains(key, k) {
			return &PolicyError{PolicyContext, "password must not contain an account or service name"}
		}
	}
	return nil
}
func comparisonKey(value string) string {
	value = cases.Fold().String(norm.NFC.String(value))
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return r
	}, value)
}
