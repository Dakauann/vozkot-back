package idempotency

import (
	"context"
	"errors"
	"strings"
	"time"

	domain "vozkot/domain/idempotency"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type IdempotencyRepository struct {
	db *gorm.DB
}

func NewIdempotencyRepository(db *gorm.DB) *IdempotencyRepository {
	return &IdempotencyRepository{db: db}
}

// Begin claims a key, or reports what the key already produced.
//
// The claim is one INSERT. That is deliberate and it is the entire mechanism:
// two retries of the same request arriving together both try to insert the same
// primary key, the database lets exactly one through, and the loser reads the
// winner's row. A SELECT-then-INSERT would let both see nothing and both start
// a checkout.
//
// ON CONFLICT DO NOTHING is what makes losing quiet: the loser is told "zero
// rows" rather than handed an error, so a client retrying over a bad
// connection — the case this table exists for — costs no rolled-back
// transaction and no line in the database log.
func (s *IdempotencyRepository) Begin(ctx context.Context, key, scope, requestHash string, lease time.Duration, now time.Time) (domain.Claim, error) {
	key = strings.TrimSpace(key)
	scope = strings.TrimSpace(scope)
	if key == "" {
		return domain.Claim{}, domain.ErrNotFound
	}
	if lease <= 0 {
		lease = domain.DefaultLease
	}
	timestamp := now.UTC()
	leaseUntil := timestamp.Add(lease)

	record := schema.IdempotencyKey{
		Key:            key,
		Scope:          scope,
		RequestHash:    requestHash,
		State:          string(domain.StateProcessing),
		CreatedAt:      timestamp,
		LeaseExpiresAt: &leaseUntil,
		ExpiresAt:      timestamp.Add(domain.TTL),
	}

	claim := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}, {Name: "scope"}}, DoNothing: true}).
		Create(&record)
	if claim.Error != nil {
		return domain.Claim{}, claim.Error
	}
	if claim.RowsAffected == 1 {
		// Nobody had this key; the caller does the work, and there is no
		// earlier attempt whose result could already exist.
		return domain.Claim{Mine: true}, nil
	}

	var existing schema.IdempotencyKey
	if err := s.db.WithContext(ctx).
		First(&existing, "key = ? AND scope = ?", key, scope).Error; err != nil {
		return domain.Claim{}, translate(err)
	}

	// An expired record is not a replay: the key's life is over and the request
	// is treated as new. Reusing the row keeps the primary key intact.
	if !existing.ExpiresAt.IsZero() && timestamp.After(existing.ExpiresAt) {
		result := s.db.WithContext(ctx).Model(&schema.IdempotencyKey{}).
			Where("key = ? AND scope = ? AND expires_at = ?", key, scope, existing.ExpiresAt).
			Updates(map[string]any{
				"request_hash":     requestHash,
				"state":            string(domain.StateProcessing),
				"status_code":      0,
				"response":         nil,
				"created_at":       timestamp,
				"completed_at":     nil,
				"lease_expires_at": leaseUntil,
				"expires_at":       timestamp.Add(domain.TTL),
			})
		if result.Error != nil {
			return domain.Claim{}, result.Error
		}
		if result.RowsAffected == 1 {
			// A day-old key reused. Recovered, because the row it belonged to
			// may still be there and the unique key on orders would refuse a
			// second one anyway — replaying beats failing.
			return domain.Claim{Mine: true, Recovered: true}, nil
		}
		// Someone else revived it first; fall through and treat theirs as the
		// authority.
		if err := s.db.WithContext(ctx).First(&existing, "key = ? AND scope = ?", key, scope).Error; err != nil {
			return domain.Claim{}, translate(err)
		}
	}

	// A claim whose lease has lapsed belonged to a request that never came
	// back: a killed process, a deploy, a panic. Taking it over is the only
	// thing that lets the buyer retry before the key's 24 hours are up.
	//
	// One conditional UPDATE, so two retries racing the takeover produce one
	// owner. The hash is a condition rather than an overwrite: a DIFFERENT body
	// under this key is a client bug and must still be refused, orphaned claim
	// or not.
	if toDomain(&existing).Lapsed(timestamp) && existing.RequestHash == requestHash {
		takeover := s.db.WithContext(ctx).Model(&schema.IdempotencyKey{}).
			Where("key = ? AND scope = ? AND state = ? AND request_hash = ?",
				key, scope, string(domain.StateProcessing), requestHash).
			Where("lease_expires_at IS NULL OR lease_expires_at <= ?", timestamp).
			Updates(map[string]any{
				"lease_expires_at": leaseUntil,
				"created_at":       timestamp,
			})
		if takeover.Error != nil {
			return domain.Claim{}, takeover.Error
		}
		if takeover.RowsAffected == 1 {
			return domain.Claim{Mine: true, Recovered: true}, nil
		}
		// Lost the takeover race; re-read so the caller is told about the
		// winner's claim rather than the stale one.
		if err := s.db.WithContext(ctx).First(&existing, "key = ? AND scope = ?", key, scope).Error; err != nil {
			return domain.Claim{}, translate(err)
		}
	}

	return domain.Claim{Existing: toDomain(&existing)}, nil
}

func (s *IdempotencyRepository) Complete(ctx context.Context, key, scope string, statusCode int, response []byte, now time.Time) error {
	timestamp := now.UTC()
	result := s.db.WithContext(ctx).Model(&schema.IdempotencyKey{}).
		Where("key = ? AND scope = ?", strings.TrimSpace(key), strings.TrimSpace(scope)).
		Updates(map[string]any{
			"state":        string(domain.StateCompleted),
			"status_code":  statusCode,
			"response":     response,
			"completed_at": timestamp,
			// A finished claim has no lease left to lapse, so nothing can take
			// a completed key over and run its work again.
			"lease_expires_at": nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Release drops a claim whose work failed, so the client can retry instead of
// being told for a day that a request is still in progress.
func (s *IdempotencyRepository) Release(ctx context.Context, key, scope string) error {
	return s.db.WithContext(ctx).
		Where("key = ? AND scope = ? AND state = ?",
			strings.TrimSpace(key), strings.TrimSpace(scope), string(domain.StateProcessing)).
		Delete(&schema.IdempotencyKey{}).Error
}

func (s *IdempotencyRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	result := s.db.WithContext(ctx).
		Where("expires_at < ?", now.UTC()).
		Delete(&schema.IdempotencyKey{})
	return result.RowsAffected, result.Error
}

func translate(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrNotFound
	}
	return err
}

func toDomain(record *schema.IdempotencyKey) *domain.Record {
	return &domain.Record{
		Key:            record.Key,
		Scope:          record.Scope,
		RequestHash:    record.RequestHash,
		State:          domain.State(record.State),
		StatusCode:     record.StatusCode,
		Response:       record.Response,
		CreatedAt:      record.CreatedAt,
		CompletedAt:    record.CompletedAt,
		LeaseExpiresAt: record.LeaseExpiresAt,
		ExpiresAt:      record.ExpiresAt,
	}
}
