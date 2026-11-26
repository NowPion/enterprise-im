package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type apiGroupStoreStub struct {
	users   map[uint64]model.User
	groups  map[uint64]model.Group
	members map[uint64][]model.GroupMember
	convs   map[uint64]model.Conversation
	convMap map[uint64]uint64
	seq     uint64
}

func newAPIGroupStoreStub() *apiGroupStoreStub {
	return &apiGroupStoreStub{
		users: map[uint64]model.User{
			10001: {ID: 10001, Username: "u1", Nickname: "Alice", Status: model.UserStatusActive},
			10002: {ID: 10002, Username: "u2", Nickname: "Bob", Status: model.UserStatusActive},
			10003: {ID: 10003, Username: "u3", Nickname: "Carol", Status: model.UserStatusActive},
		},
		groups:  make(map[uint64]model.Group),
		members: make(map[uint64][]model.GroupMember),
		convs:   make(map[uint64]model.Conversation),
		convMap: make(map[uint64]uint64),
	}
}

func (s *apiGroupStoreStub) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error { return fn(nil) }
func (s *apiGroupStoreStub) LockUsersAscending(_ context.Context, _ *gorm.DB, ids []uint64) (map[uint64]model.User, error) {
	out := make(map[uint64]model.User)
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}
func (s *apiGroupStoreStub) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
	return s.LockUsersAscending(context.Background(), nil, ids)
}
func (s *apiGroupStoreStub) GetGroupAndConversation(_ context.Context, gid uint64) (*model.Group, *model.Conversation, error) {
	g, ok := s.groups[gid]
	if !ok {
		return nil, nil, gorm.ErrRecordNotFound
	}
	c := s.convs[s.convMap[gid]]
	return &g, &c, nil
}
func (s *apiGroupStoreStub) LockGroupByID(_ context.Context, _ *gorm.DB, gid uint64) (*model.Group, error) {
	g, ok := s.groups[gid]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return &g, nil
}
func (s *apiGroupStoreStub) CreateGroup(_ context.Context, _ *gorm.DB, g *model.Group) error {
	g.ID = 3001
	s.groups[g.ID] = *g
	return nil
}
func (s *apiGroupStoreStub) UpdateGroupFields(_ context.Context, _ *gorm.DB, gid uint64, updates map[string]any) error {
	g := s.groups[gid]
	if v, ok := updates["name"]; ok {
		g.Name = v.(string)
	}
	if v, ok := updates["owner_id"]; ok {
		g.OwnerID = v.(uint64)
	}
	if v, ok := updates["status"]; ok {
		g.Status = v.(string)
	}
	s.groups[gid] = g
	return nil
}
func (s *apiGroupStoreStub) LockGroupConversationByGroupID(_ context.Context, _ *gorm.DB, gid uint64) (*model.Conversation, error) {
	c := s.convs[s.convMap[gid]]
	return &c, nil
}
func (s *apiGroupStoreStub) CreateGroupConversation(_ context.Context, _ *gorm.DB, gid uint64, now time.Time) (*model.Conversation, error) {
	c := model.Conversation{ID: 4001, Type: model.ConversationTypeGroup, GroupID: &gid, CreatedAt: now, UpdatedAt: now}
	s.convs[c.ID] = c
	s.convMap[gid] = c.ID
	return &c, nil
}
func (s *apiGroupStoreStub) LockGroupMembers(_ context.Context, _ *gorm.DB, gid uint64) ([]model.GroupMember, error) {
	return append([]model.GroupMember(nil), s.members[gid]...), nil
}
func (s *apiGroupStoreStub) GetGroupMember(_ context.Context, gid, uid uint64) (*model.GroupMember, error) {
	for _, m := range s.members[gid] {
		if m.UserID == uid {
			cp := m
			return &cp, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}
func (s *apiGroupStoreStub) LockGroupMember(_ context.Context, _ *gorm.DB, gid, uid uint64) (*model.GroupMember, error) {
	return s.GetGroupMember(context.Background(), gid, uid)
}
func (s *apiGroupStoreStub) CreateGroupMembers(_ context.Context, _ *gorm.DB, ms []model.GroupMember) error {
	for i, m := range ms {
		m.ID = uint64(len(s.members[m.GroupID]) + i + 1)
		s.members[m.GroupID] = append(s.members[m.GroupID], m)
	}
	return nil
}
func (s *apiGroupStoreStub) UpdateGroupMemberRole(_ context.Context, _ *gorm.DB, gid, uid uint64, role string, now time.Time) error {
	for i := range s.members[gid] {
		if s.members[gid][i].UserID == uid {
			s.members[gid][i].Role = role
			s.members[gid][i].UpdatedAt = now
		}
	}
	return nil
}
func (s *apiGroupStoreStub) UpdateGroupMemberNickname(_ context.Context, _ *gorm.DB, gid, uid uint64, nick string, now time.Time) error {
	for i := range s.members[gid] {
		if s.members[gid][i].UserID == uid {
			s.members[gid][i].NicknameInGroup = nick
			s.members[gid][i].UpdatedAt = now
		}
	}
	return nil
}
func (s *apiGroupStoreStub) DeleteGroupMember(_ context.Context, _ *gorm.DB, gid, uid uint64) error {
	var out []model.GroupMember
	for _, m := range s.members[gid] {
		if m.UserID != uid {
			out = append(out, m)
		}
	}
	s.members[gid] = out
	return nil
}
func (s *apiGroupStoreStub) CountGroupMembers(_ context.Context, gid uint64) (int, error) {
	return len(s.members[gid]), nil
}
func (s *apiGroupStoreStub) ListUserGroups(_ context.Context, _ repository.UserGroupListFilter) ([]repository.UserGroupSummaryRow, bool, error) {
	return []repository.UserGroupSummaryRow{}, false, nil
}
func (s *apiGroupStoreStub) ListGroupMembers(_ context.Context, filter repository.GroupMemberListFilter) ([]model.GroupMember, error) {
	return append([]model.GroupMember(nil), s.members[filter.GroupID]...), nil
}
func (s *apiGroupStoreStub) GetConversationMember(_ context.Context, _, _ uint64) (*model.ConversationMember, error) {
	return nil, nil
}
func (s *apiGroupStoreStub) CreateConversationMembers(_ context.Context, _ *gorm.DB, _ []model.ConversationMember) error {
	return nil
}
func (s *apiGroupStoreStub) UpsertConversationMembersOnJoin(_ context.Context, _ *gorm.DB, cid uint64, uids []uint64, seq uint64, unread uint32, now time.Time) ([]model.ConversationMember, error) {
	return []model.ConversationMember{{ConversationID: cid, UserID: uids[0], LastDeliveredSeq: seq, LastReadSeq: seq, UnreadCount: unread, IsVisible: true, JoinedAt: now}}, nil
}
func (s *apiGroupStoreStub) HideConversationMember(_ context.Context, _ *gorm.DB, _, _ uint64, _ time.Time) error {
	return nil
}
func (s *apiGroupStoreStub) IncrementActiveGroupMembersUnread(_ context.Context, _ *gorm.DB, _ uint64, _ []uint64, _ time.Time) error {
	return nil
}
func (s *apiGroupStoreStub) CreateSystemMessage(_ context.Context, _ *gorm.DB, _ *model.Message) error {
	return nil
}
func (s *apiGroupStoreStub) UpdateConversationLastMessage(_ context.Context, _ *gorm.DB, _ uint64, _ *model.Message) error {
	return nil
}
func (s *apiGroupStoreStub) LockAssetByAssetID(_ context.Context, _ *gorm.DB, _ string) (*model.Asset, error) {
	return nil, gorm.ErrRecordNotFound
}
func (s *apiGroupStoreStub) BindGroupAvatarAsset(_ context.Context, _ *gorm.DB, _ string, _ uint64, _ time.Time) error {
	return nil
}
func (s *apiGroupStoreStub) UnbindGroupAvatarAssets(_ context.Context, _ *gorm.DB, _ uint64, _ time.Time) error {
	return nil
}
func (s *apiGroupStoreStub) AppendAuditLog(_ context.Context, _ *gorm.DB, _ *model.AuditLog) error {
	return nil
}
func (s *apiGroupStoreStub) ListGroupAuditLogs(_ context.Context, _ repository.GroupAuditListFilter) ([]model.AuditLog, bool, error) {
	return []model.AuditLog{}, false, nil
}
func (s *apiGroupStoreStub) AcquireConvLock(_ context.Context, _ uint64) (string, error) { return "tok", nil }
func (s *apiGroupStoreStub) AllocateNextSeq(_ context.Context, _ *gorm.DB, _ uint64, _ string) (*repository.SeqReservation, error) {
	s.seq++
	return &repository.SeqReservation{Seq: s.seq}, nil
}
func (s *apiGroupStoreStub) ReleaseConvLock(_ context.Context, _ uint64, _ string) error { return nil }
func (s *apiGroupStoreStub) BatchIsOnline(_ context.Context, _ []uint64) (map[uint64]bool, error) {
	return map[uint64]bool{}, nil
}

func TestRegisterGroupRoutesMountsAllEndpointsAndFailsClosedWithoutJWT(t *testing.T) {
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	idGen, _ := snowflake.NewGenerator(5)
	store := newAPIGroupStoreStub()
	ctrl := controller.NewGroupController(service.NewGroupService(store, store, store, idGen, nil, log))

	// 1. Nil jwtAuth must fail closed with 401 + 11001.
	closedRouter := NewRouter(&config.Config{}, log, nil)
	RegisterGroupRoutes(closedRouter, nil, ctrl)
	wClosed := httptest.NewRecorder()
	reqClosed, _ := http.NewRequest(http.MethodGet, "/api/v1/groups", nil)
	closedRouter.ServeHTTP(wClosed, reqClosed)
	if wClosed.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when jwtAuth is nil, got %d", wClosed.Code)
	}
	var env errcode.Envelope
	_ = json.Unmarshal(wClosed.Body.Bytes(), &env)
	if env.Code != errcode.ErrAccessTokenInvalid.Code {
		t.Fatalf("expected 11001, got %d", env.Code)
	}

	// 2. Mounting on /api/v1 RouterGroup avoids double prefix and registers all routes cleanly.
	r := NewRouter(&config.Config{}, log, nil)
	jwtStub := func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer user-") {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		errcode.SetUserID(c, strings.TrimPrefix(auth, "Bearer user-"))
		c.Next()
	}
	v1 := r.Group("/api/v1")
	RegisterGroupRoutes(v1, jwtStub, ctrl)

	dispatch := func(method, path, uid, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer user-"+uid)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	if w := dispatch(http.MethodPost, "/api/v1/groups", "10001", `{"name":"RouteTeam","member_ids":["10002"]}`); w.Code != http.StatusCreated {
		t.Fatalf("POST /groups failed: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		method, path, uid, body string
	}{
		{http.MethodGet, "/api/v1/groups", "10001", ""},
		{http.MethodGet, "/api/v1/groups/3001", "10001", ""},
		{http.MethodPatch, "/api/v1/groups/3001", "10001", `{"name":"Renamed"}`},
		{http.MethodGet, "/api/v1/groups/3001/members", "10001", ""},
		{http.MethodPost, "/api/v1/groups/3001/members", "10001", `{"user_ids":["10003"]}`},
		{http.MethodPatch, "/api/v1/groups/3001/members/me", "10002", `{"nickname_in_group":"Nick"}`},
		{http.MethodPatch, "/api/v1/groups/3001/members/10002/role", "10001", `{"role":"admin"}`},
		{http.MethodGet, "/api/v1/groups/3001/audit", "10001", ""},
		{http.MethodPost, "/api/v1/groups/3001/leave", "10003", ""},
		{http.MethodPost, "/api/v1/groups/3001/transfer", "10001", `{"target_user_id":"10002"}`},
		{http.MethodDelete, "/api/v1/groups/3001/members/10001", "10002", ""},
		{http.MethodDelete, "/api/v1/groups/3001", "10002", ""},
	} {
		w := dispatch(tc.method, tc.path, tc.uid, tc.body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s expected 200, got %d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}
