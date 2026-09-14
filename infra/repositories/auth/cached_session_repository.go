package auth

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	domain "vozkot/domain/auth"
	cachedomain "vozkot/domain/cache"
)

// CachedSessionRepository removes the busiest query in the system.
//
// Every authenticated request asks "is this access token's session still live",
// and a checkout screen asks it on every poll for the length of a thirty-minute
// hold. The order underneath is cached for a second; without this the liveness
// check in front of it was not, so one indexed SELECT on the primary ran per
// poll per buyer and set the number of PIX screens the whole fleet could serve.
//
// Three properties make caching an authentication decision safe here:
//
//   - The entry is short-lived. Thirty seconds collapses fifteen polls into one
//     read, and bounds how long any stale answer could survive on its own.
//   - Revoking and rotating drop the entry outright, so a logout still takes
//     effect immediately rather than at the end of the window. The window is
//     the floor under a cache that lost the key some other way, not the normal
//     case.
//   - Expiry is never cached as a verdict. The session's own ExpiresAt is
//     stored and Active() is evaluated fresh on every request, so an entry
//     cannot outlive the session it describes.
//
// What remains is the ordinary cache-aside race: a read that started before a
// revoke can store its now-stale value just after it. The TTL is the bound on
// that, and it is why the TTL is seconds rather than the access token's fifteen
// minutes.
type CachedSessionRepository struct {
	inner domain.SessionRepository
	cache cachedomain.Cache
	ttl   time.Duration
}

var _ domain.SessionRepository = (*CachedSessionRepository)(nil)

// DefaultSessionTTL is the staleness bound when none is configured.
const DefaultSessionTTL = 30 * time.Second

func NewCachedSessionRepository(inner domain.SessionRepository, cache cachedomain.Cache, ttl time.Duration) *CachedSessionRepository {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &CachedSessionRepository{inner: inner, cache: cache, ttl: ttl}
}

// cachedSession is the cache's own encoding of a session.
//
// The domain type CANNOT be marshalled directly, and this is the trap this
// whole file exists to avoid: its json tags are the API's; a session is listed
// back to the user who owns it, and they drop `UserID`, `AccessJTI` and
// `RevokedAt` with `json:"-"`. Encoding the domain type would therefore store a
// session that decodes as belonging to nobody and never revoked, and
// `Active()` would wave it through. A revoked session that authenticates is the
// one bug this file must not have, so the encoding is explicit and local.
//
// The refresh-token hashes are deliberately absent. Nothing on this path reads
// them, and leaving them out keeps them in one place instead of two; the port
// documents that FindByAccessJTI does not populate them.
type cachedSession struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	AccessJTI  string     `json:"jti"`
	DeviceInfo string     `json:"device,omitempty"`
	IPAddress  string     `json:"ip,omitempty"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	RotatedAt  *time.Time `json:"rotatedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

// sessionPointer is what a session id resolves to, so revoking and rotating,
// which name a session by id; can find the entry keyed by access token.
type sessionPointer struct {
	UserID string `json:"userId"`
	JTI    string `json:"jti"`
}

func (r *CachedSessionRepository) FindByAccessJTI(ctx context.Context, userID, jti string) (*domain.Session, error) {
	key := cachedomain.SessionKey(userID, jti)
	if raw, err := r.cache.Get(ctx, key); err == nil {
		if session, ok := decodeSession(raw, userID, jti); ok {
			return session, nil
		}
	}

	session, err := r.inner.FindByAccessJTI(ctx, userID, jti)
	if err != nil {
		// Errors are never cached, and the two kinds must stay
		// distinguishable: a missing session is a 401, an unreachable database
		// is a 503, and caching either would make the wrong one permanent.
		return nil, err
	}
	r.store(ctx, key, session)
	return session, nil
}

// store writes the liveness entry and the pointer that lets a logout find it.
//
// Both carry the same TTL, so they lapse together and a pointer can never
// outlive what it points at.
func (r *CachedSessionRepository) store(ctx context.Context, key string, session *domain.Session) {
	ttl := r.ttl
	// Never cache past the session's own life: an entry that outlived its
	// session would be answering for something that no longer exists.
	if remaining := time.Until(session.ExpiresAt); remaining < ttl {
		if remaining <= 0 {
			return
		}
		ttl = remaining
	}

	encoded, err := json.Marshal(cachedSession{
		ID:         session.ID,
		UserID:     session.UserID,
		AccessJTI:  session.AccessJTI,
		DeviceInfo: session.DeviceInfo,
		IPAddress:  session.IPAddress,
		ExpiresAt:  session.ExpiresAt,
		CreatedAt:  session.CreatedAt,
		RotatedAt:  session.RotatedAt,
		RevokedAt:  session.RevokedAt,
	})
	if err != nil {
		return
	}
	pointer, err := json.Marshal(sessionPointer{UserID: session.UserID, JTI: session.AccessJTI})
	if err != nil {
		return
	}
	if err := r.cache.Set(ctx, key, encoded, ttl); err != nil {
		log.Printf("cache: store session %s: %v", session.ID, err)
		return
	}
	if err := r.cache.Set(ctx, cachedomain.SessionLiveKey(session.ID), pointer, ttl); err != nil {
		// The pointer is what a logout follows. Without it the entry above
		// would survive a revoke for its whole TTL, so the entry goes too.
		log.Printf("cache: store session pointer %s: %v", session.ID, err)
		_ = r.cache.Delete(ctx, key)
	}
}

// decodeSession refuses an entry that does not describe the account and token
// that were asked for, so a mangled or mis-keyed value can never authenticate
// the wrong person.
func decodeSession(raw []byte, userID, jti string) (*domain.Session, bool) {
	var stored cachedSession
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, false
	}
	if stored.UserID != userID || stored.AccessJTI != jti {
		return nil, false
	}
	return &domain.Session{
		ID:         stored.ID,
		UserID:     stored.UserID,
		AccessJTI:  stored.AccessJTI,
		DeviceInfo: stored.DeviceInfo,
		IPAddress:  stored.IPAddress,
		ExpiresAt:  stored.ExpiresAt,
		CreatedAt:  stored.CreatedAt,
		RotatedAt:  stored.RotatedAt,
		RevokedAt:  stored.RevokedAt,
	}, true
}

func (r *CachedSessionRepository) Revoke(ctx context.Context, sessionID string) error {
	// The source of truth moves first. Dropping the entry before the row is
	// revoked would only cost a cache miss, but leaving the row revoked and the
	// entry alive is the failure that matters, so the delete follows the write
	// and is the last thing that can go wrong.
	if err := r.inner.Revoke(ctx, sessionID); err != nil {
		return err
	}
	r.forget(ctx, sessionID)
	return nil
}

func (r *CachedSessionRepository) Rotate(ctx context.Context, sessionID, expectedHash, nextHash, nextJTI string, rotatedAt time.Time) (bool, error) {
	rotated, err := r.inner.Rotate(ctx, sessionID, expectedHash, nextHash, nextJTI, rotatedAt)
	if err != nil || !rotated {
		return rotated, err
	}
	// Rotation moves the session to a new access token, which retires the old
	// one just as surely as a revoke does.
	r.forget(ctx, sessionID)
	return true, nil
}

// forget drops the entry for a session whose access token has stopped being
// valid.
//
// A failure here is logged rather than returned: the session is already revoked
// where it counts, and the entry lapses on its own within the TTL. Nothing is
// gained by telling the buyer their logout failed when it did not.
func (r *CachedSessionRepository) forget(ctx context.Context, sessionID string) {
	liveKey := cachedomain.SessionLiveKey(sessionID)
	raw, err := r.cache.Get(ctx, liveKey)
	if err != nil {
		// Nothing cached for this session, or the cache is unreachable, in
		// which case the next request misses and reads the revoked row anyway.
		return
	}
	var pointer sessionPointer
	if err := json.Unmarshal(raw, &pointer); err == nil && pointer.JTI != "" {
		if err := r.cache.Delete(ctx, cachedomain.SessionKey(pointer.UserID, pointer.JTI)); err != nil {
			log.Printf("cache: invalidate session %s: %v", sessionID, err)
		}
	}
	if err := r.cache.Delete(ctx, liveKey); err != nil {
		log.Printf("cache: invalidate session pointer %s: %v", sessionID, err)
	}
}

// Create is not cached: a session nobody has authenticated with yet has no
// entry to write, and the first request will fill one.
func (r *CachedSessionRepository) Create(ctx context.Context, session *domain.Session) error {
	return r.inner.Create(ctx, session)
}

// The refresh-token lookups stay uncached on purpose. They run once per access
// token lifetime, they are the path that rotates credentials, and a stale
// answer on either would be a security decision made from a cache.

func (r *CachedSessionRepository) FindByRefreshTokenHash(ctx context.Context, hash string) (*domain.Session, error) {
	return r.inner.FindByRefreshTokenHash(ctx, strings.TrimSpace(hash))
}

func (r *CachedSessionRepository) FindByPreviousRefreshTokenHash(ctx context.Context, hash string) (*domain.Session, error) {
	return r.inner.FindByPreviousRefreshTokenHash(ctx, strings.TrimSpace(hash))
}
