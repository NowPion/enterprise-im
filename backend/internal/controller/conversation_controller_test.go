package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type convCtrlTestStore struct {
	convs       map[uint64]model.Conversation
	convMembers map[[2]uint64]model.ConversationMember
	users       map[uint64]model.User
	friendships map[[2]uint64]model.Friendship
	groups      map[uint64]model.Group
	groupMems   map[[2]uint64]model.GroupMember
	online      map[uint64]bool
}

func newConvCtrlTestStore() *convCtrlTestStore {
	now := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	dk := "10001:10002"
	return &convCtrlTestStore{
		convs: map[uint64]model.Conversation{
			90001: {ID: 90001, Type: model.ConversationTypeSingle, DirectKey: &dk, LastMsgID: 55, LastSeq: 1, LastMsgPreview: "hi", LastMsgAt: &now},
		},
		convMembers: map[[2]uint64]model.ConversationMember{
			{90001, 10001}: {ID: 1, ConversationID: 90001, UserID: 10001, IsVisible: true, JoinedAt: now, UpdatedAt: now},
			{90001, 10002}: {ID: 2, ConversationID: 90001, UserID: 10002, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		},
		users: map[uint64]model.User{
			10001: {ID: 10001, Username: "alice", Nickname: "Alice"},
			10002: {ID: 10002, Username: "bob", Nickname: "Bob"},
		},
		friendships: map[[2]uint64]model.Friendship{
			{10001, 10002}: {UserID: 10001, FriendID: 10002, Status: 1},
			{10002, 10001}: {UserID: 10002, FriendID: 10001, Status: 1},
		},
		groups:    make(map[uint64]model.Group),
		groupMems: make(map[[2]uint64]model.GroupMember),
		online:    map[uint64]bool{10002: true},
	}
}

func (s *convCtrlTestStore) ListUserConversations(_ context.Context, userID uint64, includeHidden bool, _ *repository.ConversationListCursor, _ int) ([]repository.ConversationListItemRow, error) {
	var rows []repository.ConversationListItemRow
	for _, cm := range s.convMembers {
		if cm.UserID != userID || (!includeHidden && !cm.IsVisible) {
			continue
		}
		c := s.convs[cm.ConversationID]
		rows = append(rows, repository.ConversationListItemRow{
			CMID: cm.ID, ConversationID: c.ID, UserID: userID, IsVisible: cm.IsVisible,
			IsPinned: cm.IsPinned, IsMuted: cm.IsMuted, JoinedAt: cm.JoinedAt, MemberUpdatedAt: cm.UpdatedAt,
			Type: c.Type, DirectKey: c.DirectKey, LastMsgID: c.LastMsgID, LastSeq: c.LastSeq,
			LastMsgPreview: c.LastMsgPreview, LastMsgAt: c.LastMsgAt,
		})
	}
	return rows, nil
}
func (s *convCtrlTestStore) GetConversationByID(_ context.Context, convID uint64) (*model.Conversation, error) {
	c, ok := s.convs[convID]
	if !ok {
		return nil, repository.ErrConversationNotFound
	}
	return &c, nil
}
func (s *convCtrlTestStore) GetConversationMember(_ context.Context, convID, userID uint64) (*model.ConversationMember, error) {
	cm, ok := s.convMembers[[2]uint64{convID, userID}]
	if !ok {
		return nil, repository.ErrNotConversationMember
	}
	return &cm, nil
}
func (s *convCtrlTestStore) ListConversationMembers(_ context.Context, convID uint64, _ string, _ *uint64, _ uint64, _ int) ([]model.ConversationMember, error) {
	var out []model.ConversationMember
	for _, cm := range s.convMembers {
		if cm.ConversationID == convID {
			out = append(out, cm)
		}
	}
	return out, nil
}
func (s *convCtrlTestStore) UpdateMemberSettings(_ context.Context, convID, userID uint64, isPinned, isMuted, isVisible *bool, now time.Time) (*model.ConversationMember, error) {
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
	cm.UpdatedAt = now.UTC()
	s.convMembers[[2]uint64{convID, userID}] = cm
	return &cm, nil
}
func (s *convCtrlTestStore) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
	out := make(map[uint64]model.User, len(ids))
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}
func (s *convCtrlTestStore) BatchGetFriendshipsForUser(_ context.Context, userID uint64, peerIDs []uint64) (map[uint64]string, map[uint64]bool, error) {
	remarks, active := make(map[uint64]string), make(map[uint64]bool)
	for _, pid := range peerIDs {
		active[pid] = s.friendships[[2]uint64{userID, pid}].Status == 1 && s.friendships[[2]uint64{pid, userID}].Status == 1
	}
	return remarks, active, nil
}
func (s *convCtrlTestStore) BatchGetGroupsByIDs(_ context.Context, _ []uint64) (map[uint64]model.Group, map[uint64]uint32, error) {
	return map[uint64]model.Group{}, map[uint64]uint32{}, nil
}
func (s *convCtrlTestStore) GetGroupByID(_ context.Context, _ uint64) (*model.Group, uint32, error) {
	return nil, 0, repository.ErrGroupNotFound
}
func (s *convCtrlTestStore) GetGroupMember(_ context.Context, _, _ uint64) (*model.GroupMember, error) {
	return nil, repository.ErrNotConversationMember
}
func (s *convCtrlTestStore) BatchGetGroupMembers(_ context.Context, _ uint64, _ []uint64) (map[uint64]model.GroupMember, error) {
	return map[uint64]model.GroupMember{}, nil
}
func (s *convCtrlTestStore) GetOnlineStatusBatch(_ context.Context, ids []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		out[id] = s.online[id]
	}
	return out, nil
}

func TestConversationControllerValidationAndEnvelopes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newConvCtrlTestStore()
	ctrl := NewConversationController(service.NewConversationService(store, store, nil))

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(errcode.ContextKeyRequestID, "r-conv-test")
		if uid := c.GetHeader("X-Test-User"); uid != "" {
			errcode.SetUserID(c, uid)
		}
		c.Next()
	})
	r.GET("/api/v1/conversations", ctrl.List)
	r.GET("/api/v1/conversations/:conversation_id", ctrl.GetDetail)
	r.PATCH("/api/v1/conversations/:conversation_id/settings", ctrl.UpdateSettings)
	r.GET("/api/v1/conversations/:conversation_id/members", ctrl.ListMembers)

	// Invalid include_hidden -> 400 / 10001.
	wBad := httptest.NewRecorder()
	reqBad, _ := http.NewRequest(http.MethodGet, "/api/v1/conversations?include_hidden=maybe", nil)
	reqBad.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wBad, reqBad)
	if wBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid include_hidden, got %d body=%s", wBad.Code, wBad.Body.String())
	}

	// Valid list -> 200.
	wList := httptest.NewRecorder()
	reqList, _ := http.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	reqList.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wList, reqList)
	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200 on list, got %d body=%s", wList.Code, wList.Body.String())
	}

	// Empty PATCH -> 400 / 10000; valid PATCH -> 200.
	wEmptyPatch := httptest.NewRecorder()
	reqEmptyPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/conversations/90001/settings", strings.NewReader(`{}`))
	reqEmptyPatch.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wEmptyPatch, reqEmptyPatch)
	if wEmptyPatch.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on empty PATCH, got %d", wEmptyPatch.Code)
	}

	wPatch := httptest.NewRecorder()
	reqPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/conversations/90001/settings", strings.NewReader(`{"is_pinned":true}`))
	reqPatch.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wPatch, reqPatch)
	if wPatch.Code != http.StatusOK {
		t.Fatalf("expected 200 on valid PATCH, got %d body=%s", wPatch.Code, wPatch.Body.String())
	}
	var env errcode.Envelope
	_ = json.Unmarshal(wPatch.Body.Bytes(), &env)
	if env.Code != 0 || env.RequestID != "r-conv-test" {
		t.Fatalf("unexpected PATCH envelope: %+v", env)
	}

	// Non-member detail -> 403 / 13002.
	wDenied := httptest.NewRecorder()
	reqDenied, _ := http.NewRequest(http.MethodGet, "/api/v1/conversations/90001", nil)
	reqDenied.Header.Set("X-Test-User", "10099")
	r.ServeHTTP(wDenied, reqDenied)
	if wDenied.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-member detail, got %d", wDenied.Code)
	}

	// Members -> 200.
	wMem := httptest.NewRecorder()
	reqMem, _ := http.NewRequest(http.MethodGet, "/api/v1/conversations/90001/members", nil)
	reqMem.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wMem, reqMem)
	if wMem.Code != http.StatusOK {
		t.Fatalf("expected 200 for members, got %d body=%s", wMem.Code, wMem.Body.String())
	}
}
