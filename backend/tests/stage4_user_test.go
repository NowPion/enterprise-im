package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/api"
	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type stage4Envelope[T any] struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      T      `json:"data"`
	RequestID string `json:"request_id"`
}

// stubJWTAuth simulates the Stage 4 JWT middleware for isolated branch tests by reading
// caller-injected test identity headers and populating gin.Context with user_id/session_id/device_id.
func stubJWTAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
		if !strings.HasPrefix(authHeader, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer ")) == "" {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		uid := strings.TrimSpace(c.GetHeader("X-Test-User-ID"))
		if uid == "" {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		errcode.SetUserID(c, uid)
		c.Set(controller.ContextKeySessionID, strings.TrimSpace(c.GetHeader("X-Test-Session-ID")))
		c.Set(controller.ContextKeyDeviceID, strings.TrimSpace(c.GetHeader("X-Test-Device-ID")))
		c.Next()
	}
}

type memUserStore struct {
	users map[uint64]model.User
}

func (m *memUserStore) GetByID(_ context.Context, userID uint64) (*model.User, error) {
	u, ok := m.users[userID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := u
	return &cp, nil
}

func (m *memUserStore) UpdateProfile(_ context.Context, userID uint64, updates map[string]any) (*model.User, error) {
	u, ok := m.users[userID]
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
	m.users[userID] = u
	cp := u
	return &cp, nil
}

func (m *memUserStore) Search(_ context.Context, params repository.UserSearchParams) ([]model.User, error) {
	var out []model.User
	for _, u := range m.users {
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

type memPresenceStore struct {
	online map[uint64]bool
}

func (p *memPresenceStore) IsUserOnline(_ context.Context, userID uint64) (bool, error) {
	return p.online[userID], nil
}

func (p *memPresenceStore) GetUsersOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		out[id] = p.online[id]
	}
	return out, nil
}

func TestStage4UserRoutesAndEnvelopeContract(t *testing.T) {
	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	store := &memUserStore{
		users: map[uint64]model.User{
			20001: {ID: 20001, Username: "alice", Nickname: "Alice", Signature: "initial", Role: model.UserRoleAdmin, Status: model.UserStatusActive},
			20002: {ID: 20002, Username: "bob_1", Nickname: "Bob_One", Role: model.UserRoleUser, Status: model.UserStatusActive},
			20003: {ID: 20003, Username: "bob_2", Nickname: "Bob_Two", Role: model.UserRoleUser, Status: model.UserStatusActive},
		},
	}
	presence := &memPresenceStore{online: map[uint64]bool{20001: true, 20003: true}}

	userSvc := service.NewUserService(store, presence, log)
	userCtrl := controller.NewUserController(userSvc)

	router := api.NewRouter(cfg, log, nil)
	api.RegisterUserRoutes(router, stubJWTAuth(), userCtrl)

	// 1. Missing Authorization rejected with 401 + 11001 + X-Request-ID
	w401 := httptest.NewRecorder()
	req401, _ := http.NewRequest(http.MethodGet, "/api/v1/me", nil)
	router.ServeHTTP(w401, req401)
	if w401.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without Bearer token, got %d", w401.Code)
	}
	var env401 stage4Envelope[any]
	if err := json.Unmarshal(w401.Body.Bytes(), &env401); err != nil {
		t.Fatalf("unmarshal 401: %v", err)
	}
	if env401.Code != errcode.ErrAccessTokenInvalid.Code || env401.RequestID == "" || w401.Header().Get(errcode.HeaderRequestID) != env401.RequestID {
		t.Fatalf("unexpected 401 envelope: %+v", env401)
	}

	// 2. Method not allowed and out-of-scope routes stay unhandled by this branch
	w405 := httptest.NewRecorder()
	req405, _ := http.NewRequest(http.MethodDelete, "/api/v1/me", nil)
	router.ServeHTTP(w405, req405)
	if w405.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on DELETE /api/v1/me, got %d", w405.Code)
	}
	w404 := httptest.NewRecorder()
	req404, _ := http.NewRequest(http.MethodPost, "/api/v1/me/avatar", nil)
	router.ServeHTTP(w404, req404)
	if w404.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on unregistered /api/v1/me/avatar, got %d", w404.Code)
	}

	// 3. GET /api/v1/me
	wMe := httptest.NewRecorder()
	reqMe, _ := http.NewRequest(http.MethodGet, "/api/v1/me", nil)
	reqMe.Header.Set("Authorization", "Bearer test-secret-token")
	reqMe.Header.Set("X-Test-User-ID", "20001")
	reqMe.Header.Set("X-Test-Session-ID", "88001")
	reqMe.Header.Set("X-Test-Device-ID", "desktop-mac-1")
	router.ServeHTTP(wMe, reqMe)
	if wMe.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /api/v1/me, got %d body=%s", wMe.Code, wMe.Body.String())
	}
	var envMe stage4Envelope[service.GetMeResult]
	if err := json.Unmarshal(wMe.Body.Bytes(), &envMe); err != nil {
		t.Fatalf("unmarshal GET /api/v1/me: %v", err)
	}
	if envMe.Code != 0 || envMe.Data.User.ID != "20001" || envMe.Data.User.Role != "admin" || envMe.Data.User.Status != 1 || envMe.Data.SessionID != "88001" || envMe.Data.DeviceID != "desktop-mac-1" {
		t.Fatalf("unexpected GET /api/v1/me data: %+v", envMe)
	}
	if strings.Contains(logBuf.String(), "test-secret-token") {
		t.Fatalf("authorization token leaked in access logs: %s", logBuf.String())
	}

	// 4. PATCH /api/v1/me updates nickname and clears signature
	wPatch := httptest.NewRecorder()
	reqPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/me", strings.NewReader(`{"nickname":"Alice Pro","signature":""}`))
	reqPatch.Header.Set("Authorization", "Bearer test-secret-token")
	reqPatch.Header.Set("Content-Type", "application/json")
	reqPatch.Header.Set("X-Test-User-ID", "20001")
	router.ServeHTTP(wPatch, reqPatch)
	if wPatch.Code != http.StatusOK {
		t.Fatalf("expected 200 on PATCH /api/v1/me, got %d body=%s", wPatch.Code, wPatch.Body.String())
	}
	var envPatch stage4Envelope[service.UpdateMeResult]
	if err := json.Unmarshal(wPatch.Body.Bytes(), &envPatch); err != nil {
		t.Fatalf("unmarshal PATCH /api/v1/me: %v", err)
	}
	if envPatch.Data.User.Nickname != "Alice Pro" || envPatch.Data.User.Signature != "" || envPatch.Data.User.Role != "admin" {
		t.Fatalf("unexpected PATCH /api/v1/me result: %+v", envPatch)
	}

	// 5. GET /api/v1/users/search cursor pagination
	wSearch1 := httptest.NewRecorder()
	reqSearch1, _ := http.NewRequest(http.MethodGet, "/api/v1/users/search?q=bob_&field=username&match=prefix&limit=1", nil)
	reqSearch1.Header.Set("Authorization", "Bearer test-secret-token")
	reqSearch1.Header.Set("X-Test-User-ID", "20001")
	router.ServeHTTP(wSearch1, reqSearch1)
	if wSearch1.Code != http.StatusOK {
		t.Fatalf("expected 200 on search page 1, got %d body=%s", wSearch1.Code, wSearch1.Body.String())
	}
	var envS1 stage4Envelope[service.UserSearchPage]
	if err := json.Unmarshal(wSearch1.Body.Bytes(), &envS1); err != nil {
		t.Fatalf("unmarshal search page 1: %v", err)
	}
	if len(envS1.Data.Items) != 1 || !envS1.Data.HasMore || envS1.Data.NextCursor == nil || envS1.Data.Items[0].ID != "20003" || !envS1.Data.Items[0].Online {
		t.Fatalf("unexpected search page 1: %+v", envS1.Data)
	}

	wSearch2 := httptest.NewRecorder()
	reqSearch2, _ := http.NewRequest(http.MethodGet, "/api/v1/users/search?q=bob_&field=username&match=prefix&limit=1&cursor="+*envS1.Data.NextCursor, nil)
	reqSearch2.Header.Set("Authorization", "Bearer test-secret-token")
	reqSearch2.Header.Set("X-Test-User-ID", "20001")
	router.ServeHTTP(wSearch2, reqSearch2)
	if wSearch2.Code != http.StatusOK {
		t.Fatalf("expected 200 on search page 2, got %d body=%s", wSearch2.Code, wSearch2.Body.String())
	}
	var envS2 stage4Envelope[service.UserSearchPage]
	if err := json.Unmarshal(wSearch2.Body.Bytes(), &envS2); err != nil {
		t.Fatalf("unmarshal search page 2: %v", err)
	}
	if len(envS2.Data.Items) != 1 || envS2.Data.HasMore || envS2.Data.NextCursor != nil || envS2.Data.Items[0].ID != "20002" || envS2.Data.Items[0].Online {
		t.Fatalf("unexpected search page 2: %+v", envS2.Data)
	}

	// 6. Fail-closed route guard when jwtAuth is nil & nil userCtrl no-op
	cfgSmall := *cfg
	cfgSmall.Server.MaxBodyBytes = 64
	rSmall := api.NewRouter(&cfgSmall, log, nil)
	api.RegisterUserRoutes(rSmall, nil, nil)
	api.RegisterUserRoutes(rSmall, nil, userCtrl)

	wNoMiddleware := httptest.NewRecorder()
	reqNoMw, _ := http.NewRequest(http.MethodGet, "/api/v1/me", nil)
	rSmall.ServeHTTP(wNoMiddleware, reqNoMw)
	if wNoMiddleware.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 fail-closed when jwtAuth is nil, got %d", wNoMiddleware.Code)
	}

	// 7. Chunked oversized body triggers MaxBytesError -> 413 + 10002
	rBig := api.NewRouter(&cfgSmall, log, nil)
	api.RegisterUserRoutes(rBig, stubJWTAuth(), userCtrl)
	wBig := httptest.NewRecorder()
	reqBig, _ := http.NewRequest(http.MethodPatch, "/api/v1/me", strings.NewReader(`{"signature":"`+strings.Repeat("x", 128)+`"}`))
	reqBig.ContentLength = -1 // simulate chunked request without Content-Length header
	reqBig.Header.Set("Authorization", "Bearer test-secret-token")
	reqBig.Header.Set("Content-Type", "application/json")
	reqBig.Header.Set("X-Test-User-ID", "20001")
	rBig.ServeHTTP(wBig, reqBig)
	if wBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d body=%s", wBig.Code, wBig.Body.String())
	}
	var envBig stage4Envelope[any]
	if err := json.Unmarshal(wBig.Body.Bytes(), &envBig); err != nil {
		t.Fatalf("unmarshal 413: %v", err)
	}
	if envBig.Code != errcode.ErrPayloadTooLarge.Code {
		t.Fatalf("expected 10002 for oversized body, got %d", envBig.Code)
	}
}

func TestStage4UserLiveMySQLAndRedis(t *testing.T) {
	mysqlPass := os.Getenv("MYSQL_PASSWORD")
	if mysqlPass == "" {
		mysqlPass = os.Getenv("IM_MYSQL_PASSWORD")
	}
	redisPass := os.Getenv("REDIS_PASSWORD")
	if redisPass == "" {
		redisPass = os.Getenv("IM_REDIS_PASSWORD")
	}
	if mysqlPass == "" && redisPass == "" {
		t.Skip("skipping live stage4 user integration test: MYSQL_PASSWORD/REDIS_PASSWORD not set")
	}

	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if v := os.Getenv("MYSQL_HOST"); v != "" {
		cfg.MySQL.Host = v
	}
	if v := os.Getenv("MYSQL_PORT"); v != "" {
		if port, convErr := strconv.Atoi(v); convErr == nil {
			cfg.MySQL.Port = port
		}
	}
	if v := os.Getenv("MYSQL_USER"); v != "" {
		cfg.MySQL.User = v
	}
	if v := os.Getenv("MYSQL_DATABASE"); v != "" {
		cfg.MySQL.Database = v
	}
	cfg.MySQL.Password = mysqlPass
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	cfg.Redis.Password = redisPass

	log, err := logger.New(config.LogConfig{Level: "warn", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	db, err := repository.NewMySQL(cfg.MySQL, log)
	if err != nil {
		t.Fatalf("live NewMySQL failed: %v", err)
	}
	defer func() { _ = repository.CloseMySQL(db) }()

	rdb, err := repository.NewRedis(cfg.Redis)
	if err != nil {
		t.Fatalf("live NewRedis failed: %v", err)
	}
	defer func() { _ = repository.CloseRedis(rdb) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Wrap DB writes in a rolled-back transaction so live DB stays clean.
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatalf("begin tx: %v", tx.Error)
	}
	defer tx.Rollback()

	tag := fmt.Sprintf("s4e2e_%d", time.Now().UnixNano())
	me := model.User{
		Username:     tag + "_me",
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     tag + "_MeInitial",
		Signature:    "old signature",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	peer1 := model.User{
		Username:     tag + "_peer_1",
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     tag + "_Peer_50%",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	peer2 := model.User{
		Username:     tag + "_peerX1",
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     tag + "_Peer509",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	for _, u := range []*model.User{&me, &peer1, &peer2} {
		if err := tx.Create(u).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	kMe := repository.PresenceUserKey(me.ID)
	kPeer1 := repository.PresenceUserKey(peer1.ID)
	defer func() { _ = rdb.Del(context.Background(), kMe, kPeer1).Err() }()
	if err := rdb.Set(ctx, kMe, "1", 30*time.Second).Err(); err != nil {
		t.Fatalf("set presence me: %v", err)
	}
	if err := rdb.Set(ctx, kPeer1, "3", 30*time.Second).Err(); err != nil {
		t.Fatalf("set presence peer1: %v", err)
	}

	userRepo := repository.NewUserRepository(tx)
	presenceRepo := repository.NewUserPresenceRepository(rdb)
	userSvc := service.NewUserService(userRepo, presenceRepo, log)
	userCtrl := controller.NewUserController(userSvc)

	router := api.NewRouter(cfg, log, nil)
	api.RegisterUserRoutes(router, stubJWTAuth(), userCtrl)

	meIDStr := strconv.FormatUint(me.ID, 10)

	// 1. Live PATCH /api/v1/me updating nickname and clearing signature
	wPatch := httptest.NewRecorder()
	reqPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/me", strings.NewReader(`{"nickname":"`+tag+`_MeUpdated","signature":""}`))
	reqPatch.Header.Set("Authorization", "Bearer live-stub")
	reqPatch.Header.Set("Content-Type", "application/json")
	reqPatch.Header.Set("X-Test-User-ID", meIDStr)
	router.ServeHTTP(wPatch, reqPatch)
	if wPatch.Code != http.StatusOK {
		t.Fatalf("live PATCH /api/v1/me failed: %d body=%s", wPatch.Code, wPatch.Body.String())
	}

	// 2. Live GET /api/v1/me confirms persisted changes and online=true from Redis
	wGet := httptest.NewRecorder()
	reqGet, _ := http.NewRequest(http.MethodGet, "/api/v1/me", nil)
	reqGet.Header.Set("Authorization", "Bearer live-stub")
	reqGet.Header.Set("X-Test-User-ID", meIDStr)
	reqGet.Header.Set("X-Test-Session-ID", "77001")
	reqGet.Header.Set("X-Test-Device-ID", "live-web-1")
	router.ServeHTTP(wGet, reqGet)
	if wGet.Code != http.StatusOK {
		t.Fatalf("live GET /api/v1/me failed: %d body=%s", wGet.Code, wGet.Body.String())
	}
	var getEnv stage4Envelope[service.GetMeResult]
	if err := json.Unmarshal(wGet.Body.Bytes(), &getEnv); err != nil {
		t.Fatalf("unmarshal live GET /me: %v", err)
	}
	if getEnv.Data.User.Nickname != tag+"_MeUpdated" || getEnv.Data.User.Signature != "" || !getEnv.Data.User.Online {
		t.Fatalf("unexpected live GET /me data: %+v", getEnv.Data)
	}

	// 3. Live GET /api/v1/users/search literal prefix with '%' must match peer1 only (not peer2)
	wSearch := httptest.NewRecorder()
	reqSearch, _ := http.NewRequest(http.MethodGet, "/api/v1/users/search?q="+tag+"_Peer_50%25&field=nickname&match=prefix", nil)
	reqSearch.Header.Set("Authorization", "Bearer live-stub")
	reqSearch.Header.Set("X-Test-User-ID", meIDStr)
	router.ServeHTTP(wSearch, reqSearch)
	if wSearch.Code != http.StatusOK {
		t.Fatalf("live GET /api/v1/users/search failed: %d body=%s", wSearch.Code, wSearch.Body.String())
	}
	var searchEnv stage4Envelope[service.UserSearchPage]
	if err := json.Unmarshal(wSearch.Body.Bytes(), &searchEnv); err != nil {
		t.Fatalf("unmarshal live search: %v", err)
	}
	if len(searchEnv.Data.Items) != 1 || searchEnv.Data.Items[0].ID != strconv.FormatUint(peer1.ID, 10) || !searchEnv.Data.Items[0].Online {
		t.Fatalf("unexpected live search results: %+v", searchEnv.Data)
	}
}
