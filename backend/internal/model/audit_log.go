package model

import "time"

const (
	AuditScopeAdmin  = "admin"
	AuditScopeGroup  = "group"
	AuditScopeSystem = "system"

	AuditTargetUser         = "user"
	AuditTargetSession      = "session"
	AuditTargetGroup        = "group"
	AuditTargetConversation = "conversation"
	AuditTargetMessage      = "message"
	AuditTargetAsset        = "asset"
	AuditTargetSystem       = "system"
)

// AuditLog maps the append-only `audit_logs` table.
// Deliberately omits UpdatedAt, DeletedAt, and any business Update/Delete hooks or methods:
// callers and consumers may only append immutable rows via INSERT (idempotent on EventID).
type AuditLog struct {
	ID             uint64       `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement;index:idx_audit_group_created,priority:3,sort:desc" json:"id,string"`
	EventID        string       `gorm:"column:event_id;type:varchar(64);not null;uniqueIndex:uk_audit_event_id" json:"event_id"`
	Scope          string       `gorm:"column:scope;type:enum('admin','group','system');not null;index:idx_audit_scope_action_created,priority:1" json:"scope"`
	Action         string       `gorm:"column:action;type:varchar(64);not null;index:idx_audit_scope_action_created,priority:2" json:"action"`
	ActorID        uint64       `gorm:"column:actor_id;type:bigint unsigned;not null;index:idx_audit_actor_created,priority:1" json:"actor_id,string"`
	ActorRole      string       `gorm:"column:actor_role;type:varchar(32);not null;default:''" json:"actor_role"`
	TargetType     string       `gorm:"column:target_type;type:enum('user','session','group','conversation','message','asset','system');not null;index:idx_audit_target_created,priority:1" json:"target_type"`
	TargetID       uint64       `gorm:"column:target_id;type:bigint unsigned;not null;default:0;index:idx_audit_target_created,priority:2" json:"target_id,string"`
	GroupID        *uint64      `gorm:"column:group_id;type:bigint unsigned;default:null;index:idx_audit_group_created,priority:1" json:"group_id,string,omitempty"`
	ConversationID *uint64      `gorm:"column:conversation_id;type:bigint unsigned;default:null;index:idx_audit_conv_created,priority:1" json:"conversation_id,string,omitempty"`
	Reason         string       `gorm:"column:reason;type:varchar(512);not null;default:''" json:"reason"`
	Detail         NullableJSON `gorm:"column:detail;type:json;default:null" json:"detail"`
	RequestID      string       `gorm:"column:request_id;type:varchar(64);not null;default:''" json:"request_id"`
	IPAddress      string       `gorm:"column:ip_address;type:varchar(64);not null;default:''" json:"ip_address"`
	UserAgent      string       `gorm:"column:user_agent;type:varchar(512);not null;default:''" json:"user_agent"`
	CreatedAt      time.Time    `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime;index:idx_audit_group_created,priority:2,sort:desc;index:idx_audit_scope_action_created,priority:3,sort:desc;index:idx_audit_actor_created,priority:2,sort:desc;index:idx_audit_target_created,priority:3,sort:desc;index:idx_audit_conv_created,priority:2,sort:desc" json:"created_at"`
}

// TableName returns the MySQL table name.
func (AuditLog) TableName() string {
	return "audit_logs"
}
