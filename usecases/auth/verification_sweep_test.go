package auth

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	domain "vozkot/domain/auth"
	authRepository "vozkot/infra/repositories/auth"
	"vozkot/infra/testsupport"
)

// Which cutoff the sweep hands the repository.
//
// This is the OTHER HALF of the bug the repository's own tests cover, and the
// half they cannot see: those call DeleteOlderThan with a cutoff of their own,
// so a correct statement driven by the wrong cutoff passes every one of them.
// The shipped fault was exactly that pairing, a delete keyed on expiry handed
// `now`, which between them erased an hour of rate-limit history ten minutes
// in. So this drives the real use case against real PostgreSQL and asserts on
// the ages that survive it.

func TestTheSweepKeepsAnHoursWorthOfChallenges(t *testing.T) {
	db := testsupport.Database(t)
	repository := authRepository.NewChallengeRepository(db)
	// This test's own destination, so a count is its rows and not whatever
	// else the database is holding.
	index := []byte(testsupport.Unique("idx"))
	purpose := string(domain.PurposeSignIn)

	// Thirty minutes old: the code died twenty minutes ago, and the ceiling
	// still has to count it for another half hour.
	kept := testsupport.SeedChallenge(t, db, index, purpose, 30*time.Minute, domain.CodeTTL)
	// Past the window: no longer answerable and no longer counted, so keeping
	// it would only be a record of who asked to sign in and when.
	gone := testsupport.SeedChallenge(t, db, index, purpose, domain.ChallengeWindow+time.Minute, domain.CodeTTL)

	// Nil for everything the sweep does not touch; it reads the challenges and
	// the clock and nothing else.
	verification := NewVerification(nil, repository, nil, nil, nil, nil, nil)
	if _, err := verification.SweepOldChallenges(context.Background(), 500); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if !challengeExists(t, db, kept) {
		t.Error("the sweep deleted a challenge issued 30 minutes ago: it is inside " +
			"ChallengeWindow, so the per-destination ceiling is counted from it, and " +
			"sweeping at expiry turns five codes an hour into five every ten minutes")
	}
	if challengeExists(t, db, gone) {
		t.Error("a challenge past ChallengeWindow survived the sweep: nothing counts it " +
			"and nothing can answer it, so it is retention with no purpose")
	}

	counted, err := repository.CountRecent(
		context.Background(), domain.PurposeSignIn, index,
		time.Now().UTC().Add(-domain.ChallengeWindow),
	)
	if err != nil {
		t.Fatalf("count recent: %v", err)
	}
	if counted != 1 {
		t.Errorf("the ceiling counts %d challenges after the sweep, want 1", counted)
	}
}

func challengeExists(t *testing.T, db *gorm.DB, id string) bool {
	t.Helper()
	var total int64
	if err := db.Table("verification_challenges").
		Where("id = ?", id).Count(&total).Error; err != nil {
		t.Fatalf("count challenge %s: %v", id, err)
	}
	return total > 0
}
