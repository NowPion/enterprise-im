package service

import (
	"context"
	"errors"
	"fmt"
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

type recordedDelivery struct {
	UserIDs   []uint64
	FrameType string
	Data      any
}

type groupNotifierSpy struct {
	mu         sync.Mutex
	deliveries []recordedDelivery
}

func (n *groupNotifierSpy) DeliverToUsers(_ context.Context, targetUserIDs []uint64, frameType string, data any) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := append([]uint64(nil), targetUserIDs...)
	n.deliveries = append(n.deliveries, recordedDelivery{
		UserIDs:   cp,
		FrameType: frameType,
		Data:      data,
	})
	return nil
}

func (n *groupNotifierSpy) reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.deliveries = nil
}

func (n *groupNotifierSpy) snapshot() []recordedDelivery {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]recordedDelivery, len(n.deliveries))
	copy(out, n.deliveries)
	return out
}

type inMemoryGroupStore struct {
	mu           sync.Mutex
	seqMu        sync.Mutex
	nextGroupID  uint64
	nextMemberID uint64
	nextConvID   uint64
	nextCMID     uint64
	nextAuditID  uint64

	users          map[uint64]model.User
	groups         map[uint64]model.Group
	groupMembers   map[uint64][]model.GroupMember
	conversations  map[uint64]model.Conversation
	convByGroupID  map[uint64]uint64
	convMembers    map[string]model.ConversationMember
	messages       []model.Message
	assets         map[string]model.Asset
	auditLogs      []model.AuditLog
	online         map[uint64]bool
	convSeqs       map[uint64]uint64
	convLocks      map[uint64]string
	failPresence   error
	failOnAuditLog error
}

func newInMemoryGroupStore() *inMemoryGroupStore {
	return &inMemoryGroupStore{
		nextGroupID:   2000,
		nextMemberID:  1,
		nextConvID:    9000,
		nextCMID:      1,
		nextAuditID:   1,
		users:         make(map[uint64]model.User),
		groups:        make(map[uint64]model.Group),
		groupMembers:  make(map[uint64][]model.GroupMember),
		conversations: make(map[uint64]model.Conversation),
		convByGroupID: make(map[uint64]uint64),
		convMembers:   make(map[string]model.ConversationMember),
		assets:        make(map[string]model.Asset),
		online:        make(map[uint64]bool),
		convSeqs:      make(map[uint64]uint64),
		convLocks:     make(map[uint64]string),
	}
}

func cmKey(convID, userID uint64) string {
	return fmt.Sprintf("%d:%d", convID, userID)
}

func (s *inMemoryGroupStore) clone() *inMemoryGroupStore {
	cp := &inMemoryGroupStore{
		nextGroupID:   s.nextGroupID,
		nextMemberID:  s.nextMemberID,
		nextConvID:    s.nextConvID,
		nextCMID:      s.nextCMID,
		nextAuditID:   s.nextAuditID,
		users:         make(map[uint64]model.User, len(s.users)),
		groups:        make(map[uint64]model.Group, len(s.groups)),
		groupMembers:  make(map[uint64][]model.GroupMember, len(s.groupMembers)),
		conversations: make(map[uint64]model.Conversation, len(s.conversations)),
		convByGroupID: make(map[uint64]uint64, len(s.convByGroupID)),
		convMembers:   make(map[string]model.ConversationMember, len(s.convMembers)),
		messages:      append([]model.Message(nil), s.messages...),
		assets:        make(map[string]model.Asset, len(s.assets)),
		auditLogs:     append([]model.AuditLog(nil), s.auditLogs...),
	}
	for k, v := range s.users {
		cp.users[k] = v
	}
	for k, v := range s.groups {
		cp.groups[k] = v
	}
	for k, v := range s.groupMembers {
		cp.groupMembers[k] = append([]model.GroupMember(nil), v...)
	}
	for k, v := range s.conversations {
		cp.conversations[k] = v
	}
	for k, v := range s.convByGroupID {
		cp.convByGroupID[k] = v
	}
	for k, v := range s.convMembers {
		cp.convMembers[k] = v
	}
	for k, v := range s.assets {
		cp.assets[k] = v
	}
	return cp
}

func (s *inMemoryGroupStore) restore(cp *inMemoryGroupStore) {
	s.nextGroupID = cp.nextGroupID
	s.nextMemberID = cp.nextMemberID
	s.nextConvID = cp.nextConvID
	s.nextCMID = cp.nextCMID
	s.nextAuditID = cp.nextAuditID
	s.users = cp.users
	s.groups = cp.groups
	s.groupMembers = cp.groupMembers
	s.conversations = cp.conversations
	s.convByGroupID = cp.convByGroupID
	s.convMembers = cp.convMembers
	s.messages = cp.messages
	s.assets = cp.assets
	s.auditLogs = cp.auditLogs
}

func (s *inMemoryGroupStore) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.clone()
	if err := fn(nil); err != nil {
		s.restore(snap)
		return err
	}
	return nil
}

func (s *inMemoryGroupStore) LockUsersAscending(_ context.Context, _ *gorm.DB, ids []uint64) (map[uint64]model.User, error) {
	out := make(map[uint64]model.User, len(ids))
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *inMemoryGroupStore) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
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

func (s *inMemoryGroupStore) GetGroupAndConversation(_ context.Context, groupID uint64) (*model.Group, *model.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[groupID]
	if !ok {
		return nil, nil, gorm.ErrRecordNotFound
	}
	cid := s.convByGroupID[groupID]
	c, ok := s.conversations[cid]
	if !ok {
		return &g, nil, nil
	}
	return &g, &c, nil
}

func (s *inMemoryGroupStore) LockGroupByID(_ context.Context, _ *gorm.DB, groupID uint64) (*model.Group, error) {
	g, ok := s.groups[groupID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return &g, nil
}

func (s *inMemoryGroupStore) CreateGroup(_ context.Context, _ *gorm.DB, group *model.Group) error {
	s.nextGroupID++
	group.ID = s.nextGroupID
	s.groups[group.ID] = *group
	return nil
}

func (s *inMemoryGroupStore) UpdateGroupFields(_ context.Context, _ *gorm.DB, groupID uint64, updates map[string]any) error {
	g, ok := s.groups[groupID]
	if !ok {
		return gorm.ErrRecordNotFound
	}
	if v, has := updates["name"]; has {
		g.Name = v.(string)
	}
	if v, has := updates["avatar_url"]; has {
		g.AvatarURL = v.(string)
	}
	if v, has := updates["announcement"]; has {
		if v == nil {
			g.Announcement = nil
		} else {
			str := v.(string)
			g.Announcement = &str
		}
	}
	if v, has := updates["owner_id"]; has {
		g.OwnerID = v.(uint64)
	}
	if v, has := updates["status"]; has {
		g.Status = v.(string)
	}
	if v, has := updates["updated_at"]; has {
		g.UpdatedAt = v.(time.Time)
	}
	s.groups[groupID] = g
	return nil
}

func (s *inMemoryGroupStore) LockGroupConversationByGroupID(_ context.Context, _ *gorm.DB, groupID uint64) (*model.Conversation, error) {
	cid, ok := s.convByGroupID[groupID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	c := s.conversations[cid]
	return &c, nil
}

func (s *inMemoryGroupStore) CreateGroupConversation(_ context.Context, _ *gorm.DB, groupID uint64, now time.Time) (*model.Conversation, error) {
	s.nextConvID++
	gid := groupID
	c := model.Conversation{
		ID:        s.nextConvID,
		Type:      model.ConversationTypeGroup,
		GroupID:   &gid,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.conversations[c.ID] = c
	s.convByGroupID[groupID] = c.ID
	return &c, nil
}

func (s *inMemoryGroupStore) LockGroupMembers(_ context.Context, _ *gorm.DB, groupID uint64) ([]model.GroupMember, error) {
	members := s.groupMembers[groupID]
	out := make([]model.GroupMember, len(members))
	copy(out, members)
	return out, nil
}

func (s *inMemoryGroupStore) GetGroupMember(_ context.Context, groupID, userID uint64) (*model.GroupMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.groupMembers[groupID] {
		if m.UserID == userID {
			cp := m
			return &cp, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *inMemoryGroupStore) LockGroupMember(_ context.Context, _ *gorm.DB, groupID, userID uint64) (*model.GroupMember, error) {
	for _, m := range s.groupMembers[groupID] {
		if m.UserID == userID {
			cp := m
			return &cp, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *inMemoryGroupStore) CreateGroupMembers(_ context.Context, _ *gorm.DB, members []model.GroupMember) error {
	for _, m := range members {
		s.nextMemberID++
		m.ID = s.nextMemberID
		s.groupMembers[m.GroupID] = append(s.groupMembers[m.GroupID], m)
	}
	return nil
}

func (s *inMemoryGroupStore) UpdateGroupMemberRole(_ context.Context, _ *gorm.DB, groupID, userID uint64, role string, now time.Time) error {
	list := s.groupMembers[groupID]
	for i := range list {
		if list[i].UserID == userID {
			list[i].Role = role
			list[i].UpdatedAt = now
		}
	}
	s.groupMembers[groupID] = list
	return nil
}

func (s *inMemoryGroupStore) UpdateGroupMemberNickname(_ context.Context, _ *gorm.DB, groupID, userID uint64, nickname string, now time.Time) error {
	list := s.groupMembers[groupID]
	for i := range list {
		if list[i].UserID == userID {
			list[i].NicknameInGroup = nickname
			list[i].UpdatedAt = now
		}
	}
	s.groupMembers[groupID] = list
	return nil
}

func (s *inMemoryGroupStore) DeleteGroupMember(_ context.Context, _ *gorm.DB, groupID, userID uint64) error {
	list := s.groupMembers[groupID]
	filtered := make([]model.GroupMember, 0, len(list))
	for _, m := range list {
		if m.UserID != userID {
			filtered = append(filtered, m)
		}
	}
	s.groupMembers[groupID] = filtered
	return nil
}

func (s *inMemoryGroupStore) CountGroupMembers(_ context.Context, groupID uint64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.groupMembers[groupID]), nil
}

func (s *inMemoryGroupStore) ListUserGroups(_ context.Context, filter repository.UserGroupListFilter) ([]repository.UserGroupSummaryRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []repository.UserGroupSummaryRow
	for gid, members := range s.groupMembers {
		for _, m := range members {
			if m.UserID != filter.UserID {
				continue
			}
			g := s.groups[gid]
			if filter.Status != "" && g.Status != filter.Status {
				continue
			}
			if filter.CursorID > 0 && g.ID >= filter.CursorID {
				continue
			}
			matched = append(matched, repository.UserGroupSummaryRow{
				Group:          g,
				Role:           m.Role,
				ConversationID: s.convByGroupID[gid],
				MemberCount:    len(members),
			})
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Group.ID > matched[j].Group.ID })
	hasMore := false
	if len(matched) > filter.Limit {
		hasMore = true
		matched = matched[:filter.Limit]
	}
	return matched, hasMore, nil
}

func (s *inMemoryGroupStore) ListGroupMembers(_ context.Context, filter repository.GroupMemberListFilter) ([]model.GroupMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []model.GroupMember
	for _, m := range s.groupMembers[filter.GroupID] {
		if filter.Role != "" && m.Role != filter.Role {
			continue
		}
		if filter.CursorID > 0 && m.ID >= filter.CursorID {
			continue
		}
		matched = append(matched, m)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID > matched[j].ID })
	if len(matched) > filter.Limit {
		matched = matched[:filter.Limit]
	}
	return matched, nil
}

func (s *inMemoryGroupStore) GetConversationMember(_ context.Context, convID, userID uint64) (*model.ConversationMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cm, ok := s.convMembers[cmKey(convID, userID)]
	if !ok {
		return nil, nil
	}
	return &cm, nil
}

func (s *inMemoryGroupStore) CreateConversationMembers(_ context.Context, _ *gorm.DB, members []model.ConversationMember) error {
	for _, cm := range members {
		s.nextCMID++
		cm.ID = s.nextCMID
		s.convMembers[cmKey(cm.ConversationID, cm.UserID)] = cm
	}
	return nil
}

func (s *inMemoryGroupStore) UpsertConversationMembersOnJoin(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, baselineSeq uint64, unreadCount uint32, now time.Time) ([]model.ConversationMember, error) {
	out := make([]model.ConversationMember, 0, len(userIDs))
	sort.Slice(userIDs, func(i, j int) bool { return userIDs[i] < userIDs[j] })
	for _, uid := range userIDs {
		k := cmKey(convID, uid)
		cm, exists := s.convMembers[k]
		if !exists {
			s.nextCMID++
			cm = model.ConversationMember{ID: s.nextCMID, ConversationID: convID, UserID: uid}
		}
		cm.LastDeliveredSeq = baselineSeq
		cm.LastReadSeq = baselineSeq
		cm.UnreadCount = unreadCount
		cm.IsVisible = true
		cm.JoinedAt = now
		cm.UpdatedAt = now
		s.convMembers[k] = cm
		out = append(out, cm)
	}
	return out, nil
}

func (s *inMemoryGroupStore) HideConversationMember(_ context.Context, _ *gorm.DB, convID, userID uint64, now time.Time) error {
	k := cmKey(convID, userID)
	if cm, ok := s.convMembers[k]; ok {
		cm.IsVisible = false
		cm.UnreadCount = 0
		cm.UpdatedAt = now
		s.convMembers[k] = cm
	}
	return nil
}

func (s *inMemoryGroupStore) IncrementActiveGroupMembersUnread(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, now time.Time) error {
	for _, uid := range userIDs {
		k := cmKey(convID, uid)
		if cm, ok := s.convMembers[k]; ok {
			cm.UnreadCount++
			cm.UpdatedAt = now
			s.convMembers[k] = cm
		}
	}
	return nil
}

func (s *inMemoryGroupStore) CreateSystemMessage(_ context.Context, _ *gorm.DB, msg *model.Message) error {
	for _, existing := range s.messages {
		if existing.ConversationID == msg.ConversationID && existing.Seq == msg.Seq {
			return errors.New("duplicate (conversation_id, seq)")
		}
		if existing.SenderID == msg.SenderID && existing.ClientMsgID == msg.ClientMsgID {
			return errors.New("duplicate (sender_id, client_msg_id)")
		}
	}
	s.messages = append(s.messages, *msg)
	return nil
}

func (s *inMemoryGroupStore) UpdateConversationLastMessage(_ context.Context, _ *gorm.DB, convID uint64, msg *model.Message) error {
	c := s.conversations[convID]
	ts := msg.CreatedAt
	c.LastMsgID = msg.ID
	c.LastSeq = msg.Seq
	c.LastMsgPreview = msg.Content
	c.LastMsgAt = &ts
	c.UpdatedAt = ts
	s.conversations[convID] = c
	return nil
}

func (s *inMemoryGroupStore) LockAssetByAssetID(_ context.Context, _ *gorm.DB, assetID string) (*model.Asset, error) {
	a, ok := s.assets[assetID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return &a, nil
}

func (s *inMemoryGroupStore) BindGroupAvatarAsset(_ context.Context, _ *gorm.DB, assetID string, groupID uint64, now time.Time) error {
	for k, a := range s.assets {
		if a.RefType == model.AssetRefGroupAvatar && a.RefID != nil && *a.RefID == groupID && a.AssetID != assetID {
			a.RefType = model.AssetRefNone
			a.RefID = nil
			a.RefCount = 0
			a.UpdatedAt = now
			s.assets[k] = a
		}
	}
	a := s.assets[assetID]
	gid := groupID
	a.RefType = model.AssetRefGroupAvatar
	a.RefID = &gid
	a.RefCount = 1
	if a.BoundAt == nil {
		ts := now
		a.BoundAt = &ts
	}
	a.UpdatedAt = now
	s.assets[assetID] = a
	return nil
}

func (s *inMemoryGroupStore) UnbindGroupAvatarAssets(_ context.Context, _ *gorm.DB, groupID uint64, now time.Time) error {
	for k, a := range s.assets {
		if a.RefType == model.AssetRefGroupAvatar && a.RefID != nil && *a.RefID == groupID {
			a.RefType = model.AssetRefNone
			a.RefID = nil
			a.RefCount = 0
			a.UpdatedAt = now
			s.assets[k] = a
		}
	}
	return nil
}

func (s *inMemoryGroupStore) AppendAuditLog(_ context.Context, _ *gorm.DB, entry *model.AuditLog) error {
	if s.failOnAuditLog != nil {
		return s.failOnAuditLog
	}
	s.nextAuditID++
	entry.ID = s.nextAuditID
	s.auditLogs = append(s.auditLogs, *entry)
	return nil
}

func (s *inMemoryGroupStore) ListGroupAuditLogs(_ context.Context, filter repository.GroupAuditListFilter) ([]model.AuditLog, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []model.AuditLog
	for _, a := range s.auditLogs {
		if a.GroupID == nil || *a.GroupID != filter.GroupID {
			continue
		}
		if filter.From != nil && a.CreatedAt.Before(*filter.From) {
			continue
		}
		if filter.To != nil && a.CreatedAt.After(*filter.To) {
			continue
		}
		if filter.CursorID > 0 && a.ID >= filter.CursorID {
			continue
		}
		matched = append(matched, a)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID > matched[j].ID })
	hasMore := false
	if len(matched) > filter.Limit {
		hasMore = true
		matched = matched[:filter.Limit]
	}
	return matched, hasMore, nil
}

// SeqAllocator implementation for inMemoryGroupStore (thread-safe spin lock using separate Redis mutex).
func (s *inMemoryGroupStore) AcquireConvLock(ctx context.Context, convID uint64) (string, error) {
	tok := fmt.Sprintf("tok-%d-%d", convID, time.Now().UnixNano())
	for {
		s.seqMu.Lock()
		if _, busy := s.convLocks[convID]; !busy {
			s.convLocks[convID] = tok
			s.seqMu.Unlock()
			return tok, nil
		}
		s.seqMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (s *inMemoryGroupStore) AllocateNextSeq(_ context.Context, _ *gorm.DB, convID uint64, lockToken string) (*repository.SeqReservation, error) {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	if s.convLocks[convID] != lockToken {
		return nil, repository.ErrConvLockTimeout
	}
	var maxSeq uint64
	for _, m := range s.messages {
		if m.ConversationID == convID && m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	prevVal, hadPrev := s.convSeqs[convID]
	s.convSeqs[convID] = maxSeq + 1
	return &repository.SeqReservation{
		Seq: maxSeq + 1,
		Rollback: func(context.Context) error {
			s.seqMu.Lock()
			defer s.seqMu.Unlock()
			if !hadPrev {
				delete(s.convSeqs, convID)
			} else {
				s.convSeqs[convID] = prevVal
			}
			return nil
		},
	}, nil
}

func (s *inMemoryGroupStore) ReleaseConvLock(_ context.Context, convID uint64, lockToken string) error {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	if s.convLocks[convID] == lockToken {
		delete(s.convLocks, convID)
	}
	return nil
}

func (s *inMemoryGroupStore) BatchIsOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPresence != nil {
		return nil, s.failPresence
	}
	out := make(map[uint64]bool, len(userIDs))
	for _, uid := range userIDs {
		out[uid] = s.online[uid]
	}
	return out, nil
}

func newTestGroupHarness(t *testing.T) (*GroupService, *inMemoryGroupStore, *groupNotifierSpy) {
	t.Helper()
	store := newInMemoryGroupStore()
	for i := uint64(1); i <= 6; i++ {
		store.users[10000+i] = model.User{
			ID:       10000 + i,
			Username: fmt.Sprintf("u%d", i),
			Nickname: fmt.Sprintf("User%d", i),
			Role:     model.UserRoleUser,
			Status:   model.UserStatusActive,
		}
	}
	store.online[10001] = true
	store.online[10002] = true

	idGen, err := snowflake.NewGenerator(3)
	if err != nil {
		t.Fatalf("snowflake generator: %v", err)
	}
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	notifier := &groupNotifierSpy{}
	svc := NewGroupService(store, store, store, idGen, notifier, log)
	return svc, store, notifier
}

func TestGroupCreateValidationAndAtomicSetup(t *testing.T) {
	ctx := context.Background()
	svc, store, notifier := newTestGroupHarness(t)

	// 1. member_ids containing only creator / duplicates must fail with 10001.
	if _, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:      "Solo",
		MemberIDs: []uint64{10001, 10001},
	}); err == nil || err.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for self-only member_ids, got %v", err)
	}

	// 2. Nonexistent user -> 12001; disabled user -> 11004; >500 members -> 15005.
	if _, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:      "MissingUser",
		MemberIDs: []uint64{99999},
	}); err == nil || err.Code != errcode.ErrUserNotFound.Code {
		t.Fatalf("expected 12001, got %v", err)
	}

	disabled := store.users[10006]
	disabled.Status = model.UserStatusDisabled
	store.users[10006] = disabled
	if _, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:      "DisabledMember",
		MemberIDs: []uint64{10006},
	}); err == nil || err.Code != errcode.ErrAccountDisabled.Code {
		t.Fatalf("expected 11004, got %v", err)
	}

	many := make([]uint64, 500)
	for i := range many {
		many[i] = uint64(20000 + i)
	}
	if _, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:      "TooBig",
		MemberIDs: many,
	}); err == nil || err.Code != errcode.ErrGroupFull.Code {
		t.Fatalf("expected 15005, got %v", err)
	}

	// 3. Avatar asset validation: missing -> 16005, wrong owner -> 16004, wrong purpose -> 10001.
	missingAsset := "11111111-1111-1111-1111-111111111111"
	if _, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:          "BadAsset",
		MemberIDs:     []uint64{10002},
		AvatarAssetID: &missingAsset,
	}); err == nil || err.Code != errcode.ErrAssetNotFound.Code {
		t.Fatalf("expected 16005, got %v", err)
	}

	foreignAsset := "22222222-2222-2222-2222-222222222222"
	store.assets[foreignAsset] = model.Asset{
		AssetID: foreignAsset,
		OwnerID: 10002,
		Purpose: model.AssetPurposeGroupAvatar,
		RefType: model.AssetRefNone,
		Status:  model.AssetStatusActive,
	}
	if _, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:          "ForeignAsset",
		MemberIDs:     []uint64{10002},
		AvatarAssetID: &foreignAsset,
	}); err == nil || err.Code != errcode.ErrAssetAccessDenied.Code {
		t.Fatalf("expected 16004, got %v", err)
	}

	wrongPurposeAsset := "33333333-3333-3333-3333-333333333333"
	store.assets[wrongPurposeAsset] = model.Asset{
		AssetID: wrongPurposeAsset,
		OwnerID: 10001,
		Purpose: model.AssetPurposeFile,
		RefType: model.AssetRefNone,
		Status:  model.AssetStatusActive,
	}
	if _, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:          "WrongPurpose",
		MemberIDs:     []uint64{10002},
		AvatarAssetID: &wrongPurposeAsset,
	}); err == nil || err.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001, got %v", err)
	}

	// 4. Valid group creation with deduplicated member_ids + creator included + valid group_avatar asset.
	validAsset := "44444444-4444-4444-4444-444444444444"
	store.assets[validAsset] = model.Asset{
		AssetID: validAsset,
		OwnerID: 10001,
		Purpose: model.AssetPurposeGroupAvatar,
		RefType: model.AssetRefNone,
		Status:  model.AssetStatusActive,
	}
	ann := "Welcome team"
	res, bizErr := svc.Create(ctx, 10001, CreateGroupInput{
		Name:          "Core Team",
		MemberIDs:     []uint64{10001, 10003, 10002, 10003},
		AvatarAssetID: &validAsset,
		Announcement:  &ann,
		Meta:          GroupAuditMeta{RequestID: "req-create-1"},
	})
	if bizErr != nil {
		t.Fatalf("Create failed: %v", bizErr)
	}
	if res.Group.MemberCount != 3 || res.Group.MaxMembers != 500 || res.Group.OwnerID != 10001 {
		t.Fatalf("unexpected group DTO: %+v", res.Group)
	}
	if res.Group.AvatarURL != "/api/v1/uploads/"+validAsset+"/download" {
		t.Fatalf("unexpected avatar_url: %q", res.Group.AvatarURL)
	}
	if res.Conversation.Type != model.ConversationTypeGroup || res.Conversation.LastSeq != 1 || !res.Conversation.CanSend {
		t.Fatalf("unexpected conversation DTO: %+v", res.Conversation)
	}
	if res.SystemMessage.MsgType != model.MessageTypeSystem || res.SystemMessage.SenderID != 0 || res.SystemMessage.Seq != 1 {
		t.Fatalf("unexpected system message: %+v", res.SystemMessage)
	}
	if len(res.Members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(res.Members))
	}
	if store.assets[validAsset].RefType != model.AssetRefGroupAvatar || *store.assets[validAsset].RefID != res.Group.ID {
		t.Fatalf("expected asset bound to group %d, got %+v", res.Group.ID, store.assets[validAsset])
	}
	if cm := store.convMembers[cmKey(res.Conversation.ID, 10001)]; cm.UnreadCount != 0 || cm.LastReadSeq != 1 {
		t.Fatalf("expected creator read watermark=1 unread=0, got %+v", cm)
	}
	if cm := store.convMembers[cmKey(res.Conversation.ID, 10002)]; cm.UnreadCount != 1 || cm.LastReadSeq != 0 {
		t.Fatalf("expected invited member unread=1 read=0, got %+v", cm)
	}
	if len(store.auditLogs) != 1 || store.auditLogs[0].Action != "group.create" {
		t.Fatalf("expected group.create audit log, got %+v", store.auditLogs)
	}

	deliveries := notifier.snapshot()
	if len(deliveries) != 2 || deliveries[0].FrameType != "msg.push" || deliveries[1].FrameType != "system.notice" {
		t.Fatalf("expected msg.push + system.notice deliveries, got %+v", deliveries)
	}
}

func TestGroupPermissionsTransferRoleLeaveRemoveAndDisband(t *testing.T) {
	ctx := context.Background()
	svc, store, notifier := newTestGroupHarness(t)

	created, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:      "Eng",
		MemberIDs: []uint64{10002, 10003},
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	gid := created.Group.ID
	cid := created.Conversation.ID

	// Non-member rejected with 15002 on Get and Update.
	if _, err := svc.Get(ctx, 10004, gid); err == nil || err.Code != errcode.ErrNotGroupMember.Code {
		t.Fatalf("expected 15002 for non-member Get, got %v", err)
	}
	newName := "Hijacked"
	if _, err := svc.Update(ctx, 10004, gid, UpdateGroupInput{Name: &newName}); err == nil || err.Code != errcode.ErrNotGroupMember.Code {
		t.Fatalf("expected 15002 for non-member Update, got %v", err)
	}

	// Regular member denied (15003) on Update, AddMembers, RemoveMember, Disband, Transfer, UpdateRole, ListAudit.
	if _, err := svc.Update(ctx, 10002, gid, UpdateGroupInput{Name: &newName}); err == nil || err.Code != errcode.ErrGroupRoleDenied.Code {
		t.Fatalf("expected 15003 for member Update, got %v", err)
	}
	if _, err := svc.AddMembers(ctx, 10002, gid, []uint64{10004}, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrGroupRoleDenied.Code {
		t.Fatalf("expected 15003 for member AddMembers, got %v", err)
	}
	if _, err := svc.ListAudit(ctx, 10002, gid, ListGroupAuditInput{}); err == nil || err.Code != errcode.ErrGroupRoleDenied.Code {
		t.Fatalf("expected 15003 for member ListAudit, got %v", err)
	}

	// Owner promotes 10002 to admin; cannot demote self (15009); admin cannot change roles (15003).
	if _, err := svc.UpdateMemberRole(ctx, 10001, gid, 10001, model.GroupRoleMember, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrCannotDemoteOwner.Code {
		t.Fatalf("expected 15009 when demoting owner, got %v", err)
	}
	roleRes, err := svc.UpdateMemberRole(ctx, 10001, gid, 10002, model.GroupRoleAdmin, GroupAuditMeta{})
	if err != nil || roleRes.Role != model.GroupRoleAdmin {
		t.Fatalf("promote to admin failed: res=%+v err=%v", roleRes, err)
	}
	if _, err := svc.UpdateMemberRole(ctx, 10002, gid, 10003, model.GroupRoleAdmin, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrGroupRoleDenied.Code {
		t.Fatalf("expected 15003 when admin updates roles, got %v", err)
	}

	// Admin can update announcement, invite 10004 & 10005, and view audit.
	ann := "Sprint 6"
	if _, err := svc.Update(ctx, 10002, gid, UpdateGroupInput{HasAnnouncement: true, Announcement: &ann}); err != nil {
		t.Fatalf("admin update announcement failed: %v", err)
	}
	if _, err := svc.AddMembers(ctx, 10002, gid, []uint64{10004, 10005}, GroupAuditMeta{}); err != nil {
		t.Fatalf("admin add members failed: %v", err)
	}
	if _, err := svc.AddMembers(ctx, 10002, gid, []uint64{10004}, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrAlreadyGroupMember.Code {
		t.Fatalf("expected 15006 for duplicate invite, got %v", err)
	}
	if _, err := svc.ListAudit(ctx, 10002, gid, ListGroupAuditInput{}); err != nil {
		t.Fatalf("admin ListAudit failed: %v", err)
	}

	// Admin cannot disband (15003), cannot transfer (15003), cannot remove owner (15003), cannot remove self/admin (15003).
	if _, err := svc.Disband(ctx, 10002, gid, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrGroupRoleDenied.Code {
		t.Fatalf("expected 15003 for admin Disband, got %v", err)
	}
	if _, err := svc.Transfer(ctx, 10002, gid, 10003, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrGroupRoleDenied.Code {
		t.Fatalf("expected 15003 for admin Transfer, got %v", err)
	}
	if _, err := svc.RemoveMember(ctx, 10002, gid, 10001, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrGroupRoleDenied.Code {
		t.Fatalf("expected 15003 for admin removing owner, got %v", err)
	}

	// Admin removes regular member 10005: removed user gets ONLY system.notice (no msg.push!).
	notifier.reset()
	remRes, err := svc.RemoveMember(ctx, 10002, gid, 10005, GroupAuditMeta{})
	if err != nil || remRes.UserID != 10005 {
		t.Fatalf("admin remove member failed: %v", err)
	}
	deliveries := notifier.snapshot()
	for _, d := range deliveries {
		if d.FrameType == "msg.push" {
			for _, uid := range d.UserIDs {
				if uid == 10005 {
					t.Fatal("removed user must not receive msg.push")
				}
			}
		}
	}
	var removedNoticed bool
	for _, d := range deliveries {
		if d.FrameType == "system.notice" && len(d.UserIDs) == 1 && d.UserIDs[0] == 10005 {
			removedNoticed = true
		}
	}
	if !removedNoticed {
		t.Fatal("expected removed user to receive system.notice")
	}

	// Owner cannot leave without transfer/disband -> 15008.
	if _, err := svc.Leave(ctx, 10001, gid, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrOwnerLeaveForbidden.Code {
		t.Fatalf("expected 15008 for owner Leave, got %v", err)
	}

	// Transfer ineligible cases -> 15010.
	if _, err := svc.Transfer(ctx, 10001, gid, 10001, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrTransferIneligible.Code {
		t.Fatalf("expected 15010 for self transfer, got %v", err)
	}
	if _, err := svc.Transfer(ctx, 10001, gid, 10005, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrTransferIneligible.Code {
		t.Fatalf("expected 15010 for non-member transfer, got %v", err)
	}

	// Owner transfers ownership to 10003; old owner (10001) becomes member and loses privileges, can now leave.
	trRes, err := svc.Transfer(ctx, 10001, gid, 10003, GroupAuditMeta{})
	if err != nil || trRes.OwnerID != 10003 || trRes.PreviousOwnerID != 10001 {
		t.Fatalf("transfer failed: res=%+v err=%v", trRes, err)
	}
	if _, err := svc.Update(ctx, 10001, gid, UpdateGroupInput{Name: &newName}); err == nil || err.Code != errcode.ErrGroupRoleDenied.Code {
		t.Fatalf("expected former owner to get 15003 on Update, got %v", err)
	}
	if _, err := svc.Leave(ctx, 10001, gid, GroupAuditMeta{}); err != nil {
		t.Fatalf("former owner should now be allowed to leave: %v", err)
	}

	// New owner (10003) disbands the group; remaining members preserved read-only, writes blocked with 15004.
	disRes, err := svc.Disband(ctx, 10003, gid, GroupAuditMeta{})
	if err != nil || disRes.Status != model.GroupStatusDisbanded {
		t.Fatalf("disband failed: res=%+v err=%v", disRes, err)
	}
	getAfter, err := svc.Get(ctx, 10002, gid)
	if err != nil || getAfter.Conversation.CanSend || getAfter.Group.Status != model.GroupStatusDisbanded {
		t.Fatalf("expected read-only disbanded group access for remaining member, got res=%+v err=%v", getAfter, err)
	}
	if _, err := svc.UpdateMyMember(ctx, 10002, gid, "alias"); err == nil || err.Code != errcode.ErrGroupDisbanded.Code {
		t.Fatalf("expected 15004 on UpdateMyMember after disband, got %v", err)
	}
	if _, err := svc.Disband(ctx, 10003, gid, GroupAuditMeta{}); err == nil || err.Code != errcode.ErrGroupDisbanded.Code {
		t.Fatalf("expected 15004 on second Disband, got %v", err)
	}
	if conv := store.conversations[cid]; conv.LastSeq != disRes.SystemMessage.Seq {
		t.Fatalf("expected conversation last_seq %d, got %d", disRes.SystemMessage.Seq, conv.LastSeq)
	}
}

func TestGroupRejoinNewMemberRecordAndWatermarkBaseline(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestGroupHarness(t)

	created, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:      "Watermark Group",
		MemberIDs: []uint64{10002, 10003},
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	gid := created.Group.ID
	cid := created.Conversation.ID

	var firstMemberID uint64
	for _, gm := range store.groupMembers[gid] {
		if gm.UserID == 10002 {
			firstMemberID = gm.ID
		}
	}

	// User 10002 leaves -> group_members deleted, conversation hidden.
	if _, err := svc.Leave(ctx, 10002, gid, GroupAuditMeta{}); err != nil {
		t.Fatalf("leave failed: %v", err)
	}
	if cm := store.convMembers[cmKey(cid, 10002)]; cm.IsVisible || cm.UnreadCount != 0 {
		t.Fatalf("expected hidden conversation member with unread=0 after leave, got %+v", cm)
	}
	if _, err := svc.Get(ctx, 10002, gid); err == nil || err.Code != errcode.ErrNotGroupMember.Code {
		t.Fatalf("expected 15002 after leaving, got %v", err)
	}

	// Advance conversation while 10002 is gone (e.g., two group updates -> seq reaches 4).
	n1 := "Renamed 1"
	n2 := "Renamed 2"
	if _, err := svc.Update(ctx, 10001, gid, UpdateGroupInput{Name: &n1}); err != nil {
		t.Fatalf("update 1 failed: %v", err)
	}
	upd2, err := svc.Update(ctx, 10001, gid, UpdateGroupInput{Name: &n2})
	if err != nil {
		t.Fatalf("update 2 failed: %v", err)
	}
	preJoinSeq := upd2.SystemMessage.Seq // 4

	// Re-invite 10002 -> new group_members ID, watermark baseline = preJoinSeq (4), unread_count = 1 (for seq 5).
	rejoin, err := svc.AddMembers(ctx, 10001, gid, []uint64{10002}, GroupAuditMeta{})
	if err != nil {
		t.Fatalf("rejoin failed: %v", err)
	}
	var secondMemberID uint64
	for _, gm := range store.groupMembers[gid] {
		if gm.UserID == 10002 {
			secondMemberID = gm.ID
		}
	}
	if secondMemberID == 0 || secondMemberID == firstMemberID {
		t.Fatalf("expected new group_members record ID on rejoin, old=%d new=%d", firstMemberID, secondMemberID)
	}
	if len(rejoin.ConversationMembers) != 1 {
		t.Fatalf("expected 1 conversation_member returned, got %d", len(rejoin.ConversationMembers))
	}
	cm := rejoin.ConversationMembers[0]
	if !cm.IsVisible || cm.LastDeliveredSeq != preJoinSeq || cm.LastReadSeq != preJoinSeq || cm.UnreadCount != 1 {
		t.Fatalf("unexpected rejoin watermarks (preJoinSeq=%d): %+v", preJoinSeq, cm)
	}
}

func TestGroupPaginationOnlineOnlyAuditAndRollback(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestGroupHarness(t)

	// Create 3 groups for 10001 to test cursor pagination & status filter.
	g1, _ := svc.Create(ctx, 10001, CreateGroupInput{Name: "G1", MemberIDs: []uint64{10002, 10003, 10004}})
	g2, _ := svc.Create(ctx, 10001, CreateGroupInput{Name: "G2", MemberIDs: []uint64{10002}})
	g3, _ := svc.Create(ctx, 10001, CreateGroupInput{Name: "G3", MemberIDs: []uint64{10002}})
	if _, err := svc.Disband(ctx, 10001, g2.Group.ID, GroupAuditMeta{}); err != nil {
		t.Fatalf("disband g2 failed: %v", err)
	}

	page1, err := svc.List(ctx, 10001, "", "", 2)
	if err != nil || len(page1.Items) != 2 || !page1.HasMore || page1.NextCursor == nil {
		t.Fatalf("expected page1 with 2 items and cursor, got %+v err=%v", page1, err)
	}
	// Cursor bound to user & filter: using 10001's cursor as 10002 or with status=active must fail with 10001.
	if _, err := svc.List(ctx, 10002, "", *page1.NextCursor, 2); err == nil || err.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for cross-user cursor, got %v", err)
	}
	if _, err := svc.List(ctx, 10001, "active", *page1.NextCursor, 2); err == nil || err.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for filter-mismatch cursor, got %v", err)
	}

	page2, err := svc.List(ctx, 10001, "", *page1.NextCursor, 2)
	if err != nil || len(page2.Items) != 1 || page2.HasMore || page2.NextCursor != nil {
		t.Fatalf("expected page2 with 1 item and nil cursor, got %+v err=%v", page2, err)
	}
	_ = g3

	// online_only=true returns only online members (10001, 10002).
	onlinePage, err := svc.ListMembers(ctx, 10001, g1.Group.ID, ListGroupMembersInput{OnlineOnly: true, Limit: 1})
	if err != nil || len(onlinePage.Items) != 1 || !onlinePage.HasMore || onlinePage.NextCursor == nil || !onlinePage.Items[0].Online {
		t.Fatalf("expected 1 online member with next cursor, got %+v err=%v", onlinePage, err)
	}
	onlinePage2, err := svc.ListMembers(ctx, 10001, g1.Group.ID, ListGroupMembersInput{OnlineOnly: true, Cursor: *onlinePage.NextCursor, Limit: 1})
	if err != nil || len(onlinePage2.Items) != 1 || onlinePage2.HasMore {
		t.Fatalf("expected second online member and has_more=false, got %+v err=%v", onlinePage2, err)
	}

	// Presence failure when online_only=true -> 19002.
	store.failPresence = errors.New("redis down")
	if _, err := svc.ListMembers(ctx, 10001, g1.Group.ID, ListGroupMembersInput{OnlineOnly: true, Limit: 10}); err == nil || err.Code != errcode.ErrRedisUnavailable.Code {
		t.Fatalf("expected 19002 when redis fails with online_only=true, got %v", err)
	}
	store.failPresence = nil

	// UpdateMyMember updates nickname_in_group and clears with "".
	meRes, err := svc.UpdateMyMember(ctx, 10002, g1.Group.ID, "DevLead")
	if err != nil || meRes.Member.NicknameInGroup != "DevLead" {
		t.Fatalf("UpdateMyMember failed: %+v err=%v", meRes, err)
	}

	// Audit log time range validation.
	future := time.Now().UTC().Add(time.Hour)
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := svc.ListAudit(ctx, 10001, g1.Group.ID, ListGroupAuditInput{From: &future, To: &past}); err == nil || err.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 when from > to, got %v", err)
	}
	auditPage, err := svc.ListAudit(ctx, 10001, g1.Group.ID, ListGroupAuditInput{From: &past, To: &future, Limit: 10})
	if err != nil || len(auditPage.Items) == 0 {
		t.Fatalf("ListAudit failed: %+v err=%v", auditPage, err)
	}

	// Transaction rollback test: fail mid-transaction on AuditLog append.
	beforeGroups := len(store.groups)
	beforeMsgs := len(store.messages)
	store.failOnAuditLog = errors.New("forced audit failure")
	if _, err := svc.Create(ctx, 10001, CreateGroupInput{Name: "RollbackGroup", MemberIDs: []uint64{10002}}); err == nil {
		t.Fatal("expected failure when audit log insert fails")
	}
	store.failOnAuditLog = nil
	if len(store.groups) != beforeGroups || len(store.messages) != beforeMsgs {
		t.Fatalf("expected rollback with zero leftover groups/messages; groups=%d->%d msgs=%d->%d",
			beforeGroups, len(store.groups), beforeMsgs, len(store.messages))
	}
}

func TestGroupConcurrentInvitesRespectCapacityAndSeq(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestGroupHarness(t)

	for i := uint64(10); i <= 20; i++ {
		store.users[10000+i] = model.User{
			ID:       10000 + i,
			Username: fmt.Sprintf("u%d", i),
			Nickname: fmt.Sprintf("User%d", i),
			Role:     model.UserRoleUser,
			Status:   model.UserStatusActive,
		}
	}

	created, err := svc.Create(ctx, 10001, CreateGroupInput{Name: "CapGroup", MemberIDs: []uint64{10002}})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	gid := created.Group.ID

	// Restrict max_members to 4 to test concurrent capacity enforcement.
	store.mu.Lock()
	g := store.groups[gid]
	g.MaxMembers = 4
	store.groups[gid] = g
	store.mu.Unlock()

	var wg sync.WaitGroup
	errs := make(chan *errcode.Error, 5)
	for i := uint64(10); i < 15; i++ {
		uid := 10000 + i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, addErr := svc.AddMembers(ctx, 10001, gid, []uint64{uid}, GroupAuditMeta{})
			errs <- addErr
		}()
	}
	wg.Wait()
	close(errs)

	var succeeded, full int
	for e := range errs {
		if e == nil {
			succeeded++
		} else if e.Code == errcode.ErrGroupFull.Code {
			full++
		} else {
			t.Fatalf("unexpected concurrent add error: %v", e)
		}
	}
	if succeeded != 2 || full != 3 {
		t.Fatalf("expected 2 succeeded and 3 ErrGroupFull, got succeeded=%d full=%d", succeeded, full)
	}
	if len(store.groupMembers[gid]) != 4 {
		t.Fatalf("expected final member count 4, got %d", len(store.groupMembers[gid]))
	}
}

func TestGroupSystemMessageExtraJSONValid(t *testing.T) {
	// Verify group names with quotes/backslashes produce valid JSON extra & content without error.
	ctx := context.Background()
	svc, _, _ := newTestGroupHarness(t)
	created, err := svc.Create(ctx, 10001, CreateGroupInput{
		Name:      `Team "Alpha" \ Go`,
		MemberIDs: []uint64{10002},
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !strings.Contains(created.SystemMessage.Content, "创建了群聊") {
		t.Fatalf("unexpected content: %q", created.SystemMessage.Content)
	}
}
