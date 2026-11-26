package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

// RelationWriter writes or restores bidirectional friendship records within an existing DB transaction.
// Stage 4 integration can swap this implementation with the friendship module's writer.
type RelationWriter interface {
	EnsureBilateralFriendship(ctx context.Context, tx *gorm.DB, userA, userB uint64) error
}

// GormRelationWriter is the default MySQL/GORM implementation of RelationWriter.
type GormRelationWriter struct{}

// NewGormRelationWriter constructs a GormRelationWriter.
func NewGormRelationWriter() *GormRelationWriter {
	return &GormRelationWriter{}
}

// EnsureBilateralFriendship inserts or restores (status=1) both directional rows in `friendships`
// in deterministic ascending user ID order inside tx.
func (w *GormRelationWriter) EnsureBilateralFriendship(ctx context.Context, tx *gorm.DB, userA, userB uint64) error {
	if tx == nil {
		return errors.New("tx must not be nil")
	}
	if userA == 0 || userB == 0 || userA == userB {
		return fmt.Errorf("invalid friendship pair (%d, %d)", userA, userB)
	}

	minID, maxID := userA, userB
	if minID > maxID {
		minID, maxID = maxID, minID
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	rows := []model.Friendship{
		{
			UserID:    minID,
			FriendID:  maxID,
			Status:    model.FriendshipStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			UserID:    maxID,
			FriendID:  minID,
			Status:    model.FriendshipStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}

	return tx.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "user_id"},
				{Name: "friend_id"},
			},
			DoUpdates: clause.Assignments(map[string]any{
				"status":     model.FriendshipStatusActive,
				"updated_at": now,
			}),
		}).
		Create(&rows).Error
}
