package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"math/big"
	"strings"
	"time"
)

// A one-time code sent to something the person controls.
//
// This is the whole of "sign in", and it replaces a password rather than
// supplementing it. The security of the scheme therefore rests entirely on the
// four properties below, and every one of them is load-bearing:
//
//  1. The code is CRYPTOGRAPHICALLY RANDOM. A six-digit code from math/rand is
//     predictable from a couple of observations, and predicting it is a
//     complete account takeover with no password to also guess.
//  2. Only a HASH of it is stored. A database that leaks live codes is a
//     database that leaks live sessions for every address currently signing in.
//  3. Attempts are CAPPED and counted server-side. A million codes falls in
//     minutes at unlimited speed; five tries makes it 1 in 200,000 per
//     challenge and the challenge dies.
//  4. It is SINGLE USE and SHORT LIVED. A code that still works an hour later
//     is a password sitting in an inbox.
//
// Comparison is constant-time throughout. The timing of a digit-by-digit
// compare leaks the code one digit at a time, which reduces a million guesses
// to sixty.

// Purpose says what proving control of the destination is FOR. It is part of
// the stored challenge, so a code issued to confirm a phone number cannot be
// replayed against the sign-in endpoint.
type Purpose string

const (
	// PurposeSignIn proves an email address, and signs in or registers.
	PurposeSignIn Purpose = "sign_in"
	// PurposePhone proves a phone number for an account that already exists.
	PurposePhone Purpose = "phone"
)

func (p Purpose) Valid() bool {
	return p == PurposeSignIn || p == PurposePhone
}

const (
	// CodeLength is six digits. Long enough that five guesses are hopeless,
	// short enough to carry across a room from a phone screen.
	CodeLength = 6
	// MaxCodeAttempts is how many wrong guesses a challenge survives.
	MaxCodeAttempts = 5
	// CodeTTL is how long a code is worth anything.
	CodeTTL = 10 * time.Minute
	// ResendInterval is the shortest gap between two codes for one
	// destination. It bounds both the mail bill and the use of this endpoint
	// as a way to flood somebody else's inbox.
	ResendInterval = 60 * time.Second
	// MaxChallengesPerDestination caps how many codes one address may be sent
	// within ChallengeWindow, however patiently the requests are spaced.
	MaxChallengesPerDestination = 5
	ChallengeWindow             = time.Hour
)

var (
	// ErrChallengeNotFound covers "no such challenge" and "not yours". The two
	// are deliberately indistinguishable: telling a caller that a challenge id
	// exists but belongs to someone else is an oracle.
	ErrChallengeNotFound = errors.New("verification challenge not found")
	ErrChallengeExpired  = errors.New("this code has expired")
	ErrChallengeUsed     = errors.New("this code has already been used")
	// ErrTooManyAttempts means the challenge is spent, not that the last guess
	// was close.
	ErrTooManyAttempts = errors.New("too many incorrect attempts; request a new code")
	ErrInvalidCode     = errors.New("the code is incorrect")
	ErrResendTooSoon   = errors.New("a code was just sent; wait a moment before asking for another")
	ErrTooManyRequests = errors.New("too many codes requested for this address; try again later")
	ErrInvalidPurpose  = errors.New("verification purpose is invalid")
	ErrInvalidContact  = errors.New("destination is invalid")
	// ErrDeliveryUnavailable means there is no configured way to get a code to
	// this destination. It is a 503, not a 500: nothing is broken, the channel
	// simply is not wired, and the caller should be told to try another way in
	// rather than shown a stack trace.
	//
	// This is what the system does INSTEAD of logging the code. A one-time code
	// is a live credential; printing one to stdout puts it in whatever collects
	// stdout, for as long as that retains logs, readable by everyone who can
	// read them.
	ErrDeliveryUnavailable = errors.New("no configured way to deliver a verification code")
)

// Challenge is one outstanding code.
//
// The destination is stored as a blind index rather than as an address, so the
// table cannot be read as a list of who has been signing in. CodeHash is a hash
// of the code; nothing here ever holds the code itself after it has been sent.
type Challenge struct {
	ID      string
	Purpose Purpose
	// DestinationIndex is the blind index of the email or phone.
	DestinationIndex []byte
	// Destination is the address itself, encrypted. It is needed to actually
	// send the message and to create the account on success.
	Destination string
	CodeHash    string
	Attempts    int
	// UserID ties a phone challenge to the account it is for. Empty for
	// sign-in, where the account may not exist yet.
	UserID     string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}

// Hasher hashes and checks a code. It is the same contract a password takes,
// and it is deliberately the same implementation: a six-digit code is a weak
// secret, and a slow hash is what makes a leaked table expensive to attack.
type Hasher interface {
	Hash(plain string) (string, error)
	Verify(hash, plain string) error
}

// NewCode returns a cryptographically random numeric code.
//
// crypto/rand, and rejection-free by construction: each digit is drawn from
// exactly ten values, so there is no modulo bias to reason about. A leading
// zero is kept, because trimming it would quietly shrink the space.
func NewCode() (string, error) {
	digits := make([]byte, CodeLength)
	for index := range digits {
		drawn, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		digits[index] = byte('0' + drawn.Int64())
	}
	return string(digits), nil
}

// NormalizeCode strips what people type around a code: spaces from reading it
// aloud, and the dash some clients insert. It does NOT strip anything that
// would change the digits.
func NormalizeCode(code string) string {
	var builder strings.Builder
	for _, char := range code {
		if char >= '0' && char <= '9' {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

// Live reports whether a challenge can still be answered.
func (c *Challenge) Live(now time.Time) error {
	if c.ConsumedAt != nil {
		return ErrChallengeUsed
	}
	if !now.UTC().Before(c.ExpiresAt) {
		return ErrChallengeExpired
	}
	if c.Attempts >= MaxCodeAttempts {
		return ErrTooManyAttempts
	}
	return nil
}

// Consume marks the challenge spent. Called only after a correct code, and
// inside the same transaction as whatever the code authorised, a challenge
// that was accepted but not consumed is a code that works twice.
func (c *Challenge) Consume(now time.Time) {
	timestamp := now.UTC()
	c.ConsumedAt = &timestamp
}

// SameCode compares two already-hashed values in constant time.
//
// Used where a hash is compared to a hash, the Hasher handles the plaintext
// path. Kept here so no caller is tempted to write ==.
func SameCode(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
