package model

import "time"

const (
	ReceiptStatusSent      = "sent"
	ReceiptStatusDelivered = "delivered"
	ReceiptStatusRead      = "read"
)

// MessageReceipt maps the `message_receipts` table.
type MessageReceipt struct {
	ID             uint64     `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"id,string"`
	MessageID      uint64     `gorm:"column:message_id;type:bigint unsigned;not null;uniqueIndex:uk_receipt_msg_user,priority:1" json:"message_id,string"`
	ConversationID uint64     `gorm:"column:conversation_id;type:bigint unsigned;not null;index:idx_receipt_conv_user_seq,priority:1;index:idx_receipt_conv_user_status,priority:1" json:"conversation_id,string"`
	Seq            uint64     `gorm:"column:seq;type:bigint unsigned;not null;index:idx_receipt_conv_user_seq,priority:3" json:"seq,string"`
	UserID         uint64     `gorm:"column:user_id;type:bigint unsigned;not null;uniqueIndex:uk_receipt_msg_user,priority:2;index:idx_receipt_conv_user_seq,priority:2;index:idx_receipt_conv_user_status,priority:2" json:"user_id,string"`
	Status         string     `gorm:"column:status;type:enum('sent','delivered','read');not null;default:'sent';index:idx_receipt_conv_user_status,priority:3" json:"status"`
	DeliveredAt    *time.Time `gorm:"column:delivered_at;type:datetime(3);default:null" json:"delivered_at"`
	ReadAt         *time.Time `gorm:"column:read_at;type:datetime(3);default:null" json:"read_at"`
	CreatedAt      time.Time  `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime" json:"created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (MessageReceipt) TableName() string {
	return "message_receipts"
}
