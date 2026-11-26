package model

import "time"

const (
	FriendshipStatusDeleted int8 = 0
	FriendshipStatusActive  int8 = 1
)

// Friendship maps the bidirectional `friendships` table.
type Friendship struct {
	ID        uint64    `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"id,string"`
	UserID    uint64    `gorm:"column:user_id;type:bigint unsigned;not null;uniqueIndex:uk_friendships_pair,priority:1;index:idx_friendships_user_status,priority:1" json:"user_id,string"`
	FriendID  uint64    `gorm:"column:friend_id;type:bigint unsigned;not null;uniqueIndex:uk_friendships_pair,priority:2;index:idx_friendships_friend_status,priority:1" json:"friend_id,string"`
	Remark    string    `gorm:"column:remark;type:varchar(64);not null;default:''" json:"remark"`
	Status    int8      `gorm:"column:status;type:tinyint;not null;default:1;index:idx_friendships_user_status,priority:2;index:idx_friendships_friend_status,priority:2" json:"status"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (Friendship) TableName() string {
	return "friendships"
}
