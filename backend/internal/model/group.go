package model

import "time"

const (
	GroupStatusActive    = "active"
	GroupStatusDisbanded = "disbanded"

	GroupRoleOwner  = "owner"
	GroupRoleAdmin  = "admin"
	GroupRoleMember = "member"
)

// Group maps the `groups` table.
type Group struct {
	ID           uint64    `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"id,string"`
	Name         string    `gorm:"column:name;type:varchar(100);not null" json:"name"`
	AvatarURL    string    `gorm:"column:avatar_url;type:varchar(512);not null;default:''" json:"avatar_url"`
	OwnerID      uint64    `gorm:"column:owner_id;type:bigint unsigned;not null;index:idx_groups_owner" json:"owner_id,string"`
	Announcement *string   `gorm:"column:announcement;type:text;default:null" json:"announcement"`
	MaxMembers   uint32    `gorm:"column:max_members;type:int unsigned;not null;default:500" json:"max_members"`
	Status       string    `gorm:"column:status;type:enum('active','disbanded');not null;default:'active';index:idx_groups_status_created,priority:1" json:"status"`
	CreatedAt    time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime;index:idx_groups_status_created,priority:2,sort:desc" json:"created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (Group) TableName() string {
	return "groups"
}

// GroupMember maps the `group_members` table.
type GroupMember struct {
	ID              uint64    `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"id,string"`
	GroupID         uint64    `gorm:"column:group_id;type:bigint unsigned;not null;uniqueIndex:uk_gm_group_user,priority:1;index:idx_gm_user_group,priority:2" json:"group_id,string"`
	UserID          uint64    `gorm:"column:user_id;type:bigint unsigned;not null;uniqueIndex:uk_gm_group_user,priority:2;index:idx_gm_user_group,priority:1" json:"user_id,string"`
	Role            string    `gorm:"column:role;type:enum('owner','admin','member');not null;default:'member'" json:"role"`
	NicknameInGroup string    `gorm:"column:nickname_in_group;type:varchar(64);not null;default:''" json:"nickname_in_group"`
	JoinedAt        time.Time `gorm:"column:joined_at;type:datetime(3);not null;autoCreateTime" json:"joined_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (GroupMember) TableName() string {
	return "group_members"
}
