package schema

import "time"

type Session struct {
	ID                       string    `gorm:"primaryKey;type:varchar(32)"`
	UserID                   string    `gorm:"not null;type:varchar(32);index:idx_sessions_user_id"`
	RefreshTokenHash         string    `gorm:"not null;type:varchar(64);uniqueIndex:idx_sessions_refresh_hash"`
	PreviousRefreshTokenHash string    `gorm:"type:varchar(64);index:idx_sessions_previous_refresh_hash"`
	AccessJTI                string    `gorm:"not null;type:varchar(64);index:idx_sessions_access_jti"`
	DeviceInfo               string    `gorm:"type:varchar(500)"`
	IPAddress                string    `gorm:"type:varchar(45)"`
	ExpiresAt                time.Time `gorm:"not null;index:idx_sessions_expires_at"`
	CreatedAt                time.Time `gorm:"not null;autoCreateTime"`
	RotatedAt                *time.Time
	RevokedAt                *time.Time `gorm:"index:idx_sessions_revoked_at"`

	User User `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (Session) TableName() string { return "sessions" }
