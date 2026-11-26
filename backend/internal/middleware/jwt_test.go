package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/repository"
)

type memoryAuthStateStore struct {
	users       map[uint64]struct {
		status int8
		role   string
	}
	blacklist   map[string]string
	sessions    map[uint64]repository.SessionAuthState
	userErr     error
	blacklistErr error
	sessionErr  error
}

func (m *memoryAuthStateStore) GetUserAuthStatus(_ context.Context, userID uint64) (int8, string, error) {
	if m.userErr != nil {
		return 0, "", m.userErr
	}
	u, ok := m.users[userID]
	if !ok {
		return 0, "", repository.ErrAuthUserNotFound
	}
	return u.status, u.role, nil
}

func (m *memoryAuthStateStore) IsAccessJTIBlacklisted(_ context.Context, jti string) (bool, string, error) {
	if m.blacklistErr != nil {
		return false, "", m.blacklistErr
	}
	reason, ok := m.blacklist[jti]
	return ok, reason, nil
}

func (m *memoryAuthStateStore) GetSessionAuthState(_ context.Context, sessionID uint64) (repository.SessionAuthState, error) {
	if m.sessionErr != nil {
		return repository.SessionAuthState{}, m.sessionErr
	}
	s, ok := m.sessions[sessionID]
	if !ok {
		return repository.SessionAuthState{}, repository.ErrSessionNotFound
	}
	return s, nil
}

func TestJWTAuthMiddlewareAllPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	mgr, err := pkgjwt.NewManagerWithClock(config.JWTConfig{
		Secret:     "middleware-test-secret-32-bytes!!",
		Issuer:     "enterprise-im",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewManagerWithClock: %v", err)
	}

	validTok, validJTI, _, err := mgr.IssueAccessToken(101, 501, model.UserRoleUser)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	newStore := func() *memoryAuthStateStore {
		return &memoryAuthStateStore{
			users: map[uint64]struct {
				status int8
				role   string
			}{
				101: {status: model.UserStatusActive, role: model.UserRoleAdmin},
			},
			blacklist: map[string]string{},
			sessions: map[uint64]repository.SessionAuthState{
				501: {
					ID:        501,
					UserID:    101,
					DeviceID:  "web-1",
					AccessJTI: validJTI,
					ExpiresAt: now.Add(24 * time.Hour),
				},
			},
		}
	}

	runReq := func(store AuthStateStore, authHeader string) (int, errcode.Envelope, AuthContext, bool) {
		var captured AuthContext
		var hasCaptured bool
		r := gin.New()
		r.Use(RequestID())
		r.GET("/protected", JWTAuthWithClock(mgr, store, func() time.Time { return now }), func(c *gin.Context) {
			captured, hasCaptured = GetAuthContext(c)
			errcode.OK(c, map[string]string{"uid": errcode.GetUserID(c)})
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/protected", nil)
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		r.ServeHTTP(w, req)

		var env errcode.Envelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		return w.Code, env, captured, hasCaptured
	}

	// 1. Valid request sets AuthContext with DB role and user_id in context.
	httpCode, env, authCtx, ok := runReq(newStore(), "Bearer "+validTok)
	if httpCode != http.StatusOK || env.Code != 0 || !ok {
		t.Fatalf("expected valid auth pass, status=%d env=%+v ok=%v", httpCode, env, ok)
	}
	if authCtx.UserID != 101 || authCtx.SessionID != 501 || authCtx.DeviceID != "web-1" || authCtx.Role != model.UserRoleAdmin || authCtx.JTI != validJTI {
		t.Fatalf("unexpected AuthContext: %+v", authCtx)
	}

	// 2. Missing / malformed header -> 11001 (401)
	for _, hdr := range []string{"", "Basic abc", "Bearer", "Bearer invalid.jwt.token"} {
		status, e, _, _ := runReq(newStore(), hdr)
		if status != http.StatusUnauthorized || e.Code != errcode.ErrAccessTokenInvalid.Code {
			t.Fatalf("header %q expected 401/11001, got %d/%d", hdr, status, e.Code)
		}
	}

	// 3. Disabled account -> 11004 (403), even if JTI is also blacklisted
	disabledStore := newStore()
	disabledStore.users[101] = struct {
		status int8
		role   string
	}{status: model.UserStatusDisabled, role: model.UserRoleUser}
	disabledStore.blacklist[validJTI] = "account_disabled"
	status, env, _, _ := runReq(disabledStore, "Bearer "+validTok)
	if status != http.StatusForbidden || env.Code != errcode.ErrAccountDisabled.Code {
		t.Fatalf("disabled account expected 403/11004, got %d/%d", status, env.Code)
	}

	// 4. Blacklisted JTI -> 11002 (401)
	blStore := newStore()
	blStore.blacklist[validJTI] = "logout"
	status, env, _, _ = runReq(blStore, "Bearer "+validTok)
	if status != http.StatusUnauthorized || env.Code != errcode.ErrAccessTokenBlacklisted.Code {
		t.Fatalf("blacklisted token expected 401/11002, got %d/%d", status, env.Code)
	}

	// 5. Revoked session -> 11005 (403)
	revokedAt := now.Add(-time.Minute)
	revStore := newStore()
	s := revStore.sessions[501]
	s.RevokedAt = &revokedAt
	revStore.sessions[501] = s
	status, env, _, _ = runReq(revStore, "Bearer "+validTok)
	if status != http.StatusForbidden || env.Code != errcode.ErrSessionRevoked.Code {
		t.Fatalf("revoked session expected 403/11005, got %d/%d", status, env.Code)
	}

	// 6. Superseded access_jti -> 11002 (401)
	rotStore := newStore()
	s = rotStore.sessions[501]
	s.AccessJTI = "newer-jti"
	rotStore.sessions[501] = s
	status, env, _, _ = runReq(rotStore, "Bearer "+validTok)
	if status != http.StatusUnauthorized || env.Code != errcode.ErrAccessTokenBlacklisted.Code {
		t.Fatalf("superseded jti expected 401/11002, got %d/%d", status, env.Code)
	}

	// 7. Redis failure fails closed -> 19002 (503)
	redisDownStore := newStore()
	redisDownStore.blacklistErr = errors.New("redis connection refused")
	status, env, _, _ = runReq(redisDownStore, "Bearer "+validTok)
	if status != http.StatusServiceUnavailable || env.Code != errcode.ErrRedisUnavailable.Code {
		t.Fatalf("redis failure expected 503/19002, got %d/%d", status, env.Code)
	}
}
