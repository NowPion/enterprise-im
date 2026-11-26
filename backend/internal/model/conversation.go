package model

import "time"

const (
	ConversationTypeSingle = "single"
	ConversationTypeGroup  = "group"
)

// Conversation maps the `conversations` table.
type Conversation struct {
	ID             uint64     `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"id,string"`
	Type           string     `gorm:"column:type;type:enum('single','group');not null" json:"type"`
	DirectKey      *string    `gorm:"column:direct_key;type:varchar(64);default:null;uniqueIndex:uk_conv_direct_key" json:"direct_key"`
	GroupID        *uint64    `gorm:"column:group_id;type:bigint unsigned;default:null;uniqueIndex:uk_conv_group_id" json:"group_id,string"`
	LastMsgID      uint64     `gorm:"column:last_msg_id;type:bigint unsigned;not null;default:0" json:"last_msg_id,string"`
	LastSeq        uint64     `gorm:"column:last_seq;type:bigint unsigned;not null;default:0" json:"last_seq,string"`
	LastMsgPreview string     `gorm:"column:last_msg_preview;type:varchar(255);not null;default:''" json:"last_msg_preview"`
	LastMsgAt      *time.Time `gorm:"column:last_msg_at;type:datetime(3);default:null;index:idx_conv_last_msg_at,sort:desc" json:"last_msg_at"`
	CreatedAt      time.Time  `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime" json:"created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (Conversation) TableName() string {
	return "conversations"
}

// ConversationMember maps the `conversation_members` table.
type ConversationMember struct {
	ID               uint64    `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"id,string"`
	ConversationID   uint64    `gorm:"column:conversation_id;type:bigint unsigned;not null;uniqueIndex:uk_cm_conv_user,priority:1" json:"conversation_id,string"`
	UserID           uint64    `gorm:"column:user_id;type:bigint unsigned;not null;uniqueIndex:uk_cm_conv_user,priority:2;index:idx_cm_user_visible_updated,priority:1" json:"user_id,string"`
	LastDeliveredSeq uint64    `gorm:"column:last_delivered_seq;type:bigint unsigned;not null;default:0" json:"last_delivered_seq,string"`
	LastReadSeq      uint64    `gorm:"column:last_read_seq;type:bigint unsigned;not null;default:0" json:"last_read_seq,string"`
	UnreadCount      uint32    `gorm:"column:unread_count;type:int unsigned;not null;default:0" json:"unread_count"`
	IsPinned         bool      `gorm:"column:is_pinned;type:tinyint(1);not null;default:0;index:idx_cm_user_visible_updated,priority:3,sort:desc" json:"is_pinned"`
	IsMuted          bool      `gorm:"column:is_muted;type:tinyint(1);not null;default:0" json:"is_muted"`
	IsVisible        bool      `gorm:"column:is_visible;type:tinyint(1);not null;default:1;index:idx_cm_user_visible_updated,priority:2" json:"is_visible"`
	JoinedAt         time.Time `gorm:"column:joined_at;type:datetime(3);not null;autoCreateTime" json:"joined_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime;index:idx_cm_user_visible_updated,priority:4,sort:desc" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (ConversationMember) TableName() string {
	return "conversation_members"
}
