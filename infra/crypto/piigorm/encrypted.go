// Package piigorm puts the encryption at the database boundary.
//
// A column typed EncryptedString is ciphertext in PostgreSQL and plaintext in
// Go, and the conversion happens in Scan and Value, which is to say, in the
// two places every read and every write must pass through. Nothing above this
// layer can forget to encrypt, because there is no path that writes the column
// without calling Value.
//
// The service is package-level state, set once at startup. That is a deliberate
// trade: threading a keyring through every repository, schema struct and GORM
// callback would be honest and would also mean one forgotten parameter writes a
// document in the clear. A missing service fails every read and write loudly
// instead.
package piigorm

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"

	"vozkot/infra/crypto/pii"
)

var ErrServiceNotConfigured = errors.New("piigorm: encryption service not configured (call SetService at startup)")

var (
	mutex   sync.RWMutex
	service *pii.Service
)

func SetService(s *pii.Service) {
	mutex.Lock()
	service = s
	mutex.Unlock()
}

func Service() *pii.Service {
	mutex.RLock()
	defer mutex.RUnlock()
	return service
}

func mustService() (*pii.Service, error) {
	if current := Service(); current != nil {
		return current, nil
	}
	return nil, ErrServiceNotConfigured
}

// EncryptedString is a string that is only ever ciphertext at rest.
//
// Valid distinguishes "no value" from "the empty string", the way sql.NullString
// does; an absent document and a document recorded as blank are different
// facts, and a NULL column is not something to encrypt.
type EncryptedString struct {
	Plain string
	Valid bool
}

func NewEncrypted(value string) EncryptedString {
	return EncryptedString{Plain: value, Valid: true}
}

func Null() EncryptedString { return EncryptedString{} }

func (e *EncryptedString) Scan(src any) error {
	if src == nil {
		*e = EncryptedString{}
		return nil
	}
	var raw []byte
	switch value := src.(type) {
	case []byte:
		raw = value
	case string:
		raw = []byte(value)
	default:
		return fmt.Errorf("piigorm: unsupported scan type %T", src)
	}
	if len(raw) == 0 {
		*e = EncryptedString{}
		return nil
	}
	current, err := mustService()
	if err != nil {
		return err
	}
	plain, err := current.Decrypt(raw)
	if err != nil {
		return fmt.Errorf("piigorm: decrypt: %w", err)
	}
	e.Plain = string(plain)
	e.Valid = true
	return nil
}

func (e EncryptedString) Value() (driver.Value, error) {
	if !e.Valid {
		return nil, nil
	}
	current, err := mustService()
	if err != nil {
		return nil, err
	}
	return current.Encrypt([]byte(e.Plain))
}

func (EncryptedString) GormDataType() string { return "bytea" }

// String returns the plaintext, and exists so a template or a log line that
// interpolates one of these gets the value rather than a struct dump. Anything
// that prints a document is a decision; this only stops it being an accident in
// the other direction.
func (e EncryptedString) String() string {
	if !e.Valid {
		return ""
	}
	return e.Plain
}

// BlindIndex is the deterministic, indexable companion to an EncryptedString.
//
// It goes in its own column with its own unique or plain index, and it is what
// every WHERE clause over a protected value actually compares.
type BlindIndex []byte

// NewBlindIndex derives the index for a value within a scope. An empty value
// yields nil, which stores as NULL, so "no document" does not collide with
// every other row that also has none under a unique index.
func NewBlindIndex(scope, value string) (BlindIndex, error) {
	if value == "" {
		return nil, nil
	}
	current, err := mustService()
	if err != nil {
		return nil, err
	}
	return BlindIndex(current.BlindIndex(scope, value)), nil
}

func (b *BlindIndex) Scan(src any) error {
	if src == nil {
		*b = nil
		return nil
	}
	switch value := src.(type) {
	case []byte:
		// Copied: the driver may reuse its buffer for the next row.
		buffer := make([]byte, len(value))
		copy(buffer, value)
		*b = buffer
		return nil
	case string:
		*b = []byte(value)
		return nil
	default:
		return fmt.Errorf("piigorm: unsupported scan type %T for BlindIndex", src)
	}
}

func (b BlindIndex) Value() (driver.Value, error) {
	if len(b) == 0 {
		return nil, nil
	}
	return []byte(b), nil
}

func (BlindIndex) GormDataType() string { return "bytea" }
