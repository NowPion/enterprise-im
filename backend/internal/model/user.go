package model

import "time"

const (
	UserRoleUser  = "user"
	UserRoleAdmin = "admin"

	UserStatusDisabled int8 = 0
	UserStatusActive   int8 = 1
)

// User maps the `users` table in deploy/mysql/init.sql.
type User struct {
	ID           uint64     `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"id,string"`
	Username     string     `gorm:"column:username;type:varchar(64);not null;uniqueIndex:uk_users_username" json:"username"`
	PasswordHash string     `gorm:"column:password_hash;type:varchar(255);not null" json:"-"`
	Nickname     string     `gorm:"column:nickname;type:varchar(64);not null;index:idx_users_nickname" json:"nickname"`
	AvatarURL    string     `gorm:"column:avatar_url;type:varchar(512);not null;default:''" json:"avatar_url"`
	Signature    string     `gorm:"column:signature;type:varchar(255);not null;default:''" json:"signature"`
	Role         string     `gorm:"column:role;type:enum('user','admin');not null;default:'user'" json:"role"`
	Status       int8       `gorm:"column:status;type:tinyint;not null;default:1;index:idx_users_status_created,priority:1" json:"status"`
	LastOnlineAt *time.Time `gorm:"column:last_online_at;type:datetime(3);default:null" json:"last_online_at"`
	CreatedAt    time.Time  `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime;index:idx_users_status_created,priority:2" json:"created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (User) TableName() string {
	return "users"
}
