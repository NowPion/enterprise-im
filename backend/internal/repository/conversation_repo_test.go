package repository

import (
	"context"
	"testing"
)

func TestParseDirectKeyPeer(t *testing.T) {
	dk := "10001:10002"
	if peer, ok := ParseDirectKeyPeer(&dk, 10001); !ok || peer != 10002 {
		t.Fatalf("expected peer 10002, got (%d, %v)", peer, ok)
	}
	if peer, ok := ParseDirectKeyPeer(&dk, 10002); !ok || peer != 10001 {
		t.Fatalf("expected peer 10001, got (%d, %v)", peer, ok)
	}
	if _, ok := ParseDirectKeyPeer(&dk, 10003); ok {
		t.Fatal("expected non-participant to return false")
	}
	bad := "10001:10001"
	if _, ok := ParseDirectKeyPeer(&bad, 10001); ok {
		t.Fatal("expected identical pair to return false")
	}
	if _, ok := ParseDirectKeyPeer(nil, 10001); ok {
		t.Fatal("expected nil direct_key to return false")
	}
}

func TestConversationRepositoryNilGuards(t *testing.T) {
	var repo *ConversationRepository
	ctx := context.Background()

	if _, err := repo.ListUserConversations(ctx, 1, false, nil, 10); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}
	if _, err := repo.GetConversationByID(ctx, 1); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}
	if _, err := repo.GetConversationMember(ctx, 1, 1); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}
	if _, _, err := repo.BatchGetFriendshipsForUser(ctx, 1, []uint64{2}); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}
	if _, _, err := repo.BatchGetGroupsByIDs(ctx, []uint64{1}); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}
}
