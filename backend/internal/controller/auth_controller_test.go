package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/password"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type ctrlMemStore struct {
	mu           sync.Mutex
	nextUserID   uint64
	nextSessID   uint64
	users        map[uint64]*model.User
	usersByName  map[string]uint64
	sessions     map[uint64]*model.UserSession
	blacklist    map[string]string
	refreshLocks map[string]string
}

func newCtrlMemStore() *ctrlMemStore {
	return &ctrlMemStore{
		nextUserID:   100,
		nextSessID:   500,
		users:        make(map[uint64]*model.User),
		usersByName:  make(map[string]uint64),
		sessions:     make(map[uint64]*model.UserSession),
		blacklist:    make(map[string]string),
		refreshLocks: make(map[string]string),
	}
}

type ctrlMemTx struct {
	s *ctrlMemStore
}

func (m *ctrlMemStore) WithTx(_ context.Context, fn func(tx service.AuthTxStore) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(&ctrlMemTx{s: m})
}

func (m *ctrlMemStore) CreateUser(_ context.Context, user *model.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.usersByName[user.Username]; exists {
		return repository.ErrDuplicateUsername
	}
	m.nextUserID++
	user.ID = m.nextUserID
	cp := *user
	m.users[user.ID] = &cp
	m.usersByName[user.Username] = user.ID
	return nil
}

func (m *ctrlMemStore) FindUserByUsername(_ context.Context, username string) (*model.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.usersByName[username]
	if !ok {
		return nil, repository.ErrAuthUserNotFound
	}
	cp := *m.users[id]
	return &cp, nil
}

func (m *ctrlMemStore) FindUserByID(_ context.Context, userID uint64) (*model.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return nil, repository.ErrAuthUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (m *ctrlMemStore) FindSessionByRefreshHash(_ context.Context, hash string) (*model.UserSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.RefreshTokenHash == hash {
			cp := *s
			return &cp, nil
		}
	}
	return nil, repository.ErrSessionNotFound
}

func (m *ctrlMemStore) BlacklistAccessJTIs(_ context.Context, jtis []string, reason string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, jti := range jtis {
		m.blacklist[jti] = reason
	}
	return nil
}

func (m *ctrlMemStore) AcquireRefreshLock(_ context.Context, refreshHash string, _ time.Duration) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.refreshLocks[refreshHash]; ok {
		return "", false, nil
	}
	m.refreshLocks[refreshHash] = "tok"
	return "tok", true, nil
}

func (m *ctrlMemStore) IsRefreshLockHeld(_ context.Context, refreshHash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.refreshLocks[refreshHash]
	return ok, nil
}

func (m *ctrlMemStore) ReleaseRefreshLock(_ context.Context, refreshHash, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refreshLocks[refreshHash] == token {
		delete(m.refreshLocks, refreshHash)
	}
	return nil
}

func (m *ctrlMemStore) IsUserOnline(context.Context, uint64) (bool, error) {
	return false, nil
}

func (m *ctrlMemStore) GetUserAuthStatus(_ context.Context, userID uint64) (int8, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return 0, "", repository.ErrAuthUserNotFound
	}
	return u.Status, u.Role, nil
}

func (m *ctrlMemStore) IsAccessJTIBlacklisted(_ context.Context, jti string) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.blacklist[jti]
	return ok, r, nil
}

func (m *ctrlMemStore) GetSessionAuthState(_ context.Context, sessionID uint64) (repository.SessionAuthState, error) {
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

func (tx *ctrlMemTx) LockUserByID(_ context.Context, userID uint64) (*model.User, error) {
	u, ok := tx.s.users[userID]
	if !ok {
		return nil, repository.ErrAuthUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (tx *ctrlMemTx) UpdateUserPasswordHash(_ context.Context, userID uint64, passwordHash string) error {
	u, ok := tx.s.users[userID]
	if !ok {
		return repository.ErrAuthUserNotFound
	}
	u.PasswordHash = passwordHash
	return nil
}

func (tx *ctrlMemTx) LockSessionByID(_ context.Context, sessionID uint64) (*model.UserSession, error) {
	s, ok := tx.s.sessions[sessionID]
	if !ok {
		return nil, repository.ErrSessionNotFound
	}
	cp := *s
	return &cp, nil
}

func (tx *ctrlMemTx) ListUnrevokedSessionsByDevice(_ context.Context, userID uint64, deviceID string) ([]model.UserSession, error) {
	var out []model.UserSession
	for _, s := range tx.s.sessions {
		if s.UserID == userID && s.DeviceID == deviceID && s.RevokedAt == nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (tx *ctrlMemTx) ListUnrevokedSessionsByUser(_ context.Context, userID uint64) ([]model.UserSession, error) {
	var out []model.UserSession
	for _, s := range tx.s.sessions {
		if s.UserID == userID && s.RevokedAt == nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (tx *ctrlMemTx) CountActiveSessionsByUser(_ context.Context, userID uint64, now time.Time) (int64, error) {
	var n int64
	for _, s := range tx.s.sessions {
		if s.UserID == userID && s.RevokedAt == nil && s.ExpiresAt.After(now) {
			n++
		}
	}
	return n, nil
}

func (tx *ctrlMemTx) RevokeSessionsByIDs(_ context.Context, sessionIDs []uint64, revokedAt time.Time) (int64, error) {
	var n int64
	ts := revokedAt.UTC()
	for _, id := range sessionIDs {
		if s, ok := tx.s.sessions[id]; ok && s.RevokedAt == nil {
			copyTS := ts
			s.RevokedAt = &copyTS
			n++
		}
	}
	return n, nil
}

func (tx *ctrlMemTx) CreateSession(_ context.Context, session *model.UserSession) error {
	tx.s.nextSessID++
	session.ID = tx.s.nextSessID
	cp := *session
	tx.s.sessions[session.ID] = &cp
	return nil
}

func (tx *ctrlMemTx) RotateSessionTokens(_ context.Context, sessionID uint64, newRefreshHash, newAccessJTI string, newExpiresAt, activeAt time.Time, ipAddress, userAgent string) error {
	s, ok := tx.s.sessions[sessionID]
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

func TestAuthControllerMaxBytesAndHTTPEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logBuf bytes.Buffer
	log, _ := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)

	jwtCfg := config.JWTConfig{
		Secret:     "controller-test-secret-32-bytes!!",
		Issuer:     "enterprise-im-test",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}
	mgr, err := pkgjwt.NewManager(jwtCfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	store := newCtrlMemStore()
	svc := service.NewAuthService(store, mgr, &config.AuthConfig{
		JWT:                jwtCfg,
		MaxSessionsPerUser: 5,
		BcryptCost:         password.DefaultCost,
	}, log, nil)
	ctrl := NewAuthController(svc)
	jwtMW := middleware.JWTAuth(mgr, store)

	r := gin.New()
	r.Use(middleware.RequestID())
	r.Use(middleware.AccessLogger(log))
	r.Use(middleware.MaxBodySize(256))
	v1 := r.Group("/api/v1")
	v1.POST("/auth/register", ctrl.Register)
	v1.POST("/auth/login", ctrl.Login)
	v1.POST("/auth/refresh", ctrl.Refresh)
	v1.POST("/auth/logout", jwtMW, ctrl.Logout)
	v1.PUT("/me/password", jwtMW, ctrl.ChangePassword)

	// 1. Register returns 201 and X-Request-ID
	wReg := httptest.NewRecorder()
	reqReg, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"username":"dana","password":"DanaPass!123","nickname":"Dana"}`))
	reqReg.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wReg, reqReg)
	if wReg.Code != http.StatusCreated {
		t.Fatalf("expected 201 on register, got %d body=%s", wReg.Code, wReg.Body.String())
	}

	// 2. Chunked oversized JSON body (ContentLength == -1) hits *http.MaxBytesError -> 413 + 10002
	oversized := `{"username":"` + strings.Repeat("a", 300) + `","password":"DanaPass!123","nickname":"Dana"}`
	wBig := httptest.NewRecorder()
	reqBig, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(oversized))
	reqBig.ContentLength = -1
	reqBig.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wBig, reqBig)
	if wBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 on MaxBytesError, got %d body=%s", wBig.Code, wBig.Body.String())
	}
	var bigEnv errcode.Envelope
	_ = json.Unmarshal(wBig.Body.Bytes(), &bigEnv)
	if bigEnv.Code != errcode.ErrPayloadTooLarge.Code {
		t.Fatalf("expected code 10002, got %d", bigEnv.Code)
	}

	// 3. Login -> Refresh -> Logout (empty body defaults to current) -> old token rejected 11002
	wLogin := httptest.NewRecorder()
	reqLogin, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"dana","password":"DanaPass!123","device_id":"web-d1","device_type":"web"}`))
	reqLogin.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wLogin, reqLogin)
	if wLogin.Code != http.StatusOK {
		t.Fatalf("expected 200 on login, got %d body=%s", wLogin.Code, wLogin.Body.String())
	}
	var loginResp struct {
		Code int                 `json:"code"`
		Data service.LoginOutput `json:"data"`
	}
	_ = json.Unmarshal(wLogin.Body.Bytes(), &loginResp)

	wLogout := httptest.NewRecorder()
	reqLogout, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	reqLogout.Header.Set("Authorization", "Bearer "+loginResp.Data.Tokens.AccessToken)
	r.ServeHTTP(wLogout, reqLogout)
	if wLogout.Code != http.StatusOK {
		t.Fatalf("expected 200 on empty-body logout, got %d body=%s", wLogout.Code, wLogout.Body.String())
	}

	// Subsequent request with logged-out token -> 401 + 11002
	wAgain := httptest.NewRecorder()
	reqAgain, _ := http.NewRequest(http.MethodPut, "/api/v1/me/password", strings.NewReader(`{"old_password":"DanaPass!123","new_password":"DanaPass!456"}`))
	reqAgain.Header.Set("Authorization", "Bearer "+loginResp.Data.Tokens.AccessToken)
	r.ServeHTTP(wAgain, reqAgain)
	if wAgain.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d body=%s", wAgain.Code, wAgain.Body.String())
	}
	var againEnv errcode.Envelope
	_ = json.Unmarshal(wAgain.Body.Bytes(), &againEnv)
	if againEnv.Code != errcode.ErrAccessTokenBlacklisted.Code {
		t.Fatalf("expected 11002, got %d", againEnv.Code)
	}

	// Verify no secret/password/token leaked into logs.
	logs := logBuf.String()
	if strings.Contains(logs, "DanaPass!123") || strings.Contains(logs, loginResp.Data.Tokens.AccessToken) || strings.Contains(logs, loginResp.Data.Tokens.RefreshToken) {
		t.Fatalf("sensitive credential leaked in logs: %s", logs)
	}
}
