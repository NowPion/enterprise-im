package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
)

type stubUserStore struct {
	users     map[uint64]model.User
	getErr    error
	updateErr error
	searchErr error
	lastQuery *repository.UserSearchParams
}

func newStubUserStore(initial ...model.User) *stubUserStore {
	m := make(map[uint64]model.User, len(initial))
	for _, u := range initial {
		m[u.ID] = u
	}
	return &stubUserStore{users: m}
}

func (s *stubUserStore) GetByID(_ context.Context, userID uint64) (*model.User, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	u, ok := s.users[userID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := u
	return &cp, nil
}

func (s *stubUserStore) UpdateProfile(_ context.Context, userID uint64, updates map[string]any) (*model.User, error) {
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	u, ok := s.users[userID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	if u.Status != model.UserStatusActive {
		return nil, repository.ErrUserDisabled
	}
	if nick, ok := updates["nickname"].(string); ok {
		u.Nickname = nick
	}
	if sig, ok := updates["signature"].(string); ok {
		u.Signature = sig
	}
	s.users[userID] = u
	cp := u
	return &cp, nil
}

func (s *stubUserStore) Search(_ context.Context, params repository.UserSearchParams) ([]model.User, error) {
	cpParams := params
	s.lastQuery = &cpParams
	if s.searchErr != nil {
		return nil, s.searchErr
	}

	var matched []model.User
	for _, u := range s.users {
		if u.Status != model.UserStatusActive {
			continue
		}
		if params.Cursor > 0 && u.ID >= params.Cursor {
			continue
		}
		target := u.Username
		if params.Field == repository.UserSearchFieldNickname {
			target = u.Nickname
		}
		if params.Match == repository.UserSearchMatchExact {
			if target == params.Query {
				matched = append(matched, u)
			}
		} else {
			if strings.HasPrefix(target, params.Query) {
				matched = append(matched, u)
			}
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].ID > matched[j].ID
	})
	if params.Limit > 0 && len(matched) > params.Limit {
		matched = matched[:params.Limit]
	}
	return matched, nil
}

type stubPresenceStore struct {
	online map[uint64]bool
	err    error
}

func (s *stubPresenceStore) IsUserOnline(_ context.Context, userID uint64) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.online[userID], nil
}

func (s *stubPresenceStore) GetUsersOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		out[id] = s.online[id]
	}
	return out, nil
}

func TestUserServiceGetMeAndUpdateMe(t *testing.T) {
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	lastOnline := time.Date(2026, 11, 26, 4, 5, 6, 7000000, time.UTC)
	store := newStubUserStore(
		model.User{
			ID:           10001,
			Username:     "alice",
			Nickname:     "Alice",
			AvatarURL:    "",
			Signature:    "hello im",
			Role:         model.UserRoleUser,
			Status:       model.UserStatusActive,
			LastOnlineAt: &lastOnline,
		},
		model.User{
			ID:       10002,
			Username: "banned",
			Nickname: "Banned",
			Role:     model.UserRoleUser,
			Status:   model.UserStatusDisabled,
		},
	)
	presence := &stubPresenceStore{online: map[uint64]bool{10001: true}}
	svc := NewUserService(store, presence, log)

	// 1. GetMe success + JSON contract check
	me, bizErr := svc.GetMe(context.Background(), 10001, "9001", "web-abc")
	if bizErr != nil {
		t.Fatalf("GetMe failed: %v", bizErr)
	}
	if me.SessionID != "9001" || me.DeviceID != "web-abc" || me.User.ID != "10001" || !me.User.Online {
		t.Fatalf("unexpected GetMe result: %+v", me)
	}
	if me.User.LastOnlineAt == nil || *me.User.LastOnlineAt != "2026-11-26T04:05:06.007Z" {
		t.Fatalf("unexpected LastOnlineAt: %v", me.User.LastOnlineAt)
	}
	rawJSON, err := json.Marshal(me.User)
	if err != nil {
		t.Fatalf("marshal SelfUserProfile: %v", err)
	}
	if !strings.Contains(string(rawJSON), `"role":"user"`) || !strings.Contains(string(rawJSON), `"status":1`) {
		t.Fatalf("SelfUserProfile missing role/status: %s", rawJSON)
	}

	// 2. GetMe disabled / missing / invalid userID
	if _, bizErr := svc.GetMe(context.Background(), 0, "1", "d"); bizErr == nil || bizErr.Code != errcode.ErrAccessTokenInvalid.Code {
		t.Fatalf("expected 11001 for userID=0, got %v", bizErr)
	}
	if _, bizErr := svc.GetMe(context.Background(), 10002, "1", "d"); bizErr == nil || bizErr.Code != errcode.ErrAccountDisabled.Code {
		t.Fatalf("expected 11004 for disabled account, got %v", bizErr)
	}
	if _, bizErr := svc.GetMe(context.Background(), 99999, "1", "d"); bizErr == nil || bizErr.Code != errcode.ErrUserNotFound.Code {
		t.Fatalf("expected 12001 for missing user, got %v", bizErr)
	}

	// 3. UpdateMe partial update: nickname only preserves signature; 64 runes CJK/Emoji allowed
	nick64 := strings.Repeat("测", 63) + "😀"
	updated, bizErr := svc.UpdateMe(context.Background(), UpdateMeInput{
		UserID:   10001,
		Nickname: &nick64,
	})
	if bizErr != nil {
		t.Fatalf("UpdateMe 64-rune nickname failed: %v", bizErr)
	}
	if updated.User.Nickname != nick64 || updated.User.Signature != "hello im" {
		t.Fatalf("unexpected UpdateMe nickname result: %+v", updated.User)
	}

	// 4. UpdateMe clear signature with empty string
	empty := ""
	cleared, bizErr := svc.UpdateMe(context.Background(), UpdateMeInput{
		UserID:    10001,
		Signature: &empty,
	})
	if bizErr != nil {
		t.Fatalf("UpdateMe clear signature failed: %v", bizErr)
	}
	if cleared.User.Signature != "" || cleared.User.Nickname != nick64 {
		t.Fatalf("expected signature cleared and nickname preserved, got %+v", cleared.User)
	}

	// 5. UpdateMe validation errors
	blank := "   "
	tooLongNick := strings.Repeat("a", 65)
	tooLongSig := strings.Repeat("字", 256)
	sig255 := strings.Repeat("字", 255)

	if _, bizErr := svc.UpdateMe(context.Background(), UpdateMeInput{UserID: 10001}); bizErr == nil || bizErr.Code != errcode.ErrInvalidParams.Code {
		t.Fatalf("expected 10000 when neither field provided, got %v", bizErr)
	}
	if _, bizErr := svc.UpdateMe(context.Background(), UpdateMeInput{UserID: 10001, Nickname: &blank}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for blank nickname, got %v", bizErr)
	}
	if _, bizErr := svc.UpdateMe(context.Background(), UpdateMeInput{UserID: 10001, Nickname: &tooLongNick}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for 65-rune nickname, got %v", bizErr)
	}
	if _, bizErr := svc.UpdateMe(context.Background(), UpdateMeInput{UserID: 10001, Signature: &tooLongSig}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for 256-rune signature, got %v", bizErr)
	}
	if _, bizErr := svc.UpdateMe(context.Background(), UpdateMeInput{UserID: 10001, Signature: &sig255}); bizErr != nil {
		t.Fatalf("expected 255-rune signature to pass, got %v", bizErr)
	}
}

func TestUserServiceSearchPaginationAndCursorBinding(t *testing.T) {
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	store := newStubUserStore(
		model.User{ID: 101, Username: "alice_1", Nickname: "Alpha", Role: model.UserRoleAdmin, Status: model.UserStatusActive},
		model.User{ID: 102, Username: "alice_2", Nickname: "Beta", Role: model.UserRoleUser, Status: model.UserStatusActive},
		model.User{ID: 103, Username: "alice_3", Nickname: "Gamma", Role: model.UserRoleUser, Status: model.UserStatusActive},
		model.User{ID: 104, Username: "alice_4", Nickname: "Delta", Role: model.UserRoleUser, Status: model.UserStatusDisabled},
	)
	presence := &stubPresenceStore{online: map[uint64]bool{103: true}}
	svc := NewUserService(store, presence, log)

	// Page 1: limit=2 -> [103, 102], has_more=true, next_cursor != nil
	p1, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{
		ViewerUserID: 900,
		Query:        "alice_",
		Field:        "username",
		Match:        "prefix",
		Limit:        2,
	})
	if bizErr != nil {
		t.Fatalf("SearchUsers page 1 failed: %v", bizErr)
	}
	if len(p1.Items) != 2 || !p1.HasMore || p1.NextCursor == nil {
		t.Fatalf("unexpected page 1: %+v", p1)
	}
	if p1.Items[0].ID != "103" || !p1.Items[0].Online || p1.Items[1].ID != "102" || p1.Items[1].Online {
		t.Fatalf("unexpected page 1 items: %+v", p1.Items)
	}
	// Public User JSON must not contain role or status.
	rawItem, _ := json.Marshal(p1.Items[0])
	if strings.Contains(string(rawItem), `"role"`) || strings.Contains(string(rawItem), `"status"`) {
		t.Fatalf("public UserProfile leaked role/status: %s", rawItem)
	}

	// Cursor bound to user/filter/sort: reject different viewer, different query, different field, or garbage.
	if _, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{
		ViewerUserID: 901,
		Query:        "alice_",
		Field:        "username",
		Match:        "prefix",
		Cursor:       *p1.NextCursor,
		Limit:        2,
	}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for cross-user cursor reuse, got %v", bizErr)
	}
	if _, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{
		ViewerUserID: 900,
		Query:        "alice",
		Field:        "username",
		Match:        "prefix",
		Cursor:       *p1.NextCursor,
		Limit:        2,
	}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for changed filter cursor reuse, got %v", bizErr)
	}
	if _, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{
		ViewerUserID: 900,
		Query:        "alice_",
		Field:        "username",
		Match:        "prefix",
		Cursor:       "not-valid-cursor",
		Limit:        2,
	}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for malformed cursor, got %v", bizErr)
	}

	// Page 2 with valid cursor -> [101], has_more=false, next_cursor=nil
	p2, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{
		ViewerUserID: 900,
		Query:        "alice_",
		Field:        "username",
		Match:        "prefix",
		Cursor:       *p1.NextCursor,
		Limit:        2,
	})
	if bizErr != nil {
		t.Fatalf("SearchUsers page 2 failed: %v", bizErr)
	}
	if len(p2.Items) != 1 || p2.Items[0].ID != "101" || p2.HasMore || p2.NextCursor != nil {
		t.Fatalf("unexpected page 2: %+v", p2)
	}

	// Empty match -> non-nil empty slice [], next_cursor=nil, has_more=false
	emptyPage, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{
		ViewerUserID: 900,
		Query:        "nonexistent",
		Field:        "nickname",
		Match:        "exact",
	})
	if bizErr != nil {
		t.Fatalf("SearchUsers empty failed: %v", bizErr)
	}
	if emptyPage.Items == nil || len(emptyPage.Items) != 0 || emptyPage.HasMore || emptyPage.NextCursor != nil {
		t.Fatalf("unexpected empty page: %+v", emptyPage)
	}
	rawEmpty, _ := json.Marshal(emptyPage)
	if !strings.Contains(string(rawEmpty), `"items":[]`) || !strings.Contains(string(rawEmpty), `"next_cursor":null`) {
		t.Fatalf("expected JSON items:[] and next_cursor:null, got %s", rawEmpty)
	}

	// Validation errors for search params
	if _, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{ViewerUserID: 900, Query: "  "}); bizErr == nil || bizErr.Code != errcode.ErrInvalidParams.Code {
		t.Fatalf("expected 10000 for blank q, got %v", bizErr)
	}
	if _, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{ViewerUserID: 900, Query: strings.Repeat("x", 65)}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for q > 64, got %v", bizErr)
	}
	if _, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{ViewerUserID: 900, Query: "a", Field: "email"}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for invalid field, got %v", bizErr)
	}
	if _, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{ViewerUserID: 900, Query: "a", Match: "contains"}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for invalid match, got %v", bizErr)
	}
	if _, bizErr := svc.SearchUsers(context.Background(), SearchUsersInput{ViewerUserID: 900, Query: "a", Limit: 101}); bizErr == nil || bizErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for limit > 100, got %v", bizErr)
	}
}

func TestUserServiceErrorDegradationAndDBMapping(t *testing.T) {
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	store := newStubUserStore(model.User{
		ID:       1,
		Username: "alice",
		Nickname: "Alice",
		Role:     model.UserRoleUser,
		Status:   model.UserStatusActive,
	})
	// Presence error degrades gracefully to online=false without failing GetMe/SearchUsers.
	svc := NewUserService(store, &stubPresenceStore{err: errors.New("redis timeout")}, log)
	me, bizErr := svc.GetMe(context.Background(), 1, "10", "dev")
	if bizErr != nil || me.User.Online {
		t.Fatalf("expected graceful degradation to offline on presence error, got me=%+v err=%v", me, bizErr)
	}

	// Network/MySQL down error maps to 19001.
	store.getErr = &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	if _, bizErr := svc.GetMe(context.Background(), 1, "10", "dev"); bizErr == nil || bizErr.Code != errcode.ErrMySQLUnavailable.Code {
		t.Fatalf("expected 19001 on MySQL net error, got %v", bizErr)
	}
}
