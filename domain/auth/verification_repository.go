package auth

import (
	"context"
	"time"
)

// ChallengeRepository persists outstanding codes.
//
// The two counting methods are not conveniences: they are the rate limit, and
// the rate limit is the only thing standing between this endpoint and using it
// to post six-digit numbers into somebody else's inbox all afternoon. They
// count in the DATABASE rather than in a process, because a limit that lives in
// one replica's memory is not a limit once there are two replicas.
type ChallengeRepository interface {
	Create(ctx context.Context, item *Challenge) error
	// FindLive returns a challenge by id, and takes its row until the enclosing
	// transaction ends.
	//
	// The lock is what makes the attempt counter honest. Read-check-increment
	// without it lets ten concurrent guesses all read "attempts = 0", all pass
	// the cap, and all count as the first try — which turns a five-guess limit
	// into no limit at all for anybody willing to open ten connections.
	FindForUpdate(ctx context.Context, id string) (*Challenge, error)
	Update(ctx context.Context, item *Challenge) error

	// LastSentTo is when a live code was last issued to this destination, for
	// the resend interval. Zero when there has been none.
	LastSentTo(ctx context.Context, purpose Purpose, destinationIndex []byte) (time.Time, error)
	// CountRecent is how many codes this destination has been sent since
	// `since`, for the hourly ceiling.
	CountRecent(ctx context.Context, purpose Purpose, destinationIndex []byte, since time.Time) (int, error)

	// DeleteExpired clears out spent and lapsed challenges. They are short
	// lived by design, and keeping them forever would be retaining a log of
	// who signed in and when, which is exactly what this table is shaped to
	// avoid holding.
	DeleteExpired(ctx context.Context, before time.Time, limit int) (int, error)
}
