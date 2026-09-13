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
func (s *IdempotencyRepository) Begin(ctx context.Context, key, scope, requestHash string, now time.Time) (*domain.Record, error) {
	key = strings.TrimSpace(key)
	scope = strings.TrimSpace(scope)
	if key == "" {
		return nil, domain.ErrNotFound
	}
	timestamp := now.UTC()

	record := schema.IdempotencyKey{
		Key:         key,
		Scope:       scope,
		RequestHash: requestHash,
		State:       string(domain.StateProcessing),
		CreatedAt:   timestamp,
		ExpiresAt:   timestamp.Add(domain.TTL),
	}

	claim := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}, {Name: "scope"}}, DoNothing: true}).
		Create(&record)
	if claim.Error != nil {
		return nil, claim.Error
	}
	if claim.RowsAffected == 1 {
		// The claim is ours; the caller does the work.
		return nil, nil
	}

	var existing schema.IdempotencyKey
	if err := s.db.WithContext(ctx).
		First(&existing, "key = ? AND scope = ?", key, scope).Error; err != nil {
		return nil, translate(err)
	}

	// An expired record is not a replay: the key's life is over and the request
	// is treated as new. Reusing the row keeps the primary key intact.
	if !existing.ExpiresAt.IsZero() && timestamp.After(existing.ExpiresAt) {
		result := s.db.WithContext(ctx).Model(&schema.IdempotencyKey{}).
			Where("key = ? AND scope = ? AND expires_at = ?", key, scope, existing.ExpiresAt).
			Updates(map[string]any{
				"request_hash": requestHash,
				"state":        string(domain.StateProcessing),
				"status_code":  0,
				"response":     nil,
				"created_at":   timestamp,
				"completed_at": nil,
				"expires_at":   timestamp.Add(domain.TTL),
			})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			return nil, nil
		}
		// Someone else revived it first; fall through and treat theirs as the
		// authority.
		if err := s.db.WithContext(ctx).First(&existing, "key = ? AND scope = ?", key, scope).Error; err != nil {
			return nil, translate(err)
		}
	}

	return toDomain(&existing), nil
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
		Key:         record.Key,
		Scope:       record.Scope,
		RequestHash: record.RequestHash,
		State:       domain.State(record.State),
		StatusCode:  record.StatusCode,
		Response:    record.Response,
		CreatedAt:   record.CreatedAt,
		CompletedAt: record.CompletedAt,
		ExpiresAt:   record.ExpiresAt,
	}
}
