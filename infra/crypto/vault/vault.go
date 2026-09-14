// Package vault is authenticated encryption with one key.
//
// AES-256-GCM, and the choice is not a preference: GCM authenticates as well as
// encrypts, so a ciphertext somebody flipped a bit in fails to open rather than
// decrypting into different plaintext. A CPF that silently becomes a different
// CPF is worse than one that cannot be read.
//
// A nonce is generated per message and prepended to the ciphertext. It is not a
// secret, it only has to be unique for the key, and shipping it with the
// message is what lets the same key encrypt more than once.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

var ErrCiphertextShort = errors.New("vault: ciphertext too short")

type Vault struct {
	aead       cipher.AEAD
	kekVersion int
	rand       io.Reader
}

func New(key []byte, kekVersion int) (*Vault, error) {
	return NewWithRand(key, kekVersion, rand.Reader)
}

// NewWithRand takes the randomness source, so a test can prove the nonce is
// actually prepended rather than assume it.
func NewWithRand(key []byte, kekVersion int, r io.Reader) (*Vault, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if r == nil {
		r = rand.Reader
	}
	return &Vault{aead: gcm, kekVersion: kekVersion, rand: r}, nil
}

func (v *Vault) Version() int { return v.kekVersion }

func (v *Vault) NonceSize() int { return v.aead.NonceSize() }

// Seal returns nonce || ciphertext.
func (v *Vault) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(v.rand, nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, plaintext, nil), nil
}

func (v *Vault) Open(envelope []byte) ([]byte, error) {
	size := v.aead.NonceSize()
	if len(envelope) < size+1 {
		return nil, ErrCiphertextShort
	}
	nonce, ciphertext := envelope[:size], envelope[size:]
	return v.aead.Open(nil, nonce, ciphertext, nil)
}
