package pii

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"vozkot/infra/crypto/vault"
)

// Pure-domain tests: this package holds no connections and needs none. What is
// under test is the envelope format and the blind index, and both are decided
// entirely in memory.

func key(fill byte) []byte {
	out := make([]byte, 32)
	for index := range out {
		out[index] = fill
	}
	return out
}

func service(t *testing.T, versions map[byte]byte, active byte) *Service {
	t.Helper()
	vaults := make(map[byte]*vault.Vault, len(versions))
	for version, fill := range versions {
		item, err := vault.New(key(fill), int(version))
		if err != nil {
			t.Fatalf("build vault v%d: %v", version, err)
		}
		vaults[version] = item
	}
	built, err := New(vaults, active, key(0xAA))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return built
}

func TestEncryptedValueSurvivesARoundTrip(t *testing.T) {
	s := service(t, map[byte]byte{1: 0x01}, 1)

	sealed, err := s.Encrypt([]byte("12345678909"))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	opened, err := s.Decrypt(sealed)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if string(opened) != "12345678909" {
		t.Fatalf("round trip = %q, want the original document", opened)
	}
}

// TestCiphertextNeverContainsThePlaintext is the whole point of the package,
// asserted directly rather than assumed from the algorithm's name.
func TestCiphertextNeverContainsThePlaintext(t *testing.T) {
	s := service(t, map[byte]byte{1: 0x01}, 1)
	const document = "12345678909"

	sealed, err := s.Encrypt([]byte(document))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if bytes.Contains(sealed, []byte(document)) {
		t.Fatal("the plaintext is present in the ciphertext")
	}
}

// TestTheSameValueEncryptsDifferentlyEveryTime is why an encrypted column
// cannot be searched, and therefore why the blind index exists.
func TestTheSameValueEncryptsDifferentlyEveryTime(t *testing.T) {
	s := service(t, map[byte]byte{1: 0x01}, 1)

	first, _ := s.Encrypt([]byte("12345678909"))
	second, _ := s.Encrypt([]byte("12345678909"))

	if bytes.Equal(first, second) {
		t.Fatal("two encryptions of one value are identical: the nonce is not random")
	}
}

// TestATamperedCiphertextFailsToOpen: GCM authenticates, so a flipped bit is an
// error rather than different plaintext. A document that silently becomes a
// different document is the failure this prevents.
func TestATamperedCiphertextFailsToOpen(t *testing.T) {
	s := service(t, map[byte]byte{1: 0x01}, 1)
	sealed, _ := s.Encrypt([]byte("12345678909"))

	tampered := make([]byte, len(sealed))
	copy(tampered, sealed)
	tampered[len(tampered)-1] ^= 0xFF

	if _, err := s.Decrypt(tampered); err == nil {
		t.Fatal("a tampered ciphertext opened successfully")
	}
}

// TestRotationKeepsOldRowsReadable is the property the version byte buys: a new
// key starts encrypting, and everything already written still opens.
func TestRotationKeepsOldRowsReadable(t *testing.T) {
	before := service(t, map[byte]byte{1: 0x01}, 1)
	written, err := before.Encrypt([]byte("written under v1"))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	// The rotation: v2 is added and becomes active, v1 stays in the keyring.
	after := service(t, map[byte]byte{1: 0x01, 2: 0x02}, 2)

	opened, err := after.Decrypt(written)
	if err != nil {
		t.Fatalf("a row written under v1 could not be read after rotating to v2: %v", err)
	}
	if string(opened) != "written under v1" {
		t.Fatalf("round trip = %q", opened)
	}
	fresh, _ := after.Encrypt([]byte("written under v2"))
	if fresh[1] != 2 {
		t.Fatalf("new envelope names key version %d, want the active 2", fresh[1])
	}
}

// TestARetiredKeyIsRefusedRatherThanReadAsEmpty.
//
// Dropping v1 from the environment before its rows were rewritten must be loud.
// Returning an empty string would read as "this buyer has no document on file"
// and quietly lose data that is still sitting in the column.
func TestARetiredKeyIsRefusedRatherThanReadAsEmpty(t *testing.T) {
	before := service(t, map[byte]byte{1: 0x01}, 1)
	written, _ := before.Encrypt([]byte("12345678909"))

	only2 := service(t, map[byte]byte{2: 0x02}, 2)

	_, err := only2.Decrypt(written)
	if !errors.Is(err, ErrUnknownKEKVersion) {
		t.Fatalf("Decrypt() error = %v, want %v", err, ErrUnknownKEKVersion)
	}
}

func TestDecryptRejectsGarbage(t *testing.T) {
	s := service(t, map[byte]byte{1: 0x01}, 1)
	for name, envelope := range map[string][]byte{
		"empty":          {},
		"header only":    {EnvelopeFormatV1, 1},
		"unknown format": append([]byte{0x99, 1}, bytes.Repeat([]byte{0}, 32)...),
	} {
		if _, err := s.Decrypt(envelope); err == nil {
			t.Fatalf("%s: Decrypt() succeeded, want an error", name)
		}
	}
}

// TestBlindIndexIsStableAndScoped: stable, or no lookup would ever match;
// scoped, or the same digits in two columns would index identically.
func TestBlindIndexIsStableAndScoped(t *testing.T) {
	s := service(t, map[byte]byte{1: 0x01}, 1)

	first := s.BlindIndex("user.document", "12345678909")
	again := s.BlindIndex("user.document", "12345678909")
	if !bytes.Equal(first, again) {
		t.Fatal("the same value indexed differently twice: no lookup could ever match")
	}

	other := s.BlindIndex("user.phone", "12345678909")
	if bytes.Equal(first, other) {
		t.Fatal("two scopes produced the same index for one value")
	}
}

// TestBlindIndexSeparatesScopeFromValue guards the concatenation mistake: with
// no separator, ("ab","c") and ("a","bc") hash the same input.
func TestBlindIndexSeparatesScopeFromValue(t *testing.T) {
	s := service(t, map[byte]byte{1: 0x01}, 1)

	if bytes.Equal(s.BlindIndex("ab", "c"), s.BlindIndex("a", "bc")) {
		t.Fatal("scope and value are concatenated without a separator")
	}
}

func TestBlindIndexHidesTheValue(t *testing.T) {
	s := service(t, map[byte]byte{1: 0x01}, 1)
	index := s.BlindIndex("user.document", "12345678909")

	if bytes.Contains(index, []byte("12345678909")) {
		t.Fatal("the blind index contains the value it indexes")
	}
	if len(index) != 32 {
		t.Fatalf("index is %d bytes, want a 32-byte HMAC-SHA256", len(index))
	}
}

func TestNewRefusesAnUnusableKeyring(t *testing.T) {
	good, _ := vault.New(key(0x01), 1)
	cases := map[string]struct {
		vaults map[byte]*vault.Vault
		active byte
		blind  []byte
		want   error
	}{
		"no keys":            {map[byte]*vault.Vault{}, 1, key(0xAA), ErrNoActiveKEK},
		"active not present": {map[byte]*vault.Vault{1: good}, 9, key(0xAA), ErrNoActiveKEK},
		"nil vault":          {map[byte]*vault.Vault{1: nil}, 1, key(0xAA), ErrNilVault},
		"short blind key":    {map[byte]*vault.Vault{1: good}, 1, []byte("too short"), ErrBlindIndexKey},
	}
	for name, testCase := range cases {
		if _, err := New(testCase.vaults, testCase.active, testCase.blind); !errors.Is(err, testCase.want) {
			t.Fatalf("%s: New() error = %v, want %v", name, err, testCase.want)
		}
	}
}

// TestTheKeyringCopiesTheBlindKey: a caller that reuses or zeroes its buffer
// must not be able to change the key under a running service.
func TestTheKeyringCopiesTheBlindKey(t *testing.T) {
	item, _ := vault.New(key(0x01), 1)
	blind := key(0xAA)
	s, err := New(map[byte]*vault.Vault{1: item}, 1, blind)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	before := s.BlindIndex("scope", "value")

	for index := range blind {
		blind[index] = 0
	}

	if !bytes.Equal(before, s.BlindIndex("scope", "value")) {
		t.Fatal("zeroing the caller's buffer changed the service's key")
	}
}

// --- loading from the environment ------------------------------------------

func environment(values map[string]string) ([]string, func(string) string) {
	pairs := make([]string, 0, len(values))
	for name, value := range values {
		pairs = append(pairs, name+"="+value)
	}
	return pairs, func(name string) string { return values[name] }
}

func encoded(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

func TestLoadDefaultsToTheHighestKeyVersion(t *testing.T) {
	environ, getenv := environment(map[string]string{
		EnvKEKPrefix + "1":  encoded(key(0x01)),
		EnvKEKPrefix + "2":  encoded(key(0x02)),
		EnvBlindIndexKey:    encoded(key(0xAA)),
		"SOMETHING_ELSE":    "ignored",
		"VOZKOT_PII_KEK_VX": encoded(key(0x03)),
	})

	s, err := LoadFromEnviron(environ, getenv)
	if err != nil {
		t.Fatalf("LoadFromEnviron() error = %v", err)
	}
	if s.ActiveKEKVersion() != 2 {
		t.Fatalf("active version = %d, want the highest, 2", s.ActiveKEKVersion())
	}
}

func TestLoadHonoursAnExplicitActiveVersion(t *testing.T) {
	environ, getenv := environment(map[string]string{
		EnvKEKPrefix + "1":  encoded(key(0x01)),
		EnvKEKPrefix + "2":  encoded(key(0x02)),
		EnvActiveKEKVersion: "1",
		EnvBlindIndexKey:    encoded(key(0xAA)),
	})

	s, err := LoadFromEnviron(environ, getenv)
	if err != nil {
		t.Fatalf("LoadFromEnviron() error = %v", err)
	}
	if s.ActiveKEKVersion() != 1 {
		t.Fatalf("active version = %d, want the configured 1", s.ActiveKEKVersion())
	}
}

func TestLoadRefusesAMisconfiguredKeyring(t *testing.T) {
	cases := map[string]map[string]string{
		"no keys at all": {
			EnvBlindIndexKey: encoded(key(0xAA)),
		},
		"no blind index key": {
			EnvKEKPrefix + "1": encoded(key(0x01)),
		},
		"blind index key too short": {
			EnvKEKPrefix + "1": encoded(key(0x01)),
			EnvBlindIndexKey:   encoded([]byte("short")),
		},
		"key is not 32 bytes": {
			EnvKEKPrefix + "1": encoded([]byte("sixteen bytes!!!")),
			EnvBlindIndexKey:   encoded(key(0xAA)),
		},
		"key is not base64": {
			EnvKEKPrefix + "1": "not base64 at all!!",
			EnvBlindIndexKey:   encoded(key(0xAA)),
		},
		"active names a key that is not set": {
			EnvKEKPrefix + "1":  encoded(key(0x01)),
			EnvActiveKEKVersion: "7",
			EnvBlindIndexKey:    encoded(key(0xAA)),
		},
	}
	for name, values := range cases {
		environ, getenv := environment(values)
		if _, err := LoadFromEnviron(environ, getenv); err == nil {
			t.Fatalf("%s: LoadFromEnviron() succeeded, want a refusal", name)
		}
	}
}

// TestLoadAcceptsUnpaddedBase64: a key copied out of a secret manager arrives
// either way, and refusing over padding is a boot failure nobody can diagnose.
func TestLoadAcceptsUnpaddedBase64(t *testing.T) {
	environ, getenv := environment(map[string]string{
		EnvKEKPrefix + "1": strings.TrimRight(encoded(key(0x01)), "="),
		EnvBlindIndexKey:   strings.TrimRight(encoded(key(0xAA)), "="),
	})

	if _, err := LoadFromEnviron(environ, getenv); err != nil {
		t.Fatalf("LoadFromEnviron() error = %v", err)
	}
}
