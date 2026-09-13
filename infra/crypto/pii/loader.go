package pii

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"vozkot/infra/crypto/vault"
)

// The keys come from the environment, one variable per version:
//
//	VOZKOT_PII_KEK_V1=<base64 of 32 random bytes>
//	VOZKOT_PII_KEK_V2=<base64 of 32 random bytes>   # after a rotation
//	VOZKOT_PII_ACTIVE_KEK_VERSION=2
//	VOZKOT_PII_BLIND_INDEX_KEY=<base64 of >= 32 random bytes>
//
// Rotation is therefore: add V2, point ACTIVE at it, deploy. Every row written
// from then on uses V2; every row already written keeps opening under V1, which
// stays in the environment until those rows have been rewritten. Removing V1
// before that is what makes old data unreadable, so nothing here removes it for
// you.
//
// The blind index key is NOT versioned, and cannot be: changing it changes
// every index derived from it, which would orphan every lookup at once.
// Rotating it is a re-index of the whole table and a deliberate operation.
const (
	EnvKEKPrefix        = "VOZKOT_PII_KEK_V"
	EnvActiveKEKVersion = "VOZKOT_PII_ACTIVE_KEK_VERSION"
	EnvBlindIndexKey    = "VOZKOT_PII_BLIND_INDEX_KEY"
)

// LoadFromEnv builds the keyring from the process environment.
func LoadFromEnv() (*Service, error) {
	return LoadFromEnviron(os.Environ(), os.Getenv)
}

// LoadFromEnviron is LoadFromEnv with the environment injected, so the parsing
// rules can be tested without setting real variables in the test process.
func LoadFromEnviron(environ []string, getenv func(string) string) (*Service, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	keys, err := collectKEKs(environ, getenv)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no %s<n> variables set", ErrNoActiveKEK, EnvKEKPrefix)
	}

	vaults := make(map[byte]*vault.Vault, len(keys))
	for version, raw := range keys {
		item, err := vault.New(raw, int(version))
		if err != nil {
			return nil, fmt.Errorf("pii: %s%d is not a usable key: %w", EnvKEKPrefix, version, err)
		}
		vaults[version] = item
	}

	active, err := resolveActiveVersion(getenv, keys)
	if err != nil {
		return nil, err
	}

	raw := strings.TrimSpace(getenv(EnvBlindIndexKey))
	if raw == "" {
		return nil, fmt.Errorf("%w: %s not set", ErrBlindIndexKey, EnvBlindIndexKey)
	}
	blindKey, err := decodeKey(EnvBlindIndexKey, raw, 0)
	if err != nil {
		return nil, err
	}
	return New(vaults, active, blindKey)
}

func collectKEKs(environ []string, getenv func(string) string) (map[byte][]byte, error) {
	out := make(map[byte][]byte)
	for _, entry := range environ {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 {
			continue
		}
		name := entry[:separator]
		if !strings.HasPrefix(name, EnvKEKPrefix) {
			continue
		}
		version, err := strconv.Atoi(name[len(EnvKEKPrefix):])
		if err != nil || version <= 0 || version > 255 {
			continue
		}
		value := getenv(name)
		if value == "" {
			continue
		}
		key, err := decodeKey(name, value, 32)
		if err != nil {
			return nil, err
		}
		out[byte(version)] = key
	}
	return out, nil
}

// resolveActiveVersion honours an explicit choice, and otherwise takes the
// highest version present — so adding a key without saying which is active
// starts using it, which is the thing an operator adding a key meant.
func resolveActiveVersion(getenv func(string) string, keys map[byte][]byte) (byte, error) {
	if raw := strings.TrimSpace(getenv(EnvActiveKEKVersion)); raw != "" {
		version, err := strconv.Atoi(raw)
		if err != nil || version <= 0 || version > 255 {
			return 0, fmt.Errorf("%w: %s=%q is not a number between 1 and 255",
				ErrNoActiveKEK, EnvActiveKEKVersion, raw)
		}
		if _, found := keys[byte(version)]; !found {
			return 0, fmt.Errorf("%w: %s=%d but %s%d is not set",
				ErrNoActiveKEK, EnvActiveKEKVersion, version, EnvKEKPrefix, version)
		}
		return byte(version), nil
	}

	versions := make([]int, 0, len(keys))
	for version := range keys {
		versions = append(versions, int(version))
	}
	sort.Ints(versions)
	return byte(versions[len(versions)-1]), nil
}

func decodeKey(name, encoded string, requireLen int) ([]byte, error) {
	trimmed := strings.TrimSpace(encoded)
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		// Accept unpadded base64 too: a key copied out of a secret manager is
		// as likely to arrive one way as the other, and refusing over padding
		// is a boot failure nobody can diagnose from the message.
		unpadded, unpaddedErr := base64.RawStdEncoding.DecodeString(trimmed)
		if unpaddedErr != nil {
			return nil, fmt.Errorf("pii: %s is not valid base64: %w", name, err)
		}
		raw = unpadded
	}
	if requireLen > 0 && len(raw) != requireLen {
		return nil, fmt.Errorf("pii: %s must decode to %d bytes, got %d", name, requireLen, len(raw))
	}
	if requireLen == 0 && len(raw) < MinBlindIndexKeyLen {
		return nil, fmt.Errorf("%w: %s decoded to %d bytes, need at least %d",
			ErrBlindIndexKey, name, len(raw), MinBlindIndexKeyLen)
	}
	return raw, nil
}
