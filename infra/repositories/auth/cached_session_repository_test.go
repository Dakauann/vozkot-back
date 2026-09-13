package auth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	domain "vozkot/domain/auth"
	cachedomain "vozkot/domain/cache"
	"vozkot/infra/testsupport"
)

// Caching an authentication decision is worth doing and easy to get wrong, so
// these run against real PostgreSQL and real Redis.
//
// The lookup being cached is the busiest query in the system: every
// authenticated request makes it, and a checkout screen polls for a PIX code
// for the length of a thirty-minute hold. What has to stay true is that a
// session which STOPPED being valid stops authenticating — immediately when the
// system is told, and within the TTL when it is not.

type sessionHarness struct {
	db     *gorm.DB
	cache  cachedomain.Cache
	cached *CachedSessionRepository
	inner  *SessionRepository
	userID string
}

func newSessionHarness(t *testing.T, ttl time.Duration) *sessionHarness {
	t.Helper()
	db := testsupport.Database(t)
	cache := testsupport.Cache(t)

	userID := testsupport.Unique("usr")
	err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Session Cache', ?, 'x', 'user', 0, NOW(), NOW())`,
		userID, userID+"@vozkot.test").Error
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
		db.Exec("DELETE FROM users WHERE id = ?", userID)
	})

	inner := NewSessionRepository(db)
	return &sessionHarness{
		db:     db,
		cache:  cache,
		inner:  inner,
		cached: NewCachedSessionRepository(inner, cache, ttl),
		userID: userID,
	}
}

func (h *sessionHarness) create(t *testing.T) *domain.Session {
	t.Helper()
	session := &domain.Session{
		ID:               testsupport.Unique("sess"),
		UserID:           h.userID,
		RefreshTokenHash: testsupport.Unique("refresh"),
		AccessJTI:        testsupport.Unique("jti"),
		DeviceInfo:       "Firefox",
		IPAddress:        "203.0.113.9",
		CreatedAt:        time.Now().UTC(),
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
	}
	if err := h.cached.Create(context.Background(), session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

// TestSecondLookupSkipsPostgreSQL proves the query is actually removed, not
// merely wrapped.
//
// The row is deleted out from under the cache: if the second lookup still
// answers, it answered from Redis. That is the whole point of the change — at a
// two-second poll rate this turns fifteen primary reads per buyer per window
// into one.
func TestSecondLookupSkipsPostgreSQL(t *testing.T) {
	h := newSessionHarness(t, 30*time.Second)
	ctx := context.Background()
	session := h.create(t)

	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI); err != nil {
		t.Fatalf("first lookup: %v", err)
	}

	// Checked separately from the hit below, so a failure says WHICH half broke:
	// a lookup that never populated the cache, or a cache that did not answer.
	// Redis is best-effort by design — the repository logs and carries on when a
	// write fails — so a bare "it went to PostgreSQL" would be ambiguous.
	if _, err := h.cache.Get(ctx, cachedomain.SessionKey(h.userID, session.AccessJTI)); err != nil {
		t.Fatalf("the first lookup did not populate the cache (%v); every request would keep hitting PostgreSQL", err)
	}

	h.db.Exec("DELETE FROM sessions WHERE id = ?", session.ID)

	found, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI)

	if err != nil {
		t.Fatalf("the cache was populated but did not answer; the lookup went to PostgreSQL: %v", err)
	}
	if found.ID != session.ID || found.UserID != h.userID {
		t.Fatalf("cached session = %+v, want %s for %s", found, session.ID, h.userID)
	}
	// And what came back is usable: the middleware asks exactly this.
	if !found.Active(time.Now()) {
		t.Fatal("the cached session is not Active; every request would be refused")
	}
}

// TestRevokeStopsAuthenticatingImmediately is the property that makes caching an
// authentication decision acceptable at all.
//
// A logout today kills the access token on the spot. Caching liveness for the
// token's own lifetime would have quietly weakened that to "up to fifteen
// minutes"; a short TTL plus an explicit invalidation keeps it immediate.
func TestRevokeStopsAuthenticatingImmediately(t *testing.T) {
	h := newSessionHarness(t, 30*time.Second)
	ctx := context.Background()
	session := h.create(t)

	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	if err := h.cached.Revoke(ctx, session.ID); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	_, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI)
	if !errors.Is(err, domain.ErrSessionNotFound) {
		t.Fatalf("a revoked session still authenticates: err = %v, want %v", err, domain.ErrSessionNotFound)
	}
}

// TestRotateRetiresTheOldAccessToken: rotation moves the session to a new JTI,
// which retires the old access token just as surely as a revoke does.
func TestRotateRetiresTheOldAccessToken(t *testing.T) {
	h := newSessionHarness(t, 30*time.Second)
	ctx := context.Background()
	session := h.create(t)

	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	nextJTI := testsupport.Unique("jti")
	rotated, err := h.cached.Rotate(ctx, session.ID, session.RefreshTokenHash,
		testsupport.Unique("refresh"), nextJTI, time.Now().UTC())
	if err != nil || !rotated {
		t.Fatalf("Rotate() = (%v, %v)", rotated, err)
	}

	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Fatalf("the pre-rotation access token still authenticates: err = %v", err)
	}
	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, nextJTI); err != nil {
		t.Fatalf("the rotated access token does not authenticate: %v", err)
	}
}

// TestAFailedRotationInvalidatesNothing: Rotate reports false when the expected
// hash no longer matches, and an attempt that changed nothing must not evict a
// live session and send its buyer back to the database for no reason.
func TestAFailedRotationInvalidatesNothing(t *testing.T) {
	h := newSessionHarness(t, 30*time.Second)
	ctx := context.Background()
	session := h.create(t)
	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	rotated, err := h.cached.Rotate(ctx, session.ID, "a-hash-that-was-never-current",
		testsupport.Unique("refresh"), testsupport.Unique("jti"), time.Now().UTC())
	if err != nil || rotated {
		t.Fatalf("Rotate() = (%v, %v), want (false, nil)", rotated, err)
	}

	h.db.Exec("DELETE FROM sessions WHERE id = ?", session.ID)
	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI); err != nil {
		t.Fatalf("a no-op rotation evicted a live session: %v", err)
	}
}

// TestCachedSessionCarriesRevocationAndIdentity guards the trap this decorator
// exists around.
//
// domain.Session's json tags are the API's: UserID, AccessJTI and RevokedAt are
// all `json:"-"`, because a session listed back to its owner should not carry
// them. Encoding the domain type into the cache would therefore store a session
// that decodes as belonging to nobody and never revoked — and Active() would
// wave it through. A revoked session that authenticates is the one bug this
// file must not have.
func TestCachedSessionCarriesRevocationAndIdentity(t *testing.T) {
	revoked := time.Now().UTC().Add(-time.Minute)
	session := &domain.Session{
		ID:        "sess_encoding",
		UserID:    "usr_encoding",
		AccessJTI: "jti_encoding",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		RevokedAt: &revoked,
	}

	// What the domain's own tags would have produced, for contrast.
	naive, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var throughDomainTags domain.Session
	if err := json.Unmarshal(naive, &throughDomainTags); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if throughDomainTags.RevokedAt != nil || throughDomainTags.UserID != "" {
		t.Skip("domain.Session no longer drops these fields; this guard can be simplified")
	}
	if throughDomainTags.Active(time.Now()) != true {
		t.Fatal("the premise of this test no longer holds")
	}

	// What this file actually stores.
	encoded, err := json.Marshal(cachedSession{
		ID: session.ID, UserID: session.UserID, AccessJTI: session.AccessJTI,
		ExpiresAt: session.ExpiresAt, RevokedAt: session.RevokedAt,
	})
	if err != nil {
		t.Fatalf("marshal cachedSession: %v", err)
	}
	decoded, ok := decodeSession(encoded, session.UserID, session.AccessJTI)
	if !ok {
		t.Fatal("decodeSession refused an entry it wrote")
	}
	if decoded.RevokedAt == nil {
		t.Fatal("the cache encoding lost RevokedAt: a revoked session would authenticate")
	}
	if decoded.Active(time.Now()) {
		t.Fatal("a revoked session came back Active from the cache")
	}
}

// TestAnEntryNeverAnswersForAnotherAccount: both the account and the token id
// are checked on the way out, so a mangled or mis-keyed value can never
// authenticate the wrong person.
func TestAnEntryNeverAnswersForAnotherAccount(t *testing.T) {
	encoded, err := json.Marshal(cachedSession{
		ID: "sess_1", UserID: "usr_owner", AccessJTI: "jti_1",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, ok := decodeSession(encoded, "usr_someone_else", "jti_1"); ok {
		t.Fatal("an entry answered for a different account")
	}
	if _, ok := decodeSession(encoded, "usr_owner", "jti_2"); ok {
		t.Fatal("an entry answered for a different access token")
	}
	if _, ok := decodeSession([]byte("not json"), "usr_owner", "jti_1"); ok {
		t.Fatal("a corrupt entry was accepted")
	}
	if _, ok := decodeSession(encoded, "usr_owner", "jti_1"); !ok {
		t.Fatal("a valid entry was refused")
	}
}

// TestTheEntryNeverOutlivesItsSession: a session expiring in a second is not
// cached for thirty, or it would answer for something that no longer exists.
func TestTheEntryNeverOutlivesItsSession(t *testing.T) {
	h := newSessionHarness(t, time.Hour)
	ctx := context.Background()

	session := &domain.Session{
		ID:               testsupport.Unique("sess"),
		UserID:           h.userID,
		RefreshTokenHash: testsupport.Unique("refresh"),
		AccessJTI:        testsupport.Unique("jti"),
		CreatedAt:        time.Now().UTC(),
		ExpiresAt:        time.Now().UTC().Add(time.Second),
	}
	if err := h.cached.Create(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	// Even if the entry survived, liveness is evaluated fresh every request —
	// so the session is refused on its own terms, not the cache's.
	time.Sleep(1200 * time.Millisecond)
	found, err := h.cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI)
	if err == nil && found.Active(time.Now()) {
		t.Fatal("an expired session is still Active through the cache")
	}
}

// TestAMissIsNeverCached: a lookup that found nothing must not become a
// permanent refusal, and a lookup that FAILED must stay distinguishable from
// one that found nothing — 401 and 503 are different answers.
func TestAMissIsNeverCached(t *testing.T) {
	h := newSessionHarness(t, 30*time.Second)
	ctx := context.Background()
	jti := testsupport.Unique("jti")

	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, jti); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Fatalf("lookup of an unknown token = %v, want %v", err, domain.ErrSessionNotFound)
	}

	// The session arrives afterwards; the earlier miss must not shadow it.
	session := &domain.Session{
		ID:               testsupport.Unique("sess"),
		UserID:           h.userID,
		RefreshTokenHash: testsupport.Unique("refresh"),
		AccessJTI:        jti,
		CreatedAt:        time.Now().UTC(),
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
	}
	if err := h.cached.Create(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := h.cached.FindByAccessJTI(ctx, h.userID, jti); err != nil {
		t.Fatalf("a cached miss shadowed a real session: %v", err)
	}
}

// TestTheKeysAreTheOnesTheDomainNames: the pointer a logout follows is written
// under the key the domain declares, so an invalidation and the read it
// invalidates cannot drift apart.
func TestTheKeysAreTheOnesTheDomainNames(t *testing.T) {
	h := newSessionHarness(t, 30*time.Second)
	ctx := context.Background()
	cache := testsupport.Cache(t)
	cached := NewCachedSessionRepository(h.inner, cache, 30*time.Second)
	session := h.create(t)

	if _, err := cached.FindByAccessJTI(ctx, h.userID, session.AccessJTI); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	if _, err := cache.Get(ctx, cachedomain.SessionKey(h.userID, session.AccessJTI)); err != nil {
		t.Fatalf("no entry under SessionKey: %v", err)
	}
	pointer, err := cache.Get(ctx, cachedomain.SessionLiveKey(session.ID))
	if err != nil {
		t.Fatalf("no pointer under SessionLiveKey: %v", err)
	}
	var decoded sessionPointer
	if err := json.Unmarshal(pointer, &decoded); err != nil {
		t.Fatalf("decode pointer: %v", err)
	}
	if decoded.UserID != h.userID || decoded.JTI != session.AccessJTI {
		t.Fatalf("pointer = %+v, want %s/%s", decoded, h.userID, session.AccessJTI)
	}
}
