package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

type stubAdminRepo struct {
	users          map[uint64]*model.User
	sessions       map[uint64]*model.UserSession
	groups         map[uint64]*model.Group
	convs          map[uint64]*model.Conversation
	groupMembers   map[uint64][]model.GroupMember
	messages       []model.Message
	audits         []*model.AuditLog
	onlineUsers    map[uint64]bool
	liveOnlineCnt  int64
	liveWSConnCnt  int64
	liveSessionMap map[uint64]int64
	blacklisted    []string
	mysqlErr       error
	redisErr       error
}

func newStubAdminRepo() *stubAdminRepo {
	now := time.Now().UTC()
	return &stubAdminRepo{
		users: map[uint64]*model.User{
			1: {ID: 1, Username: "admin", Nickname: "管理员", Role: model.UserRoleAdmin, Status: model.UserStatusActive, CreatedAt: now, UpdatedAt: now},
			2: {ID: 2, Username: "alice", Nickname: "Alice", Role: model.UserRoleUser, Status: model.UserStatusActive, CreatedAt: now, UpdatedAt: now},
		},
		sessions: map[uint64]*model.UserSession{
			10: {ID: 10, UserID: 2, DeviceID: "web-1", DeviceType: "web", AccessJTI: "jti-2-10", ExpiresAt: now.Add(time.Hour), LastActiveAt: now, CreatedAt: now},
		},
		groups: map[uint64]*model.Group{
			100: {ID: 100, Name: "Test Group", OwnerID: 2, MaxMembers: 500, Status: model.GroupStatusActive, CreatedAt: now, UpdatedAt: now},
		},
		convs: map[uint64]*model.Conversation{
			100: {ID: 900, Type: model.ConversationTypeGroup, LastSeq: 5},
		},
		groupMembers: map[uint64][]model.GroupMember{
			100: {{ID: 1, GroupID: 100, UserID: 2, Role: model.GroupRoleOwner, JoinedAt: now}},
		},
		onlineUsers:    map[uint64]bool{2: true},
		liveOnlineCnt:  1,
		liveWSConnCnt:  2,
		liveSessionMap: map[uint64]int64{10: 2},
	}
}

func (r *stubAdminRepo) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error {
	return fn(&gorm.DB{})
}

func (r *stubAdminRepo) ListUsers(_ context.Context, _ repository.AdminUserListFilter) ([]model.User, bool, error) {
	out := make([]model.User, 0, len(r.users))
	for _, u := range r.users {
		out = append(out, *u)
	}
	return out, false, nil
}

func (r *stubAdminRepo) BatchCountActiveSessions(_ context.Context, userIDs []uint64, _ time.Time) (map[uint64]int, error) {
	out := make(map[uint64]int, len(userIDs))
	for _, s := range r.sessions {
		if s.RevokedAt == nil {
			out[s.UserID]++
		}
	}
	return out, nil
}

func (r *stubAdminRepo) GetUserByID(_ context.Context, userID uint64) (*model.User, error) {
	u, ok := r.users[userID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := *u
	return &cp, nil
}

func (r *stubAdminRepo) LockUserByID(_ context.Context, _ *gorm.DB, userID uint64) (*model.User, error) {
	return r.GetUserByID(context.Background(), userID)
}

func (r *stubAdminRepo) CountActiveAdminsForUpdate(_ context.Context, _ *gorm.DB) (int64, error) {
	var cnt int64
	for _, u := range r.users {
		if u.Role == model.UserRoleAdmin && u.Status == model.UserStatusActive {
			cnt++
		}
	}
	return cnt, nil
}

func (r *stubAdminRepo) UpdateUserStatus(_ context.Context, _ *gorm.DB, userID uint64, status int8, now time.Time) error {
	u, ok := r.users[userID]
	if !ok {
		return gorm.ErrRecordNotFound
	}
	u.Status = status
	u.UpdatedAt = now
	return nil
}

func (r *stubAdminRepo) ListUserSessions(_ context.Context, userID uint64, _ int) ([]model.UserSession, error) {
	var out []model.UserSession
	for _, s := range r.sessions {
		if s.UserID == userID {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (r *stubAdminRepo) CountUserFriends(_ context.Context, _ uint64) (int64, error) { return 3, nil }
func (r *stubAdminRepo) CountUserGroups(_ context.Context, _ uint64) (int64, error)  { return 1, nil }
func (r *stubAdminRepo) ListUserAuditLogs(_ context.Context, _ uint64, _ int) ([]model.AuditLog, error) {
	return nil, nil
}

func (r *stubAdminRepo) LockUnrevokedSessionsByUser(_ context.Context, _ *gorm.DB, userID uint64) ([]model.UserSession, error) {
	var out []model.UserSession
	for _, s := range r.sessions {
		if s.UserID == userID && s.RevokedAt == nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (r *stubAdminRepo) LockSessionByID(_ context.Context, _ *gorm.DB, sessionID uint64) (*model.UserSession, error) {
	s, ok := r.sessions[sessionID]
	if !ok {
		return nil, repository.ErrSessionNotFound
	}
	cp := *s
	return &cp, nil
}

func (r *stubAdminRepo) RevokeSessionsByIDs(_ context.Context, _ *gorm.DB, sessionIDs []uint64, revokedAt time.Time) (int64, error) {
	var cnt int64
	for _, id := range sessionIDs {
		if s, ok := r.sessions[id]; ok && s.RevokedAt == nil {
			ts := revokedAt
			s.RevokedAt = &ts
			cnt++
		}
	}
	return cnt, nil
}

func (r *stubAdminRepo) ListGroups(_ context.Context, _ repository.AdminGroupListFilter) ([]repository.AdminGroupSummaryRow, bool, error) {
	return nil, false, nil
}

func (r *stubAdminRepo) GetGroupDetail(_ context.Context, groupID uint64) (*repository.AdminGroupSummaryRow, []model.GroupMember, map[uint64]model.User, []model.AuditLog, error) {
	g, ok := r.groups[groupID]
	if !ok {
		return nil, nil, nil, nil, gorm.ErrRecordNotFound
	}
	conv := r.convs[groupID]
	var convID, lastSeq uint64
	if conv != nil {
		convID = conv.ID
		lastSeq = conv.LastSeq
	}
	return &repository.AdminGroupSummaryRow{
		Group:          *g,
		ConversationID: convID,
		LastSeq:        lastSeq,
		MemberCount:    len(r.groupMembers[groupID]),
	}, r.groupMembers[groupID], map[uint64]model.User{}, nil, nil
}

func (r *stubAdminRepo) LockGroupAndConversation(_ context.Context, _ *gorm.DB, groupID uint64) (*model.Group, *model.Conversation, []model.GroupMember, error) {
	g, ok := r.groups[groupID]
	if !ok {
		return nil, nil, nil, gorm.ErrRecordNotFound
	}
	return g, r.convs[groupID], r.groupMembers[groupID], nil
}

func (r *stubAdminRepo) MarkGroupDisbanded(_ context.Context, _ *gorm.DB, groupID uint64, now time.Time) error {
	g, ok := r.groups[groupID]
	if !ok {
		return gorm.ErrRecordNotFound
	}
	g.Status = model.GroupStatusDisbanded
	g.UpdatedAt = now
	return nil
}

func (r *stubAdminRepo) CreateSystemMessageAndUpdateConv(_ context.Context, _ *gorm.DB, _ *model.Message, _ uint64, _ []uint64, _ time.Time) error {
	return nil
}

func (r *stubAdminRepo) AppendAuditLog(_ context.Context, _ *gorm.DB, entry *model.AuditLog) error {
	r.audits = append(r.audits, entry)
	return nil
}

func (r *stubAdminRepo) ListMessageAudit(_ context.Context, _ repository.AdminMessageAuditFilter) ([]model.Message, map[uint64]model.User, bool, error) {
	return r.messages, map[uint64]model.User{}, false, nil
}

func (r *stubAdminRepo) CountMessagesSince(_ context.Context, _ time.Time) (int64, error) {
	return 7, nil
}

func (r *stubAdminRepo) ProbeMySQL(_ context.Context) (string, int64, error) {
	if r.mysqlErr != nil {
		return "", 0, r.mysqlErr
	}
	return "8.0.44", 2, nil
}

func (r *stubAdminRepo) ProbeRedis(_ context.Context) (string, int64, error) {
	if r.redisErr != nil {
		return "", 0, r.redisErr
	}
	return "7.4.0", 1, nil
}

func (r *stubAdminRepo) GetUsersOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		out[id] = r.onlineUsers[id]
	}
	return out, nil
}

func (r *stubAdminRepo) CountLivePresence(_ context.Context) (int64, int64, error) {
	return r.liveOnlineCnt, r.liveWSConnCnt, nil
}

func (r *stubAdminRepo) CountLiveConnectionsForSessions(_ context.Context, _ uint64, sessionIDs []uint64, _ bool) (int64, []uint64, error) {
	var total int64
	for _, id := range sessionIDs {
		total += r.liveSessionMap[id]
	}
	return total, sessionIDs, nil
}

func (r *stubAdminRepo) BlacklistAccessJTIs(_ context.Context, jtis []string, _ string, _ time.Duration) error {
	r.blacklisted = append(r.blacklisted, jtis...)
	return nil
}

func (r *stubAdminRepo) CollectStreamsOverview(_ context.Context) (repository.AdminStreamsSummary, error) {
	return repository.AdminStreamsSummary{MessagesLen: 3, EventsLen: 4, PendingTotal: 0, DeadLetterCount: 0}, nil
}

func (r *stubAdminRepo) CollectStreamConsumers(_ context.Context) ([]repository.AdminStreamConsumerRow, error) {
	return []repository.AdminStreamConsumerRow{{Stream: "im:stream:messages", Group: "im-msg-persist", Consumer: "c1"}}, nil
}

type stubSessionRevoker struct {
	calls []struct {
		userID     uint64
		sessionIDs []uint64
		reason     string
	}
}

func (s *stubSessionRevoker) RevokeSessions(_ context.Context, userID uint64, sessionIDs []uint64, reason string) error {
	s.calls = append(s.calls, struct {
		userID     uint64
		sessionIDs []uint64
		reason     string
	}{userID: userID, sessionIDs: sessionIDs, reason: reason})
	return nil
}

func TestAdminServiceDisableEnableAndProtectsOnlyAdmin(t *testing.T) {
	repo := newStubAdminRepo()
	revoker := &stubSessionRevoker{}
	svc := NewAdminService(repo, nil, nil, revoker, nil, 15*time.Minute, nil)

	// Cannot disable the only active admin
	_, bizErr := svc.UpdateUserStatus(context.Background(), 1, 1, model.UserStatusDisabled, "test", GroupAuditMeta{})
	if bizErr == nil || bizErr.Code != errcode.ErrCannotDisableOnlyAdmin.Code {
		t.Fatalf("expected ErrCannotDisableOnlyAdmin, got %v", bizErr)
	}

	// Disable user 2 -> revokes session 10, blacklists JTI, kicks 2 WS connections
	res, bizErr := svc.UpdateUserStatus(context.Background(), 1, 2, model.UserStatusDisabled, "violation", GroupAuditMeta{RequestID: "req-1"})
	if bizErr != nil {
		t.Fatalf("unexpected error disabling user: %v", bizErr)
	}
	if res.Status != 0 || res.RevokedSessionCount != 1 || res.KickedConnectionCount != 2 {
		t.Fatalf("unexpected disable result: %+v", res)
	}
	if len(repo.blacklisted) != 1 || repo.blacklisted[0] != "jti-2-10" {
		t.Fatalf("expected jti-2-10 blacklisted, got %v", repo.blacklisted)
	}
	if len(revoker.calls) != 1 || revoker.calls[0].reason != "account.disabled" {
		t.Fatalf("expected RevokeSessions with account.disabled, got %+v", revoker.calls)
	}

	// Disabling again conflicts with 17003
	_, bizErr = svc.UpdateUserStatus(context.Background(), 1, 2, model.UserStatusDisabled, "again", GroupAuditMeta{})
	if bizErr == nil || bizErr.Code != errcode.ErrAdminStateConflict.Code {
		t.Fatalf("expected ErrAdminStateConflict, got %v", bizErr)
	}

	// Re-enable user 2
	enableRes, bizErr := svc.UpdateUserStatus(context.Background(), 1, 2, model.UserStatusActive, "restored", GroupAuditMeta{})
	if bizErr != nil || enableRes.Status != 1 {
		t.Fatalf("expected re-enable success, got res=%+v err=%v", enableRes, bizErr)
	}
}

func TestAdminServiceAuditValidationAndMonitoring(t *testing.T) {
	repo := newStubAdminRepo()
	svc := NewAdminService(repo, nil, nil, nil, nil, 15*time.Minute, nil)

	from := time.Now().UTC()
	to := from.Add(-time.Hour)
	_, bizErr := svc.ListMessageAudit(context.Background(), ListAdminMessageAuditInput{From: &from, To: &to})
	if bizErr == nil || bizErr.Code != errcode.ErrInvalidAuditFilter.Code {
		t.Fatalf("expected ErrInvalidAuditFilter when from > to, got %v", bizErr)
	}

	overview, bizErr := svc.GetMetricsOverview(context.Background())
	if bizErr != nil {
		t.Fatalf("unexpected overview error: %v", bizErr)
	}
	if overview.OnlineUsers != 1 || overview.WSConnections != 2 || overview.MessagesPerMinute != 7 || overview.Goroutines <= 0 {
		t.Fatalf("unexpected overview metrics: %+v", overview)
	}

	repo.mysqlErr = errors.New("connection refused")
	health, bizErr := svc.GetDependenciesHealth(context.Background())
	if bizErr == nil || bizErr.Code != errcode.ErrMetricsUnavailable.Code {
		t.Fatalf("expected ErrMetricsUnavailable when mysql fails, got %v", bizErr)
	}
	if health.MySQL.Status != HealthStateUnavailable || health.Redis.Status != HealthStateOK || health.MySQL.LastError == nil {
		t.Fatalf("unexpected health payload: %+v", health)
	}
}
