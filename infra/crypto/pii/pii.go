// Package pii encrypts the fields the law says we must hold and must protect:
// a buyer's document, their legal name, their date of birth, their phone.
//
// Two mechanisms, because encryption alone cannot answer "have I seen this CPF
// before".
//
//  1. ENVELOPE ENCRYPTION. A value is sealed with the active key and stored as
//     `[format][key version][nonce][ciphertext]`. The version is IN the
//     envelope, so a key can be rotated by adding a new one and changing which
//     is active — old rows keep opening under the old key, and nothing has to
//     be re-encrypted in a single terrifying migration.
//
//  2. BLIND INDEX. A keyed HMAC of the same value, stored beside it. AES-GCM
//     produces a different ciphertext every time — that is the point of the
//     nonce — so an encrypted column cannot be searched or made unique. The
//     HMAC is deterministic, so it can be indexed, while still being useless to
//     anyone who takes the database without the key: it is not reversible, and
//     it is not a plain hash an attacker can attack with a list of every valid
//     CPF.
//
// The scope string is what stops one value colliding with itself across
// columns: the index of a phone number and the index of a document are
// different values even when the digits are the same.
package pii

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"

	"vozkot/infra/crypto/vault"
)

const (
	// EnvelopeFormatV1 is the first byte of every envelope. It exists so a
	// future change of scheme is readable rather than a guess.
	EnvelopeFormatV1 byte = 0x01

	headerLen = 2

	// MinBlindIndexKeyLen is 256 bits, matching the HMAC's own output. A
	// shorter key weakens the only thing standing between a stolen database
	// and a list of everyone's documents.
	MinBlindIndexKeyLen = 32
)

var (
	ErrUnknownKEKVersion = errors.New("pii: ciphertext references unknown key version")
	ErrUnknownFormat     = errors.New("pii: unsupported envelope format")
	ErrEnvelopeShort     = errors.New("pii: envelope too short")
	ErrNoActiveKEK       = errors.New("pii: no active key configured")
	ErrBlindIndexKey     = errors.New("pii: blind index key missing or too short")
	ErrNilVault          = errors.New("pii: nil vault in keyring")
)

// Service is the keyring: every key that can DECRYPT, and the one that
// currently encrypts.
type Service struct {
	vaults   map[byte]*vault.Vault
	active   byte
	blindKey []byte
}

func New(vaults map[byte]*vault.Vault, activeVersion byte, blindIndexKey []byte) (*Service, error) {
	if len(vaults) == 0 {
		return nil, ErrNoActiveKEK
	}
	for _, item := range vaults {
		if item == nil {
			return nil, ErrNilVault
		}
	}
	if _, found := vaults[activeVersion]; !found {
		return nil, ErrNoActiveKEK
	}
	if len(blindIndexKey) < MinBlindIndexKeyLen {
		return nil, ErrBlindIndexKey
	}
	// Copied, so a caller that reuses or zeroes its buffer cannot change the
	// key underneath a running service.
	key := make([]byte, len(blindIndexKey))
	copy(key, blindIndexKey)
	return &Service{vaults: vaults, active: activeVersion, blindKey: key}, nil
}

func (s *Service) ActiveKEKVersion() byte { return s.active }

func (s *Service) Encrypt(plaintext []byte) ([]byte, error) {
	sealed, err := s.vaults[s.active].Seal(plaintext)
	if err != nil {
		return nil, err
	}
	envelope := make([]byte, 0, headerLen+len(sealed))
	envelope = append(envelope, EnvelopeFormatV1, s.active)
	return append(envelope, sealed...), nil
}

// Decrypt opens an envelope under whichever key wrote it.
func (s *Service) Decrypt(envelope []byte) ([]byte, error) {
	if len(envelope) < headerLen {
		return nil, ErrEnvelopeShort
	}
	if envelope[0] != EnvelopeFormatV1 {
		return nil, ErrUnknownFormat
	}
	item, found := s.vaults[envelope[1]]
	if !found {
		// A row written by a key this process was not given. Refusing loudly is
		// the only safe answer: returning empty would read as "no document on
		// file" and quietly lose data that is still there.
		return nil, ErrUnknownKEKVersion
	}
	return item.Open(envelope[headerLen:])
}

// BlindIndex is the searchable, non-reversible form of a value.
//
// The scope and the value are separated by a zero byte so that ("ab", "c") and
// ("a", "bc") cannot produce the same index — the classic length-extension
// mistake in a concatenated MAC input.
func (s *Service) BlindIndex(scope, value string) []byte {
	mac := hmac.New(sha256.New, s.blindKey)
	mac.Write([]byte(scope))
	mac.Write([]byte{0x00})
	mac.Write([]byte(value))
	return mac.Sum(nil)
}
