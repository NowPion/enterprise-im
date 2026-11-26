package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

var (
	// ErrAssetNotFound indicates the asset does not exist or has already been deleted.
	ErrAssetNotFound = errors.New("asset not found")
	// ErrAssetForbidden indicates the caller does not own the asset.
	ErrAssetForbidden = errors.New("asset access forbidden")
	// ErrAssetReferencedByMessage indicates the asset is referenced by at least one message and cannot be deleted.
	ErrAssetReferencedByMessage = errors.New("asset referenced by message")
)

// AssetRepository provides MySQL persistence and authorization queries against `assets`.
type AssetRepository struct {
	db *gorm.DB
}

// NewAssetRepository creates an AssetRepository backed by a GORM MySQL handle.
func NewAssetRepository(db *gorm.DB) *AssetRepository {
	return &AssetRepository{db: db}
}

// Create persists a newly uploaded asset metadata record.
func (r *AssetRepository) Create(ctx context.Context, asset *model.Asset) error {
	if r == nil || r.db == nil {
		return errors.New("mysql db handle is nil")
	}
	if asset == nil || strings.TrimSpace(asset.AssetID) == "" || asset.OwnerID == 0 {
		return errors.New("invalid asset payload")
	}
	return r.db.WithContext(ctx).Create(asset).Error
}

// CreateAvatarAndBindUser atomically inserts a bound user_avatar asset, clears prior active
// user_avatar bindings for userID, and updates `users.avatar_url` in one transaction.
func (r *AssetRepository) CreateAvatarAndBindUser(
	ctx context.Context,
	asset *model.Asset,
	userID uint64,
	avatarURL string,
	boundAt time.Time,
) (*model.User, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("mysql db handle is nil")
	}
	if asset == nil || strings.TrimSpace(asset.AssetID) == "" || userID == 0 {
		return nil, errors.New("invalid avatar asset payload")
	}

	ts := boundAt.UTC()
	var updated model.User
	err := WithTx(ctx, r.db, func(tx *gorm.DB) error {
		var current model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select(selfUserSelectColumns).
			Where("id = ?", userID).
			Take(&current).Error; err != nil {
			return err
		}
		if current.Status != model.UserStatusActive {
			return ErrUserDisabled
		}

		if err := tx.Model(&model.Asset{}).
			Where("ref_type = ? AND ref_id = ? AND status = ?", model.AssetRefUserAvatar, userID, model.AssetStatusActive).
			Updates(map[string]any{
				"ref_type": model.AssetRefNone,
				"ref_id":   nil,
				"ref_count": 0,
			}).Error; err != nil {
			return err
		}

		refID := userID
		asset.OwnerID = userID
		asset.RefType = model.AssetRefUserAvatar
		asset.RefID = &refID
		asset.RefConversationID = nil
		asset.RefCount = 1
		asset.BoundAt = &ts
		asset.Status = model.AssetStatusActive
		if err := tx.Create(asset).Error; err != nil {
			return err
		}

		if err := tx.Model(&model.User{}).
			Where("id = ?", userID).
			Update("avatar_url", avatarURL).Error; err != nil {
			return err
		}

		return tx.Select(selfUserSelectColumns).
			Where("id = ?", userID).
			Take(&updated).Error
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// GetActiveByAssetID loads an active asset record by its external UUID.
func (r *AssetRepository) GetActiveByAssetID(ctx context.Context, assetID string) (*model.Asset, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("mysql db handle is nil")
	}
	trimmed := strings.TrimSpace(assetID)
	if trimmed == "" {
		return nil, ErrAssetNotFound
	}

	var asset model.Asset
	err := r.db.WithContext(ctx).
		Where("asset_id = ? AND status = ?", trimmed, model.AssetStatusActive).
		Take(&asset).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAssetNotFound
		}
		return nil, err
	}
	return &asset, nil
}

// IsConversationMember reports whether userID currently has authorized access to conversationID.
// For single conversations, membership in `conversation_members` is sufficient (preserving read access
// to historical attachments even after unfriending/hiding). For group conversations, the user must also
// hold an active `group_members` record so removed or left members lose attachment access immediately.
func (r *AssetRepository) IsConversationMember(ctx context.Context, conversationID, userID uint64) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("mysql db handle is nil")
	}
	if conversationID == 0 || userID == 0 {
		return false, nil
	}

	var one int
	res := r.db.WithContext(ctx).Raw(
		"SELECT 1 FROM `conversations` AS c "+
			"INNER JOIN `conversation_members` AS cm ON cm.conversation_id = c.id AND cm.user_id = ? "+
			"LEFT JOIN `group_members` AS gm ON gm.group_id = c.group_id AND gm.user_id = ? "+
			"WHERE c.id = ? AND (c.type = ? OR (c.type = ? AND c.group_id IS NOT NULL AND gm.id IS NOT NULL)) "+
			"LIMIT 1",
		userID, userID, conversationID, model.ConversationTypeSingle, model.ConversationTypeGroup,
	).Scan(&one)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0 && one == 1, nil
}

// SoftDeleteOwned atomically verifies asset ownership and absence of message references,
// then marks the asset deleted in MySQL.
func (r *AssetRepository) SoftDeleteOwned(ctx context.Context, assetID string, ownerID uint64, deletedAt time.Time) (*model.Asset, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("mysql db handle is nil")
	}
	trimmed := strings.TrimSpace(assetID)
	if trimmed == "" || ownerID == 0 {
		return nil, ErrAssetNotFound
	}

	ts := deletedAt.UTC()
	var deleted model.Asset
	err := WithTx(ctx, r.db, func(tx *gorm.DB) error {
		var current model.Asset
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("asset_id = ?", trimmed).
			Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAssetNotFound
			}
			return err
		}
		if current.Status != model.AssetStatusActive {
			return ErrAssetNotFound
		}
		if current.OwnerID != ownerID {
			return ErrAssetForbidden
		}
		if current.IsReferencedByMessage() {
			return ErrAssetReferencedByMessage
		}

		if err := tx.Model(&model.Asset{}).
			Where("id = ?", current.ID).
			Updates(map[string]any{
				"status":              model.AssetStatusDeleted,
				"deleted_at":          ts,
				"ref_type":            model.AssetRefNone,
				"ref_id":              nil,
				"ref_conversation_id": nil,
				"ref_count":           0,
			}).Error; err != nil {
			return err
		}

		current.Status = model.AssetStatusDeleted
		current.DeletedAt = &ts
		deleted = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &deleted, nil
}
