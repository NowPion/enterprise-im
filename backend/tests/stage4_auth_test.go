package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/api"
	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type apiEnvelope[T any] struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      T      `json:"data"`
	RequestID string `json:"request_id"`
}

func TestStage4AuthRedisUnavailableFailsClosedWithoutMocking(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtCfg := config.JWTConfig{
		Secret:     "stage4-fail-closed-test-secret-32b!",
		Issuer:     "enterprise-im-test",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}
	mgr, err := pkgjwt.NewManager(jwtCfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	deadRedis := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 150 * time.Millisecond,
	})
	defer func() { _ = deadRedis.Close() }()

	sessRepo := repository.NewSessionRepository(nil, deadRedis)
	store := liveAuthStoreAdapter{sessionRepo: sessRepo}

	tok, _, _, err := mgr.IssueAccessToken(10001, 20001, model.UserRoleUser)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	r := gin.New()
	r.Use(middleware.RequestID())
	r.GET("/api/v1/auth/check", middleware.JWTAuth(mgr, store), func(c *gin.Context) {
		errcode.OK(c, nil)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/auth/check", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when Redis unreachable, got %d body=%s", w.Code, w.Body.String())
	}
	var env errcode.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Code != errcode.ErrRedisUnavailable.Code {
		t.Fatalf("expected 19002 when Redis unreachable, got %d", env.Code)
	}
}

type liveAuthStoreAdapter struct {
	sessionRepo *repository.SessionRepository
}

func (a liveAuthStoreAdapter) GetUserAuthStatus(context.Context, uint64) (int8, string, error) {
	return model.UserStatusActive, model.UserRoleUser, nil
}

func (a liveAuthStoreAdapter) IsAccessJTIBlacklisted(ctx context.Context, jti string) (bool, string, error) {
	return a.sessionRepo.IsAccessJTIBlacklisted(ctx, jti)
}

func (a liveAuthStoreAdapter) GetSessionAuthState(context.Context, uint64) (repository.SessionAuthState, error) {
	return repository.SessionAuthState{}, repository.ErrSessionNotFound
}

func TestStage4AuthLiveMySQLAndRedisIntegration(t *testing.T) {
	mysqlPass := firstNonEmptyEnv("MYSQL_PASSWORD", "IM_MYSQL_PASSWORD")
	redisPass := firstNonEmptyEnv("REDIS_PASSWORD", "IM_REDIS_PASSWORD")
	if mysqlPass == "" && redisPass == "" && os.Getenv("IM_RUN_LIVE_TESTS") == "" {
		t.Skip("skipping live Stage 4 auth integration test: MYSQL_PASSWORD/REDIS_PASSWORD not set")
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

	db, err := repository.NewMySQL(cfg.MySQL, log)
	if err != nil {
		t.Fatalf("NewMySQL failed: %v", err)
	}
	defer func() { _ = repository.CloseMySQL(db) }()

	rdb, err := repository.NewRedis(cfg.Redis)
	if err != nil {
		t.Fatalf("NewRedis failed: %v", err)
	}
	defer func() { _ = repository.CloseRedis(rdb) }()

	jwtCfg := config.JWTConfig{
		Secret:     "stage4-live-integration-secret-32b!",
		Issuer:     "enterprise-im-stage4-test",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}
	authCfg := &config.AuthConfig{
		JWT:                jwtCfg,
		MaxSessionsPerUser: 2,
		BcryptCost:         10,
	}
	jwtMgr, err := pkgjwt.NewManager(jwtCfg)
	if err != nil {
		t.Fatalf("pkgjwt.NewManager: %v", err)
	}

	authRepo := repository.NewAuthRepository(db)
	sessionRepo := repository.NewSessionRepository(db, rdb)

	var hookMu sync.Mutex
	type hookRecord struct {
		userID     uint64
		sessionIDs []uint64
		reason     string
	}
	var hookCalls []hookRecord
	revokeHook := func(_ context.Context, userID uint64, sessionIDs []uint64, reason string) error {
		hookMu.Lock()
		defer hookMu.Unlock()
		cp := make([]uint64, len(sessionIDs))
		copy(cp, sessionIDs)
		hookCalls = append(hookCalls, hookRecord{userID: userID, sessionIDs: cp, reason: reason})
		return nil
	}

	authSvc := service.NewAuthServiceWithRepos(authRepo, sessionRepo, jwtMgr, authCfg, log, revokeHook)
	authCtrl := controller.NewAuthController(authSvc)
	jwtMW := middleware.JWTAuth(jwtMgr, middleware.NewAuthStateStore(authRepo, sessionRepo))

	router := api.NewRouter(cfg, log, nil)
	api.RegisterAuthRoutes(router, authCtrl, jwtMW)

	// Extra protected route on test router only to verify middleware.JWTAuth + AuthContext propagation.
	router.GET("/api/v1/test/whoami", jwtMW, func(c *gin.Context) {
		authCtx, ok := middleware.GetAuthContext(c)
		if !ok {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			return
		}
		errcode.OK(c, map[string]any{
			"user_id":    strconv.FormatUint(authCtx.UserID, 10),
			"session_id": strconv.FormatUint(authCtx.SessionID, 10),
			"device_id":  authCtx.DeviceID,
			"role":       authCtx.Role,
		})
	})

	var createdUserIDs []uint64
	var cleanupSessionIDs []uint64
	var cleanupJTIs []string
	var cleanupMu sync.Mutex
	trackUser := func(idStr string) {
		if id, err := strconv.ParseUint(idStr, 10, 64); err == nil && id > 0 {
			cleanupMu.Lock()
			createdUserIDs = append(createdUserIDs, id)
			cleanupMu.Unlock()
		}
	}
	trackSession := func(idStr string) {
		if id, err := strconv.ParseUint(idStr, 10, 64); err == nil && id > 0 {
			cleanupMu.Lock()
			cleanupSessionIDs = append(cleanupSessionIDs, id)
			cleanupMu.Unlock()
		}
	}
	trackTokenJTI := func(accessToken string) {
		if claims, err := jwtMgr.ParseAccessToken(accessToken); err == nil && claims.ID != "" {
			cleanupMu.Lock()
			cleanupJTIs = append(cleanupJTIs, claims.ID)
			cleanupMu.Unlock()
		}
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		cleanupMu.Lock()
		uIDs := append([]uint64(nil), createdUserIDs...)
		sIDs := append([]uint64(nil), cleanupSessionIDs...)
		jtis := append([]string(nil), cleanupJTIs...)
		cleanupMu.Unlock()

		if len(uIDs) > 0 {
			var sessJTIs []string
			_ = db.WithContext(ctx).Model(&model.UserSession{}).Where("user_id IN ?", uIDs).Pluck("access_jti", &sessJTIs).Error
			jtis = append(jtis, sessJTIs...)
			_ = db.WithContext(ctx).Where("user_id IN ?", uIDs).Delete(&model.UserSession{}).Error
			_ = db.WithContext(ctx).Where("id IN ?", uIDs).Delete(&model.User{}).Error
		}
		if len(sIDs) > 0 {
			_ = db.WithContext(ctx).Where("id IN ?", sIDs).Delete(&model.UserSession{}).Error
		}
		if len(jtis) > 0 {
			keys := make([]string, 0, len(jtis))
			for _, jti := range jtis {
				keys = append(keys, repository.TokenBlacklistKey(jti))
			}
			_ = rdb.Del(ctx, keys...).Err()
		}
	})

	prefix := fmt.Sprintf("s4auth_%d", time.Now().UnixNano())
	aliceUser := prefix + "_alice"
	alicePass := "AlicePass!2026"
	aliceNewPass := "AliceNewPass!2026"

	// 1. Register user (201, role=user, no sessions created)
	w, regEnv := doJSON[apiEnvelope[service.RegisterOutput]](t, router, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"username": aliceUser,
		"password": alicePass,
		"nickname": "Stage4 Alice",
	})
	if w.Code != http.StatusCreated || regEnv.Code != 0 {
		t.Fatalf("register failed: http=%d env=%+v", w.Code, regEnv)
	}
	trackUser(regEnv.Data.User.ID)
	if regEnv.Data.User.Role != model.UserRoleUser || regEnv.Data.User.Status != 1 || regEnv.Data.User.Online {
		t.Fatalf("unexpected registered user: %+v", regEnv.Data.User)
	}
	if regEnv.RequestID == "" || w.Header().Get(errcode.HeaderRequestID) != regEnv.RequestID {
		t.Fatalf("request_id mismatch: %q vs %q", regEnv.RequestID, w.Header().Get(errcode.HeaderRequestID))
	}

	var sessionCount int64
	aliceID, _ := strconv.ParseUint(regEnv.Data.User.ID, 10, 64)
	_ = db.Model(&model.UserSession{}).Where("user_id = ?", aliceID).Count(&sessionCount).Error
	if sessionCount != 0 {
		t.Fatalf("register must not create user_sessions, got %d", sessionCount)
	}

	// Duplicate register -> 409 + 11007
	wDup, dupEnv := doJSON[apiEnvelope[any]](t, router, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"username": aliceUser,
		"password": alicePass,
		"nickname": "Dup Alice",
	})
	if wDup.Code != http.StatusConflict || dupEnv.Code != errcode.ErrUsernameExists.Code {
		t.Fatalf("duplicate register expected 409/11007, got %d/%d", wDup.Code, dupEnv.Code)
	}

	// 2. Multi-device login (web + desktop), same-device replacement, and max session limit (11009)
	wL1, l1Env := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    alicePass,
		"device_id":   "web-1",
		"device_type": "web",
	})
	if wL1.Code != http.StatusOK || l1Env.Code != 0 {
		t.Fatalf("login web-1 failed: %d %+v", wL1.Code, l1Env)
	}
	trackTokenJTI(l1Env.Data.Tokens.AccessToken)

	// Verify refresh_token is stored only as SHA-256 hex digest in MySQL.
	var storedSess model.UserSession
	s1ID, _ := strconv.ParseUint(l1Env.Data.Tokens.SessionID, 10, 64)
	if err := db.Where("id = ?", s1ID).First(&storedSess).Error; err != nil {
		t.Fatalf("query stored session: %v", err)
	}
	if storedSess.RefreshTokenHash != pkgjwt.HashRefreshToken(l1Env.Data.Tokens.RefreshToken) || storedSess.RefreshTokenHash == l1Env.Data.Tokens.RefreshToken {
		t.Fatalf("refresh_token_hash mismatch or plaintext stored")
	}

	wL2, l2Env := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    alicePass,
		"device_id":   "desktop-1",
		"device_type": "desktop",
	})
	if wL2.Code != http.StatusOK || l2Env.Code != 0 {
		t.Fatalf("login desktop-1 failed: %d %+v", wL2.Code, l2Env)
	}
	trackTokenJTI(l2Env.Data.Tokens.AccessToken)

	// 3rd distinct device exceeds MaxSessionsPerUser=2 -> 409 + 11009
	wLimit, limitEnv := doJSON[apiEnvelope[any]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    alicePass,
		"device_id":   "mobile-1",
		"device_type": "mobile",
	})
	if wLimit.Code != http.StatusConflict || limitEnv.Code != errcode.ErrSessionLimitReached.Code {
		t.Fatalf("expected 409/11009 for session limit, got %d/%d", wLimit.Code, limitEnv.Code)
	}

	// Re-login on web-1 replaces old web-1 session, blacklists old JTI in Redis, keeps desktop-1 active.
	wL1b, l1bEnv := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    alicePass,
		"device_id":   "web-1",
		"device_type": "web",
	})
	if wL1b.Code != http.StatusOK || l1bEnv.Code != 0 {
		t.Fatalf("re-login web-1 failed: %d %+v", wL1b.Code, l1bEnv)
	}
	trackTokenJTI(l1bEnv.Data.Tokens.AccessToken)

	// Old web-1 access token is blacklisted in Redis and rejected with 11002.
	wOldAT, oldATEnv := doJSON[apiEnvelope[any]](t, router, http.MethodGet, "/api/v1/test/whoami", l1Env.Data.Tokens.AccessToken, nil)
	if wOldAT.Code != http.StatusUnauthorized || oldATEnv.Code != errcode.ErrAccessTokenBlacklisted.Code {
		t.Fatalf("replaced access token expected 401/11002, got %d/%d", wOldAT.Code, oldATEnv.Code)
	}
	// Old web-1 refresh token is rejected with 11005.
	wOldRT, oldRTEnv := doJSON[apiEnvelope[any]](t, router, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": l1Env.Data.Tokens.RefreshToken,
		"device_id":     "web-1",
	})
	if wOldRT.Code != http.StatusForbidden || oldRTEnv.Code != errcode.ErrSessionRevoked.Code {
		t.Fatalf("replaced refresh token expected 403/11005, got %d/%d", wOldRT.Code, oldRTEnv.Code)
	}

	// 3. Refresh rotation on desktop-1 -> sequential reuse -> 11003
	wRef, refEnv := doJSON[apiEnvelope[service.RefreshOutput]](t, router, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": l2Env.Data.Tokens.RefreshToken,
		"device_id":     "desktop-1",
	})
	if wRef.Code != http.StatusOK || refEnv.Code != 0 {
		t.Fatalf("refresh desktop-1 failed: %d %+v", wRef.Code, refEnv)
	}
	trackTokenJTI(refEnv.Data.Tokens.AccessToken)

	wReuse, reuseEnv := doJSON[apiEnvelope[any]](t, router, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": l2Env.Data.Tokens.RefreshToken,
		"device_id":     "desktop-1",
	})
	if wReuse.Code != http.StatusUnauthorized || reuseEnv.Code != errcode.ErrRefreshTokenInvalid.Code {
		t.Fatalf("reused refresh token expected 401/11003, got %d/%d", wReuse.Code, reuseEnv.Code)
	}

	// 4. Logout current on web-1 -> blacklists web-1, invokes WS close hook, desktop-1 remains valid
	wLogWeb, logWebEnv := doJSON[apiEnvelope[service.LogoutOutput]](t, router, http.MethodPost, "/api/v1/auth/logout", l1bEnv.Data.Tokens.AccessToken, nil)
	if wLogWeb.Code != http.StatusOK || logWebEnv.Code != 0 || logWebEnv.Data.Scope != "current" || logWebEnv.Data.RevokedCount != 1 {
		t.Fatalf("logout current unexpected: %d %+v", wLogWeb.Code, logWebEnv)
	}
	wWhoDesk, whoDeskEnv := doJSON[apiEnvelope[map[string]any]](t, router, http.MethodGet, "/api/v1/test/whoami", refEnv.Data.Tokens.AccessToken, nil)
	if wWhoDesk.Code != http.StatusOK || whoDeskEnv.Code != 0 {
		t.Fatalf("desktop-1 should still be valid after web-1 logout: %d %+v", wWhoDesk.Code, whoDeskEnv)
	}

	// 5. Concurrent refresh race against live MySQL + Redis -> 1 wins, others get 11008
	const raceWorkers = 6
	startCh := make(chan struct{})
	var raceWG sync.WaitGroup
	var raceWin atomic.Int32
	var raceConflict atomic.Int32
	var raceUnexpected atomic.Int32
	var rotatedRT string
	var rotatedMu sync.Mutex

	raceWG.Add(raceWorkers)
	for i := 0; i < raceWorkers; i++ {
		go func() {
			defer raceWG.Done()
			<-startCh
			wRace, raceEnv := doJSON[apiEnvelope[service.RefreshOutput]](t, router, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
				"refresh_token": refEnv.Data.Tokens.RefreshToken,
				"device_id":     "desktop-1",
			})
			switch {
			case wRace.Code == http.StatusOK && raceEnv.Code == 0:
				raceWin.Add(1)
				rotatedMu.Lock()
				rotatedRT = raceEnv.Data.Tokens.RefreshToken
				rotatedMu.Unlock()
				trackTokenJTI(raceEnv.Data.Tokens.AccessToken)
			case wRace.Code == http.StatusConflict && raceEnv.Code == errcode.ErrRefreshConflict.Code:
				raceConflict.Add(1)
			default:
				raceUnexpected.Add(1)
				t.Errorf("unexpected concurrent refresh result: http=%d code=%d msg=%s", wRace.Code, raceEnv.Code, raceEnv.Message)
			}
		}()
	}
	close(startCh)
	raceWG.Wait()
	if raceWin.Load() != 1 || raceConflict.Load() < 1 || raceWin.Load()+raceConflict.Load() != raceWorkers || raceUnexpected.Load() != 0 {
		t.Fatalf("concurrent refresh unexpected counts: win=%d conflict=%d unexpected=%d", raceWin.Load(), raceConflict.Load(), raceUnexpected.Load())
	}

	// 6. ChangePassword revokes all active devices atomically
	// First log in web-1 again so user has 2 active sessions (web-1 + desktop-1).
	wL1c, l1cEnv := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    alicePass,
		"device_id":   "web-1",
		"device_type": "web",
	})
	if wL1c.Code != http.StatusOK {
		t.Fatalf("re-login web-1 before password change failed: %d", wL1c.Code)
	}
	trackTokenJTI(l1cEnv.Data.Tokens.AccessToken)

	wCP, cpEnv := doJSON[apiEnvelope[service.ChangePasswordOutput]](t, router, http.MethodPut, "/api/v1/me/password", l1cEnv.Data.Tokens.AccessToken, map[string]string{
		"old_password": alicePass,
		"new_password": aliceNewPass,
	})
	if wCP.Code != http.StatusOK || cpEnv.Code != 0 || cpEnv.Data.RevokedCount != 2 {
		t.Fatalf("change password unexpected: http=%d env=%+v", wCP.Code, cpEnv)
	}
	// Old desktop-1 refresh token is revoked (11005).
	wAfterCP, afterCPEnv := doJSON[apiEnvelope[any]](t, router, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": rotatedRT,
		"device_id":     "desktop-1",
	})
	if wAfterCP.Code != http.StatusForbidden || afterCPEnv.Code != errcode.ErrSessionRevoked.Code {
		t.Fatalf("expected 403/11005 after password change, got %d/%d", wAfterCP.Code, afterCPEnv.Code)
	}

	// 6b. Logout scope=all revokes all active devices at once.
	wLA1, la1Env := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    aliceNewPass,
		"device_id":   "web-all-1",
		"device_type": "web",
	})
	if wLA1.Code != http.StatusOK {
		t.Fatalf("login web-all-1 failed: %d %+v", wLA1.Code, la1Env)
	}
	trackTokenJTI(la1Env.Data.Tokens.AccessToken)
	wLA2, la2Env := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    aliceNewPass,
		"device_id":   "mob-all-2",
		"device_type": "mobile",
	})
	if wLA2.Code != http.StatusOK {
		t.Fatalf("login mob-all-2 failed: %d %+v", wLA2.Code, la2Env)
	}
	trackTokenJTI(la2Env.Data.Tokens.AccessToken)

	wLogAll, logAllEnv := doJSON[apiEnvelope[service.LogoutOutput]](t, router, http.MethodPost, "/api/v1/auth/logout", la1Env.Data.Tokens.AccessToken, map[string]string{
		"scope": "all",
	})
	if wLogAll.Code != http.StatusOK || logAllEnv.Code != 0 || logAllEnv.Data.Scope != "all" || logAllEnv.Data.RevokedCount != 2 {
		t.Fatalf("logout all unexpected: http=%d env=%+v", wLogAll.Code, logAllEnv)
	}
	wMobCheck, mobCheckEnv := doJSON[apiEnvelope[any]](t, router, http.MethodGet, "/api/v1/test/whoami", la2Env.Data.Tokens.AccessToken, nil)
	if wMobCheck.Code != http.StatusUnauthorized || mobCheckEnv.Code != errcode.ErrAccessTokenBlacklisted.Code {
		t.Fatalf("expected mob-all-2 access token blacklisted (11002) after logout all, got %d/%d", wMobCheck.Code, mobCheckEnv.Code)
	}

	// 6c. Expired refresh token returns 401/11003.
	wExpL, expLEnv := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    aliceNewPass,
		"device_id":   "web-exp-1",
		"device_type": "web",
	})
	if wExpL.Code != http.StatusOK {
		t.Fatalf("login web-exp-1 failed: %d", wExpL.Code)
	}
	trackTokenJTI(expLEnv.Data.Tokens.AccessToken)
	expSID, _ := strconv.ParseUint(expLEnv.Data.Tokens.SessionID, 10, 64)
	if err := db.Model(&model.UserSession{}).Where("id = ?", expSID).Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("expire session in DB: %v", err)
	}
	wExpRef, expRefEnv := doJSON[apiEnvelope[any]](t, router, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": expLEnv.Data.Tokens.RefreshToken,
		"device_id":     "web-exp-1",
	})
	if wExpRef.Code != http.StatusUnauthorized || expRefEnv.Code != errcode.ErrRefreshTokenInvalid.Code {
		t.Fatalf("expired refresh token expected 401/11003, got %d/%d", wExpRef.Code, expRefEnv.Code)
	}

	// Re-login with new password succeeds.
	wNewLogin, newLoginEnv := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    aliceNewPass,
		"device_id":   "web-2",
		"device_type": "web",
	})
	if wNewLogin.Code != http.StatusOK || newLoginEnv.Code != 0 {
		t.Fatalf("login with new password failed: %d %+v", wNewLogin.Code, newLoginEnv)
	}
	trackTokenJTI(newLoginEnv.Data.Tokens.AccessToken)

	// 7. Disabled account (status=0) cannot login and existing access/refresh tokens return 11004.
	if err := db.Model(&model.User{}).Where("id = ?", aliceID).Update("status", model.UserStatusDisabled).Error; err != nil {
		t.Fatalf("disable user in DB: %v", err)
	}
	wDisabledAT, disabledATEnv := doJSON[apiEnvelope[any]](t, router, http.MethodGet, "/api/v1/test/whoami", newLoginEnv.Data.Tokens.AccessToken, nil)
	if wDisabledAT.Code != http.StatusForbidden || disabledATEnv.Code != errcode.ErrAccountDisabled.Code {
		t.Fatalf("disabled user existing access token expected 403/11004, got %d/%d", wDisabledAT.Code, disabledATEnv.Code)
	}
	wDisabledRT, disabledRTEnv := doJSON[apiEnvelope[any]](t, router, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": newLoginEnv.Data.Tokens.RefreshToken,
		"device_id":     "web-2",
	})
	if wDisabledRT.Code != http.StatusForbidden || disabledRTEnv.Code != errcode.ErrAccountDisabled.Code {
		t.Fatalf("disabled user refresh token expected 403/11004, got %d/%d", wDisabledRT.Code, disabledRTEnv.Code)
	}
	wDisabledLogin, disabledLoginEnv := doJSON[apiEnvelope[any]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    aliceUser,
		"password":    aliceNewPass,
		"device_id":   "web-3",
		"device_type": "web",
	})
	if wDisabledLogin.Code != http.StatusForbidden || disabledLoginEnv.Code != errcode.ErrAccountDisabled.Code {
		t.Fatalf("disabled user login expected 403/11004, got %d/%d", wDisabledLogin.Code, disabledLoginEnv.Code)
	}

	// 8. Chunked oversized JSON body hits *http.MaxBytesError -> 413 + 10002
	oversized := `{"username":"` + strings.Repeat("x", int(cfg.Server.MaxBodyBytes)+64) + `","password":"Password!1","nickname":"Big"}`
	wBig := httptest.NewRecorder()
	reqBig, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(oversized))
	reqBig.ContentLength = -1
	reqBig.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(wBig, reqBig)
	if wBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for chunked oversized JSON, got %d body=%s", wBig.Code, wBig.Body.String())
	}
	var bigEnv apiEnvelope[any]
	_ = json.Unmarshal(wBig.Body.Bytes(), &bigEnv)
	if bigEnv.Code != errcode.ErrPayloadTooLarge.Code {
		t.Fatalf("expected 10002, got %d", bigEnv.Code)
	}

	// 9. Optional seeded admin login verification when IM_TEST_ADMIN_PASSWORD is set via environment.
	if adminPass := os.Getenv("IM_TEST_ADMIN_PASSWORD"); adminPass != "" {
		wAdmin, adminEnv := doJSON[apiEnvelope[service.LoginOutput]](t, router, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
			"username":    "admin",
			"password":    adminPass,
			"device_id":   "stage4-admin-check",
			"device_type": "desktop",
		})
		if wAdmin.Code != http.StatusOK || adminEnv.Code != 0 {
			t.Fatalf("seeded admin login failed: %d %+v", wAdmin.Code, adminEnv)
		}
		if adminEnv.Data.User.Role != model.UserRoleAdmin || adminEnv.Data.User.Status != model.UserStatusActive {
			t.Fatalf("unexpected seeded admin metadata: %+v", adminEnv.Data.User)
		}
		trackSession(adminEnv.Data.Tokens.SessionID)
		trackTokenJTI(adminEnv.Data.Tokens.AccessToken)

		wAdminWho, adminWhoEnv := doJSON[apiEnvelope[map[string]any]](t, router, http.MethodGet, "/api/v1/test/whoami", adminEnv.Data.Tokens.AccessToken, nil)
		if wAdminWho.Code != http.StatusOK || adminWhoEnv.Data["role"] != model.UserRoleAdmin {
			t.Fatalf("seeded admin JWTAuth failed: %d %+v", wAdminWho.Code, adminWhoEnv)
		}
	}

	// Verify hook fired for session_replaced, logout, and password_changed.
	hookMu.Lock()
	hookCount := len(hookCalls)
	hookMu.Unlock()
	if hookCount < 3 {
		t.Fatalf("expected at least 3 revokeHook calls, got %d", hookCount)
	}

	// Verify logs contain zero passwords or tokens.
	rawLogs := logBuf.String()
	for _, secret := range []string{
		alicePass,
		aliceNewPass,
		l1Env.Data.Tokens.AccessToken,
		l1Env.Data.Tokens.RefreshToken,
		l2Env.Data.Tokens.AccessToken,
		l2Env.Data.Tokens.RefreshToken,
		newLoginEnv.Data.Tokens.AccessToken,
	} {
		if secret != "" && strings.Contains(rawLogs, secret) {
			t.Fatalf("sensitive credential leaked in structured logs")
		}
	}
}

func doJSON[T any](t *testing.T, h http.Handler, method, path, bearer string, body any) (*httptest.ResponseRecorder, T) {
	t.Helper()
	var reqBody *bytes.Reader
	if body == nil {
		reqBody = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, path, reqBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var out T
	if len(w.Body.Bytes()) > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal response (%s %s status=%d body=%s): %v", method, path, w.Code, w.Body.String(), err)
		}
	}
	return w, out
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
