package user

import (
	"context"
	"errors"
	"strings"
	"time"

	domain "vozkot/domain/user"
	"vozkot/infra/crypto/piigorm"
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

// SaveProfile seals the identity block and writes it with its blind indexes.
//
// The uniqueness of a document is enforced by the index, not by looking first:
// two sign-ups racing with the same CPF would both find nothing and both write.
// The database refuses the second, and that refusal is translated here.
func (r *UserRepository) SaveProfile(ctx context.Context, id string, profile domain.Profile) error {
	documentBlind, err := piigorm.NewBlindIndex(schema.UserDocumentBlindScope, profile.Document)
	if err != nil {
		return err
	}
	phoneBlind, err := piigorm.NewBlindIndex(schema.UserPhoneBlindScope, profile.Phone)
	if err != nil {
		return err
	}

	updates := map[string]any{
		"document_type":        string(profile.DocumentType),
		"document":             encryptedOrNull(profile.Document),
		"document_blind":       documentBlind,
		"legal_name":           encryptedOrNull(profile.LegalName),
		"birth_date":           encryptedOrNull(profile.BirthDate),
		"phone":                encryptedOrNull(profile.Phone),
		"phone_blind":          phoneBlind,
		"gender":               encryptedOrNull(string(profile.Gender)),
		"city":                 encryptedOrNull(profile.City),
		"uf":                   encryptedOrNull(profile.UF),
		"phone_verified_at":    profile.PhoneVerifiedAt,
		"profile_completed_at": profile.CompletedAt,
		"updated_at":           time.Now().UTC(),
	}

	result := r.db.WithContext(ctx).Model(&schema.User{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		if isDocumentConflict(result.Error) {
			return domain.ErrDocumentInUse
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *UserRepository) SavePassword(ctx context.Context, id, hash string) error {
	result := r.db.WithContext(ctx).Model(&schema.User{}).Where("id = ?", id).
		Updates(map[string]any{"password_hash": hash, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *UserRepository) FindByDocument(ctx context.Context, document string) (*domain.User, error) {
	blind, err := piigorm.NewBlindIndex(schema.UserDocumentBlindScope, document)
	if err != nil {
		return nil, err
	}
	if len(blind) == 0 {
		return nil, domain.ErrNotFound
	}
	var record schema.User
	if err := r.db.WithContext(ctx).Where("document_blind = ?", []byte(blind)).First(&record).Error; err != nil {
		return nil, userError(err)
	}
	return userToDomain(&record), nil
}

// encryptedOrNull keeps "not supplied" as NULL rather than as an encrypted
// empty string, so an absent value is absent in the column too.
func encryptedOrNull(value string) piigorm.EncryptedString {
	if value == "" {
		return piigorm.Null()
	}
	return piigorm.NewEncrypted(value)
}

// isDocumentConflict recognises the unique index losing a race.
//
// Matched on the constraint NAME rather than on the message text, so a
// PostgreSQL upgrade that rewords its errors does not turn "this document is
// already linked to another account" into a 500.
func isDocumentConflict(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	return strings.Contains(err.Error(), "idx_users_document_blind")
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
		Profile: domain.Profile{
			DocumentType:    domain.DocumentType(record.DocumentType),
			Document:        record.Document.Plain,
			LegalName:       record.LegalName.Plain,
			BirthDate:       record.BirthDate.Plain,
			Phone:           record.Phone.Plain,
			Gender:          domain.Gender(record.Gender.Plain),
			City:            record.City.Plain,
			UF:              record.UF.Plain,
			PhoneVerifiedAt: record.PhoneVerifiedAt,
			CompletedAt:     record.ProfileCompletedAt,
		},
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
	}
}
