package model

import "time"

const (
	DeviceTypeWeb     = "web"
	DeviceTypeDesktop = "desktop"
	DeviceTypeMobile  = "mobile"
)

// UserSession maps the `user_sessions` table for multi-device Refresh Token tracking.
type UserSession struct {
	ID               uint64     `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"id,string"`
	UserID           uint64     `gorm:"column:user_id;type:bigint unsigned;not null;index:idx_sessions_user_device,priority:1;index:idx_sessions_user_revoked_exp,priority:1" json:"user_id,string"`
	DeviceID         string     `gorm:"column:device_id;type:varchar(128);not null;index:idx_sessions_user_device,priority:2" json:"device_id"`
	DeviceType       string     `gorm:"column:device_type;type:enum('web','desktop','mobile');not null;default:'web'" json:"device_type"`
	RefreshTokenHash string     `gorm:"column:refresh_token_hash;type:char(64);not null;uniqueIndex:uk_sessions_refresh_hash" json:"-"`
	AccessJTI        string     `gorm:"column:access_jti;type:varchar(64);not null" json:"-"`
	IPAddress        string     `gorm:"column:ip_address;type:varchar(64);not null;default:''" json:"ip_address"`
	UserAgent        string     `gorm:"column:user_agent;type:varchar(512);not null;default:''" json:"user_agent"`
	ExpiresAt        time.Time  `gorm:"column:expires_at;type:datetime(3);not null;index:idx_sessions_user_revoked_exp,priority:3" json:"expires_at"`
	RevokedAt        *time.Time `gorm:"column:revoked_at;type:datetime(3);default:null;index:idx_sessions_user_revoked_exp,priority:2" json:"revoked_at"`
	LastActiveAt     time.Time  `gorm:"column:last_active_at;type:datetime(3);not null" json:"last_active_at"`
	CreatedAt        time.Time  `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime" json:"created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (UserSession) TableName() string {
	return "user_sessions"
}
