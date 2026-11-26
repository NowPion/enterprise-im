package repository

import (
	"context"

	"gorm.io/gorm"
)

var _ RelationWriter = (*FriendshipRepository)(nil)

// EnsureBilateralFriendship adapts FriendshipRepository to repository.RelationWriter
// so friend-request acceptance delegates bilateral friendship writes to the friendship module.
func (r *FriendshipRepository) EnsureBilateralFriendship(ctx context.Context, tx *gorm.DB, userA, userB uint64) error {
	if r == nil {
		return ErrRepositoryUnavailable
	}
	return UpsertBidirectionalFriendshipTx(ctx, tx, userA, userB)
}
