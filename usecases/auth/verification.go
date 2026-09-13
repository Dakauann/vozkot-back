package auth

import (
	"context"
	"errors"
	"log"
	"net/mail"
	"strings"
	"time"

	domain "vozkot/domain/auth"
	"vozkot/domain/user"
)

// Passwordless sign-in: prove you can read an address, and you are in.
//
// The threat model is worth stating, because it is not the same as a password's.
// There is no secret to steal and no secret to reuse across sites — which
// removes the two biggest causes of account takeover outright. What it adds is
// that the mailbox becomes the account, and that the code in flight is a live
// credential for a few minutes. So the code is random, hashed at rest, capped
// at five guesses, single use, short lived, and bound to the purpose it was
// issued for.
//
// The other property this file is careful about is ENUMERATION. Starting a
// sign-in answers identically whether or not the address has an account: same
// status, same body, same shape. Anything else turns the endpoint into a way to
// ask "does this person have an account here", which for a ticketing site is a
// question about who went to which events.

// Sender delivers a code to a destination. Email is the only implementation
// that reaches a real person today; the phone one is deliberately a stub.
type Sender interface {
	Send(ctx context.Context, destination, code string, purpose domain.Purpose) error
}

// Verification is the sign-in and confirmation use case.
type Verification struct {
	users      user.Repository
	challenges domain.ChallengeRepository
	hasher     domain.Hasher
	// blindIndex derives the searchable form of a destination. Injected rather
	// than reached for, so this package never imports the crypto layer.
	blindIndex func(scope, value string) []byte
	email      Sender
	phone      Sender
	sessions   *Service
	now        func() time.Time
	newID      func(prefix string) string
}

func NewVerification(
	users user.Repository,
	challenges domain.ChallengeRepository,
	hasher domain.Hasher,
	blindIndex func(scope, value string) []byte,
	email Sender,
	phone Sender,
	sessions *Service,
) *Verification {
	return &Verification{
		users:      users,
		challenges: challenges,
		hasher:     hasher,
		blindIndex: blindIndex,
		email:      email,
		phone:      phone,
		sessions:   sessions,
		now:        time.Now,
		newID:      newID,
	}
}

// Started is what the caller is told after a code goes out.
//
// It carries no hint about whether the address is known. The challenge id is
// safe to return — it is random, it is useless without the code, and the client
// needs something to send the code back against.
type Started struct {
	ChallengeID string
	ExpiresAt   time.Time
	// ResendAt is when another code may be asked for, so the UI can show a real
	// countdown instead of guessing.
	ResendAt time.Time
}

// StartEmailSignIn issues a code to an email address.
func (v *Verification) StartEmailSignIn(ctx context.Context, address string) (Started, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if _, err := mail.ParseAddress(address); err != nil {
		return Started{}, domain.ErrInvalidContact
	}
	return v.start(ctx, domain.PurposeSignIn, address, "", v.email)
}

// StartPhoneVerification issues a code to a phone number, for an account that
// already exists.
func (v *Verification) StartPhoneVerification(ctx context.Context, userID, rawPhone string) (Started, error) {
	phone, err := user.NormalizePhone(rawPhone)
	if err != nil {
		return Started{}, err
	}
	if strings.TrimSpace(userID) == "" {
		return Started{}, domain.ErrUnauthorized
	}
	return v.start(ctx, domain.PurposePhone, phone, userID, v.phone)
}

// start is the shared issuing path: rate limit, generate, hash, store, send.
func (v *Verification) start(
	ctx context.Context,
	purpose domain.Purpose,
	destination, userID string,
	sender Sender,
) (Started, error) {
	now := v.now().UTC()
	index := v.blindIndex(destinationScope, destination)

	// Two limits, because they stop two different things. The interval stops a
	// held-down button; the hourly count stops a patient script using this
	// endpoint to post six-digit numbers into somebody else's inbox all
	// afternoon.
	last, err := v.challenges.LastSentTo(ctx, purpose, index)
	if err != nil {
		return Started{}, err
	}
	if !last.IsZero() && now.Sub(last) < domain.ResendInterval {
		return Started{}, domain.ErrResendTooSoon
	}
	recent, err := v.challenges.CountRecent(ctx, purpose, index, now.Add(-domain.ChallengeWindow))
	if err != nil {
		return Started{}, err
	}
	if recent >= domain.MaxChallengesPerDestination {
		return Started{}, domain.ErrTooManyRequests
	}

	code, err := domain.NewCode()
	if err != nil {
		return Started{}, err
	}
	hash, err := v.hasher.Hash(code)
	if err != nil {
		return Started{}, err
	}

	challenge := &domain.Challenge{
		ID:               v.newID("vch"),
		Purpose:          purpose,
		DestinationIndex: index,
		Destination:      destination,
		CodeHash:         hash,
		UserID:           userID,
		ExpiresAt:        now.Add(domain.CodeTTL),
		CreatedAt:        now,
	}
	if err := v.challenges.Create(ctx, challenge); err != nil {
		return Started{}, err
	}

	// Sent after the row is committed, never before. A code that reached
	// somebody's inbox but was not stored is a code that cannot be accepted,
	// and the person holding it has no way to know that.
	if err := sender.Send(ctx, destination, code, purpose); err != nil {
		// The challenge stays. It expires on its own, and it keeps counting
		// against the rate limit — which is correct: a failing mail provider
		// must not become an unlimited retry loop.
		return Started{}, err
	}

	return Started{
		ChallengeID: challenge.ID,
		ExpiresAt:   challenge.ExpiresAt,
		ResendAt:    now.Add(domain.ResendInterval),
	}, nil
}

// destinationScope namespaces the blind index for challenge destinations.
const destinationScope = "verification.destination.v1"

// VerifiedSignIn is the result of a correct sign-in code.
type VerifiedSignIn struct {
	Tokens *domain.TokenPair
	// Created reports whether this code made the account rather than found it.
	// The UI uses it to decide whether to ask for the identity block next.
	Created bool
}

// VerifyEmailSignIn checks a code and signs the person in, creating the account
// on first use.
//
// Registration and sign-in are the same call on purpose. Separating them would
// mean answering "is this address already registered" before a code is even
// sent, which is the enumeration oracle the whole flow is shaped to avoid.
func (v *Verification) VerifyEmailSignIn(
	ctx context.Context,
	challengeID, code, ipAddress, deviceInfo string,
) (*VerifiedSignIn, error) {
	challenge, err := v.consume(ctx, challengeID, code, domain.PurposeSignIn)
	if err != nil {
		return nil, err
	}

	address := challenge.Destination
	existing, err := v.users.FindByEmail(ctx, address)
	switch {
	case err == nil:
		if existing.DisabledAt != nil {
			return nil, domain.ErrInvalidCredentials
		}
		tokens, err := v.sessions.StartSessionFor(ctx, existing, ipAddress, deviceInfo)
		if err != nil {
			return nil, err
		}
		return &VerifiedSignIn{Tokens: tokens, Created: false}, nil
	case errors.Is(err, user.ErrNotFound):
		// First time. The account is made now, with no password — there is
		// none to make — and with no name either: the name arrives with the
		// identity block, and inventing one from the address would put a
		// mangled string on somebody's ticket.
		now := v.now().UTC()
		account := &user.User{
			ID:        v.newID("usr"),
			Email:     address,
			Role:      user.RoleUser,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := v.users.Create(ctx, account); err != nil {
			// Two codes for one new address, verified at the same moment. The
			// unique index refuses the second; the person is signed in to the
			// account the first one made.
			if errors.Is(err, user.ErrEmailAlreadyExists) {
				found, findErr := v.users.FindByEmail(ctx, address)
				if findErr != nil {
					return nil, err
				}
				tokens, tokenErr := v.sessions.StartSessionFor(ctx, found, ipAddress, deviceInfo)
				if tokenErr != nil {
					return nil, tokenErr
				}
				return &VerifiedSignIn{Tokens: tokens, Created: false}, nil
			}
			return nil, err
		}
		tokens, err := v.sessions.StartSessionFor(ctx, account, ipAddress, deviceInfo)
		if err != nil {
			return nil, err
		}
		return &VerifiedSignIn{Tokens: tokens, Created: true}, nil
	default:
		return nil, err
	}
}

// VerifyPhone checks a phone code and marks the number proven.
func (v *Verification) VerifyPhone(ctx context.Context, userID, challengeID, code string) (*user.User, error) {
	challenge, err := v.consume(ctx, challengeID, code, domain.PurposePhone)
	if err != nil {
		return nil, err
	}
	// The challenge knows whose it is. A code issued for one account must not
	// confirm a number on another, however the ids were supplied.
	if challenge.UserID != userID {
		return nil, domain.ErrChallengeNotFound
	}

	account, err := v.users.FindByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := v.now().UTC()
	profile := account.Profile
	profile.Phone = challenge.Destination
	profile.PhoneVerifiedAt = &now
	if err := v.users.SaveProfile(ctx, userID, profile); err != nil {
		return nil, err
	}
	account.Profile = profile
	return account, nil
}

// consume validates a code and spends the challenge.
//
// Everything that decides the outcome happens under the row lock the repository
// takes: read, check, count the attempt, and mark it used. Without the lock,
// concurrent guesses all read the same attempt count and the five-guess cap
// stops being a cap.
func (v *Verification) consume(
	ctx context.Context,
	challengeID, code string,
	purpose domain.Purpose,
) (*domain.Challenge, error) {
	normalized := domain.NormalizeCode(code)
	if len(normalized) != domain.CodeLength {
		// Not counted as an attempt: it cannot be a code, so counting it would
		// let a bystander burn somebody else's five guesses with junk.
		return nil, domain.ErrInvalidCode
	}

	challenge, err := v.challenges.FindForUpdate(ctx, strings.TrimSpace(challengeID))
	if err != nil {
		return nil, err
	}
	// A sign-in code answered at the phone endpoint, or the reverse.
	if challenge.Purpose != purpose {
		return nil, domain.ErrChallengeNotFound
	}
	if err := challenge.Live(v.now()); err != nil {
		return nil, err
	}

	if verifyErr := v.hasher.Verify(challenge.CodeHash, normalized); verifyErr != nil {
		// Counted BEFORE returning, and persisted even though the request
		// fails: an attempt that is only recorded on success is not a limit.
		challenge.Attempts++
		if err := v.challenges.Update(ctx, challenge); err != nil {
			return nil, err
		}
		if challenge.Attempts >= domain.MaxCodeAttempts {
			return nil, domain.ErrTooManyAttempts
		}
		return nil, domain.ErrInvalidCode
	}

	challenge.Consume(v.now())
	if err := v.challenges.Update(ctx, challenge); err != nil {
		return nil, err
	}
	return challenge, nil
}

// SweepExpiredChallenges clears out codes that can no longer be answered.
//
// They are deliberately short lived, and keeping them afterwards would be
// retaining a record of who signed in and when — which is the thing this
// table's shape is designed not to hold.
func (v *Verification) SweepExpiredChallenges(ctx context.Context, limit int) (int, error) {
	removed, err := v.challenges.DeleteExpired(ctx, v.now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	if removed > 0 {
		log.Printf("auth: removed %d expired verification challenge(s)", removed)
	}
	return removed, nil
}
