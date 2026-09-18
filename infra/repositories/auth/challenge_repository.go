package auth

import (
	"context"
	"errors"
	"time"

	domain "vozkot/domain/auth"
	"vozkot/infra/crypto/piigorm"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ChallengeBlindScope namespaces the destination index for this table.
//
// Distinct from the user table's phone scope on purpose: the same number
// indexed for "who owns this" and for "how many codes has this been sent" must
// not be the same value, so a leak of one index tells nothing about the other.
const ChallengeBlindScope = "verification.destination.v1"

type ChallengeRepository struct {
	db *gorm.DB
}

func NewChallengeRepository(db *gorm.DB) *ChallengeRepository {
	return &ChallengeRepository{db: db}
}

var _ domain.ChallengeRepository = (*ChallengeRepository)(nil)

func (r *ChallengeRepository) Create(ctx context.Context, item *domain.Challenge) error {
	record := schema.VerificationChallenge{
		ID:               item.ID,
		Purpose:          string(item.Purpose),
		DestinationBlind: piigorm.BlindIndex(item.DestinationIndex),
		Destination:      piigorm.NewEncrypted(item.Destination),
		CodeHash:         item.CodeHash,
		Attempts:         item.Attempts,
		UserID:           item.UserID,
		ExpiresAt:        item.ExpiresAt,
		ConsumedAt:       item.ConsumedAt,
		CreatedAt:        item.CreatedAt,
	}
	return r.db.WithContext(ctx).Create(&record).Error
}

// FindForUpdate reads a challenge and holds its row.
//
// See the interface for why the lock is the attempt counter's correctness and
// not a performance detail.
func (r *ChallengeRepository) FindForUpdate(ctx context.Context, id string) (*domain.Challenge, error) {
	var record schema.VerificationChallenge
	err := r.db.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&record, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrChallengeNotFound
	}
	if err != nil {
		return nil, err
	}
	return toDomainChallenge(&record), nil
}

// Update writes back only what answering a challenge can change: the attempt
// count and whether it has been spent. The destination and the code hash are
// settled when the row is created.
func (r *ChallengeRepository) Update(ctx context.Context, item *domain.Challenge) error {
	result := r.db.WithContext(ctx).Model(&schema.VerificationChallenge{}).
		Where("id = ?", item.ID).
		Updates(map[string]any{
			"attempts":    item.Attempts,
			"consumed_at": item.ConsumedAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrChallengeNotFound
	}
	return nil
}

func (r *ChallengeRepository) LastSentTo(
	ctx context.Context,
	purpose domain.Purpose,
	destinationIndex []byte,
) (time.Time, error) {
	var record schema.VerificationChallenge
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND destination_blind = ?", string(purpose), []byte(destinationIndex)).
		Order("created_at DESC").
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return record.CreatedAt, nil
}

func (r *ChallengeRepository) CountRecent(
	ctx context.Context,
	purpose domain.Purpose,
	destinationIndex []byte,
	since time.Time,
) (int, error) {
	var total int64
	err := r.db.WithContext(ctx).Model(&schema.VerificationChallenge{}).
		Where("purpose = ? AND destination_blind = ? AND created_at >= ?",
			string(purpose), []byte(destinationIndex), since.UTC()).
		Count(&total).Error
	if err != nil {
		return 0, err
	}
	return int(total), nil
}

// DeleteOlderThan removes challenges created before the cutoff.
//
// Bounded, so the sweep is a short statement rather than a table-wide delete
// that holds locks while somebody is trying to sign in.
//
// created_at rather than expires_at, and ordered by it too: see the port for
// why a row has to outlive the code it carries. Both the predicate and the
// ordering ride idx_challenges_created_at, so this stays an index scan of the
// oldest rows rather than a sort of the table.
func (r *ChallengeRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	result := r.db.WithContext(ctx).Exec(`
		DELETE FROM verification_challenges
		WHERE id IN (
			SELECT id FROM verification_challenges
			WHERE created_at < ?
			ORDER BY created_at
			LIMIT ?
		)`, cutoff.UTC(), limit)
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

func toDomainChallenge(record *schema.VerificationChallenge) *domain.Challenge {
	return &domain.Challenge{
		ID:               record.ID,
		Purpose:          domain.Purpose(record.Purpose),
		DestinationIndex: []byte(record.DestinationBlind),
		Destination:      record.Destination.Plain,
		CodeHash:         record.CodeHash,
		Attempts:         record.Attempts,
		UserID:           record.UserID,
		ExpiresAt:        record.ExpiresAt,
		ConsumedAt:       record.ConsumedAt,
		CreatedAt:        record.CreatedAt,
	}
}
