package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	domain "vozkot/domain/auth"
	"vozkot/infra/testsupport"
)

// What the challenge sweep is allowed to delete, against real PostgreSQL.
//
// THE BUG THESE EXIST FOR, which shipped: the sweep deleted on expires_at while
// the per-destination ceiling counts on created_at over ChallengeWindow. A code
// dies after ten minutes and the window is an hour, so every row between those
// two ages, which is most of the hour the ceiling is counted from, was deleted
// within a minute of expiring. The limit of five codes an hour to one address
// silently became five every ten minutes: send five, wait for the sweep, send
// five more. That is the inbox flood the ceiling exists to stop, and nothing
// failed, because nothing tested this sweep at all.
//
// So these assert the two ages separately: a row young enough to still be
// counted survives, and a row past the window goes.

type challengeHarness struct {
	db    *gorm.DB
	repo  *ChallengeRepository
	index []byte
	now   time.Time
}

func newChallengeHarness(t *testing.T) *challengeHarness {
	t.Helper()
	// The destination is sealed at rest, so writing one needs the keyring.
	testsupport.Encryption(t)
	db := testsupport.Database(t)

	// A blind index of this test's own, so a count is this test's rows and not
	// whatever else the database is holding.
	index := []byte(testsupport.Unique("idx"))
	t.Cleanup(func() {
		db.Exec("DELETE FROM verification_challenges WHERE destination_blind = ?", index)
	})

	return &challengeHarness{
		db:    db,
		repo:  NewChallengeRepository(db),
		index: index,
		now:   time.Now().UTC(),
	}
}

// seed writes one challenge aged `age`, dated exactly as start() would have.
func (h *challengeHarness) seed(t *testing.T, age time.Duration) string {
	t.Helper()
	created := h.now.Add(-age)
	id := testsupport.Unique("vch")
	err := h.repo.Create(context.Background(), &domain.Challenge{
		ID:               id,
		Purpose:          domain.PurposeSignIn,
		DestinationIndex: h.index,
		Destination:      "alguem@vozkot.test",
		CodeHash:         "$2a$04$notarealhashbutlongenoughtostore",
		ExpiresAt:        created.Add(domain.CodeTTL),
		CreatedAt:        created,
	})
	if err != nil {
		t.Fatalf("seed challenge aged %s: %v", age, err)
	}
	return id
}

func (h *challengeHarness) exists(t *testing.T, id string) bool {
	t.Helper()
	var total int64
	if err := h.db.Table("verification_challenges").
		Where("id = ?", id).Count(&total).Error; err != nil {
		t.Fatalf("count challenge %s: %v", id, err)
	}
	return total > 0
}

// sweep runs it exactly as the worker does.
func (h *challengeHarness) sweep(t *testing.T, limit int) int {
	t.Helper()
	removed, err := h.repo.DeleteOlderThan(context.Background(), h.now.Add(-domain.ChallengeWindow), limit)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return removed
}

func (h *challengeHarness) countRecent(t *testing.T) int {
	t.Helper()
	total, err := h.repo.CountRecent(
		context.Background(), domain.PurposeSignIn, h.index, h.now.Add(-domain.ChallengeWindow),
	)
	if err != nil {
		t.Fatalf("count recent: %v", err)
	}
	return total
}

// The regression. A row older than its code but younger than the window is
// precisely what the ceiling is made of.
func TestTheSweepKeepsTheRowsTheCeilingCounts(t *testing.T) {
	h := newChallengeHarness(t)
	// Thirty minutes: the code died twenty minutes ago, since CodeTTL is ten,
	// and the hour-long window still has half of it to run.
	kept := h.seed(t, 30*time.Minute)

	h.sweep(t, 500)

	if !h.exists(t, kept) {
		t.Fatal("the sweep deleted a challenge that is still inside ChallengeWindow: " +
			"the per-destination ceiling counts these rows, so deleting them turns " +
			"five codes an hour into five every ten minutes")
	}
	if got := h.countRecent(t); got != 1 {
		t.Errorf("the ceiling counts %d challenges after the sweep, want 1: "+
			"a sweep that empties the counter is a rate limit that resets itself", got)
	}
}

// Five codes, the ceiling's worth, must still all be counted afterwards. One
// surviving row could be luck; the whole allowance surviving is the limit.
func TestAFullAllowanceSurvivesTheSweep(t *testing.T) {
	h := newChallengeHarness(t)
	// Spread across the window and all past CodeTTL, which is what an hour of
	// a held-down resend button leaves behind.
	for _, age := range []time.Duration{
		11 * time.Minute, 20 * time.Minute, 35 * time.Minute, 47 * time.Minute, 58 * time.Minute,
	} {
		h.seed(t, age)
	}

	h.sweep(t, 500)

	if got := h.countRecent(t); got != domain.MaxChallengesPerDestination {
		t.Errorf("the ceiling counts %d challenges after the sweep, want %d: "+
			"the destination should be at its limit and refused, not handed a fresh allowance",
			got, domain.MaxChallengesPerDestination)
	}
}

// And the sweep still has to do its job, or the table becomes the log of who
// signed in and when that this schema is shaped to avoid.
func TestTheSweepRemovesChallengesPastTheWindow(t *testing.T) {
	h := newChallengeHarness(t)
	old := h.seed(t, domain.ChallengeWindow+time.Minute)
	young := h.seed(t, time.Minute)

	h.sweep(t, 500)

	if h.exists(t, old) {
		t.Error("a challenge past ChallengeWindow survived the sweep: it can no longer be " +
			"answered and no longer counts for anything, so it is only a retained record " +
			"of who asked to sign in and when")
	}
	if !h.exists(t, young) {
		t.Error("the sweep deleted a challenge issued a minute ago")
	}
	if got := h.countRecent(t); got != 1 {
		t.Errorf("the ceiling counts %d challenges, want 1: only the young one remains", got)
	}
}

// The bound is what keeps this a short statement rather than one that holds
// locks across the table while somebody is signing in.
func TestTheSweepRespectsItsLimit(t *testing.T) {
	h := newChallengeHarness(t)
	for range 3 {
		h.seed(t, domain.ChallengeWindow+time.Hour)
	}

	// Asserted as an upper bound, not an exact count: the sweep is table-wide
	// and this database may hold other expired challenges of its own.
	if removed := h.sweep(t, 2); removed > 2 {
		t.Errorf("the sweep removed %d rows with a limit of 2", removed)
	}
}

// Retention is NOT what makes an expired code unusable, and this is the test
// that says so: the read path refuses a live row whose code has lapsed, so the
// sweep can safely keep it for the hour the ceiling needs.
func TestAnUnsweptCodeIsStillRefusedOnceItExpires(t *testing.T) {
	h := newChallengeHarness(t)
	id := h.seed(t, 30*time.Minute)

	h.sweep(t, 500)

	found, err := h.repo.FindForUpdate(context.Background(), id)
	if err != nil {
		t.Fatalf("read the kept challenge: %v", err)
	}
	if err := found.Live(h.now); err == nil {
		t.Fatal("a challenge kept for the ceiling was accepted as answerable: " +
			"retention must not extend the ten minutes a code is good for")
	} else if !errors.Is(err, domain.ErrChallengeExpired) {
		t.Errorf("refused with %v, want %v", err, domain.ErrChallengeExpired)
	}
}
