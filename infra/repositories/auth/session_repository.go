package auth

import (
	"context"
	"errors"
	"time"

	domain "vozkot/domain/auth"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
)

type SessionRepository struct {
	db *gorm.DB
}

func NewSessionRepository(db *gorm.DB) *SessionRepository {
	return &SessionRepository{db: db}
}

func (r *SessionRepository) Create(ctx context.Context, item *domain.Session) error {
	record := sessionToSchema(item)
	if err := r.db.WithContext(ctx).Create(&record).Error; err != nil {
		return err
	}
	*item = *sessionToDomain(&record)
	return nil
}

func (r *SessionRepository) FindByRefreshTokenHash(ctx context.Context, hash string) (*domain.Session, error) {
	return r.find(ctx, "refresh_token_hash = ?", hash)
}

func (r *SessionRepository) FindByPreviousRefreshTokenHash(ctx context.Context, hash string) (*domain.Session, error) {
	return r.find(ctx, "previous_refresh_token_hash = ?", hash)
}

func (r *SessionRepository) FindByAccessJTI(ctx context.Context, userID, jti string) (*domain.Session, error) {
	return r.find(ctx, "user_id = ? AND access_jti = ? AND revoked_at IS NULL", userID, jti)
}

func (r *SessionRepository) Rotate(ctx context.Context, sessionID, expectedHash, nextHash, nextJTI string, rotatedAt time.Time) (bool, error) {
	result := r.db.WithContext(ctx).Model(&schema.Session{}).
		Where("id = ? AND refresh_token_hash = ? AND revoked_at IS NULL", sessionID, expectedHash).
		Updates(map[string]any{
			"previous_refresh_token_hash": expectedHash,
			"refresh_token_hash":          nextHash,
			"access_jti":                  nextJTI,
			"rotated_at":                  rotatedAt.UTC(),
		})
	return result.RowsAffected == 1, result.Error
}

func (r *SessionRepository) Revoke(ctx context.Context, sessionID string) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&schema.Session{}).
		Where("id = ? AND revoked_at IS NULL", sessionID).
		Update("revoked_at", &now)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrSessionNotFound
	}
	return nil
}

func (r *SessionRepository) find(ctx context.Context, query string, args ...any) (*domain.Session, error) {
	var record schema.Session
	if err := r.db.WithContext(ctx).Where(query, args...).First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrSessionNotFound
		}
		return nil, err
	}
	return sessionToDomain(&record), nil
}

func sessionToSchema(item *domain.Session) schema.Session {
	return schema.Session{
		ID:                       item.ID,
		UserID:                   item.UserID,
		RefreshTokenHash:         item.RefreshTokenHash,
		PreviousRefreshTokenHash: item.PreviousRefreshTokenHash,
		AccessJTI:                item.AccessJTI,
		DeviceInfo:               item.DeviceInfo,
		IPAddress:                item.IPAddress,
		ExpiresAt:                item.ExpiresAt,
		CreatedAt:                item.CreatedAt,
		RotatedAt:                item.RotatedAt,
		RevokedAt:                item.RevokedAt,
	}
}

func sessionToDomain(record *schema.Session) *domain.Session {
	return &domain.Session{
		ID:                       record.ID,
		UserID:                   record.UserID,
		RefreshTokenHash:         record.RefreshTokenHash,
		PreviousRefreshTokenHash: record.PreviousRefreshTokenHash,
		AccessJTI:                record.AccessJTI,
		DeviceInfo:               record.DeviceInfo,
		IPAddress:                record.IPAddress,
		ExpiresAt:                record.ExpiresAt,
		CreatedAt:                record.CreatedAt,
		RotatedAt:                record.RotatedAt,
		RevokedAt:                record.RevokedAt,
	}
}
