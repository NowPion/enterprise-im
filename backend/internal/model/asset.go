package model

import "time"

const (
	AssetPurposeImage       = "image"
	AssetPurposeFile        = "file"
	AssetPurposeVoice       = "voice"
	AssetPurposeAvatar      = "avatar"
	AssetPurposeGroupAvatar = "group_avatar"

	AssetRefNone        = "none"
	AssetRefMessage     = "message"
	AssetRefUserAvatar  = "user_avatar"
	AssetRefGroupAvatar = "group_avatar"

	AssetStatusActive  = "active"
	AssetStatusDeleted = "deleted"
)

// Asset maps the `assets` table for uploaded media metadata and reference checks.
// SECURITY INVARIANT: StorageKey and ThumbnailKey are internal relative storage keys
// and MUST NEVER be serialized as external API fields (enforced with `json:"-"`).
type Asset struct {
	ID                 uint64     `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement" json:"-"`
	AssetID            string     `gorm:"column:asset_id;type:char(36);not null;uniqueIndex:uk_assets_asset_id" json:"asset_id"`
	OwnerID            uint64     `gorm:"column:owner_id;type:bigint unsigned;not null;index:idx_assets_owner_status_created,priority:1" json:"owner_id,string"`
	Purpose            string     `gorm:"column:purpose;type:enum('image','file','voice','avatar','group_avatar');not null" json:"purpose"`
	Filename           string     `gorm:"column:filename;type:varchar(255);not null" json:"filename"`
	Mime               string     `gorm:"column:mime;type:varchar(128);not null" json:"mime"`
	SizeBytes          uint64     `gorm:"column:size_bytes;type:bigint unsigned;not null" json:"size_bytes"`
	SHA256             string     `gorm:"column:sha256;type:char(64);not null;default:''" json:"-"`
	Width              *uint32    `gorm:"column:width;type:int unsigned;default:null" json:"width,omitempty"`
	Height             *uint32    `gorm:"column:height;type:int unsigned;default:null" json:"height,omitempty"`
	DurationMs         *uint32    `gorm:"column:duration_ms;type:int unsigned;default:null" json:"duration_ms,omitempty"`
	StorageKey         string     `gorm:"column:storage_key;type:varchar(512);not null" json:"-"`                 // Internal relative storage key only; never expose in API responses.
	ThumbnailKey       *string    `gorm:"column:thumbnail_key;type:varchar(512);default:null" json:"-"`          // Internal relative storage key only; never expose in API responses.
	ThumbnailMime      *string    `gorm:"column:thumbnail_mime;type:varchar(128);default:null" json:"-"`
	ThumbnailSizeBytes *uint64    `gorm:"column:thumbnail_size_bytes;type:bigint unsigned;default:null" json:"-"`
	RefType            string     `gorm:"column:ref_type;type:enum('none','message','user_avatar','group_avatar');not null;default:'none';index:idx_assets_ref,priority:1;index:idx_assets_status_ref_created,priority:2" json:"ref_type"`
	RefID              *uint64    `gorm:"column:ref_id;type:bigint unsigned;default:null;index:idx_assets_ref,priority:2" json:"ref_id,string,omitempty"`
	RefConversationID  *uint64    `gorm:"column:ref_conversation_id;type:bigint unsigned;default:null;index:idx_assets_ref_conv" json:"ref_conversation_id,string,omitempty"`
	RefCount           uint32     `gorm:"column:ref_count;type:int unsigned;not null;default:0" json:"ref_count"`
	BoundAt            *time.Time `gorm:"column:bound_at;type:datetime(3);default:null" json:"bound_at,omitempty"`
	Status             string     `gorm:"column:status;type:enum('active','deleted');not null;default:'active';index:idx_assets_owner_status_created,priority:2;index:idx_assets_status_ref_created,priority:1" json:"status"`
	DeletedAt          *time.Time `gorm:"column:deleted_at;type:datetime(3);default:null" json:"deleted_at,omitempty"`
	CreatedAt          time.Time  `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime;index:idx_assets_owner_status_created,priority:3,sort:desc;index:idx_assets_status_ref_created,priority:3" json:"created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime" json:"updated_at"`
}

// TableName returns the MySQL table name.
func (Asset) TableName() string {
	return "assets"
}

// IsReferencedByMessage reports whether deletion must be rejected with code 16006.
func (a Asset) IsReferencedByMessage() bool {
	return a.RefType == AssetRefMessage && a.RefCount > 0
}
