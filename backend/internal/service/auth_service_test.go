package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/password"
	"enterprise-im/backend/internal/repository"
)

type memoryAuthStore struct {
	mu                  sync.Mutex
	nextUserID          uint64
	nextSessID          uint64
	users               map[uint64]*model.User
	usersByName         map[string]uint64
	sessions            map[uint64]*model.UserSession
	blacklist           map[string]string
	refreshLocks        map[string]string
	onlineUsers         map[uint64]bool
	txDelay             time.Duration
	txGate              <-chan struct{}
	onAcquireLockResult func(acquired bool)
}

func newMemoryAuthStore() *memoryAuthStore {
	return &memoryAuthStore{
		nextUserID:   1000,
		nextSessID:   5000,
		users:        make(map[uint64]*model.User),
		usersByName:  make(map[string]uint64),
		sessions:     make(map[uint64]*model.UserSession),
		blacklist:    make(map[string]string),
		refreshLocks: make(map[string]string),
		onlineUsers:  make(map[uint64]bool),
	}
}

func cloneUser(u *model.User) *model.User {
	if u == nil {
		return nil
	}
	cp := *u
	if u.LastOnlineAt != nil {
		t := *u.LastOnlineAt
		cp.LastOnlineAt = &t
	}
	return &cp
}

func cloneSession(s *model.UserSession) *model.UserSession {
	if s == nil {
		return nil
	}
	cp := *s
	if s.RevokedAt != nil {
		t := *s.RevokedAt
		cp.RevokedAt = &t
	}
	return &cp
}

type memoryAuthTx struct {
	parent      *memoryAuthStore
	users       map[uint64]*model.User
	usersByName map[string]uint64
	sessions    map[uint64]*model.UserSession
	nextSessID  uint64
}

func (m *memoryAuthStore) WithTx(_ context.Context, fn func(tx AuthTxStore) error) error {
	if m.txGate != nil {
		select {
		case <-m.txGate:
		case <-time.After(2 * time.Second):
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.txDelay > 0 {
		time.Sleep(m.txDelay)
	}

	tx := &memoryAuthTx{
		parent:      m,
		users:       make(map[uint64]*model.User, len(m.users)),
		usersByName: make(map[string]uint64, len(m.usersByName)),
		sessions:    make(map[uint64]*model.UserSession, len(m.sessions)),
		nextSessID:  m.nextSessID,
	}
	for id, u := range m.users {
		tx.users[id] = cloneUser(u)
	}
	for name, id := range m.usersByName {
		tx.usersByName[name] = id
	}
	for id, s := range m.sessions {
		tx.sessions[id] = cloneSession(s)
	}

	if err := fn(tx); err != nil {
		return err
	}

	m.users = tx.users
	m.usersByName = tx.usersByName
	m.sessions = tx.sessions
	m.nextSessID = tx.nextSessID
	return nil
}

func (m *memoryAuthStore) CreateUser(_ context.Context, user *model.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.usersByName[user.Username]; exists {
		return repository.ErrDuplicateUsername
	}
	m.nextUserID++
	user.ID = m.nextUserID
	user.CreatedAt = time.Now().UTC()
	user.UpdatedAt = user.CreatedAt
	m.users[user.ID] = cloneUser(user)
	m.usersByName[user.Username] = user.ID
	return nil
}

func (m *memoryAuthStore) FindUserByUsername(_ context.Context, username string) (*model.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.usersByName[username]
	if !ok {
		return nil, repository.ErrAuthUserNotFound
	}
	return cloneUser(m.users[id]), nil
}

func (m *memoryAuthStore) FindUserByID(_ context.Context, userID uint64) (*model.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return nil, repository.ErrAuthUserNotFound
	}
	return cloneUser(u), nil
}

func (m *memoryAuthStore) FindSessionByRefreshHash(_ context.Context, hash string) (*model.UserSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.RefreshTokenHash == hash {
			return cloneSession(s), nil
		}
	}
	return nil, repository.ErrSessionNotFound
}

func (m *memoryAuthStore) BlacklistAccessJTIs(_ context.Context, jtis []string, reason string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, jti := range jtis {
		if jti != "" {
			m.blacklist[jti] = reason
		}
	}
	return nil
}

func (m *memoryAuthStore) AcquireRefreshLock(_ context.Context, refreshHash string, _ time.Duration) (string, bool, error) {
	m.mu.Lock()
	if _, exists := m.refreshLocks[refreshHash]; exists {
		cb := m.onAcquireLockResult
		m.mu.Unlock()
		if cb != nil {
			cb(false)
		}
		return "", false, nil
	}
	tok := "lock-" + refreshHash
	m.refreshLocks[refreshHash] = tok
	cb := m.onAcquireLockResult
	m.mu.Unlock()
	if cb != nil {
		cb(true)
	}
	return tok, true, nil
}

func (m *memoryAuthStore) IsRefreshLockHeld(_ context.Context, refreshHash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.refreshLocks[refreshHash]
	return exists, nil
}

func (m *memoryAuthStore) ReleaseRefreshLock(_ context.Context, refreshHash, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, exists := m.refreshLocks[refreshHash]; exists && cur == token {
		delete(m.refreshLocks, refreshHash)
	}
	return nil
}

func (m *memoryAuthStore) IsUserOnline(_ context.Context, userID uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.onlineUsers[userID], nil
}

func (m *memoryAuthStore) GetUserAuthStatus(_ context.Context, userID uint64) (int8, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return 0, "", repository.ErrAuthUserNotFound
	}
	return u.Status, u.Role, nil
}

func (m *memoryAuthStore) IsAccessJTIBlacklisted(_ context.Context, jti string) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.blacklist[jti]
	return ok, r, nil
}

func (m *memoryAuthStore) GetSessionAuthState(_ context.Context, sessionID uint64) (repository.SessionAuthState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
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

func (tx *memoryAuthTx) LockUserByID(_ context.Context, userID uint64) (*model.User, error) {
	u, ok := tx.users[userID]
	if !ok {
		return nil, repository.ErrAuthUserNotFound
	}
	return cloneUser(u), nil
}

func (tx *memoryAuthTx) UpdateUserPasswordHash(_ context.Context, userID uint64, passwordHash string) error {
	u, ok := tx.users[userID]
	if !ok {
		return repository.ErrAuthUserNotFound
	}
	u.PasswordHash = passwordHash
	return nil
}

func (tx *memoryAuthTx) LockSessionByID(_ context.Context, sessionID uint64) (*model.UserSession, error) {
	s, ok := tx.sessions[sessionID]
	if !ok {
		return nil, repository.ErrSessionNotFound
	}
	return cloneSession(s), nil
}

func (tx *memoryAuthTx) ListUnrevokedSessionsByDevice(_ context.Context, userID uint64, deviceID string) ([]model.UserSession, error) {
	var out []model.UserSession
	for _, s := range tx.sessions {
		if s.UserID == userID && s.DeviceID == deviceID && s.RevokedAt == nil {
			out = append(out, *cloneSession(s))
		}
	}
	return out, nil
}

func (tx *memoryAuthTx) ListUnrevokedSessionsByUser(_ context.Context, userID uint64) ([]model.UserSession, error) {
	var out []model.UserSession
	for _, s := range tx.sessions {
		if s.UserID == userID && s.RevokedAt == nil {
			out = append(out, *cloneSession(s))
		}
	}
	return out, nil
}

func (tx *memoryAuthTx) CountActiveSessionsByUser(_ context.Context, userID uint64, now time.Time) (int64, error) {
	var cnt int64
	for _, s := range tx.sessions {
		if s.UserID == userID && s.RevokedAt == nil && s.ExpiresAt.After(now) {
			cnt++
		}
	}
	return cnt, nil
}

func (tx *memoryAuthTx) RevokeSessionsByIDs(_ context.Context, sessionIDs []uint64, revokedAt time.Time) (int64, error) {
	var n int64
	ts := revokedAt.UTC()
	for _, id := range sessionIDs {
		if s, ok := tx.sessions[id]; ok && s.RevokedAt == nil {
			copyTS := ts
			s.RevokedAt = &copyTS
			n++
		}
	}
	return n, nil
}

func (tx *memoryAuthTx) CreateSession(_ context.Context, session *model.UserSession) error {
	tx.nextSessID++
	session.ID = tx.nextSessID
	tx.sessions[session.ID] = cloneSession(session)
	return nil
}

func (tx *memoryAuthTx) RotateSessionTokens(_ context.Context, sessionID uint64, newRefreshHash, newAccessJTI string, newExpiresAt, activeAt time.Time, ipAddress, userAgent string) error {
	s, ok := tx.sessions[sessionID]
	if !ok || s.RevokedAt != nil {
		return repository.ErrSessionNotFound
	}
	s.RefreshTokenHash = newRefreshHash
	s.AccessJTI = newAccessJTI
	s.ExpiresAt = newExpiresAt.UTC()
	s.LastActiveAt = activeAt.UTC()
	s.IPAddress = ipAddress
	s.UserAgent = userAgent
	return nil
}

func newTestAuthService(t *testing.T, maxSessions int) (*AuthService, *memoryAuthStore, *pkgjwt.Manager, *[]string) {
	t.Helper()
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	jwtCfg := config.JWTConfig{
		Secret:     "service-unit-test-secret-32bytes!",
		Issuer:     "enterprise-im-test",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}
	mgr, err := pkgjwt.NewManagerWithClock(jwtCfg, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewManagerWithClock: %v", err)
	}
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}

	store := newMemoryAuthStore()
	var hookEvents []string
	var hookMu sync.Mutex
	hook := func(_ context.Context, userID uint64, sessionIDs []uint64, reason string) error {
		hookMu.Lock()
		defer hookMu.Unlock()
		for range sessionIDs {
			hookEvents = append(hookEvents, reason)
		}
		return nil
	}

	svc := NewAuthService(store, mgr, &config.AuthConfig{
		JWT:                jwtCfg,
		MaxSessionsPerUser: maxSessions,
		BcryptCost:         password.DefaultCost,
	}, log, hook).WithClock(func() time.Time { return now })
	return svc, store, mgr, &hookEvents
}

func TestAuthServiceRegisterLoginRefreshLogoutAndChangePassword(t *testing.T) {
	ctx := context.Background()
	svc, store, mgr, hookEvents := newTestAuthService(t, 2)

	// 1. Register validation + duplicate username + no session created
	if _, err := svc.Register(ctx, RegisterInput{Username: "alice", Password: "short", Nickname: "Alice"}); err == nil || err.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 for short password, got %v", err)
	}
	regOut, bizErr := svc.Register(ctx, RegisterInput{Username: "alice", Password: "AlicePass!1", Nickname: "Alice"})
	if bizErr != nil {
		t.Fatalf("Register failed: %v", bizErr)
	}
	if regOut.User.Role != model.UserRoleUser || regOut.User.Status != model.UserStatusActive || regOut.User.Online {
		t.Fatalf("unexpected registered user view: %+v", regOut.User)
	}
	if len(store.sessions) != 0 {
		t.Fatalf("register must not create sessions, got %d", len(store.sessions))
	}
	if _, err := svc.Register(ctx, RegisterInput{Username: "alice", Password: "AlicePass!1", Nickname: "Alice2"}); err == nil || err.Code != errcode.ErrUsernameExists.Code {
		t.Fatalf("expected 11007 for duplicate username, got %v", err)
	}

	// 2. Wrong password -> 11006
	if _, err := svc.Login(ctx, LoginInput{Username: "alice", Password: "WrongPassword!", DeviceID: "dev-web", DeviceType: "web"}); err == nil || err.Code != errcode.ErrInvalidCredentials.Code {
		t.Fatalf("expected 11006, got %v", err)
	}

	// 3. Multi-device login (web + desktop) and session limit (mobile -> 11009)
	webLogin, bizErr := svc.Login(ctx, LoginInput{Username: "alice", Password: "AlicePass!1", DeviceID: "dev-web", DeviceType: "web"})
	if bizErr != nil {
		t.Fatalf("web Login failed: %v", bizErr)
	}
	deskLogin, bizErr := svc.Login(ctx, LoginInput{Username: "alice", Password: "AlicePass!1", DeviceID: "dev-desk", DeviceType: "desktop"})
	if bizErr != nil {
		t.Fatalf("desktop Login failed: %v", bizErr)
	}
	if _, err := svc.Login(ctx, LoginInput{Username: "alice", Password: "AlicePass!1", DeviceID: "dev-mob", DeviceType: "mobile"}); err == nil || err.Code != errcode.ErrSessionLimitReached.Code {
		t.Fatalf("expected 11009 when exceeding max sessions, got %v", err)
	}

	// 4. Re-login on same device replaces web session without disturbing desktop session
	webLogin2, bizErr := svc.Login(ctx, LoginInput{Username: "alice", Password: "AlicePass!1", DeviceID: "dev-web", DeviceType: "web"})
	if bizErr != nil {
		t.Fatalf("same-device re-login failed: %v", bizErr)
	}
	if webLogin2.Tokens.SessionID == webLogin.Tokens.SessionID {
		t.Fatal("same-device re-login should issue a new session id")
	}
	oldWebClaims, _ := mgr.ParseAccessToken(webLogin.Tokens.AccessToken)
	if _, blacklisted := store.blacklist[oldWebClaims.ID]; !blacklisted {
		t.Fatal("expected replaced web session JTI to be blacklisted")
	}
	if _, err := svc.Refresh(ctx, RefreshInput{RefreshToken: webLogin.Tokens.RefreshToken, DeviceID: "dev-web"}); err == nil || err.Code != errcode.ErrSessionRevoked.Code {
		t.Fatalf("expected 11005 for replaced session refresh, got %v", err)
	}

	// 5. Device mismatch -> 11005; valid refresh rotates both tokens; sequential reuse -> 11003
	if _, err := svc.Refresh(ctx, RefreshInput{RefreshToken: deskLogin.Tokens.RefreshToken, DeviceID: "wrong-dev"}); err == nil || err.Code != errcode.ErrSessionRevoked.Code {
		t.Fatalf("expected 11005 on device mismatch, got %v", err)
	}
	refOut, bizErr := svc.Refresh(ctx, RefreshInput{RefreshToken: deskLogin.Tokens.RefreshToken, DeviceID: "dev-desk"})
	if bizErr != nil {
		t.Fatalf("valid Refresh failed: %v", bizErr)
	}
	if refOut.Tokens.AccessToken == deskLogin.Tokens.AccessToken || refOut.Tokens.RefreshToken == deskLogin.Tokens.RefreshToken {
		t.Fatal("refresh must rotate both access and refresh tokens")
	}
	if _, err := svc.Refresh(ctx, RefreshInput{RefreshToken: deskLogin.Tokens.RefreshToken, DeviceID: "dev-desk"}); err == nil || err.Code != errcode.ErrRefreshTokenInvalid.Code {
		t.Fatalf("expected 11003 on sequential reuse of consumed refresh token, got %v", err)
	}

	// 6. Logout current revokes only webLogin2, leaving desk session active
	web2Claims, _ := mgr.ParseAccessToken(webLogin2.Tokens.AccessToken)
	web2UID, _ := web2Claims.UserID()
	web2SID, _ := web2Claims.SessionIDUint64()
	logoutOut, bizErr := svc.Logout(ctx, LogoutInput{
		UserID:     web2UID,
		SessionID:  web2SID,
		CurrentJTI: web2Claims.ID,
		Scope:      "",
	})
	if bizErr != nil || logoutOut.Scope != LogoutScopeCurrent || logoutOut.RevokedCount != 1 {
		t.Fatalf("unexpected logout output: %+v err=%v", logoutOut, bizErr)
	}

	// Desktop session still refreshes cleanly
	refOut2, bizErr := svc.Refresh(ctx, RefreshInput{RefreshToken: refOut.Tokens.RefreshToken, DeviceID: "dev-desk"})
	if bizErr != nil {
		t.Fatalf("desktop session should remain active after web logout: %v", bizErr)
	}

	// 7. ChangePassword revokes all remaining sessions and updates bcrypt hash
	desk2Claims, _ := mgr.ParseAccessToken(refOut2.Tokens.AccessToken)
	if _, err := svc.ChangePassword(ctx, ChangePasswordInput{
		UserID:      web2UID,
		CurrentJTI:  desk2Claims.ID,
		OldPassword: "WrongOldPass",
		NewPassword: "NewAlicePass!2",
	}); err == nil || err.Code != errcode.ErrInvalidCredentials.Code {
		t.Fatalf("expected 11006 for wrong old_password, got %v", err)
	}

	cpOut, bizErr := svc.ChangePassword(ctx, ChangePasswordInput{
		UserID:      web2UID,
		CurrentJTI:  desk2Claims.ID,
		OldPassword: "AlicePass!1",
		NewPassword: "NewAlicePass!2",
	})
	if bizErr != nil || cpOut.RevokedCount != 1 {
		t.Fatalf("unexpected ChangePassword output: %+v err=%v", cpOut, bizErr)
	}
	if _, err := svc.Refresh(ctx, RefreshInput{RefreshToken: refOut2.Tokens.RefreshToken, DeviceID: "dev-desk"}); err == nil || err.Code != errcode.ErrSessionRevoked.Code {
		t.Fatalf("expected 11005 after password change revoked sessions, got %v", err)
	}
	if _, err := svc.Login(ctx, LoginInput{Username: "alice", Password: "AlicePass!1", DeviceID: "dev-desk", DeviceType: "desktop"}); err == nil || err.Code != errcode.ErrInvalidCredentials.Code {
		t.Fatalf("expected old password rejected after change, got %v", err)
	}
	if _, bizErr := svc.Login(ctx, LoginInput{Username: "alice", Password: "NewAlicePass!2", DeviceID: "dev-desk", DeviceType: "desktop"}); bizErr != nil {
		t.Fatalf("expected new password accepted, got %v", bizErr)
	}

	if len(*hookEvents) < 3 {
		t.Fatalf("expected session revoke hook invocations, got %v", *hookEvents)
	}
}

func TestAuthServiceConcurrentRefreshConflictReturns11008(t *testing.T) {
	ctx := context.Background()
	svc, store, _, _ := newTestAuthService(t, 5)

	if _, bizErr := svc.Register(ctx, RegisterInput{Username: "bob", Password: "BobPass!123", Nickname: "Bob"}); bizErr != nil {
		t.Fatalf("Register failed: %v", bizErr)
	}
	loginOut, bizErr := svc.Login(ctx, LoginInput{Username: "bob", Password: "BobPass!123", DeviceID: "dev-1", DeviceType: "web"})
	if bizErr != nil {
		t.Fatalf("Login failed: %v", bizErr)
	}

	const workers = 8
	// Gate the single lock-winning tx until all concurrent workers hit AcquireRefreshLock
	// so scheduling jitter under -race never causes late workers to see a post-rotation token (11003).
	gate := make(chan struct{})
	var closeOnce sync.Once
	var lockConflicts atomic.Int32
	store.txGate = gate
	store.onAcquireLockResult = func(acquired bool) {
		if !acquired && lockConflicts.Add(1) == workers-1 {
			closeOnce.Do(func() { close(gate) })
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var successCount atomic.Int32
	var conflictCount atomic.Int32
	var otherErrs atomic.Int32

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.Refresh(ctx, RefreshInput{
				RefreshToken: loginOut.Tokens.RefreshToken,
				DeviceID:     "dev-1",
			})
			if err == nil {
				successCount.Add(1)
				return
			}
			if errors.Is(err, errcode.ErrRefreshConflict) || err.Code == errcode.ErrRefreshConflict.Code {
				conflictCount.Add(1)
				return
			}
			otherErrs.Add(1)
			t.Errorf("unexpected error code in concurrent refresh: %v", err)
		}()
	}

	close(start)
	wg.Wait()

	if successCount.Load() != 1 || conflictCount.Load() != workers-1 || otherErrs.Load() != 0 {
		t.Fatalf("unexpected concurrent refresh outcome: success=%d conflict=%d other=%d",
			successCount.Load(), conflictCount.Load(), otherErrs.Load())
	}
}

func TestAuthServiceDisabledUserDeniedOnLoginAndRefresh(t *testing.T) {
	ctx := context.Background()
	svc, store, _, _ := newTestAuthService(t, 5)

	regOut, bizErr := svc.Register(ctx, RegisterInput{Username: "carol", Password: "CarolPass!1", Nickname: "Carol"})
	if bizErr != nil {
		t.Fatalf("Register failed: %v", bizErr)
	}
	loginOut, bizErr := svc.Login(ctx, LoginInput{Username: "carol", Password: "CarolPass!1", DeviceID: "dev-1", DeviceType: "mobile"})
	if bizErr != nil {
		t.Fatalf("Login failed: %v", bizErr)
	}

	// Disable carol (status = 0)
	store.mu.Lock()
	for _, u := range store.users {
		if u.Username == regOut.User.Username {
			u.Status = model.UserStatusDisabled
		}
	}
	store.mu.Unlock()

	if _, err := svc.Login(ctx, LoginInput{Username: "carol", Password: "CarolPass!1", DeviceID: "dev-2", DeviceType: "web"}); err == nil || err.Code != errcode.ErrAccountDisabled.Code {
		t.Fatalf("expected 11004 on disabled account login, got %v", err)
	}
	if _, err := svc.Refresh(ctx, RefreshInput{RefreshToken: loginOut.Tokens.RefreshToken, DeviceID: "dev-1"}); err == nil || err.Code != errcode.ErrAccountDisabled.Code {
		t.Fatalf("expected 11004 on disabled account refresh, got %v", err)
	}
}
