package repository

import (
	"context"
	"errors"
	"testing"
)

func TestFriendshipRelationWriterGuards(t *testing.T) {
	var writer RelationWriter = (*FriendshipRepository)(nil)
	if err := writer.EnsureBilateralFriendship(context.Background(), nil, 1, 2); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil FriendshipRepository, got %v", err)
	}

	repo := NewFriendshipRepository(nil, nil)
	if err := repo.EnsureBilateralFriendship(context.Background(), nil, 1, 2); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil tx, got %v", err)
	}
}
