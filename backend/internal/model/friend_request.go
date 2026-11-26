package model

import "time"

const (
	FriendRequestStatusPending  = "pending"
	FriendRequestStatusAccepted = "accepted"
	FriendRequestStatusRejected = "rejected"
	FriendRequestStatusCanceled = "canceled"
)

// FriendRequest maps the `friend_requests` table.
type FriendRequest struct {
	ID         uint64     `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"id,string"`
	FromUserID uint64     `gorm:"column:from_user_id;type:bigint unsigned;not null;index:idx_fr_from_to_status,priority:1" json:"from_user_id,string"`
	ToUserID   uint64     `gorm:"column:to_user_id;type:bigint unsigned;not null;index:idx_fr_to_status_created,priority:1;index:idx_fr_from_to_status,priority:2" json:"to_user_id,string"`
	Message    string     `gorm:"column:message;type:varchar(255);not null;default:''" json:"message"`
	Status     string     `gorm:"column:status;type:enum('pending','accepted','rejected','canceled');not null;default:'pending';index:idx_fr_to_status_created,priority:2;index:idx_fr_from_to_status,priority:3" json:"status"`
	HandledAt  *time.Time `gorm:"column:handled_at;type:datetime(3);default:null" json:"handled_at"`
	CreatedAt  time.Time  `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime;index:idx_fr_to_status_created,priority:3,sort:desc" json:"created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (FriendRequest) TableName() string {
	return "friend_requests"
}
