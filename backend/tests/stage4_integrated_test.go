package tests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/api"
	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

// integratedMemoryBackend unifies all Stage 4 stores into a single in-memory stateful backend
// so router, middleware.JWTAuth, AuthService, UserService, FriendRequestService, and FriendService
// can be verified end-to-end on one Gin engine without mocks or stubs for JWT/auth.
type integratedMemoryBackend struct {
	mu              sync.Mutex
	nextUserID      uint64
	nextSessionID   uint64
	nextRequestID   uint64
	nextFriendship  uint64
	nextConvID      uint64
	users           map[uint64]model.User
	usersByName     map[string]uint64
	sessions        map[uint64]model.UserSession
	blacklist       map[string]string
	refreshLocks    map[string]string
	requests        map[uint64]model.FriendRequest
	friendships     map[[2]uint64]model.Friendship
	conversations   map[uint64]model.Conversation
	convByDirectKey map[string]uint64
	members         map[string]model.ConversationMember
	messages        []model.Message
	receipts        []model.MessageReceipt
	online          map[uint64]bool
}

func newIntegratedMemoryBackend() *integratedMemoryBackend {
	return &integratedMemoryBackend{
		nextUserID:      10000,
		nextSessionID:   50000,
		nextRequestID:   1,
		nextFriendship:  1,
		nextConvID:      90000,
		users:           make(map[uint64]model.User),
		usersByName:     make(map[string]uint64),
		sessions:        make(map[uint64]model.UserSession),
		blacklist:       make(map[string]string),
		refreshLocks:    make(map[string]string),
		requests:        make(map[uint64]model.FriendRequest),
		friendships:     make(map[[2]uint64]model.Friendship),
		conversations:   make(map[uint64]model.Conversation),
		convByDirectKey: make(map[string]uint64),
		members:         make(map[string]model.ConversationMember),
		online:          make(map[uint64]bool),
	}
}

type integratedAuthTx struct {
	b *integratedMemoryBackend
}

// --- service.AuthDataStore ---
type integratedAuthStoreAdapter struct {
	b *integratedMemoryBackend
}

func (a *integratedAuthStoreAdapter) WithTx(_ context.Context, fn func(tx service.AuthTxStore) error) error {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return fn(&integratedAuthTx{b: a.b})
}

func (a *integratedAuthStoreAdapter) CreateUser(_ context.Context, user *model.User) error {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	if _, exists := a.b.usersByName[user.Username]; exists {
		return repository.ErrDuplicateUsername
	}
	a.b.nextUserID++
	user.ID = a.b.nextUserID
	now := time.Now().UTC().Truncate(time.Millisecond)
	user.CreatedAt = now
	user.UpdatedAt = now
	a.b.users[user.ID] = *user
	a.b.usersByName[user.Username] = user.ID
	return nil
}

func (a *integratedAuthStoreAdapter) FindUserByUsername(_ context.Context, username string) (*model.User, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	id, ok := a.b.usersByName[username]
	if !ok {
		return nil, repository.ErrAuthUserNotFound
	}
	cp := a.b.users[id]
	return &cp, nil
}

func (a *integratedAuthStoreAdapter) FindUserByID(_ context.Context, userID uint64) (*model.User, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	u, ok := a.b.users[userID]
	if !ok {
		return nil, repository.ErrAuthUserNotFound
	}
	cp := u
	return &cp, nil
}

func (a *integratedAuthStoreAdapter) FindSessionByRefreshHash(_ context.Context, hash string) (*model.UserSession, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	for _, s := range a.b.sessions {
		if s.RefreshTokenHash == hash {
			cp := s
			return &cp, nil
		}
	}
	return nil, repository.ErrSessionNotFound
}

func (a *integratedAuthStoreAdapter) BlacklistAccessJTIs(_ context.Context, jtis []string, reason string, _ time.Duration) error {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	for _, jti := range jtis {
		if jti != "" {
			a.b.blacklist[jti] = reason
		}
	}
	return nil
}

func (a *integratedAuthStoreAdapter) AcquireRefreshLock(_ context.Context, refreshHash string, _ time.Duration) (string, bool, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	if _, exists := a.b.refreshLocks[refreshHash]; exists {
		return "", false, nil
	}
	tok := "lock-" + refreshHash
	a.b.refreshLocks[refreshHash] = tok
	return tok, true, nil
}

func (a *integratedAuthStoreAdapter) IsRefreshLockHeld(_ context.Context, refreshHash string) (bool, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	_, exists := a.b.refreshLocks[refreshHash]
	return exists, nil
}

func (a *integratedAuthStoreAdapter) ReleaseRefreshLock(_ context.Context, refreshHash, token string) error {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	if a.b.refreshLocks[refreshHash] == token {
		delete(a.b.refreshLocks, refreshHash)
	}
	return nil
}

func (a *integratedAuthStoreAdapter) IsUserOnline(_ context.Context, userID uint64) (bool, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return a.b.online[userID], nil
}

// --- middleware.AuthStateStore ---
func (b *integratedMemoryBackend) GetUserAuthStatus(_ context.Context, userID uint64) (int8, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.users[userID]
	if !ok {
		return 0, "", repository.ErrAuthUserNotFound
	}
	return u.Status, u.Role, nil
}

func (b *integratedMemoryBackend) IsAccessJTIBlacklisted(_ context.Context, jti string) (bool, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.blacklist[jti]
	return ok, r, nil
}

func (b *integratedMemoryBackend) GetSessionAuthState(_ context.Context, sessionID uint64) (repository.SessionAuthState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sessionID]
	if !ok {
		return repository.SessionAuthState{}, repository.ErrSessionNotFound
	}
	return repository.SessionAuthState{
		ID:        s.ID,
		UserID:    s.UserID,
		DeviceID:  s.DeviceID,
		AccessJTI: s.AccessJTI,
		ExpiresAt: s.ExpiresAt,
		RevokedAt: s.RevokedAt,
	}, nil
}

func (tx *integratedAuthTx) LockUserByID(_ context.Context, userID uint64) (*model.User, error) {
	u, ok := tx.b.users[userID]
	if !ok {
		return nil, repository.ErrAuthUserNotFound
	}
	cp := u
	return &cp, nil
}

func (tx *integratedAuthTx) UpdateUserPasswordHash(_ context.Context, userID uint64, passwordHash string) error {
	u, ok := tx.b.users[userID]
	if !ok {
		return repository.ErrAuthUserNotFound
	}
	u.PasswordHash = passwordHash
	tx.b.users[userID] = u
	return nil
}

func (tx *integratedAuthTx) LockSessionByID(_ context.Context, sessionID uint64) (*model.UserSession, error) {
	s, ok := tx.b.sessions[sessionID]
	if !ok {
		return nil, repository.ErrSessionNotFound
	}
	cp := s
	return &cp, nil
}

func (tx *integratedAuthTx) ListUnrevokedSessionsByDevice(_ context.Context, userID uint64, deviceID string) ([]model.UserSession, error) {
	var out []model.UserSession
	for _, s := range tx.b.sessions {
		if s.UserID == userID && s.DeviceID == deviceID && s.RevokedAt == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

func (tx *integratedAuthTx) ListUnrevokedSessionsByUser(_ context.Context, userID uint64) ([]model.UserSession, error) {
	var out []model.UserSession
	for _, s := range tx.b.sessions {
		if s.UserID == userID && s.RevokedAt == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

func (tx *integratedAuthTx) CountActiveSessionsByUser(_ context.Context, userID uint64, now time.Time) (int64, error) {
	var n int64
	for _, s := range tx.b.sessions {
		if s.UserID == userID && s.RevokedAt == nil && s.ExpiresAt.After(now) {
			n++
		}
	}
	return n, nil
}

func (tx *integratedAuthTx) RevokeSessionsByIDs(_ context.Context, sessionIDs []uint64, revokedAt time.Time) (int64, error) {
	var n int64
	ts := revokedAt.UTC()
	for _, id := range sessionIDs {
		if s, ok := tx.b.sessions[id]; ok && s.RevokedAt == nil {
			t := ts
			s.RevokedAt = &t
			tx.b.sessions[id] = s
			n++
		}
	}
	return n, nil
}

func (tx *integratedAuthTx) CreateSession(_ context.Context, session *model.UserSession) error {
	tx.b.nextSessionID++
	session.ID = tx.b.nextSessionID
	tx.b.sessions[session.ID] = *session
	return nil
}

func (tx *integratedAuthTx) RotateSessionTokens(_ context.Context, sessionID uint64, newRefreshHash, newAccessJTI string, newExpiresAt, activeAt time.Time, ipAddress, userAgent string) error {
	s, ok := tx.b.sessions[sessionID]
	if !ok || s.RevokedAt != nil {
		return repository.ErrSessionNotFound
	}
	s.RefreshTokenHash = newRefreshHash
	s.AccessJTI = newAccessJTI
	s.ExpiresAt = newExpiresAt.UTC()
	s.LastActiveAt = activeAt.UTC()
	s.IPAddress = ipAddress
	s.UserAgent = userAgent
	tx.b.sessions[sessionID] = s
	return nil
}

// --- UserStore & PresenceStore ---
func (b *integratedMemoryBackend) GetByID(_ context.Context, userID uint64) (*model.User, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.users[userID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := u
	return &cp, nil
}

func (b *integratedMemoryBackend) UpdateProfile(_ context.Context, userID uint64, updates map[string]any) (*model.User, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.users[userID]
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
	u.UpdatedAt = time.Now().UTC()
	b.users[userID] = u
	cp := u
	return &cp, nil
}

func (b *integratedMemoryBackend) Search(_ context.Context, params repository.UserSearchParams) ([]model.User, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []model.User
	for _, u := range b.users {
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
		if (params.Match == repository.UserSearchMatchExact && target == params.Query) ||
			(params.Match == repository.UserSearchMatchPrefix && strings.HasPrefix(target, params.Query)) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if params.Limit > 0 && len(out) > params.Limit {
		out = out[:params.Limit]
	}
	return out, nil
}

func (b *integratedMemoryBackend) IsUserOnline(_ context.Context, userID uint64) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.online[userID], nil
}

func (b *integratedMemoryBackend) GetUsersOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		out[id] = b.online[id]
	}
	return out, nil
}

// --- FriendRequestRepository, RelationWriter, SeqAllocator, PresenceChecker ---
type integratedFriendRequestAdapter struct {
	b *integratedMemoryBackend
}

func (a *integratedFriendRequestAdapter) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return fn(nil)
}

func (a *integratedFriendRequestAdapter) LockUsersAscending(_ context.Context, _ *gorm.DB, userA, userB uint64) (map[uint64]model.User, error) {
	out := make(map[uint64]model.User, 2)
	for _, id := range []uint64{userA, userB} {
		if u, ok := a.b.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (a *integratedFriendRequestAdapter) HasActiveFriendship(_ context.Context, _ *gorm.DB, userA, userB uint64) (bool, error) {
	f1 := a.b.friendships[[2]uint64{userA, userB}]
	f2 := a.b.friendships[[2]uint64{userB, userA}]
	return f1.Status == model.FriendshipStatusActive || f2.Status == model.FriendshipStatusActive, nil
}

func (a *integratedFriendRequestAdapter) HasPendingFriendRequest(_ context.Context, _ *gorm.DB, userA, userB uint64) (bool, error) {
	for _, fr := range a.b.requests {
		if fr.Status == model.FriendRequestStatusPending &&
			((fr.FromUserID == userA && fr.ToUserID == userB) || (fr.FromUserID == userB && fr.ToUserID == userA)) {
			return true, nil
		}
	}
	return false, nil
}

func (a *integratedFriendRequestAdapter) CreateFriendRequest(_ context.Context, _ *gorm.DB, req *model.FriendRequest) error {
	req.ID = a.b.nextRequestID
	a.b.nextRequestID++
	a.b.requests[req.ID] = *req
	return nil
}

func (a *integratedFriendRequestAdapter) GetFriendRequestByID(_ context.Context, id uint64) (*model.FriendRequest, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	fr, ok := a.b.requests[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := fr
	return &cp, nil
}

func (a *integratedFriendRequestAdapter) LockFriendRequestByID(_ context.Context, _ *gorm.DB, id uint64) (*model.FriendRequest, error) {
	fr, ok := a.b.requests[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := fr
	return &cp, nil
}

func (a *integratedFriendRequestAdapter) UpdateFriendRequestStatus(_ context.Context, _ *gorm.DB, id uint64, fromStatus, toStatus string, handledAt time.Time) (bool, error) {
	fr, ok := a.b.requests[id]
	if !ok || fr.Status != fromStatus {
		return false, nil
	}
	fr.Status = toStatus
	ht := handledAt
	fr.HandledAt = &ht
	fr.UpdatedAt = handledAt
	a.b.requests[id] = fr
	return true, nil
}

func (a *integratedFriendRequestAdapter) ListFriendRequests(_ context.Context, filter repository.FriendRequestListFilter) ([]model.FriendRequest, bool, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	var out []model.FriendRequest
	for _, fr := range a.b.requests {
		if filter.Direction == "received" && fr.ToUserID != filter.UserID {
			continue
		}
		if filter.Direction == "sent" && fr.FromUserID != filter.UserID {
			continue
		}
		if filter.Status != "" && fr.Status != filter.Status {
			continue
		}
		if filter.CursorID > 0 && fr.ID >= filter.CursorID {
			continue
		}
		out = append(out, fr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	hasMore := false
	if len(out) > filter.Limit {
		hasMore = true
		out = out[:filter.Limit]
	}
	return out, hasMore, nil
}

func (a *integratedFriendRequestAdapter) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	out := make(map[uint64]model.User, len(ids))
	for _, id := range ids {
		if u, ok := a.b.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (a *integratedFriendRequestAdapter) FindSingleConversationIDByDirectKey(_ context.Context, directKey string) (uint64, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return a.b.convByDirectKey[directKey], nil
}

func (a *integratedFriendRequestAdapter) LockSingleConversationByDirectKey(_ context.Context, _ *gorm.DB, directKey string) (*model.Conversation, error) {
	id, ok := a.b.convByDirectKey[directKey]
	if !ok {
		return nil, nil
	}
	c := a.b.conversations[id]
	return &c, nil
}

func (a *integratedFriendRequestAdapter) CreateSingleConversation(_ context.Context, _ *gorm.DB, directKey string) (*model.Conversation, error) {
	a.b.nextConvID++
	id := a.b.nextConvID
	dk := directKey
	c := model.Conversation{ID: id, Type: model.ConversationTypeSingle, DirectKey: &dk}
	a.b.conversations[id] = c
	a.b.convByDirectKey[directKey] = id
	return &c, nil
}

func (a *integratedFriendRequestAdapter) EnsureConversationMembersVisible(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, baselineSeq uint64, now time.Time) error {
	for _, uid := range userIDs {
		k := fmt.Sprintf("%d:%d", convID, uid)
		m := a.b.members[k]
		m.ConversationID = convID
		m.UserID = uid
		m.IsVisible = true
		if m.JoinedAt.IsZero() {
			m.LastDeliveredSeq = baselineSeq
			m.LastReadSeq = baselineSeq
			m.JoinedAt = now
		}
		m.UpdatedAt = now
		a.b.members[k] = m
	}
	return nil
}

func (a *integratedFriendRequestAdapter) CreateSystemMessage(_ context.Context, _ *gorm.DB, msg *model.Message) error {
	a.b.messages = append(a.b.messages, *msg)
	return nil
}

func (a *integratedFriendRequestAdapter) UpdateConversationLastMessage(_ context.Context, _ *gorm.DB, convID uint64, msg *model.Message) error {
	c := a.b.conversations[convID]
	c.LastMsgID = msg.ID
	c.LastSeq = msg.Seq
	c.LastMsgPreview = msg.Content
	a.b.conversations[convID] = c
	return nil
}

func (a *integratedFriendRequestAdapter) IncrementMembersUnreadCount(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, now time.Time) error {
	for _, uid := range userIDs {
		k := fmt.Sprintf("%d:%d", convID, uid)
		m := a.b.members[k]
		m.UnreadCount++
		m.UpdatedAt = now
		a.b.members[k] = m
	}
	return nil
}

func (a *integratedFriendRequestAdapter) CreateMessageReceipts(_ context.Context, _ *gorm.DB, receipts []model.MessageReceipt) error {
	a.b.receipts = append(a.b.receipts, receipts...)
	return nil
}

func (a *integratedFriendRequestAdapter) EnsureBilateralFriendship(_ context.Context, _ *gorm.DB, userA, userB uint64) error {
	now := time.Now().UTC()
	for _, p := range [][2]uint64{{userA, userB}, {userB, userA}} {
		f := a.b.friendships[p]
		if f.ID == 0 {
			f.ID = a.b.nextFriendship
			a.b.nextFriendship++
			f.CreatedAt = now
		}
		f.UserID = p[0]
		f.FriendID = p[1]
		f.Status = model.FriendshipStatusActive
		f.UpdatedAt = now
		a.b.friendships[p] = f
	}
	return nil
}

func (a *integratedFriendRequestAdapter) AcquireConvLock(_ context.Context, convID uint64) (string, error) {
	return fmt.Sprintf("lock-%d", convID), nil
}

func (a *integratedFriendRequestAdapter) AllocateNextSeq(_ context.Context, _ *gorm.DB, convID uint64, _ string) (*repository.SeqReservation, error) {
	var maxSeq uint64
	for _, m := range a.b.messages {
		if m.ConversationID == convID && m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	return &repository.SeqReservation{Seq: maxSeq + 1, Rollback: func(context.Context) error { return nil }}, nil
}

func (a *integratedFriendRequestAdapter) ReleaseConvLock(context.Context, uint64, string) error {
	return nil
}

func (a *integratedFriendRequestAdapter) BatchIsOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		out[id] = a.b.online[id]
	}
	return out, nil
}

// --- service.FriendStore ---
type integratedFriendStoreAdapter struct {
	b *integratedMemoryBackend
}

func (a *integratedFriendStoreAdapter) ListActiveFriendships(_ context.Context, userID uint64, cursorID uint64, limit int) ([]model.Friendship, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	var rows []model.Friendship
	for _, f := range a.b.friendships {
		if f.UserID == userID && f.Status == model.FriendshipStatusActive {
			if cursorID > 0 && f.ID >= cursorID {
				continue
			}
			rows = append(rows, f)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	if len(rows) > limit+1 {
		rows = rows[:limit+1]
	}
	return rows, nil
}

func (a *integratedFriendStoreAdapter) BatchGetUsersByIDs(_ context.Context, userIDs []uint64) (map[uint64]model.User, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	out := make(map[uint64]model.User, len(userIDs))
	for _, id := range userIDs {
		if u, ok := a.b.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (a *integratedFriendStoreAdapter) BatchGetSingleConversationsByDirectKeys(_ context.Context, directKeys []string) (map[string]model.Conversation, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	out := make(map[string]model.Conversation, len(directKeys))
	for _, dk := range directKeys {
		if cid, ok := a.b.convByDirectKey[dk]; ok {
			out[dk] = a.b.conversations[cid]
		}
	}
	return out, nil
}

func (a *integratedFriendStoreAdapter) GetOnlineStatusBatch(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		out[id] = a.b.online[id]
	}
	return out, nil
}

func (a *integratedFriendStoreAdapter) AreActiveFriends(_ context.Context, userA, userB uint64) (bool, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return a.b.friendships[[2]uint64{userA, userB}].Status == model.FriendshipStatusActive &&
		a.b.friendships[[2]uint64{userB, userA}].Status == model.FriendshipStatusActive, nil
}

func (a *integratedFriendStoreAdapter) UpdateRemark(_ context.Context, userID, friendID uint64, remark string) error {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	row, ok := a.b.friendships[[2]uint64{userID, friendID}]
	if !ok {
		return repository.ErrFriendshipRecordNotFound
	}
	if row.Status != model.FriendshipStatusActive {
		return repository.ErrFriendshipInactive
	}
	row.Remark = remark
	a.b.friendships[[2]uint64{userID, friendID}] = row
	return nil
}

func (a *integratedFriendStoreAdapter) DeactivateBidirectionalFriendship(_ context.Context, userID, friendID uint64) (repository.DeleteFriendshipOutcome, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	row, ok := a.b.friendships[[2]uint64{userID, friendID}]
	if !ok {
		return repository.DeleteFriendshipOutcome{}, repository.ErrFriendshipRecordNotFound
	}
	if row.Status != model.FriendshipStatusActive {
		return repository.DeleteFriendshipOutcome{}, repository.ErrFriendshipInactive
	}
	for _, p := range [][2]uint64{{userID, friendID}, {friendID, userID}} {
		if f, exists := a.b.friendships[p]; exists {
			f.Status = model.FriendshipStatusDeleted
			a.b.friendships[p] = f
		}
	}
	dk, _ := repository.FormatDirectKey(userID, friendID)
	cid := a.b.convByDirectKey[dk]
	for _, uid := range []uint64{userID, friendID} {
		k := fmt.Sprintf("%d:%d", cid, uid)
		if cm, exists := a.b.members[k]; exists {
			cm.IsVisible = false
			a.b.members[k] = cm
		}
	}
	return repository.DeleteFriendshipOutcome{
		FriendID:       friendID,
		ConversationID: &cid,
		IsVisible:      false,
	}, nil
}

func (a *integratedFriendStoreAdapter) FindSingleConversationByDirectKey(_ context.Context, directKey string) (*model.Conversation, error) {
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	cid, ok := a.b.convByDirectKey[directKey]
	if !ok {
		return nil, errors.New("not found")
	}
	c := a.b.conversations[cid]
	return &c, nil
}

func TestStage4IntegratedRouterRealJWTAuthAndCrossModuleFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	idGen, err := snowflake.NewGenerator(cfg.Snowflake.NodeID)
	if err != nil {
		t.Fatalf("snowflake.NewGenerator: %v", err)
	}
	jwtCfg := config.JWTConfig{
		Secret:     "stage4-integrated-router-secret-32b",
		Issuer:     "enterprise-im-test",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}
	authCfg := &config.AuthConfig{
		JWT:                jwtCfg,
		MaxSessionsPerUser: 5,
		BcryptCost:         10,
	}
	jwtMgr, err := pkgjwt.NewManager(jwtCfg)
	if err != nil {
		t.Fatalf("pkgjwt.NewManager: %v", err)
	}

	backend := newIntegratedMemoryBackend()
	authStore := &integratedAuthStoreAdapter{b: backend}
	frStore := &integratedFriendRequestAdapter{b: backend}
	friendStore := &integratedFriendStoreAdapter{b: backend}

	var hookCalls []string
	var hookMu sync.Mutex
	revokeHook := func(ctx context.Context, userID uint64, sessionIDs []uint64, reason string) error {
		hookMu.Lock()
		hookCalls = append(hookCalls, fmt.Sprintf("%d:%s:%d", userID, reason, len(sessionIDs)))
		hookMu.Unlock()
		return service.NoopSessionRevokeHook(ctx, userID, sessionIDs, reason)
	}

	healthSvc := service.NewHealthService(
		func(context.Context) error { return nil },
		func(context.Context) error { return nil },
		cfg.Health.ProbeTimeout,
		log,
	)
	authSvc := service.NewAuthService(authStore, jwtMgr, authCfg, log, revokeHook)
	userSvc := service.NewUserService(backend, backend, log)
	frSvc := service.NewFriendRequestService(frStore, frStore, frStore, frStore, idGen, log)
	friendSvc := service.NewFriendService(friendStore, log)

	jwtAuth := middleware.JWTAuth(jwtMgr, backend)
	router := api.NewRouterWithStage4(cfg, log, controller.NewHealthController(healthSvc), api.Stage4Dependencies{
		JWTAuth:           jwtAuth,
		AuthController:    controller.NewAuthController(authSvc),
		UserController:    controller.NewUserController(userSvc),
		FriendRequestCtrl: controller.NewFriendRequestController(frSvc),
		FriendController:  controller.NewFriendController(friendSvc),
	})

	// 1. /healthz remains public and healthy.
	wHealth, healthEnv := doJSON[apiEnvelope[service.HealthReport]](t, router, http.MethodGet, "/healthz", "", nil)
	if wHealth.Code != http.StatusOK || healthEnv.Code != 0 || healthEnv.Data.Status != "ok" {
		t.Fatalf("GET /healthz failed: status=%d env=%+v", wHealth.Code, healthEnv)
	}

	// 2. Register Alice & Bob (public auth routes).
	wRegA, regA := doJSON[apiEnvelope[service.RegisterOutput]](t, router, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"username": "alice_int",
		"password": "Password!123",
		"nickname": "Alice Int",
	})
	if wRegA.Code != http.StatusCreated || regA.Code != 0 {
		t.Fatalf("register alice failed: %d %+v", wRegA.Code, regA)
	}
	wRegB, regB := doJSON[apiEnvelope[service.RegisterOutput]](t, router, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"username": "bob_int",
		"password": "Password!123",
		"nickname": "Bob Int",
	})
	if wRegB.Code != http.StatusCreated || regB.Code != 0 {
		t.Fatalf("register bob failed: %d %+v", wRegB.Code, regB)
	}

	// 3. Login Alice & Bob -> obtain real Access JWTs.
	wLoginA, loginA := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    "alice_int",
		"password":    "Password!123",
		"device_id":   "web-alice-1",
		"device_type": "web",
	})
	if wLoginA.Code != http.StatusOK || loginA.Code != 0 {
		t.Fatalf("login alice failed: %d %+v", wLoginA.Code, loginA)
	}
	wLoginB, loginB := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    "bob_int",
		"password":    "Password!123",
		"device_id":   "mobile-bob-1",
		"device_type": "mobile",
	})
	if wLoginB.Code != http.StatusOK || loginB.Code != 0 {
		t.Fatalf("login bob failed: %d %+v", wLoginB.Code, loginB)
	}

	aliceToken := loginA.Data.Tokens.AccessToken
	bobToken := loginB.Data.Tokens.AccessToken

	// 4. GET /api/v1/me through real middleware.JWTAuth returns exact session_id and device_id.
	wMe, meEnv := doJSON[apiEnvelope[service.GetMeResult]](t, router, http.MethodGet, "/api/v1/me", aliceToken, nil)
	if wMe.Code != http.StatusOK || meEnv.Code != 0 {
		t.Fatalf("GET /api/v1/me failed: %d %+v", wMe.Code, meEnv)
	}
	if meEnv.Data.User.ID != regA.Data.User.ID ||
		meEnv.Data.SessionID != loginA.Data.Tokens.SessionID ||
		meEnv.Data.DeviceID != "web-alice-1" {
		t.Fatalf("GET /api/v1/me identity mismatch: got %+v want session_id=%s device_id=web-alice-1",
			meEnv.Data, loginA.Data.Tokens.SessionID)
	}

	// 5. PATCH /api/v1/me and GET /api/v1/users/search using real JWTAuth.
	wPatchMe, patchMeEnv := doJSON[apiEnvelope[service.UpdateMeResult]](t, router, http.MethodPatch, "/api/v1/me", aliceToken, map[string]string{
		"nickname":  "Alice Updated",
		"signature": "Integrated!",
	})
	if wPatchMe.Code != http.StatusOK || patchMeEnv.Data.User.Nickname != "Alice Updated" || patchMeEnv.Data.User.Signature != "Integrated!" {
		t.Fatalf("PATCH /api/v1/me failed: %d %+v", wPatchMe.Code, patchMeEnv)
	}
	wSearch, searchEnv := doJSON[apiEnvelope[service.UserSearchPage]](t, router, http.MethodGet, "/api/v1/users/search?q=bob_int&field=username&match=exact", aliceToken, nil)
	if wSearch.Code != http.StatusOK || len(searchEnv.Data.Items) != 1 || searchEnv.Data.Items[0].ID != regB.Data.User.ID {
		t.Fatalf("GET /api/v1/users/search failed: %d %+v", wSearch.Code, searchEnv)
	}

	// 6. FriendRequest flow: Alice creates -> Bob lists -> Bob accepts -> bilateral friends & conversation.
	wFR, frEnv := doJSON[apiEnvelope[struct {
		Request service.FriendRequestDTO `json:"request"`
	}]](t, router, http.MethodPost, "/api/v1/friend-requests", aliceToken, map[string]string{
		"to_user_id": regB.Data.User.ID,
		"message":    "hi bob",
	})
	if wFR.Code != http.StatusCreated || frEnv.Code != 0 {
		t.Fatalf("POST /api/v1/friend-requests failed: %d %+v", wFR.Code, frEnv)
	}

	wFRList, frListEnv := doJSON[apiEnvelope[service.FriendRequestPageDTO]](t, router, http.MethodGet, "/api/v1/friend-requests?direction=received&status=pending", bobToken, nil)
	if wFRList.Code != http.StatusOK || len(frListEnv.Data.Items) != 1 || frListEnv.Data.Items[0].ID != frEnv.Data.Request.ID {
		t.Fatalf("GET /api/v1/friend-requests failed: %d %+v", wFRList.Code, frListEnv)
	}

	acceptPath := fmt.Sprintf("/api/v1/friend-requests/%d/accept", frEnv.Data.Request.ID)
	wAccept, acceptEnv := doJSON[apiEnvelope[service.AcceptFriendRequestResult]](t, router, http.MethodPost, acceptPath, bobToken, nil)
	if wAccept.Code != http.StatusOK || acceptEnv.Code != 0 || acceptEnv.Data.ConversationID == 0 || acceptEnv.Data.SystemMsgID == 0 {
		t.Fatalf("POST accept failed: %d %+v", wAccept.Code, acceptEnv)
	}

	// 7. Friends flow: GET list -> PATCH remark -> DELETE friend.
	wFriendsA, friendsA := doJSON[apiEnvelope[service.FriendListResult]](t, router, http.MethodGet, "/api/v1/friends", aliceToken, nil)
	if wFriendsA.Code != http.StatusOK || len(friendsA.Data.Items) != 1 ||
		strconv.FormatUint(friendsA.Data.Items[0].FriendID, 10) != regB.Data.User.ID ||
		friendsA.Data.Items[0].ConversationID == nil ||
		*friendsA.Data.Items[0].ConversationID != strconv.FormatUint(acceptEnv.Data.ConversationID, 10) {
		t.Fatalf("GET /api/v1/friends failed: %d %+v", wFriendsA.Code, friendsA)
	}

	wRemark, remarkEnv := doJSON[apiEnvelope[service.UpdateFriendRemarkResult]](t, router, http.MethodPatch, "/api/v1/friends/"+regB.Data.User.ID, aliceToken, map[string]string{
		"remark": "Bob Partner",
	})
	if wRemark.Code != http.StatusOK || remarkEnv.Data.Remark != "Bob Partner" {
		t.Fatalf("PATCH /api/v1/friends failed: %d %+v", wRemark.Code, remarkEnv)
	}

	wDel, delEnv := doJSON[apiEnvelope[service.DeleteFriendResult]](t, router, http.MethodDelete, "/api/v1/friends/"+regB.Data.User.ID, aliceToken, nil)
	if wDel.Code != http.StatusOK || delEnv.Data.IsVisible {
		t.Fatalf("DELETE /api/v1/friends failed: %d %+v", wDel.Code, delEnv)
	}

	// 8. Logout & ChangePassword invalidate tokens across ALL protected endpoints via the shared JWTAuth.
	wLogout, logoutEnv := doJSON[apiEnvelope[service.LogoutOutput]](t, router, http.MethodPost, "/api/v1/auth/logout", aliceToken, nil)
	if wLogout.Code != http.StatusOK || logoutEnv.Data.RevokedCount != 1 {
		t.Fatalf("POST /api/v1/auth/logout failed: %d %+v", wLogout.Code, logoutEnv)
	}
	wMeAfterLogout, meAfterLogout := doJSON[apiEnvelope[any]](t, router, http.MethodGet, "/api/v1/me", aliceToken, nil)
	if wMeAfterLogout.Code != http.StatusUnauthorized || meAfterLogout.Code != errcode.ErrAccessTokenBlacklisted.Code {
		t.Fatalf("expected 401/11002 on /me after logout, got %d/%d", wMeAfterLogout.Code, meAfterLogout.Code)
	}

	wCP, cpEnv := doJSON[apiEnvelope[service.ChangePasswordOutput]](t, router, http.MethodPut, "/api/v1/me/password", bobToken, map[string]string{
		"old_password": "Password!123",
		"new_password": "NewPassword!456",
	})
	if wCP.Code != http.StatusOK || cpEnv.Data.RevokedCount != 1 {
		t.Fatalf("PUT /api/v1/me/password failed: %d %+v", wCP.Code, cpEnv)
	}
	wFriendsAfterCP, friendsAfterCP := doJSON[apiEnvelope[any]](t, router, http.MethodGet, "/api/v1/friends", bobToken, nil)
	if wFriendsAfterCP.Code != http.StatusUnauthorized || friendsAfterCP.Code != errcode.ErrAccessTokenBlacklisted.Code {
		t.Fatalf("expected 401/11002 on /friends after password change, got %d/%d", wFriendsAfterCP.Code, friendsAfterCP.Code)
	}

	hookMu.Lock()
	numHookCalls := len(hookCalls)
	hookMu.Unlock()
	if numHookCalls < 2 {
		t.Fatalf("expected at least 2 revokeHook calls (logout + change_password), got %d", numHookCalls)
	}

	// 9. RegisterStage4Routes fails closed when JWTAuth is nil.
	rClosed := api.NewRouterWithStage4(cfg, log, controller.NewHealthController(healthSvc), api.Stage4Dependencies{
		AuthController:    controller.NewAuthController(authSvc),
		UserController:    controller.NewUserController(userSvc),
		FriendRequestCtrl: controller.NewFriendRequestController(frSvc),
		FriendController:  controller.NewFriendController(friendSvc),
	})
	for _, tc := range [][2]string{
		{http.MethodGet, "/api/v1/me"},
		{http.MethodGet, "/api/v1/friends"},
		{http.MethodGet, "/api/v1/friend-requests?direction=received"},
		{http.MethodPost, "/api/v1/auth/logout"},
	} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(tc[0], tc[1], nil)
		rClosed.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("nil JWTAuth expected 401 on %s %s, got %d", tc[0], tc[1], w.Code)
		}
	}

	// 10. Verify secrets/tokens never appear in logs.
	rawLogs := logBuf.String()
	for _, secret := range []string{"Password!123", "NewPassword!456", aliceToken, bobToken, loginA.Data.Tokens.RefreshToken} {
		if strings.Contains(rawLogs, secret) {
			t.Fatalf("sensitive credential leaked in integrated logs")
		}
	}
}

func TestStage4IntegratedLiveMySQLAndRedis(t *testing.T) {
	mysqlPass := firstNonEmptyEnv("MYSQL_PASSWORD", "IM_MYSQL_PASSWORD")
	redisPass := firstNonEmptyEnv("REDIS_PASSWORD", "IM_REDIS_PASSWORD")
	if mysqlPass == "" && redisPass == "" && os.Getenv("IM_RUN_LIVE_TESTS") == "" {
		t.Skip("skipping live integrated Stage 4 test: MYSQL_PASSWORD/REDIS_PASSWORD not set")
	}

	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if v := firstNonEmptyEnv("MYSQL_HOST", "IM_MYSQL_HOST"); v != "" {
		cfg.MySQL.Host = v
	}
	if v := firstNonEmptyEnv("MYSQL_PORT", "IM_MYSQL_PORT"); v != "" {
		if port, convErr := strconv.Atoi(v); convErr == nil {
			cfg.MySQL.Port = port
		}
	}
	if v := firstNonEmptyEnv("MYSQL_USER", "IM_MYSQL_USER"); v != "" {
		cfg.MySQL.User = v
	}
	if v := firstNonEmptyEnv("MYSQL_DATABASE", "IM_MYSQL_DATABASE"); v != "" {
		cfg.MySQL.Database = v
	}
	cfg.MySQL.Password = mysqlPass
	if v := firstNonEmptyEnv("REDIS_ADDR", "IM_REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	cfg.Redis.Password = redisPass

	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	idGen, err := snowflake.NewGenerator(cfg.Snowflake.NodeID)
	if err != nil {
		t.Fatalf("snowflake.NewGenerator: %v", err)
	}
	jwtCfg := config.JWTConfig{
		Secret:     "stage4-live-integrated-secret-32b",
		Issuer:     "enterprise-im-stage4-integrated",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}
	authCfg := &config.AuthConfig{
		JWT:                jwtCfg,
		MaxSessionsPerUser: 5,
		BcryptCost:         10,
	}
	jwtMgr, err := pkgjwt.NewManager(jwtCfg)
	if err != nil {
		t.Fatalf("pkgjwt.NewManager: %v", err)
	}

	db, err := repository.NewMySQL(cfg.MySQL, log)
	if err != nil {
		t.Fatalf("NewMySQL failed: %v", err)
	}
	t.Cleanup(func() { _ = repository.CloseMySQL(db) })

	rdb, err := repository.NewRedis(cfg.Redis)
	if err != nil {
		t.Fatalf("NewRedis failed: %v", err)
	}
	t.Cleanup(func() { _ = repository.CloseRedis(rdb) })

	healthSvc := service.NewHealthService(
		func(ctx context.Context) error { return repository.PingMySQL(ctx, db) },
		func(ctx context.Context) error { return repository.PingRedis(ctx, rdb) },
		cfg.Health.ProbeTimeout,
		log,
	)
	authRepo := repository.NewAuthRepository(db)
	sessionRepo := repository.NewSessionRepository(db, rdb)
	userRepo := repository.NewUserRepository(db)
	userPresenceRepo := repository.NewUserPresenceRepository(rdb)
	friendRepo := repository.NewFriendshipRepository(db, rdb)
	friendReqRepo := repository.NewGormFriendRequestRepository(db)
	seqAlloc := repository.NewRedisSeqAllocator(rdb, 5*time.Second)
	frPresence := repository.NewRedisPresenceChecker(rdb)

	jwtAuth := middleware.JWTAuth(jwtMgr, middleware.NewAuthStateStore(authRepo, sessionRepo))
	authSvc := service.NewAuthServiceWithRepos(authRepo, sessionRepo, jwtMgr, authCfg, log, service.NoopSessionRevokeHook)
	userSvc := service.NewUserService(userRepo, userPresenceRepo, log)
	friendReqSvc := service.NewFriendRequestService(friendReqRepo, friendRepo, seqAlloc, frPresence, idGen, log)
	friendSvc := service.NewFriendService(friendRepo, log)

	router := api.NewRouterWithStage4(cfg, log, controller.NewHealthController(healthSvc), api.Stage4Dependencies{
		JWTAuth:           jwtAuth,
		AuthController:    controller.NewAuthController(authSvc),
		UserController:    controller.NewUserController(userSvc),
		FriendRequestCtrl: controller.NewFriendRequestController(friendReqSvc),
		FriendController:  controller.NewFriendController(friendSvc),
	})

	var (
		userIDs []uint64
		convID  uint64
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if convID > 0 {
			_ = db.WithContext(ctx).Where("conversation_id = ?", convID).Delete(&model.MessageReceipt{}).Error
			_ = db.WithContext(ctx).Where("conversation_id = ?", convID).Delete(&model.Message{}).Error
			_ = db.WithContext(ctx).Where("conversation_id = ?", convID).Delete(&model.ConversationMember{}).Error
			_ = db.WithContext(ctx).Where("id = ?", convID).Delete(&model.Conversation{}).Error
			_ = rdb.Del(ctx, fmt.Sprintf("conv:seq:%d", convID), fmt.Sprintf("conv:lock:%d", convID)).Err()
		}
		if len(userIDs) == 2 {
			_ = db.WithContext(ctx).Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", userIDs[0], userIDs[1], userIDs[1], userIDs[0]).Delete(&model.Friendship{}).Error
			_ = db.WithContext(ctx).Where("(from_user_id = ? AND to_user_id = ?) OR (from_user_id = ? AND to_user_id = ?)", userIDs[0], userIDs[1], userIDs[1], userIDs[0]).Delete(&model.FriendRequest{}).Error
		}
		if len(userIDs) > 0 {
			var jtis []string
			_ = db.WithContext(ctx).Model(&model.UserSession{}).Where("user_id IN ?", userIDs).Pluck("access_jti", &jtis).Error
			if len(jtis) > 0 {
				keys := make([]string, 0, len(jtis))
				for _, jti := range jtis {
					keys = append(keys, repository.TokenBlacklistKey(jti))
				}
				_ = rdb.Del(ctx, keys...).Err()
			}
			_ = db.WithContext(ctx).Where("user_id IN ?", userIDs).Delete(&model.UserSession{}).Error
			_ = db.WithContext(ctx).Where("id IN ?", userIDs).Delete(&model.User{}).Error
		}
	})

	tag := fmt.Sprintf("s4int_%d", time.Now().UnixNano())
	_, regA := doJSON[apiEnvelope[service.RegisterOutput]](t, router, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"username": tag + "_a",
		"password": "LivePass!123",
		"nickname": "LiveA",
	})
	_, regB := doJSON[apiEnvelope[service.RegisterOutput]](t, router, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"username": tag + "_b",
		"password": "LivePass!123",
		"nickname": "LiveB",
	})
	idA, _ := strconv.ParseUint(regA.Data.User.ID, 10, 64)
	idB, _ := strconv.ParseUint(regB.Data.User.ID, 10, 64)
	if idA == 0 || idB == 0 {
		t.Fatalf("expected non-zero registered user IDs, got %+v %+v", regA, regB)
	}
	userIDs = []uint64{idA, idB}

	_, loginA := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    tag + "_a",
		"password":    "LivePass!123",
		"device_id":   "live-web-a",
		"device_type": "web",
	})
	_, loginB := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    tag + "_b",
		"password":    "LivePass!123",
		"device_id":   "live-web-b",
		"device_type": "web",
	})

	wMe, meEnv := doJSON[apiEnvelope[service.GetMeResult]](t, router, http.MethodGet, "/api/v1/me", loginA.Data.Tokens.AccessToken, nil)
	if wMe.Code != http.StatusOK || meEnv.Data.SessionID != loginA.Data.Tokens.SessionID || meEnv.Data.DeviceID != "live-web-a" {
		t.Fatalf("live GET /api/v1/me unexpected: status=%d env=%+v", wMe.Code, meEnv)
	}

	wFR, frEnv := doJSON[apiEnvelope[struct {
		Request service.FriendRequestDTO `json:"request"`
	}]](t, router, http.MethodPost, "/api/v1/friend-requests", loginA.Data.Tokens.AccessToken, map[string]string{
		"to_user_id": regB.Data.User.ID,
		"message":    "live integrated",
	})
	if wFR.Code != http.StatusCreated || frEnv.Data.Request.ID == 0 {
		t.Fatalf("live create friend request failed: %d %+v", wFR.Code, frEnv)
	}

	wAcc, accEnv := doJSON[apiEnvelope[service.AcceptFriendRequestResult]](t, router, http.MethodPost, fmt.Sprintf("/api/v1/friend-requests/%d/accept", frEnv.Data.Request.ID), loginB.Data.Tokens.AccessToken, nil)
	if wAcc.Code != http.StatusOK || accEnv.Data.ConversationID == 0 {
		t.Fatalf("live accept failed: %d %+v", wAcc.Code, accEnv)
	}
	convID = accEnv.Data.ConversationID

	wFriends, friendsEnv := doJSON[apiEnvelope[service.FriendListResult]](t, router, http.MethodGet, "/api/v1/friends", loginA.Data.Tokens.AccessToken, nil)
	if wFriends.Code != http.StatusOK || len(friendsEnv.Data.Items) != 1 || friendsEnv.Data.Items[0].FriendID != idB {
		t.Fatalf("live GET /api/v1/friends failed: %d %+v", wFriends.Code, friendsEnv)
	}
}
