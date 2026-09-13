package schema

import "time"

type User struct {
	ID           string     `gorm:"primaryKey;type:varchar(32)"`
	Name         string     `gorm:"not null;type:varchar(255)"`
	Email        string     `gorm:"not null;type:varchar(320);uniqueIndex:idx_users_email"`
	PasswordHash string     `gorm:"not null;type:varchar(255)"`
	Role         string     `gorm:"not null;type:varchar(32);default:'user'"`
	TokenVersion int        `gorm:"not null;default:0"`
	DisabledAt   *time.Time `gorm:"index:idx_users_disabled_at"`
	CreatedAt    time.Time  `gorm:"not null;autoCreateTime"`
	UpdatedAt    time.Time  `gorm:"not null;autoUpdateTime"`
}

func (User) TableName() string { return "users" }
