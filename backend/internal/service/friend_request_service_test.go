package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
)

// inMemoryFriendStore is a real stateful, transactional in-memory repository used to test
// service invariants, lock ordering, rollback behavior, and concurrency without mocks.
type inMemoryFriendStore struct {
	mu                  sync.Mutex
	users               map[uint64]model.User
	requests            map[uint64]model.FriendRequest
	nextRequestID       uint64
	friendships         map[string]model.Friendship
	conversations       map[uint64]model.Conversation
	convByDirectKey     map[string]uint64
	nextConvID          uint64
	members             map[string]model.ConversationMember
	messages            []model.Message
	receipts            []model.MessageReceipt
	onlineUsers         map[uint64]bool
	convSeqCache        map[uint64]uint64
	convLocks           map[uint64]string
	lockLog             [][]uint64
	failOnRelationWrite error
	failOnMsgInsert     error
	failOnPresence      error
}

func newInMemoryFriendStore() *inMemoryFriendStore {
	return &inMemoryFriendStore{
		users:           make(map[uint64]model.User),
		requests:        make(map[uint64]model.FriendRequest),
		nextRequestID:   1,
		friendships:     make(map[string]model.Friendship),
		conversations:   make(map[uint64]model.Conversation),
		convByDirectKey: make(map[string]uint64),
		nextConvID:      100,
		members:         make(map[string]model.ConversationMember),
		onlineUsers:     make(map[uint64]bool),
		convSeqCache:    make(map[uint64]uint64),
		convLocks:       make(map[uint64]string),
	}
}

func (s *inMemoryFriendStore) cloneState() *inMemoryFriendStore {
	cp := &inMemoryFriendStore{
		users:           make(map[uint64]model.User, len(s.users)),
		requests:        make(map[uint64]model.FriendRequest, len(s.requests)),
		nextRequestID:   s.nextRequestID,
		friendships:     make(map[string]model.Friendship, len(s.friendships)),
		conversations:   make(map[uint64]model.Conversation, len(s.conversations)),
		convByDirectKey: make(map[string]uint64, len(s.convByDirectKey)),
		nextConvID:      s.nextConvID,
		members:         make(map[string]model.ConversationMember, len(s.members)),
		messages:        append([]model.Message(nil), s.messages...),
		receipts:        append([]model.MessageReceipt(nil), s.receipts...),
	}
	for k, v := range s.users {
		cp.users[k] = v
	}
	for k, v := range s.requests {
		cp.requests[k] = v
	}
	for k, v := range s.friendships {
		cp.friendships[k] = v
	}
	for k, v := range s.conversations {
		cp.conversations[k] = v
	}
	for k, v := range s.convByDirectKey {
		cp.convByDirectKey[k] = v
	}
	for k, v := range s.members {
		cp.members[k] = v
	}
	return cp
}

func (s *inMemoryFriendStore) restoreState(cp *inMemoryFriendStore) {
	s.users = cp.users
	s.requests = cp.requests
	s.nextRequestID = cp.nextRequestID
	s.friendships = cp.friendships
	s.conversations = cp.conversations
	s.convByDirectKey = cp.convByDirectKey
	s.nextConvID = cp.nextConvID
	s.members = cp.members
	s.messages = cp.messages
	s.receipts = cp.receipts
}

func (s *inMemoryFriendStore) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.cloneState()
	if err := fn(nil); err != nil {
		s.restoreState(snapshot)
		return err
	}
	return nil
}

func (s *inMemoryFriendStore) LockUsersAscending(_ context.Context, _ *gorm.DB, userA, userB uint64) (map[uint64]model.User, error) {
	ids := []uint64{userA, userB}
	if userA == userB {
		ids = []uint64{userA}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	s.lockLog = append(s.lockLog, append([]uint64(nil), ids...))

	out := make(map[uint64]model.User, len(ids))
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *inMemoryFriendStore) HasActiveFriendship(_ context.Context, _ *gorm.DB, userA, userB uint64) (bool, error) {
	k1 := fmt.Sprintf("%d:%d", userA, userB)
	k2 := fmt.Sprintf("%d:%d", userB, userA)
	if f, ok := s.friendships[k1]; ok && f.Status == model.FriendshipStatusActive {
		return true, nil
	}
	if f, ok := s.friendships[k2]; ok && f.Status == model.FriendshipStatusActive {
		return true, nil
	}
	return false, nil
}

func (s *inMemoryFriendStore) HasPendingFriendRequest(_ context.Context, _ *gorm.DB, userA, userB uint64) (bool, error) {
	for _, fr := range s.requests {
		if fr.Status != model.FriendRequestStatusPending {
			continue
		}
		if (fr.FromUserID == userA && fr.ToUserID == userB) || (fr.FromUserID == userB && fr.ToUserID == userA) {
			return true, nil
		}
	}
	return false, nil
}

func (s *inMemoryFriendStore) CreateFriendRequest(_ context.Context, _ *gorm.DB, req *model.FriendRequest) error {
	id := s.nextRequestID
	s.nextRequestID++
	req.ID = id
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	}
	if req.UpdatedAt.IsZero() {
		req.UpdatedAt = req.CreatedAt
	}
	s.requests[id] = *req
	return nil
}

func (s *inMemoryFriendStore) GetFriendRequestByID(_ context.Context, id uint64) (*model.FriendRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fr, ok := s.requests[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := fr
	return &cp, nil
}

func (s *inMemoryFriendStore) LockFriendRequestByID(_ context.Context, _ *gorm.DB, id uint64) (*model.FriendRequest, error) {
	fr, ok := s.requests[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := fr
	return &cp, nil
}

func (s *inMemoryFriendStore) UpdateFriendRequestStatus(_ context.Context, _ *gorm.DB, id uint64, fromStatus, toStatus string, handledAt time.Time) (bool, error) {
	fr, ok := s.requests[id]
	if !ok || fr.Status != fromStatus {
		return false, nil
	}
	fr.Status = toStatus
	ht := handledAt
	fr.HandledAt = &ht
	fr.UpdatedAt = handledAt
	s.requests[id] = fr
	return true, nil
}

func (s *inMemoryFriendStore) ListFriendRequests(_ context.Context, filter repository.FriendRequestListFilter) ([]model.FriendRequest, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var matched []model.FriendRequest
	for _, fr := range s.requests {
		switch filter.Direction {
		case "received":
			if fr.ToUserID != filter.UserID {
				continue
			}
		case "sent":
			if fr.FromUserID != filter.UserID {
				continue
			}
		default:
			return nil, false, fmt.Errorf("unsupported direction %q", filter.Direction)
		}
		if filter.Status != "" && fr.Status != filter.Status {
			continue
		}
		if filter.CursorID > 0 && fr.ID >= filter.CursorID {
			continue
		}
		matched = append(matched, fr)
	}

	sort.Slice(matched, func(i, j int) bool { return matched[i].ID > matched[j].ID })
	hasMore := false
	if len(matched) > filter.Limit {
		hasMore = true
		matched = matched[:filter.Limit]
	}
	return matched, hasMore, nil
}

func (s *inMemoryFriendStore) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uint64]model.User, len(ids))
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *inMemoryFriendStore) FindSingleConversationIDByDirectKey(_ context.Context, directKey string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.convByDirectKey[directKey], nil
}

func (s *inMemoryFriendStore) LockSingleConversationByDirectKey(_ context.Context, _ *gorm.DB, directKey string) (*model.Conversation, error) {
	id, ok := s.convByDirectKey[directKey]
	if !ok {
		return nil, nil
	}
	conv := s.conversations[id]
	return &conv, nil
}

func (s *inMemoryFriendStore) CreateSingleConversation(_ context.Context, _ *gorm.DB, directKey string) (*model.Conversation, error) {
	if _, exists := s.convByDirectKey[directKey]; exists {
		return nil, gorm.ErrDuplicatedKey
	}
	id := s.nextConvID
	s.nextConvID++
	dk := directKey
	now := time.Now().UTC().Truncate(time.Millisecond)
	conv := model.Conversation{
		ID:        id,
		Type:      model.ConversationTypeSingle,
		DirectKey: &dk,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.conversations[id] = conv
	s.convByDirectKey[directKey] = id
	return &conv, nil
}

func (s *inMemoryFriendStore) EnsureConversationMembersVisible(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, baselineSeq uint64, now time.Time) error {
	for _, uid := range userIDs {
		k := fmt.Sprintf("%d:%d", convID, uid)
		if m, exists := s.members[k]; exists {
			m.IsVisible = true
			m.UpdatedAt = now
			s.members[k] = m
		} else {
			s.members[k] = model.ConversationMember{
				ID:               uint64(len(s.members) + 1),
				ConversationID:   convID,
				UserID:           uid,
				LastDeliveredSeq: baselineSeq,
				LastReadSeq:      baselineSeq,
				IsVisible:        true,
				JoinedAt:         now,
				UpdatedAt:        now,
			}
		}
	}
	return nil
}

func (s *inMemoryFriendStore) CreateSystemMessage(_ context.Context, _ *gorm.DB, msg *model.Message) error {
	if s.failOnMsgInsert != nil {
		return s.failOnMsgInsert
	}
	if err := msg.BeforeCreate(nil); err != nil {
		return err
	}
	for _, m := range s.messages {
		if m.ConversationID == msg.ConversationID && m.Seq == msg.Seq {
			return gorm.ErrDuplicatedKey
		}
		if m.SenderID == msg.SenderID && m.ClientMsgID == msg.ClientMsgID {
			return gorm.ErrDuplicatedKey
		}
	}
	s.messages = append(s.messages, *msg)
	return nil
}

func (s *inMemoryFriendStore) UpdateConversationLastMessage(_ context.Context, _ *gorm.DB, convID uint64, msg *model.Message) error {
	conv := s.conversations[convID]
	conv.LastMsgID = msg.ID
	conv.LastSeq = msg.Seq
	conv.LastMsgPreview = msg.Content
	ts := msg.CreatedAt
	conv.LastMsgAt = &ts
	conv.UpdatedAt = ts
	s.conversations[convID] = conv
	return nil
}

func (s *inMemoryFriendStore) IncrementMembersUnreadCount(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, now time.Time) error {
	for _, uid := range userIDs {
		k := fmt.Sprintf("%d:%d", convID, uid)
		m := s.members[k]
		m.UnreadCount++
		m.UpdatedAt = now
		s.members[k] = m
	}
	return nil
}

func (s *inMemoryFriendStore) CreateMessageReceipts(_ context.Context, _ *gorm.DB, receipts []model.MessageReceipt) error {
	s.receipts = append(s.receipts, receipts...)
	return nil
}

func (s *inMemoryFriendStore) EnsureBilateralFriendship(_ context.Context, _ *gorm.DB, userA, userB uint64) error {
	if s.failOnRelationWrite != nil {
		return s.failOnRelationWrite
	}
	minID, maxID := userA, userB
	if minID > maxID {
		minID, maxID = maxID, minID
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	pairs := [][2]uint64{{minID, maxID}, {maxID, minID}}
	for _, p := range pairs {
		k := fmt.Sprintf("%d:%d", p[0], p[1])
		f := s.friendships[k]
		f.UserID = p[0]
		f.FriendID = p[1]
		f.Status = model.FriendshipStatusActive
		if f.CreatedAt.IsZero() {
			f.CreatedAt = now
		}
		f.UpdatedAt = now
		s.friendships[k] = f
	}
	return nil
}

func (s *inMemoryFriendStore) AcquireConvLock(_ context.Context, convID uint64) (string, error) {
	return fmt.Sprintf("lock-%d", convID), nil
}

func (s *inMemoryFriendStore) AllocateNextSeq(_ context.Context, _ *gorm.DB, convID uint64, _ string) (*repository.SeqReservation, error) {
	var maxSeq uint64
	for _, m := range s.messages {
		if m.ConversationID == convID && m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	prev, existed := s.convSeqCache[convID]
	s.convSeqCache[convID] = maxSeq + 1
	return &repository.SeqReservation{
		Seq: maxSeq + 1,
		Rollback: func(context.Context) error {
			if !existed {
				delete(s.convSeqCache, convID)
			} else {
				s.convSeqCache[convID] = prev
			}
			return nil
		},
	}, nil
}

func (s *inMemoryFriendStore) ReleaseConvLock(context.Context, uint64, string) error {
	return nil
}

func (s *inMemoryFriendStore) BatchIsOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	if s.failOnPresence != nil {
		return nil, s.failOnPresence
	}
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		if s.onlineUsers[id] {
			out[id] = true
		}
	}
	return out, nil
}

func newTestService(t *testing.T) (*FriendRequestService, *inMemoryFriendStore) {
	t.Helper()
	store := newInMemoryFriendStore()
	now := time.Now().UTC().Truncate(time.Millisecond)
	store.users[10001] = model.User{ID: 10001, Username: "alice", Nickname: "Alice", Status: model.UserStatusActive, CreatedAt: now}
	store.users[10002] = model.User{ID: 10002, Username: "bob", Nickname: "Bob", Signature: "hi", Status: model.UserStatusActive, CreatedAt: now}
	store.users[10003] = model.User{ID: 10003, Username: "charlie", Nickname: "Charlie", Status: model.UserStatusActive, CreatedAt: now}
	store.users[10004] = model.User{ID: 10004, Username: "disabled_user", Nickname: "Disabled", Status: model.UserStatusDisabled, CreatedAt: now}

	gen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatalf("new snowflake generator: %v", err)
	}
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	svc := NewFriendRequestService(store, store, store, store, gen, log)
	return svc, store
}

func TestCreateFriendRequestValidationDuplicatesAndLockOrder(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t)

	// Self request -> 12002
	if _, err := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 10001, Message: "hi"}); err == nil || err.BizErr.Code != errcode.ErrFriendRequestSelf.Code {
		t.Fatalf("expected 12002 for self request, got %v", err)
	}

	// Nonexistent target -> 12001
	if _, err := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 99999}); err == nil || err.BizErr.Code != errcode.ErrUserNotFound.Code {
		t.Fatalf("expected 12001 for missing target, got %v", err)
	}

	// Disabled target -> 11004
	if _, err := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 10004}); err == nil || err.BizErr.Code != errcode.ErrAccountDisabled.Code {
		t.Fatalf("expected 11004 for disabled target, got %v", err)
	}

	// Message > 255 runes -> 10001
	tooLong := strings.Repeat("好", 256)
	if _, err := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 10002, Message: tooLong}); err == nil || err.BizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for message > 255 runes, got %v", err)
	}

	// Exactly 255 UTF-8 runes succeeds (and locks ascending: [10001, 10002] even if Bob -> Alice).
	exact255 := strings.Repeat("好", 255)
	res, svcErr := svc.Create(ctx, 10002, CreateFriendRequestInput{ToUserID: 10001, Message: exact255})
	if svcErr != nil {
		t.Fatalf("expected create with 255 runes to succeed, got %v", svcErr)
	}
	if res.FromUserID != 10002 || res.ToUserID != 10001 || res.Status != model.FriendRequestStatusPending || res.HandledAt != nil {
		t.Fatalf("unexpected created dto: %+v", res)
	}
	if _, parseErr := time.Parse("2006-01-02T15:04:05.000Z", res.CreatedAt); parseErr != nil {
		t.Fatalf("expected UTC millis timestamp, got %q: %v", res.CreatedAt, parseErr)
	}
	lastLock := store.lockLog[len(store.lockLog)-1]
	if len(lastLock) != 2 || lastLock[0] != 10001 || lastLock[1] != 10002 {
		t.Fatalf("expected ascending lock order [10001, 10002], got %v", lastLock)
	}

	// Same direction duplicate pending -> 12004
	if _, err := svc.Create(ctx, 10002, CreateFriendRequestInput{ToUserID: 10001}); err == nil || err.BizErr.Code != errcode.ErrFriendRequestPending.Code {
		t.Fatalf("expected 12004 for same-direction duplicate, got %v", err)
	}

	// Reverse direction duplicate pending -> 12004
	if _, err := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 10002}); err == nil || err.BizErr.Code != errcode.ErrFriendRequestPending.Code {
		t.Fatalf("expected 12004 for reverse-direction duplicate, got %v", err)
	}

	// Reject allows creating a new request afterward.
	if _, err := svc.Reject(ctx, 10001, res.ID); err != nil {
		t.Fatalf("reject failed: %v", err)
	}
	res2, svcErr := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 10002, Message: "retry"})
	if svcErr != nil {
		t.Fatalf("expected create after reject to succeed, got %v", svcErr)
	}

	// Accept creates active friendship -> subsequent create returns 12003.
	if _, err := svc.Accept(ctx, 10002, res2.ID); err != nil {
		t.Fatalf("accept failed: %v", err)
	}
	if _, err := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 10002}); err == nil || err.BizErr.Code != errcode.ErrAlreadyFriends.Code {
		t.Fatalf("expected 12003 when already friends, got %v", err)
	}
}

func TestListFriendRequestsPaginationFilterAndCursorBinding(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t)
	store.onlineUsers[10002] = true

	// Create 3 received requests for 10001 (from 10002, 10003, and another from 10002 after cancel).
	r1, err := svc.Create(ctx, 10002, CreateFriendRequestInput{ToUserID: 10001, Message: "m1"})
	if err != nil {
		t.Fatalf("create r1: %v", err)
	}
	if _, err := svc.Cancel(ctx, 10002, r1.ID); err != nil {
		t.Fatalf("cancel r1: %v", err)
	}
	r2, err := svc.Create(ctx, 10003, CreateFriendRequestInput{ToUserID: 10001, Message: "m2"})
	if err != nil {
		t.Fatalf("create r2: %v", err)
	}
	r3, err := svc.Create(ctx, 10002, CreateFriendRequestInput{ToUserID: 10001, Message: "m3"})
	if err != nil {
		t.Fatalf("create r3: %v", err)
	}

	// Page 1 (limit=2) -> r3, r2 with has_more=true.
	page1, svcErr := svc.List(ctx, 10001, ListFriendRequestsInput{Direction: "received", Limit: 2})
	if svcErr != nil {
		t.Fatalf("list page 1: %v", svcErr)
	}
	if len(page1.Items) != 2 || !page1.HasMore || page1.NextCursor == nil {
		t.Fatalf("unexpected page1: %+v", page1)
	}
	if page1.Items[0].ID != r3.ID || page1.Items[1].ID != r2.ID {
		t.Fatalf("expected descending order [%d, %d], got [%d, %d]", r3.ID, r2.ID, page1.Items[0].ID, page1.Items[1].ID)
	}
	if page1.Items[0].Peer == nil || page1.Items[0].Peer.ID != 10002 || !page1.Items[0].Peer.Online {
		t.Fatalf("expected peer Bob online=true, got %+v", page1.Items[0].Peer)
	}
	if page1.Items[1].Peer == nil || page1.Items[1].Peer.ID != 10003 || page1.Items[1].Peer.Online {
		t.Fatalf("expected peer Charlie online=false, got %+v", page1.Items[1].Peer)
	}

	// Page 2 -> r1 with has_more=false, next_cursor=nil.
	page2, svcErr := svc.List(ctx, 10001, ListFriendRequestsInput{Direction: "received", Limit: 2, Cursor: *page1.NextCursor})
	if svcErr != nil {
		t.Fatalf("list page 2: %v", svcErr)
	}
	if len(page2.Items) != 1 || page2.Items[0].ID != r1.ID || page2.HasMore || page2.NextCursor != nil {
		t.Fatalf("unexpected page2: %+v", page2)
	}

	// Cursor bound check: reusing page1 cursor for different user, direction, or status must fail 10001.
	if _, err := svc.List(ctx, 10002, ListFriendRequestsInput{Direction: "received", Limit: 2, Cursor: *page1.NextCursor}); err == nil || err.BizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 reusing cursor across users, got %v", err)
	}
	if _, err := svc.List(ctx, 10001, ListFriendRequestsInput{Direction: "sent", Limit: 2, Cursor: *page1.NextCursor}); err == nil || err.BizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 reusing cursor across directions, got %v", err)
	}
	if _, err := svc.List(ctx, 10001, ListFriendRequestsInput{Direction: "received", Status: "pending", Limit: 2, Cursor: *page1.NextCursor}); err == nil || err.BizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 reusing cursor across statuses, got %v", err)
	}

	// Filter by status=pending on sent direction for 10002 -> only r3.
	sentPending, svcErr := svc.List(ctx, 10002, ListFriendRequestsInput{Direction: "sent", Status: "pending"})
	if svcErr != nil {
		t.Fatalf("list sent pending: %v", svcErr)
	}
	if len(sentPending.Items) != 1 || sentPending.Items[0].ID != r3.ID || sentPending.Items[0].Peer.ID != 10001 {
		t.Fatalf("unexpected sentPending: %+v", sentPending)
	}

	// Presence failure degrades gracefully to online=false rather than failing list.
	store.failOnPresence = errors.New("redis blip")
	degradedPage, svcErr := svc.List(ctx, 10001, ListFriendRequestsInput{Direction: "received", Status: "pending"})
	if svcErr != nil || len(degradedPage.Items) != 2 {
		t.Fatalf("expected graceful list despite presence error, got %+v err=%v", degradedPage, svcErr)
	}

	// Query validation bounds.
	if _, err := svc.List(ctx, 10001, ListFriendRequestsInput{}); err == nil || err.BizErr.Code != errcode.ErrInvalidParams.Code {
		t.Fatalf("expected 10000 for missing direction, got %v", err)
	}
	if _, err := svc.List(ctx, 10001, ListFriendRequestsInput{Direction: "all"}); err == nil || err.BizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for invalid direction, got %v", err)
	}
	if _, err := svc.List(ctx, 10001, ListFriendRequestsInput{Direction: "received", Status: "unknown"}); err == nil || err.BizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for invalid status, got %v", err)
	}
	if _, err := svc.List(ctx, 10001, ListFriendRequestsInput{Direction: "received", Limit: -1}); err == nil || err.BizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for negative limit, got %v", err)
	}
	if _, err := svc.List(ctx, 10001, ListFriendRequestsInput{Direction: "received", Limit: 101}); err == nil || err.BizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for limit > 100, got %v", err)
	}
}

func TestConcurrentOppositeDirectionCreateOnlyAllowsOnePending(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	var wg sync.WaitGroup
	okCount := 0
	pendingConflictCount := 0
	var mu sync.Mutex

	pairs := [][2]uint64{{10001, 10002}, {10002, 10001}, {10001, 10002}, {10002, 10001}}
	for _, p := range pairs {
		wg.Add(1)
		go func(from, to uint64) {
			defer wg.Done()
			_, err := svc.Create(ctx, from, CreateFriendRequestInput{ToUserID: to, Message: "race"})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okCount++
			} else if err.BizErr.Code == errcode.ErrFriendRequestPending.Code {
				pendingConflictCount++
			} else {
				t.Errorf("unexpected error in concurrent opposite create: %v", err)
			}
		}(p[0], p[1])
	}
	wg.Wait()

	if okCount != 1 || pendingConflictCount != len(pairs)-1 {
		t.Fatalf("expected 1 success and %d 12004 conflicts, got ok=%d conflict=%d", len(pairs)-1, okCount, pendingConflictCount)
	}
}

func TestAcceptTransactionRollbackRestorationAndConcurrency(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t)

	// Create request 10001 -> 10002
	req, svcErr := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 10002, Message: "let's connect"})
	if svcErr != nil {
		t.Fatalf("create failed: %v", svcErr)
	}

	// Sender (10001) or third party (10003) cannot accept -> 403.
	if _, err := svc.Accept(ctx, 10001, req.ID); err == nil || err.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 when sender accepts, got %v", err)
	}
	if _, err := svc.Accept(ctx, 10003, req.ID); err == nil || err.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 when third party accepts, got %v", err)
	}

	// Simulate rollback on DB failure inside Accept -> nothing persisted, request stays pending.
	store.failOnMsgInsert = errors.New("simulated insert failure")
	if _, err := svc.Accept(ctx, 10002, req.ID); err == nil {
		t.Fatal("expected accept to fail on simulated insert failure")
	}
	store.failOnMsgInsert = nil

	if store.requests[req.ID].Status != model.FriendRequestStatusPending {
		t.Fatalf("expected request status to stay pending after rollback, got %q", store.requests[req.ID].Status)
	}
	if len(store.friendships) != 0 || len(store.conversations) != 0 || len(store.messages) != 0 {
		t.Fatalf("expected zero dirty rows after rollback: friendships=%d convs=%d msgs=%d", len(store.friendships), len(store.conversations), len(store.messages))
	}

	// Concurrent accept race: 8 goroutines attempt Accept simultaneously -> 1 succeeds, 7 get 12006.
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan *AcceptFriendRequestResult, workers)
	errs := make(chan *ServiceError, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.Accept(ctx, 10002, req.ID)
			if err != nil {
				errs <- err
				return
			}
			results <- res
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	if len(results) != 1 {
		t.Fatalf("expected exactly 1 successful accept under concurrency, got %d", len(results))
	}
	for err := range errs {
		if err.BizErr.Code != errcode.ErrFriendRequestHandled.Code {
			t.Fatalf("expected 12006 for concurrent loser, got %v", err)
		}
	}
	accepted := <-results
	if accepted.FriendID != 10001 || accepted.ConversationID == 0 || accepted.SystemMsgID == 0 {
		t.Fatalf("unexpected accept result: %+v", accepted)
	}
	if len(store.messages) != 1 || store.messages[0].Seq != 1 || store.messages[0].MsgType != model.MessageTypeSystem || store.messages[0].SenderID != 0 {
		t.Fatalf("unexpected system messages: %+v", store.messages)
	}
	if len(store.friendships) != 2 {
		t.Fatalf("expected 2 bilateral friendships, got %d", len(store.friendships))
	}

	// Simulate unfriend (status=0 + conversation_members.is_visible=false), then re-request and re-accept:
	// must restore friendships, reuse the SAME conversation ID (direct_key 10001:10002), restore members, and emit seq=2.
	store.mu.Lock()
	for k, f := range store.friendships {
		f.Status = model.FriendshipStatusDeleted
		store.friendships[k] = f
	}
	for k, m := range store.members {
		m.IsVisible = false
		store.members[k] = m
	}
	store.mu.Unlock()

	req2, svcErr := svc.Create(ctx, 10002, CreateFriendRequestInput{ToUserID: 10001, Message: "add back"})
	if svcErr != nil {
		t.Fatalf("re-create after unfriend failed: %v", svcErr)
	}
	accepted2, svcErr := svc.Accept(ctx, 10001, req2.ID)
	if svcErr != nil {
		t.Fatalf("re-accept failed: %v", svcErr)
	}
	if accepted2.FriendID != 10002 || accepted2.ConversationID != accepted.ConversationID {
		t.Fatalf("expected same conversation %d and friend_id 10002, got %+v", accepted.ConversationID, accepted2)
	}
	if len(store.messages) != 2 || store.messages[1].Seq != 2 {
		t.Fatalf("expected second system message with seq=2, got %+v", store.messages)
	}
	for _, uid := range []uint64{10001, 10002} {
		m := store.members[fmt.Sprintf("%d:%d", accepted2.ConversationID, uid)]
		if !m.IsVisible {
			t.Fatalf("expected conversation member %d restored visible=true", uid)
		}
	}
}

func TestRejectAndCancelPermissionsAndStateGuards(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t)

	// Missing request -> 12005
	if _, err := svc.Reject(ctx, 10002, 999); err == nil || err.BizErr.Code != errcode.ErrFriendRequestNotFound.Code {
		t.Fatalf("expected 12005 on reject missing, got %v", err)
	}
	if _, err := svc.Cancel(ctx, 10001, 999); err == nil || err.BizErr.Code != errcode.ErrFriendRequestNotFound.Code {
		t.Fatalf("expected 12005 on cancel missing, got %v", err)
	}

	req1, svcErr := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 10002})
	if svcErr != nil {
		t.Fatalf("create req1: %v", svcErr)
	}

	// Sender cannot reject; recipient cannot cancel.
	if _, err := svc.Reject(ctx, 10001, req1.ID); err == nil || err.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 when sender rejects, got %v", err)
	}
	if _, err := svc.Cancel(ctx, 10002, req1.ID); err == nil || err.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 when recipient cancels, got %v", err)
	}

	// Recipient rejects -> rejected, handled_at set, no friendships/convs created.
	rej, svcErr := svc.Reject(ctx, 10002, req1.ID)
	if svcErr != nil {
		t.Fatalf("reject failed: %v", svcErr)
	}
	if rej.Status != model.FriendRequestStatusRejected || rej.HandledAt == nil {
		t.Fatalf("unexpected rejected dto: %+v", rej)
	}
	if len(store.friendships) != 0 || len(store.conversations) != 0 {
		t.Fatalf("reject must not create friendships or conversations")
	}

	// Rejecting or canceling a handled request -> 12006.
	if _, err := svc.Reject(ctx, 10002, req1.ID); err == nil || err.BizErr.Code != errcode.ErrFriendRequestHandled.Code {
		t.Fatalf("expected 12006 on repeat reject, got %v", err)
	}
	if _, err := svc.Cancel(ctx, 10001, req1.ID); err == nil || err.BizErr.Code != errcode.ErrFriendRequestHandled.Code {
		t.Fatalf("expected 12006 on cancel after reject, got %v", err)
	}

	// Sender cancels pending request -> canceled, handled_at set; cannot accept afterward.
	req2, svcErr := svc.Create(ctx, 10001, CreateFriendRequestInput{ToUserID: 10002})
	if svcErr != nil {
		t.Fatalf("create req2: %v", svcErr)
	}
	can, svcErr := svc.Cancel(ctx, 10001, req2.ID)
	if svcErr != nil {
		t.Fatalf("cancel failed: %v", svcErr)
	}
	if can.Status != model.FriendRequestStatusCanceled || can.HandledAt == nil {
		t.Fatalf("unexpected canceled dto: %+v", can)
	}
	if _, err := svc.Accept(ctx, 10002, req2.ID); err == nil || err.BizErr.Code != errcode.ErrFriendRequestHandled.Code {
		t.Fatalf("expected 12006 accepting canceled request, got %v", err)
	}
}
