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
	// the cap, and all count as the first try, which turns a five-guess limit
	// into no limit at all for anybody willing to open ten connections.
	FindForUpdate(ctx context.Context, id string) (*Challenge, error)
	Update(ctx context.Context, item *Challenge) error

	// LastSentTo is when a live code was last issued to this destination, for
	// the resend interval. Zero when there has been none.
	LastSentTo(ctx context.Context, purpose Purpose, destinationIndex []byte) (time.Time, error)
	// CountRecent is how many codes this destination has been sent since
	// `since`, for the hourly ceiling.
	CountRecent(ctx context.Context, purpose Purpose, destinationIndex []byte, since time.Time) (int, error)

	// DeleteOlderThan clears out challenges created before the cutoff. They are
	// short lived by design, and keeping them forever would be retaining a log
	// of who signed in and when, which is exactly what this table is shaped to
	// avoid holding.
	//
	// Keyed on CREATED, never on expiry, and that distinction is the whole
	// point. A row stops being answerable after CodeTTL, which Live() enforces
	// on the read path, but it keeps doing a second job for ChallengeWindow:
	// it is what CountRecent counts, and the ceiling of
	// MaxChallengesPerDestination is only ever as good as the rows still there
	// to be counted.
	//
	// Sweeping on expires_at deleted exactly the ten-to-sixty-minute-old rows
	// that ceiling is made of, an hour's evidence erased ten minutes in. That
	// turned "five codes an hour" into five every ten minutes, roughly thirty,
	// which is the inbox flood the ceiling exists to stop.
	DeleteOlderThan(ctx context.Context, cutoff time.Time, limit int) (int, error)
}
