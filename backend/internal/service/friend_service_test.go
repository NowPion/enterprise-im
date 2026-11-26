package service

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
)

// inMemoryFriendStore models MySQL friendships/users/conversations/conversation_members
// and Redis presence/route state in memory for deterministic unit testing without mocking production code.
type inMemoryFriendStore struct {
	mu                  sync.Mutex
	nextFriendshipID    uint64
	nextConversationID  uint64
	users               map[uint64]model.User
	friendships         map[[2]uint64]model.Friendship
	conversations       map[string]model.Conversation
	conversationMembers map[[2]uint64]model.ConversationMember

	// Simulated Redis keys: im:presence:user:{uid}, im:route:user:{uid}, im:presence:conn:{conn_id}
	presenceUserCounts map[uint64]int
	routeMembers       map[uint64][]string
	connLeases         map[string]bool

	// Call counters to verify zero N+1 batching
	batchUsersCalls         int
	batchConversationsCalls int
	batchOnlineCalls        int

	forceDBErr    error
	forceRedisErr error
}

func newInMemoryFriendStore() *inMemoryFriendStore {
	return &inMemoryFriendStore{
		nextFriendshipID:    1,
		nextConversationID:  90001,
		users:               make(map[uint64]model.User),
		friendships:         make(map[[2]uint64]model.Friendship),
		conversations:       make(map[string]model.Conversation),
		conversationMembers: make(map[[2]uint64]model.ConversationMember),
		presenceUserCounts:  make(map[uint64]int),
		routeMembers:        make(map[uint64][]string),
		connLeases:          make(map[string]bool),
	}
}

func (m *inMemoryFriendStore) seedUser(u model.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[u.ID] = u
}

func (m *inMemoryFriendStore) seedFriendshipAndConv(uA, uB uint64) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()

	for _, pair := range [][2]uint64{{uA, uB}, {uB, uA}} {
		if existing, ok := m.friendships[pair]; ok {
			existing.Status = model.FriendshipStatusActive
			existing.UpdatedAt = now
			m.friendships[pair] = existing
		} else {
			m.friendships[pair] = model.Friendship{
				ID:        m.nextFriendshipID,
				UserID:    pair[0],
				FriendID:  pair[1],
				Remark:    "",
				Status:    model.FriendshipStatusActive,
				CreatedAt: now,
				UpdatedAt: now,
			}
			m.nextFriendshipID++
		}
	}

	dk, _ := repository.FormatDirectKey(uA, uB)
	conv, ok := m.conversations[dk]
	if !ok {
		convID := m.nextConversationID
		m.nextConversationID++
		dkCopy := dk
		conv = model.Conversation{
			ID:        convID,
			Type:      model.ConversationTypeSingle,
			DirectKey: &dkCopy,
			CreatedAt: now,
			UpdatedAt: now,
		}
		m.conversations[dk] = conv
	}
	for _, uid := range []uint64{uA, uB} {
		cmKey := [2]uint64{conv.ID, uid}
		if cm, exists := m.conversationMembers[cmKey]; exists {
			cm.IsVisible = true
			cm.UpdatedAt = now
			m.conversationMembers[cmKey] = cm
		} else {
			m.conversationMembers[cmKey] = model.ConversationMember{
				ConversationID: conv.ID,
				UserID:         uid,
				IsVisible:      true,
				JoinedAt:       now,
				UpdatedAt:      now,
			}
		}
	}
	return conv.ID
}

func (m *inMemoryFriendStore) ListActiveFriendships(_ context.Context, userID uint64, cursorID uint64, limit int) ([]model.Friendship, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.forceDBErr != nil {
		return nil, m.forceDBErr
	}

	var matched []model.Friendship
	for _, f := range m.friendships {
		if f.UserID == userID && f.Status == model.FriendshipStatusActive {
			if cursorID > 0 && f.ID >= cursorID {
				continue
			}
			matched = append(matched, f)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].ID > matched[j].ID
	})
	if len(matched) > limit+1 {
		matched = matched[:limit+1]
	}
	return matched, nil
}

func (m *inMemoryFriendStore) BatchGetUsersByIDs(_ context.Context, userIDs []uint64) (map[uint64]model.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchUsersCalls++
	if m.forceDBErr != nil {
		return nil, m.forceDBErr
	}
	out := make(map[uint64]model.User, len(userIDs))
	for _, id := range userIDs {
		if u, ok := m.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (m *inMemoryFriendStore) BatchGetSingleConversationsByDirectKeys(_ context.Context, directKeys []string) (map[string]model.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchConversationsCalls++
	if m.forceDBErr != nil {
		return nil, m.forceDBErr
	}
	out := make(map[string]model.Conversation, len(directKeys))
	for _, dk := range directKeys {
		if c, ok := m.conversations[dk]; ok {
			out[dk] = c
		}
	}
	return out, nil
}

func (m *inMemoryFriendStore) GetOnlineStatusBatch(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchOnlineCalls++
	if m.forceRedisErr != nil {
		return nil, m.forceRedisErr
	}
	out := make(map[uint64]bool, len(userIDs))
	for _, uid := range userIDs {
		members := m.routeMembers[uid]
		if len(members) > 0 {
			online := false
			for _, member := range members {
				connID := member
				if idx := strings.IndexByte(member, ':'); idx >= 0 {
					connID = member[idx+1:]
				}
				if m.connLeases[connID] {
					online = true
					break
				}
			}
			out[uid] = online
			continue
		}
		out[uid] = m.presenceUserCounts[uid] > 0
	}
	return out, nil
}

func (m *inMemoryFriendStore) AreActiveFriends(_ context.Context, userA, userB uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.forceDBErr != nil {
		return false, m.forceDBErr
	}
	ab, ok1 := m.friendships[[2]uint64{userA, userB}]
	ba, ok2 := m.friendships[[2]uint64{userB, userA}]
	return ok1 && ok2 && ab.Status == model.FriendshipStatusActive && ba.Status == model.FriendshipStatusActive, nil
}

func (m *inMemoryFriendStore) UpdateRemark(_ context.Context, userID, friendID uint64, remark string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.forceDBErr != nil {
		return m.forceDBErr
	}
	if _, _, err := repository.OrderedUserPair(userID, friendID); err != nil {
		return repository.ErrFriendshipRecordNotFound
	}
	row, ok := m.friendships[[2]uint64{userID, friendID}]
	if !ok {
		return repository.ErrFriendshipRecordNotFound
	}
	if row.Status != model.FriendshipStatusActive {
		return repository.ErrFriendshipInactive
	}
	row.Remark = remark
	row.UpdatedAt = time.Now().UTC()
	m.friendships[[2]uint64{userID, friendID}] = row
	return nil
}

func (m *inMemoryFriendStore) DeactivateBidirectionalFriendship(_ context.Context, userID, friendID uint64) (repository.DeleteFriendshipOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.forceDBErr != nil {
		return repository.DeleteFriendshipOutcome{}, m.forceDBErr
	}
	dk, err := repository.FormatDirectKey(userID, friendID)
	if err != nil {
		return repository.DeleteFriendshipOutcome{}, repository.ErrFriendshipRecordNotFound
	}
	caller, ok := m.friendships[[2]uint64{userID, friendID}]
	if !ok {
		return repository.DeleteFriendshipOutcome{}, repository.ErrFriendshipRecordNotFound
	}
	if caller.Status != model.FriendshipStatusActive {
		return repository.DeleteFriendshipOutcome{}, repository.ErrFriendshipInactive
	}

	now := time.Now().UTC()
	for _, pair := range [][2]uint64{{userID, friendID}, {friendID, userID}} {
		if row, exists := m.friendships[pair]; exists {
			row.Status = model.FriendshipStatusDeleted
			row.UpdatedAt = now
			m.friendships[pair] = row
		}
	}

	outcome := repository.DeleteFriendshipOutcome{
		FriendID:  friendID,
		IsVisible: false,
	}
	if conv, exists := m.conversations[dk]; exists {
		convID := conv.ID
		outcome.ConversationID = &convID
		for _, uid := range []uint64{userID, friendID} {
			cmKey := [2]uint64{conv.ID, uid}
			if cm, cmOk := m.conversationMembers[cmKey]; cmOk {
				cm.IsVisible = false
				cm.UpdatedAt = now
				m.conversationMembers[cmKey] = cm
			}
		}
	}
	return outcome, nil
}

func (m *inMemoryFriendStore) FindSingleConversationByDirectKey(_ context.Context, directKey string) (*model.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.forceDBErr != nil {
		return nil, m.forceDBErr
	}
	conv, ok := m.conversations[directKey]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := conv
	return &cp, nil
}

func TestFriendServiceListBatchPaginationAndPresence(t *testing.T) {
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	store := newInMemoryFriendStore()
	svc := NewFriendService(store, log)

	lastOnline := time.Date(2026, 11, 26, 6, 30, 0, 123000000, time.FixedZone("CST", 8*3600))
	store.seedUser(model.User{ID: 10001, Username: "alice", Nickname: "Alice"})
	store.seedUser(model.User{ID: 10002, Username: "bob", Nickname: "Bob", Signature: "hi", LastOnlineAt: &lastOnline})
	store.seedUser(model.User{ID: 10003, Username: "charlie", Nickname: "Charlie"})
	store.seedUser(model.User{ID: 10004, Username: "dave", Nickname: "Dave"})

	conv12 := store.seedFriendshipAndConv(10001, 10002)
	conv13 := store.seedFriendshipAndConv(10001, 10003)
	conv14 := store.seedFriendshipAndConv(10001, 10004)

	// Multi-device presence:
	// - 10002: 2 devices in route set, 1 active conn lease -> online=true
	// - 10003: stale route set with expired conn lease -> online=false
	// - 10004: no WS connections -> online=false
	store.routeMembers[10002] = []string{"node-1:c-dead", "node-2:c-live"}
	store.connLeases["c-live"] = true
	store.routeMembers[10003] = []string{"node-1:c-expired"}
	store.presenceUserCounts[10003] = 1

	ctx := context.Background()
	page1, err := svc.ListFriends(ctx, 10001, "", 2)
	if err != nil {
		t.Fatalf("ListFriends page1 failed: %v", err)
	}
	if !page1.HasMore || page1.NextCursor == nil || len(page1.Items) != 2 {
		t.Fatalf("unexpected page1 metadata: %+v", page1)
	}
	// Ordered by friendships.id DESC: 10004 first, then 10003.
	if page1.Items[0].FriendID != 10004 || page1.Items[1].FriendID != 10003 {
		t.Fatalf("unexpected page1 order: %+v", page1.Items)
	}
	if page1.Items[0].Online || page1.Items[1].Online {
		t.Fatalf("expected 10004 and 10003 offline, got %+v", page1.Items)
	}
	if *page1.Items[0].ConversationID != strconv.FormatUint(conv14, 10) ||
		*page1.Items[1].ConversationID != strconv.FormatUint(conv13, 10) {
		t.Fatalf("unexpected conversation IDs on page1: %+v", page1.Items)
	}

	// Zero N+1 verification: each batch method called exactly once for page1.
	if store.batchUsersCalls != 1 || store.batchConversationsCalls != 1 || store.batchOnlineCalls != 1 {
		t.Fatalf("expected 1 batch call each, got users=%d convs=%d online=%d",
			store.batchUsersCalls, store.batchConversationsCalls, store.batchOnlineCalls)
	}

	// Cursor bound to user: another user cannot replay 10001's cursor.
	if _, curErr := svc.ListFriends(ctx, 10002, *page1.NextCursor, 2); curErr == nil || curErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 when using another user's cursor, got %v", curErr)
	}

	page2, err := svc.ListFriends(ctx, 10001, *page1.NextCursor, 2)
	if err != nil {
		t.Fatalf("ListFriends page2 failed: %v", err)
	}
	if page2.HasMore || page2.NextCursor != nil || len(page2.Items) != 1 {
		t.Fatalf("unexpected page2 metadata: %+v", page2)
	}
	bob := page2.Items[0]
	if bob.FriendID != 10002 || !bob.Online || !bob.User.Online || *bob.ConversationID != strconv.FormatUint(conv12, 10) {
		t.Fatalf("unexpected bob item: %+v", bob)
	}
	if bob.User.LastOnlineAt == nil || *bob.User.LastOnlineAt != "2026-11-25T22:30:00.123Z" {
		t.Fatalf("expected UTC RFC3339 ms last_online_at, got %v", bob.User.LastOnlineAt)
	}

	// Empty list returns [] (non-nil slice), next_cursor=nil, has_more=false.
	emptyPage, err := svc.ListFriends(ctx, 10099, "", 20)
	if err != nil {
		t.Fatalf("ListFriends empty failed: %v", err)
	}
	if emptyPage.Items == nil || len(emptyPage.Items) != 0 || emptyPage.NextCursor != nil || emptyPage.HasMore {
		t.Fatalf("unexpected empty page: %+v", emptyPage)
	}
}

func TestFriendServiceRemarkSingleSideAndDeleteBidirectional(t *testing.T) {
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	store := newInMemoryFriendStore()
	svc := NewFriendService(store, log)

	store.seedUser(model.User{ID: 10001, Username: "alice", Nickname: "Alice"})
	store.seedUser(model.User{ID: 10002, Username: "bob", Nickname: "Bob"})
	convID := store.seedFriendshipAndConv(10001, 10002)
	ctx := context.Background()

	// 64 UTF-8 runes allowed; 65 runes rejected with 10001.
	maxRemark := strings.Repeat("友", 64)
	resRemark, err := svc.UpdateRemark(ctx, 10001, 10002, maxRemark)
	if err != nil || resRemark.Remark != maxRemark || resRemark.FriendID != 10002 {
		t.Fatalf("expected 64-rune remark to succeed, got %+v err=%v", resRemark, err)
	}
	if _, err := svc.UpdateRemark(ctx, 10001, 10002, strings.Repeat("友", 65)); err == nil || err.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for 65-rune remark, got %v", err)
	}

	// Only caller (10001) remark updated; 10002's side remains empty.
	listA, err := svc.ListFriends(ctx, 10001, "", 20)
	if err != nil || len(listA.Items) != 1 || listA.Items[0].Remark != maxRemark {
		t.Fatalf("expected 10001 to see remark, got %+v err=%v", listA, err)
	}
	listB, err := svc.ListFriends(ctx, 10002, "", 20)
	if err != nil || len(listB.Items) != 1 || listB.Items[0].Remark != "" {
		t.Fatalf("expected 10002 remark untouched, got %+v err=%v", listB, err)
	}

	// Clearing remark with empty string succeeds.
	cleared, err := svc.UpdateRemark(ctx, 10001, 10002, "")
	if err != nil || cleared.Remark != "" {
		t.Fatalf("expected remark clear to succeed, got %+v err=%v", cleared, err)
	}

	// Non-friend remark/delete returns 12008.
	if _, err := svc.UpdateRemark(ctx, 10001, 10099, "nobody"); err == nil || err.Code != errcode.ErrFriendshipNotFound.Code {
		t.Fatalf("expected 12008 for non-friend remark, got %v", err)
	}
	if _, err := svc.DeleteFriend(ctx, 10001, 10099); err == nil || err.Code != errcode.ErrFriendshipNotFound.Code {
		t.Fatalf("expected 12008 for non-friend delete, got %v", err)
	}

	// Delete deactivates both directions and hides both conversation_members.
	delRes, err := svc.DeleteFriend(ctx, 10001, 10002)
	if err != nil {
		t.Fatalf("DeleteFriend failed: %v", err)
	}
	if delRes.FriendID != 10002 || delRes.ConversationID == nil || *delRes.ConversationID != strconv.FormatUint(convID, 10) || delRes.IsVisible {
		t.Fatalf("unexpected delete result: %+v", delRes)
	}

	listAAfter, _ := svc.ListFriends(ctx, 10001, "", 20)
	listBAfter, _ := svc.ListFriends(ctx, 10002, "", 20)
	if len(listAAfter.Items) != 0 || len(listBAfter.Items) != 0 {
		t.Fatalf("expected both friend lists empty after delete, got A=%d B=%d", len(listAAfter.Items), len(listBAfter.Items))
	}

	if cmA := store.conversationMembers[[2]uint64{convID, 10001}]; cmA.IsVisible || cmA.ConversationID != convID {
		t.Fatalf("expected 10001 conversation member hidden and preserved, got %+v", cmA)
	}
	if cmB := store.conversationMembers[[2]uint64{convID, 10002}]; cmB.IsVisible || cmB.ConversationID != convID {
		t.Fatalf("expected 10002 conversation member hidden and preserved, got %+v", cmB)
	}

	// Post-delete remark/delete returns 12007.
	if _, err := svc.UpdateRemark(ctx, 10002, 10001, "x"); err == nil || err.Code != errcode.ErrNotFriends.Code {
		t.Fatalf("expected 12007 on remark after delete, got %v", err)
	}
	if _, err := svc.DeleteFriend(ctx, 10001, 10002); err == nil || err.Code != errcode.ErrNotFriends.Code {
		t.Fatalf("expected 12007 on second delete, got %v", err)
	}

	// Re-adding restores original conversation_id and sets is_visible=true.
	restoredConvID := store.seedFriendshipAndConv(10002, 10001)
	if restoredConvID != convID {
		t.Fatalf("expected restored conversation ID %d, got %d", convID, restoredConvID)
	}
	if !store.conversationMembers[[2]uint64{convID, 10001}].IsVisible || !store.conversationMembers[[2]uint64{convID, 10002}].IsVisible {
		t.Fatal("expected both conversation members visible after restore")
	}
}

func TestFriendServiceInfrastructureErrorsAndCursorValidation(t *testing.T) {
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	store := newInMemoryFriendStore()
	svc := NewFriendService(store, log)
	store.seedUser(model.User{ID: 10002, Username: "bob", Nickname: "Bob"})
	store.seedFriendshipAndConv(10001, 10002)

	ctx := context.Background()
	for _, bad := range []string{"not-base64!", "e30", "eyJzIjoiZ3JvdXBzIiwidSI6IjEwMDAxIiwiaWQiOiIxIn0", "eyJzIjoiZnJpZW5kcyIsInUiOiIxMDAwMSIsImlkIjoiMCJ9"} {
		if _, err := DecodeFriendCursor(bad, 10001); err == nil || err.Code != errcode.ErrInvalidValue.Code {
			t.Fatalf("expected 10001 for cursor %q, got %v", bad, err)
		}
	}

	store.forceRedisErr = errors.New("redis connection refused")
	if _, err := svc.ListFriends(ctx, 10001, "", 20); err == nil || err.Code != errcode.ErrRedisUnavailable.Code {
		t.Fatalf("expected 19002 on redis error, got %v", err)
	}
	store.forceRedisErr = nil

	store.forceDBErr = &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	if _, err := svc.ListFriends(ctx, 10001, "", 20); err == nil || err.Code != errcode.ErrMySQLUnavailable.Code {
		t.Fatalf("expected 19001 on mysql net error, got %v", err)
	}
}
