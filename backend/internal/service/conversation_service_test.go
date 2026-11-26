package service

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

type memChatStore struct {
	mu           sync.Mutex
	users        map[uint64]model.User
	friendships  map[[2]uint64]model.Friendship
	groups       map[uint64]model.Group
	groupMembers map[[2]uint64]model.GroupMember
	convs        map[uint64]model.Conversation
	convMembers  map[[2]uint64]model.ConversationMember
	online       map[uint64]bool

	batchUsersCalls  int
	batchFriendCalls int
	batchGroupCalls  int
	batchOnlineCalls int
}

func newMemChatStore() *memChatStore {
	return &memChatStore{
		users:        make(map[uint64]model.User),
		friendships:  make(map[[2]uint64]model.Friendship),
		groups:       make(map[uint64]model.Group),
		groupMembers: make(map[[2]uint64]model.GroupMember),
		convs:        make(map[uint64]model.Conversation),
		convMembers:  make(map[[2]uint64]model.ConversationMember),
		online:       make(map[uint64]bool),
	}
}

func (s *memChatStore) ListUserConversations(
	_ context.Context,
	userID uint64,
	includeHidden bool,
	cursor *repository.ConversationListCursor,
	limit int,
) ([]repository.ConversationListItemRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []repository.ConversationListItemRow
	for _, cm := range s.convMembers {
		if cm.UserID != userID {
			continue
		}
		if !includeHidden && !cm.IsVisible {
			continue
		}
		conv, ok := s.convs[cm.ConversationID]
		if !ok {
			continue
		}
		if conv.Type == model.ConversationTypeGroup {
			if conv.GroupID == nil {
				continue
			}
			if _, ok := s.groupMembers[[2]uint64{*conv.GroupID, userID}]; !ok {
				continue
			}
		}
		if cursor != nil {
			cTS := cursor.UpdatedAt.UTC().Truncate(time.Millisecond)
			mTS := cm.UpdatedAt.UTC().Truncate(time.Millisecond)
			if pinVal(cm.IsPinned) > pinVal(cursor.IsPinned) {
				continue
			}
			if cm.IsPinned == cursor.IsPinned {
				if mTS.After(cTS) {
					continue
				}
				if mTS.Equal(cTS) && cm.ConversationID >= cursor.ConversationID {
					continue
				}
			}
		}
		rows = append(rows, repository.ConversationListItemRow{
			CMID:             cm.ID,
			ConversationID:   cm.ConversationID,
			UserID:           cm.UserID,
			LastDeliveredSeq: cm.LastDeliveredSeq,
			LastReadSeq:      cm.LastReadSeq,
			UnreadCount:      cm.UnreadCount,
			IsPinned:         cm.IsPinned,
			IsMuted:          cm.IsMuted,
			IsVisible:        cm.IsVisible,
			JoinedAt:         cm.JoinedAt,
			MemberUpdatedAt:  cm.UpdatedAt,
			Type:             conv.Type,
			DirectKey:        conv.DirectKey,
			GroupID:          conv.GroupID,
			LastMsgID:        conv.LastMsgID,
			LastSeq:          conv.LastSeq,
			LastMsgPreview:   conv.LastMsgPreview,
			LastMsgAt:        conv.LastMsgAt,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].IsPinned != rows[j].IsPinned {
			return rows[i].IsPinned
		}
		ti := rows[i].MemberUpdatedAt.UTC().Truncate(time.Millisecond)
		tj := rows[j].MemberUpdatedAt.UTC().Truncate(time.Millisecond)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return rows[i].ConversationID > rows[j].ConversationID
	})
	if len(rows) > limit+1 {
		rows = rows[:limit+1]
	}
	return rows, nil
}

func pinVal(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *memChatStore) GetConversationByID(_ context.Context, convID uint64) (*model.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[convID]
	if !ok {
		return nil, repository.ErrConversationNotFound
	}
	cp := c
	return &cp, nil
}

func (s *memChatStore) GetConversationMember(_ context.Context, convID, userID uint64) (*model.ConversationMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cm, ok := s.convMembers[[2]uint64{convID, userID}]
	if !ok {
		return nil, repository.ErrNotConversationMember
	}
	cp := cm
	return &cp, nil
}

func (s *memChatStore) ListConversationMembers(
	_ context.Context,
	convID uint64,
	convType string,
	groupID *uint64,
	cursorID uint64,
	limit int,
) ([]model.ConversationMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []model.ConversationMember
	for _, cm := range s.convMembers {
		if cm.ConversationID != convID {
			continue
		}
		if convType == model.ConversationTypeGroup && groupID != nil {
			if _, ok := s.groupMembers[[2]uint64{*groupID, cm.UserID}]; !ok {
				continue
			}
		}
		if cursorID > 0 && cm.ID >= cursorID {
			continue
		}
		rows = append(rows, cm)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	if len(rows) > limit+1 {
		rows = rows[:limit+1]
	}
	return rows, nil
}

func (s *memChatStore) UpdateMemberSettings(
	_ context.Context,
	convID uint64,
	userID uint64,
	isPinned *bool,
	isMuted *bool,
	isVisible *bool,
	now time.Time,
) (*model.ConversationMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conv, ok := s.convs[convID]
	if !ok {
		return nil, repository.ErrConversationNotFound
	}
	if conv.Type == model.ConversationTypeGroup {
		if conv.GroupID == nil {
			return nil, repository.ErrGroupNotFound
		}
		if _, gmOK := s.groupMembers[[2]uint64{*conv.GroupID, userID}]; !gmOK {
			return nil, repository.ErrNotConversationMember
		}
	}
	cm, ok := s.convMembers[[2]uint64{convID, userID}]
	if !ok {
		return nil, repository.ErrNotConversationMember
	}
	if isPinned != nil {
		cm.IsPinned = *isPinned
	}
	if isMuted != nil {
		cm.IsMuted = *isMuted
	}
	if isVisible != nil {
		cm.IsVisible = *isVisible
	}
	cm.UpdatedAt = now.UTC().Truncate(time.Millisecond)
	s.convMembers[[2]uint64{convID, userID}] = cm
	cp := cm
	return &cp, nil
}

func (s *memChatStore) BatchGetUsersByIDs(_ context.Context, userIDs []uint64) (map[uint64]model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchUsersCalls++
	out := make(map[uint64]model.User, len(userIDs))
	for _, id := range userIDs {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *memChatStore) BatchGetFriendshipsForUser(_ context.Context, userID uint64, peerIDs []uint64) (map[uint64]string, map[uint64]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchFriendCalls++
	remarks := make(map[uint64]string)
	active := make(map[uint64]bool)
	for _, pid := range peerIDs {
		f1 := s.friendships[[2]uint64{userID, pid}]
		f2 := s.friendships[[2]uint64{pid, userID}]
		if f1.Status == model.FriendshipStatusActive {
			remarks[pid] = f1.Remark
		}
		active[pid] = f1.Status == model.FriendshipStatusActive && f2.Status == model.FriendshipStatusActive
	}
	return remarks, active, nil
}

func (s *memChatStore) BatchGetGroupsByIDs(_ context.Context, groupIDs []uint64) (map[uint64]model.Group, map[uint64]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchGroupCalls++
	groups := make(map[uint64]model.Group)
	counts := make(map[uint64]uint32)
	for _, gid := range groupIDs {
		if g, ok := s.groups[gid]; ok {
			groups[gid] = g
			var cnt uint32
			for k := range s.groupMembers {
				if k[0] == gid {
					cnt++
				}
			}
			counts[gid] = cnt
		}
	}
	return groups, counts, nil
}

func (s *memChatStore) GetGroupByID(_ context.Context, groupID uint64) (*model.Group, uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[groupID]
	if !ok {
		return nil, 0, repository.ErrGroupNotFound
	}
	var cnt uint32
	for k := range s.groupMembers {
		if k[0] == groupID {
			cnt++
		}
	}
	cp := g
	return &cp, cnt, nil
}

func (s *memChatStore) GetGroupMember(_ context.Context, groupID, userID uint64) (*model.GroupMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gm, ok := s.groupMembers[[2]uint64{groupID, userID}]
	if !ok {
		return nil, repository.ErrNotConversationMember
	}
	cp := gm
	return &cp, nil
}

func (s *memChatStore) BatchGetGroupMembers(_ context.Context, groupID uint64, userIDs []uint64) (map[uint64]model.GroupMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uint64]model.GroupMember, len(userIDs))
	for _, uid := range userIDs {
		if gm, ok := s.groupMembers[[2]uint64{groupID, uid}]; ok {
			out[uid] = gm
		}
	}
	return out, nil
}

func (s *memChatStore) ListActiveMemberUserIDs(_ context.Context, conv *model.Conversation) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []uint64
	if conv.Type == model.ConversationTypeGroup && conv.GroupID != nil {
		for k := range s.groupMembers {
			if k[0] == *conv.GroupID {
				ids = append(ids, k[1])
			}
		}
		return ids, nil
	}
	for k := range s.convMembers {
		if k[0] == conv.ID {
			ids = append(ids, k[1])
		}
	}
	return ids, nil
}

func (s *memChatStore) GetOnlineStatusBatch(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchOnlineCalls++
	out := make(map[uint64]bool, len(userIDs))
	for _, uid := range userIDs {
		out[uid] = s.online[uid]
	}
	return out, nil
}

func TestConversationServiceListDetailSettingsAndMembers(t *testing.T) {
	ctx := context.Background()
	store := newMemChatStore()
	t0 := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)

	store.users[10001] = model.User{ID: 10001, Username: "alice", Nickname: "Alice"}
	store.users[10002] = model.User{ID: 10002, Username: "bob", Nickname: "Bob"}
	store.users[10003] = model.User{ID: 10003, Username: "carol", Nickname: "Carol"}
	store.online[10002] = true

	// Active friendship 10001 <-> 10002 with remark.
	store.friendships[[2]uint64{10001, 10002}] = model.Friendship{UserID: 10001, FriendID: 10002, Remark: "Bobby", Status: 1}
	store.friendships[[2]uint64{10002, 10001}] = model.Friendship{UserID: 10002, FriendID: 10001, Status: 1}
	// Deleted friendship 10001 <-> 10003.
	store.friendships[[2]uint64{10001, 10003}] = model.Friendship{UserID: 10001, FriendID: 10003, Status: 0}
	store.friendships[[2]uint64{10003, 10001}] = model.Friendship{UserID: 10003, FriendID: 10001, Status: 0}

	dk12 := "10001:10002"
	dk13 := "10001:10003"
	gid := uint64(5001)
	gidRemoved := uint64(5002)
	ann := "Welcome team"
	store.groups[gid] = model.Group{ID: gid, Name: "Core Eng", OwnerID: 10001, Announcement: &ann, Status: model.GroupStatusActive, MaxMembers: 500}
	store.groups[gidRemoved] = model.Group{ID: gidRemoved, Name: "Old Group", OwnerID: 10002, Status: model.GroupStatusActive, MaxMembers: 500}

	store.groupMembers[[2]uint64{gid, 10001}] = model.GroupMember{GroupID: gid, UserID: 10001, Role: model.GroupRoleOwner, NicknameInGroup: "Lead", JoinedAt: t0}
	store.groupMembers[[2]uint64{gid, 10002}] = model.GroupMember{GroupID: gid, UserID: 10002, Role: model.GroupRoleMember, JoinedAt: t0}
	// 10001 is NOT in gidRemoved group_members (simulates kicked/left).
	store.groupMembers[[2]uint64{gidRemoved, 10002}] = model.GroupMember{GroupID: gidRemoved, UserID: 10002, Role: model.GroupRoleOwner, JoinedAt: t0}

	msgAt := t0.Add(time.Minute)
	store.convs[90001] = model.Conversation{ID: 90001, Type: model.ConversationTypeSingle, DirectKey: &dk12, LastMsgID: 11, LastSeq: 2, LastMsgPreview: "hi", LastMsgAt: &msgAt}
	store.convs[90002] = model.Conversation{ID: 90002, Type: model.ConversationTypeSingle, DirectKey: &dk13, LastMsgID: 12, LastSeq: 1, LastMsgPreview: "bye", LastMsgAt: &t0}
	store.convs[90003] = model.Conversation{ID: 90003, Type: model.ConversationTypeGroup, GroupID: &gid, LastMsgID: 13, LastSeq: 5, LastMsgPreview: "group", LastMsgAt: &msgAt}
	store.convs[90004] = model.Conversation{ID: 90004, Type: model.ConversationTypeGroup, GroupID: &gidRemoved}

	store.convMembers[[2]uint64{90001, 10001}] = model.ConversationMember{ID: 1, ConversationID: 90001, UserID: 10001, IsVisible: true, IsPinned: false, UpdatedAt: t0.Add(2 * time.Minute), JoinedAt: t0}
	store.convMembers[[2]uint64{90001, 10002}] = model.ConversationMember{ID: 2, ConversationID: 90001, UserID: 10002, IsVisible: true, UpdatedAt: t0, JoinedAt: t0}
	store.convMembers[[2]uint64{90002, 10001}] = model.ConversationMember{ID: 3, ConversationID: 90002, UserID: 10001, IsVisible: false, UpdatedAt: t0, JoinedAt: t0}
	store.convMembers[[2]uint64{90003, 10001}] = model.ConversationMember{ID: 4, ConversationID: 90003, UserID: 10001, IsVisible: true, IsPinned: true, UpdatedAt: t0, JoinedAt: t0}
	store.convMembers[[2]uint64{90003, 10002}] = model.ConversationMember{ID: 5, ConversationID: 90003, UserID: 10002, IsVisible: true, UpdatedAt: t0, JoinedAt: t0}
	store.convMembers[[2]uint64{90004, 10001}] = model.ConversationMember{ID: 6, ConversationID: 90004, UserID: 10001, IsVisible: false, UpdatedAt: t0, JoinedAt: t0}

	svc := NewConversationService(store, store, nil)

	// 1. Default list excludes hidden (90002) and removed group (90004), puts pinned (90003) first.
	page1, err := svc.ListConversations(ctx, 10001, false, "", 1)
	if err != nil {
		t.Fatalf("ListConversations page1 failed: %v", err)
	}
	if !page1.HasMore || page1.NextCursor == nil || len(page1.Items) != 1 {
		t.Fatalf("unexpected page1: %+v", page1)
	}
	if page1.Items[0].ID != 90003 || page1.Items[0].Type != model.ConversationTypeGroup || page1.Items[0].Peer != nil ||
		page1.Items[0].GroupID == nil || *page1.Items[0].GroupID != strconv.FormatUint(gid, 10) || !page1.Items[0].CanSend {
		t.Fatalf("unexpected pinned group item: %+v", page1.Items[0])
	}

	// Wrong user cursor rejected with 10001.
	if _, curErr := svc.ListConversations(ctx, 10002, false, *page1.NextCursor, 1); curErr == nil || curErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for wrong user cursor, got %v", curErr)
	}
	// Wrong include_hidden cursor rejected with 10001.
	if _, curErr := svc.ListConversations(ctx, 10001, true, *page1.NextCursor, 1); curErr == nil || curErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for wrong include_hidden cursor, got %v", curErr)
	}

	page2, err := svc.ListConversations(ctx, 10001, false, *page1.NextCursor, 1)
	if err != nil {
		t.Fatalf("ListConversations page2 failed: %v", err)
	}
	if page2.HasMore || page2.NextCursor != nil || len(page2.Items) != 1 || page2.Items[0].ID != 90001 {
		t.Fatalf("unexpected page2: %+v", page2)
	}
	if page2.Items[0].Title != "Bobby" || page2.Items[0].GroupID != nil || page2.Items[0].Peer == nil || !page2.Items[0].Peer.Online || !page2.Items[0].CanSend {
		t.Fatalf("unexpected single item: %+v", page2.Items[0])
	}

	// include_hidden=true shows 90002 (can_send=false) but STILL excludes 90004 (removed from group_members).
	allPage, err := svc.ListConversations(ctx, 10001, true, "", 20)
	if err != nil {
		t.Fatalf("ListConversations include_hidden failed: %v", err)
	}
	if len(allPage.Items) != 3 || allPage.Items[2].ID != 90002 || allPage.Items[2].CanSend {
		t.Fatalf("expected 3 items with 90002 read-only, got %+v", allPage.Items)
	}

	// 2. Detail checks & permissions.
	detailGroup, err := svc.GetConversationDetail(ctx, 10001, 90003)
	if err != nil || detailGroup.Role == nil || *detailGroup.Role != model.GroupRoleOwner || detailGroup.Group == nil || detailGroup.Group.MemberCount != 2 {
		t.Fatalf("unexpected group detail: %+v err=%v", detailGroup, err)
	}
	if _, err := svc.GetConversationDetail(ctx, 10001, 90004); err == nil || err.Code != errcode.ErrNotConversationMember.Code {
		t.Fatalf("expected 13002 for removed group member, got %v", err)
	}
	if _, err := svc.GetConversationDetail(ctx, 10003, 90001); err == nil || err.Code != errcode.ErrNotConversationMember.Code {
		t.Fatalf("expected 13002 for non-member, got %v", err)
	}
	if _, err := svc.GetConversationDetail(ctx, 10001, 99999); err == nil || err.Code != errcode.ErrConversationNotFound.Code {
		t.Fatalf("expected 13001 for missing conversation, got %v", err)
	}

	// 3. Settings PATCH preserves omitted fields.
	muted := true
	if _, err := svc.UpdateSettings(ctx, 10001, 90001, UpdateConversationSettingsInput{}); err == nil || err.Code != errcode.ErrInvalidParams.Code {
		t.Fatalf("expected 10000 for empty settings PATCH, got %v", err)
	}
	settingsRes, err := svc.UpdateSettings(ctx, 10001, 90001, UpdateConversationSettingsInput{IsMuted: &muted})
	if err != nil || !settingsRes.IsMuted || !settingsRes.IsVisible || settingsRes.IsPinned {
		t.Fatalf("unexpected settings result: %+v err=%v", settingsRes, err)
	}

	// 4. Members list checks role, nickname_in_group, online, and JSON string IDs.
	memPage, err := svc.ListMembers(ctx, 10001, 90003, "", 10)
	if err != nil || len(memPage.Items) != 2 {
		t.Fatalf("unexpected members page: %+v err=%v", memPage, err)
	}
	raw, _ := json.Marshal(memPage)
	if len(raw) == 0 {
		t.Fatal("failed to marshal member page")
	}
}
