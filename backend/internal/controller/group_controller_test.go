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

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
	"gorm.io/gorm"
)

type ctrlGroupStore struct {
	users   map[uint64]model.User
	groups  map[uint64]model.Group
	members map[uint64][]model.GroupMember
	convs   map[uint64]model.Conversation
	convMap map[uint64]uint64
	cm      map[string]model.ConversationMember
	msgs    []model.Message
	audits  []model.AuditLog
	seq     uint64
}

func newCtrlGroupStore() *ctrlGroupStore {
	return &ctrlGroupStore{
		users: map[uint64]model.User{
			10001: {ID: 10001, Username: "u1", Nickname: "Alice", Status: model.UserStatusActive},
			10002: {ID: 10002, Username: "u2", Nickname: "Bob", Status: model.UserStatusActive},
			10003: {ID: 10003, Username: "u3", Nickname: "Carol", Status: model.UserStatusActive},
		},
		groups:  make(map[uint64]model.Group),
		members: make(map[uint64][]model.GroupMember),
		convs:   make(map[uint64]model.Conversation),
		convMap: make(map[uint64]uint64),
		cm:      make(map[string]model.ConversationMember),
	}
}

func (s *ctrlGroupStore) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error { return fn(nil) }
func (s *ctrlGroupStore) LockUsersAscending(_ context.Context, _ *gorm.DB, ids []uint64) (map[uint64]model.User, error) {
	out := make(map[uint64]model.User)
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}
func (s *ctrlGroupStore) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
	return s.LockUsersAscending(context.Background(), nil, ids)
}
func (s *ctrlGroupStore) GetGroupAndConversation(_ context.Context, gid uint64) (*model.Group, *model.Conversation, error) {
	g, ok := s.groups[gid]
	if !ok {
		return nil, nil, gorm.ErrRecordNotFound
	}
	c := s.convs[s.convMap[gid]]
	return &g, &c, nil
}
func (s *ctrlGroupStore) LockGroupByID(_ context.Context, _ *gorm.DB, gid uint64) (*model.Group, error) {
	g, ok := s.groups[gid]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return &g, nil
}
func (s *ctrlGroupStore) CreateGroup(_ context.Context, _ *gorm.DB, g *model.Group) error {
	g.ID = 7001
	s.groups[g.ID] = *g
	return nil
}
func (s *ctrlGroupStore) UpdateGroupFields(_ context.Context, _ *gorm.DB, gid uint64, updates map[string]any) error {
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
func (s *ctrlGroupStore) LockGroupConversationByGroupID(_ context.Context, _ *gorm.DB, gid uint64) (*model.Conversation, error) {
	c := s.convs[s.convMap[gid]]
	return &c, nil
}
func (s *ctrlGroupStore) CreateGroupConversation(_ context.Context, _ *gorm.DB, gid uint64, now time.Time) (*model.Conversation, error) {
	id := uint64(8001)
	c := model.Conversation{ID: id, Type: model.ConversationTypeGroup, GroupID: &gid, CreatedAt: now, UpdatedAt: now}
	s.convs[id] = c
	s.convMap[gid] = id
	return &c, nil
}
func (s *ctrlGroupStore) LockGroupMembers(_ context.Context, _ *gorm.DB, gid uint64) ([]model.GroupMember, error) {
	return append([]model.GroupMember(nil), s.members[gid]...), nil
}
func (s *ctrlGroupStore) GetGroupMember(_ context.Context, gid, uid uint64) (*model.GroupMember, error) {
	for _, m := range s.members[gid] {
		if m.UserID == uid {
			cp := m
			return &cp, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}
func (s *ctrlGroupStore) LockGroupMember(_ context.Context, _ *gorm.DB, gid, uid uint64) (*model.GroupMember, error) {
	return s.GetGroupMember(context.Background(), gid, uid)
}
func (s *ctrlGroupStore) CreateGroupMembers(_ context.Context, _ *gorm.DB, ms []model.GroupMember) error {
	for i, m := range ms {
		m.ID = uint64(len(s.members[m.GroupID]) + i + 1)
		s.members[m.GroupID] = append(s.members[m.GroupID], m)
	}
	return nil
}
func (s *ctrlGroupStore) UpdateGroupMemberRole(_ context.Context, _ *gorm.DB, gid, uid uint64, role string, now time.Time) error {
	for i := range s.members[gid] {
		if s.members[gid][i].UserID == uid {
			s.members[gid][i].Role = role
			s.members[gid][i].UpdatedAt = now
		}
	}
	return nil
}
func (s *ctrlGroupStore) UpdateGroupMemberNickname(_ context.Context, _ *gorm.DB, gid, uid uint64, nick string, now time.Time) error {
	for i := range s.members[gid] {
		if s.members[gid][i].UserID == uid {
			s.members[gid][i].NicknameInGroup = nick
			s.members[gid][i].UpdatedAt = now
		}
	}
	return nil
}
func (s *ctrlGroupStore) DeleteGroupMember(_ context.Context, _ *gorm.DB, gid, uid uint64) error {
	var out []model.GroupMember
	for _, m := range s.members[gid] {
		if m.UserID != uid {
			out = append(out, m)
		}
	}
	s.members[gid] = out
	return nil
}
func (s *ctrlGroupStore) CountGroupMembers(_ context.Context, gid uint64) (int, error) {
	return len(s.members[gid]), nil
}
func (s *ctrlGroupStore) ListUserGroups(_ context.Context, filter repository.UserGroupListFilter) ([]repository.UserGroupSummaryRow, bool, error) {
	var rows []repository.UserGroupSummaryRow
	for gid, ms := range s.members {
		for _, m := range ms {
			if m.UserID == filter.UserID {
				rows = append(rows, repository.UserGroupSummaryRow{
					Group: s.groups[gid], Role: m.Role, ConversationID: s.convMap[gid], MemberCount: len(ms),
				})
			}
		}
	}
	return rows, false, nil
}
func (s *ctrlGroupStore) ListGroupMembers(_ context.Context, filter repository.GroupMemberListFilter) ([]model.GroupMember, error) {
	return append([]model.GroupMember(nil), s.members[filter.GroupID]...), nil
}
func (s *ctrlGroupStore) GetConversationMember(_ context.Context, cid, uid uint64) (*model.ConversationMember, error) {
	return nil, nil
}
func (s *ctrlGroupStore) CreateConversationMembers(_ context.Context, _ *gorm.DB, _ []model.ConversationMember) error {
	return nil
}
func (s *ctrlGroupStore) UpsertConversationMembersOnJoin(_ context.Context, _ *gorm.DB, cid uint64, uids []uint64, seq uint64, unread uint32, now time.Time) ([]model.ConversationMember, error) {
	var out []model.ConversationMember
	for _, uid := range uids {
		out = append(out, model.ConversationMember{ConversationID: cid, UserID: uid, LastDeliveredSeq: seq, LastReadSeq: seq, UnreadCount: unread, IsVisible: true, JoinedAt: now})
	}
	return out, nil
}
func (s *ctrlGroupStore) HideConversationMember(_ context.Context, _ *gorm.DB, _, _ uint64, _ time.Time) error {
	return nil
}
func (s *ctrlGroupStore) IncrementActiveGroupMembersUnread(_ context.Context, _ *gorm.DB, _ uint64, _ []uint64, _ time.Time) error {
	return nil
}
func (s *ctrlGroupStore) CreateSystemMessage(_ context.Context, _ *gorm.DB, msg *model.Message) error {
	s.msgs = append(s.msgs, *msg)
	return nil
}
func (s *ctrlGroupStore) UpdateConversationLastMessage(_ context.Context, _ *gorm.DB, cid uint64, msg *model.Message) error {
	c := s.convs[cid]
	c.LastMsgID = msg.ID
	c.LastSeq = msg.Seq
	s.convs[cid] = c
	return nil
}
func (s *ctrlGroupStore) LockAssetByAssetID(_ context.Context, _ *gorm.DB, _ string) (*model.Asset, error) {
	return nil, gorm.ErrRecordNotFound
}
func (s *ctrlGroupStore) BindGroupAvatarAsset(_ context.Context, _ *gorm.DB, _ string, _ uint64, _ time.Time) error {
	return nil
}
func (s *ctrlGroupStore) UnbindGroupAvatarAssets(_ context.Context, _ *gorm.DB, _ uint64, _ time.Time) error {
	return nil
}
func (s *ctrlGroupStore) AppendAuditLog(_ context.Context, _ *gorm.DB, entry *model.AuditLog) error {
	entry.ID = uint64(len(s.audits) + 1)
	s.audits = append(s.audits, *entry)
	return nil
}
func (s *ctrlGroupStore) ListGroupAuditLogs(_ context.Context, filter repository.GroupAuditListFilter) ([]model.AuditLog, bool, error) {
	return append([]model.AuditLog(nil), s.audits...), false, nil
}
func (s *ctrlGroupStore) AcquireConvLock(_ context.Context, _ uint64) (string, error) { return "tok", nil }
func (s *ctrlGroupStore) AllocateNextSeq(_ context.Context, _ *gorm.DB, _ uint64, _ string) (*repository.SeqReservation, error) {
	s.seq++
	return &repository.SeqReservation{Seq: s.seq}, nil
}
func (s *ctrlGroupStore) ReleaseConvLock(_ context.Context, _ uint64, _ string) error { return nil }
func (s *ctrlGroupStore) BatchIsOnline(_ context.Context, _ []uint64) (map[uint64]bool, error) {
	return map[uint64]bool{}, nil
}

func TestGroupControllerValidationAndEnvelopes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCtrlGroupStore()
	idGen, _ := snowflake.NewGenerator(4)
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	svc := service.NewGroupService(store, store, store, idGen, nil, log)
	ctrl := NewGroupController(svc)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		if uid := c.GetHeader("X-User-ID"); uid != "" {
			errcode.SetUserID(c, uid)
		}
		c.Next()
	})
	g := r.Group("/api/v1/groups")
	g.POST("", ctrl.Create)
	g.GET("", ctrl.List)
	g.GET("/:group_id", ctrl.Get)
	g.PATCH("/:group_id", ctrl.Update)
	g.DELETE("/:group_id", ctrl.Disband)
	g.GET("/:group_id/members", ctrl.ListMembers)
	g.POST("/:group_id/members", ctrl.AddMembers)
	g.DELETE("/:group_id/members/:user_id", ctrl.RemoveMember)
	g.PATCH("/:group_id/members/:user_id", ctrl.PatchMember)
	g.PATCH("/:group_id/members/:user_id/role", ctrl.UpdateMemberRole)
	g.POST("/:group_id/leave", ctrl.Leave)
	g.POST("/:group_id/transfer", ctrl.Transfer)
	g.GET("/:group_id/audit", ctrl.ListAudit)

	doReq := func(method, path, uid, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(method, path, strings.NewReader(body))
		if uid != "" {
			req.Header.Set("X-User-ID", uid)
		}
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	// Missing member_ids -> 400 / 10000.
	w := doReq(http.MethodPost, "/api/v1/groups", "10001", `{"name":"Alpha"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	var env errcode.Envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != errcode.ErrInvalidParams.Code {
		t.Fatalf("expected 10000, got %d", env.Code)
	}

	// Valid create -> 201.
	w = doReq(http.MethodPost, "/api/v1/groups", "10001", `{"name":"Alpha","member_ids":["10002"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", w.Code, w.Body.String())
	}

	// Empty PATCH -> 400 / 10000.
	w = doReq(http.MethodPatch, "/api/v1/groups/7001", "10001", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on empty PATCH, got %d", w.Code)
	}

	// PATCH /members/me routes cleanly without conflict with /members/:user_id/role.
	w = doReq(http.MethodPatch, "/api/v1/groups/7001/members/me", "10002", `{"nickname_in_group":"Bobby"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on PATCH /members/me, got %d body=%s", w.Code, w.Body.String())
	}

	// PATCH /members/10002/role -> 200.
	w = doReq(http.MethodPatch, "/api/v1/groups/7001/members/10002/role", "10001", `{"role":"admin"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on PATCH /role, got %d body=%s", w.Code, w.Body.String())
	}

	// DELETE /members/me -> 405.
	w = doReq(http.MethodDelete, "/api/v1/groups/7001/members/me", "10002", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on DELETE /members/me, got %d", w.Code)
	}

	// Invalid online_only -> 400 / 10001.
	w = doReq(http.MethodGet, "/api/v1/groups/7001/members?online_only=maybe", "10001", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid online_only, got %d", w.Code)
	}

	// Invalid audit timestamp -> 400 / 10001.
	w = doReq(http.MethodGet, "/api/v1/groups/7001/audit?from=not-a-date", "10001", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid audit from, got %d", w.Code)
	}

	// Valid audit query -> 200.
	w = doReq(http.MethodGet, "/api/v1/groups/7001/audit", "10001", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /audit, got %d body=%s", w.Code, w.Body.String())
	}
}
