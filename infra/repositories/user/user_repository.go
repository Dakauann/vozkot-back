package user

import (
	"context"
	"errors"
	"strings"

	domain "vozkot/domain/user"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
)

type UserRepository struct {
	db *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

func (r *UserRepository) Create(ctx context.Context, item *domain.User) error {
	record := userToSchema(item)
	if err := r.db.WithContext(ctx).Create(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return domain.ErrEmailAlreadyExists
		}
		return err
	}
	*item = *userToDomain(&record)
	return nil
}

func (r *UserRepository) FindByID(ctx context.Context, id string) (*domain.User, error) {
	var record schema.User
	if err := r.db.WithContext(ctx).First(&record, "id = ?", id).Error; err != nil {
		return nil, userError(err)
	}
	return userToDomain(&record), nil
}

func (r *UserRepository) FindByEmail(ctx context.Context, email string) (*domain.User, error) {
	var record schema.User
	email = strings.ToLower(strings.TrimSpace(email))
	if err := r.db.WithContext(ctx).Where("email = ?", email).First(&record).Error; err != nil {
		return nil, userError(err)
	}
	return userToDomain(&record), nil
}

func userError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrNotFound
	}
	return err
}

func userToSchema(item *domain.User) schema.User {
	return schema.User{
		ID:           item.ID,
		Name:         item.Name,
		Email:        strings.ToLower(strings.TrimSpace(item.Email)),
		PasswordHash: item.PasswordHash,
		Role:         string(item.Role),
		TokenVersion: item.TokenVersion,
		DisabledAt:   item.DisabledAt,
		CreatedAt:    item.CreatedAt,
		UpdatedAt:    item.UpdatedAt,
	}
}

func userToDomain(record *schema.User) *domain.User {
	return &domain.User{
		ID:           record.ID,
		Name:         record.Name,
		Email:        record.Email,
		PasswordHash: record.PasswordHash,
		Role:         domain.Role(record.Role),
		TokenVersion: record.TokenVersion,
		DisabledAt:   record.DisabledAt,
		CreatedAt:    record.CreatedAt,
		UpdatedAt:    record.UpdatedAt,
	}
}
