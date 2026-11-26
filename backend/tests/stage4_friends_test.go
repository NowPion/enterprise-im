package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/api"
	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type friendListEnvelope struct {
	Code      int                      `json:"code"`
	Message   string                   `json:"message"`
	Data      service.FriendListResult `json:"data"`
	RequestID string                   `json:"request_id"`
}

type remarkEnvelope struct {
	Code      int                              `json:"code"`
	Message   string                           `json:"message"`
	Data      service.UpdateFriendRemarkResult `json:"data"`
	RequestID string                           `json:"request_id"`
}

type deleteFriendEnvelope struct {
	Code      int                        `json:"code"`
	Message   string                     `json:"message"`
	Data      service.DeleteFriendResult `json:"data"`
	RequestID string                     `json:"request_id"`
}

// stage4MemStore implements service.FriendStore in memory for full HTTP contract tests
// when external MySQL/Redis credentials are not provided.
type stage4MemStore struct {
	mu           sync.Mutex
	nextFriendID uint64
	nextConvID   uint64
	users        map[uint64]model.User
	friendships  map[[2]uint64]model.Friendship
	convs        map[string]model.Conversation
	convMembers  map[[2]uint64]model.ConversationMember
	routeSets    map[uint64][]string
	connLeases   map[string]bool
	userPresence map[uint64]int

	batchUsersCalls int
	batchConvsCalls int
	batchRedisCalls int
}

func newStage4MemStore() *stage4MemStore {
	return &stage4MemStore{
		nextFriendID: 1,
		nextConvID:   90001,
		users:        make(map[uint64]model.User),
		friendships:  make(map[[2]uint64]model.Friendship),
		convs:        make(map[string]model.Conversation),
		convMembers:  make(map[[2]uint64]model.ConversationMember),
		routeSets:    make(map[uint64][]string),
		connLeases:   make(map[string]bool),
		userPresence: make(map[uint64]int),
	}
}

func (s *stage4MemStore) seedPair(uA, uB uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, p := range [][2]uint64{{uA, uB}, {uB, uA}} {
		if row, ok := s.friendships[p]; ok {
			row.Status = model.FriendshipStatusActive
			row.UpdatedAt = now
			s.friendships[p] = row
		} else {
			s.friendships[p] = model.Friendship{
				ID:        s.nextFriendID,
				UserID:    p[0],
				FriendID:  p[1],
				Status:    model.FriendshipStatusActive,
				CreatedAt: now,
				UpdatedAt: now,
			}
			s.nextFriendID++
		}
	}
	dk, _ := repository.FormatDirectKey(uA, uB)
	conv, ok := s.convs[dk]
	if !ok {
		dkCopy := dk
		conv = model.Conversation{
			ID:        s.nextConvID,
			Type:      model.ConversationTypeSingle,
			DirectKey: &dkCopy,
		}
		s.nextConvID++
		s.convs[dk] = conv
	}
	for _, uid := range []uint64{uA, uB} {
		key := [2]uint64{conv.ID, uid}
		cm := s.convMembers[key]
		cm.ConversationID = conv.ID
		cm.UserID = uid
		cm.IsVisible = true
		s.convMembers[key] = cm
	}
	return conv.ID
}

func (s *stage4MemStore) ListActiveFriendships(_ context.Context, userID uint64, cursorID uint64, limit int) ([]model.Friendship, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []model.Friendship
	for _, f := range s.friendships {
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

func (s *stage4MemStore) BatchGetUsersByIDs(_ context.Context, userIDs []uint64) (map[uint64]model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchUsersCalls++
	out := make(map[uint64]model.User, len(userIDs))
	for _, id := range userIDs {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *stage4MemStore) BatchGetSingleConversationsByDirectKeys(_ context.Context, directKeys []string) (map[string]model.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchConvsCalls++
	out := make(map[string]model.Conversation, len(directKeys))
	for _, dk := range directKeys {
		if c, ok := s.convs[dk]; ok {
			out[dk] = c
		}
	}
	return out, nil
}

func (s *stage4MemStore) GetOnlineStatusBatch(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchRedisCalls++
	out := make(map[uint64]bool, len(userIDs))
	for _, uid := range userIDs {
		members := s.routeSets[uid]
		if len(members) > 0 {
			online := false
			for _, m := range members {
				connID := m
				if idx := strings.IndexByte(m, ':'); idx >= 0 {
					connID = m[idx+1:]
				}
				if s.connLeases[connID] {
					online = true
					break
				}
			}
			out[uid] = online
			continue
		}
		out[uid] = s.userPresence[uid] > 0
	}
	return out, nil
}

func (s *stage4MemStore) AreActiveFriends(_ context.Context, a, b uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.friendships[[2]uint64{a, b}].Status == 1 && s.friendships[[2]uint64{b, a}].Status == 1, nil
}

func (s *stage4MemStore) UpdateRemark(_ context.Context, userID, friendID uint64, remark string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.friendships[[2]uint64{userID, friendID}]
	if !ok {
		return repository.ErrFriendshipRecordNotFound
	}
	if row.Status != model.FriendshipStatusActive {
		return repository.ErrFriendshipInactive
	}
	row.Remark = remark
	s.friendships[[2]uint64{userID, friendID}] = row
	return nil
}

func (s *stage4MemStore) DeactivateBidirectionalFriendship(_ context.Context, userID, friendID uint64) (repository.DeleteFriendshipOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.friendships[[2]uint64{userID, friendID}]
	if !ok {
		return repository.DeleteFriendshipOutcome{}, repository.ErrFriendshipRecordNotFound
	}
	if row.Status != model.FriendshipStatusActive {
		return repository.DeleteFriendshipOutcome{}, repository.ErrFriendshipInactive
	}
	for _, p := range [][2]uint64{{userID, friendID}, {friendID, userID}} {
		if f, exists := s.friendships[p]; exists {
			f.Status = model.FriendshipStatusDeleted
			s.friendships[p] = f
		}
	}
	dk, _ := repository.FormatDirectKey(userID, friendID)
	convID := s.convs[dk].ID
	for _, uid := range []uint64{userID, friendID} {
		cm := s.convMembers[[2]uint64{convID, uid}]
		cm.IsVisible = false
		s.convMembers[[2]uint64{convID, uid}] = cm
	}
	return repository.DeleteFriendshipOutcome{
		FriendID:       friendID,
		ConversationID: &convID,
		IsVisible:      false,
	}, nil
}

func (s *stage4MemStore) FindSingleConversationByDirectKey(_ context.Context, dk string) (*model.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[dk]
	if !ok {
		return nil, errors.New("not found")
	}
	return &c, nil
}

func stubJWTAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		const prefix = "Bearer test-token-user-"
		if !strings.HasPrefix(auth, prefix) {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		rawUID := strings.TrimSpace(strings.TrimPrefix(auth, prefix))
		if _, err := strconv.ParseUint(rawUID, 10, 64); err != nil || rawUID == "0" {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		errcode.SetUserID(c, rawUID)
		c.Next()
	}
}

func TestStage4FriendsHTTPContractAndAuthStub(t *testing.T) {
	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	store := newStage4MemStore()
	lastOnline := time.Date(2026, 11, 26, 4, 0, 0, 500000000, time.UTC)
	store.users[10001] = model.User{ID: 10001, Username: "alice", Nickname: "Alice"}
	store.users[10002] = model.User{ID: 10002, Username: "bob", Nickname: "Bob", LastOnlineAt: &lastOnline}
	store.users[10003] = model.User{ID: 10003, Username: "charlie", Nickname: "Charlie"}
	store.users[10004] = model.User{ID: 10004, Username: "dave", Nickname: "Dave"}

	conv12 := store.seedPair(10001, 10002)
	store.seedPair(10001, 10003)
	store.seedPair(10001, 10004)

	// 10002 has two devices: one active conn lease -> online=true.
	store.routeSets[10002] = []string{"node-1:conn-a", "node-2:conn-b"}
	store.connLeases["conn-b"] = true
	// 10003 has no WS connections -> online=false.

	r := api.NewRouter(cfg, log, nil)
	friendSvc := service.NewFriendService(store, log)
	api.RegisterFriendRoutes(r, stubJWTAuthMiddleware(), controller.NewFriendController(friendSvc))

	// Page 1 (limit=2)
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest(http.MethodGet, "/api/v1/friends?limit=2", nil)
	req1.Header.Set("Authorization", "Bearer test-token-user-10001")
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/friends status=%d body=%s", w1.Code, w1.Body.String())
	}
	var env1 friendListEnvelope
	if err := json.Unmarshal(w1.Body.Bytes(), &env1); err != nil {
		t.Fatalf("unmarshal list page1: %v", err)
	}
	if env1.Code != 0 || env1.RequestID == "" || w1.Header().Get(errcode.HeaderRequestID) != env1.RequestID {
		t.Fatalf("envelope/header mismatch: %+v header=%q", env1, w1.Header().Get(errcode.HeaderRequestID))
	}
	if !env1.Data.HasMore || env1.Data.NextCursor == nil || len(env1.Data.Items) != 2 {
		t.Fatalf("unexpected page1 data: %+v", env1.Data)
	}
	if store.batchUsersCalls != 1 || store.batchConvsCalls != 1 || store.batchRedisCalls != 1 {
		t.Fatalf("expected single batch calls per page, got users=%d convs=%d redis=%d",
			store.batchUsersCalls, store.batchConvsCalls, store.batchRedisCalls)
	}

	// Page 2 using next_cursor
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodGet, "/api/v1/friends?limit=2&cursor="+*env1.Data.NextCursor, nil)
	req2.Header.Set("Authorization", "Bearer test-token-user-10001")
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/friends page2 status=%d body=%s", w2.Code, w2.Body.String())
	}
	var env2 friendListEnvelope
	if err := json.Unmarshal(w2.Body.Bytes(), &env2); err != nil {
		t.Fatalf("unmarshal list page2: %v", err)
	}
	if env2.Data.HasMore || env2.Data.NextCursor != nil || len(env2.Data.Items) != 1 {
		t.Fatalf("unexpected page2 data: %+v", env2.Data)
	}
	if env2.Data.Items[0].FriendID != 10002 || !env2.Data.Items[0].Online || !env2.Data.Items[0].User.Online {
		t.Fatalf("expected 10002 online on page2, got %+v", env2.Data.Items[0])
	}

	// PATCH remark updates only caller side (10001 -> 10002)
	wPatch := httptest.NewRecorder()
	reqPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/friends/10002", strings.NewReader(`{"remark":"Project Lead"}`))
	reqPatch.Header.Set("Authorization", "Bearer test-token-user-10001")
	reqPatch.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wPatch, reqPatch)
	if wPatch.Code != http.StatusOK {
		t.Fatalf("PATCH remark failed: %d body=%s", wPatch.Code, wPatch.Body.String())
	}
	var patchEnv remarkEnvelope
	_ = json.Unmarshal(wPatch.Body.Bytes(), &patchEnv)
	if patchEnv.Data.FriendID != 10002 || patchEnv.Data.Remark != "Project Lead" {
		t.Fatalf("unexpected patch payload: %+v", patchEnv.Data)
	}

	// Check 10002's own list still has empty remark.
	wB := httptest.NewRecorder()
	reqB, _ := http.NewRequest(http.MethodGet, "/api/v1/friends", nil)
	reqB.Header.Set("Authorization", "Bearer test-token-user-10002")
	r.ServeHTTP(wB, reqB)
	var envB friendListEnvelope
	_ = json.Unmarshal(wB.Body.Bytes(), &envB)
	if len(envB.Data.Items) != 1 || envB.Data.Items[0].Remark != "" {
		t.Fatalf("expected 10002 remark untouched, got %+v", envB.Data)
	}

	// DELETE friend deactivates both sides and hides both conversations
	wDel := httptest.NewRecorder()
	reqDel, _ := http.NewRequest(http.MethodDelete, "/api/v1/friends/10002", nil)
	reqDel.Header.Set("Authorization", "Bearer test-token-user-10001")
	r.ServeHTTP(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("DELETE failed: %d body=%s", wDel.Code, wDel.Body.String())
	}
	var delEnv deleteFriendEnvelope
	_ = json.Unmarshal(wDel.Body.Bytes(), &delEnv)
	if delEnv.Data.FriendID != 10002 || delEnv.Data.ConversationID == nil ||
		*delEnv.Data.ConversationID != strconv.FormatUint(conv12, 10) || delEnv.Data.IsVisible {
		t.Fatalf("unexpected delete response: %+v", delEnv.Data)
	}

	// Verify 10002 no longer sees 10001 in friends list and both conversation_members are hidden.
	wBAfter := httptest.NewRecorder()
	reqBAfter, _ := http.NewRequest(http.MethodGet, "/api/v1/friends", nil)
	reqBAfter.Header.Set("Authorization", "Bearer test-token-user-10002")
	r.ServeHTTP(wBAfter, reqBAfter)
	var envBAfter friendListEnvelope
	_ = json.Unmarshal(wBAfter.Body.Bytes(), &envBAfter)
	if len(envBAfter.Data.Items) != 0 {
		t.Fatalf("expected 10002 friends list empty after delete, got %+v", envBAfter.Data.Items)
	}
	if store.convMembers[[2]uint64{conv12, 10001}].IsVisible || store.convMembers[[2]uint64{conv12, 10002}].IsVisible {
		t.Fatal("expected both conversation_members hidden after delete")
	}

	// Verify 401 unauthenticated, 400 invalid cursor (10001), 403 deleted friend remark (12007), and 404 unknown friend (12008).
	wUnauth := httptest.NewRecorder()
	reqUnauth, _ := http.NewRequest(http.MethodGet, "/api/v1/friends", nil)
	r.ServeHTTP(wUnauth, reqUnauth)
	if wUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", wUnauth.Code)
	}

	wBadCur := httptest.NewRecorder()
	reqBadCur, _ := http.NewRequest(http.MethodGet, "/api/v1/friends?cursor=invalid-cursor", nil)
	reqBadCur.Header.Set("Authorization", "Bearer test-token-user-10001")
	r.ServeHTTP(wBadCur, reqBadCur)
	if wBadCur.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid cursor, got %d", wBadCur.Code)
	}

	wPostDelPatch := httptest.NewRecorder()
	reqPostDelPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/friends/10002", strings.NewReader(`{"remark":"x"}`))
	reqPostDelPatch.Header.Set("Authorization", "Bearer test-token-user-10001")
	reqPostDelPatch.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wPostDelPatch, reqPostDelPatch)
	if wPostDelPatch.Code != http.StatusForbidden {
		t.Fatalf("expected 403 after delete, got %d body=%s", wPostDelPatch.Code, wPostDelPatch.Body.String())
	}

	wUnknownDel := httptest.NewRecorder()
	reqUnknownDel, _ := http.NewRequest(http.MethodDelete, "/api/v1/friends/10099", nil)
	reqUnknownDel.Header.Set("Authorization", "Bearer test-token-user-10001")
	r.ServeHTTP(wUnknownDel, reqUnknownDel)
	if wUnknownDel.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown friend delete, got %d body=%s", wUnknownDel.Code, wUnknownDel.Body.String())
	}

	// Ensure bearer token secret is redacted in structured logs.
	if strings.Contains(logBuf.String(), "test-token-user-10001") {
		t.Fatalf("bearer token leaked in access logs: %s", logBuf.String())
	}
}

func TestStage4FriendsLiveMySQLAndRedisHTTP(t *testing.T) {
	mysqlPass := firstEnv("MYSQL_PASSWORD", "IM_MYSQL_PASSWORD")
	redisPass := firstEnv("REDIS_PASSWORD", "IM_REDIS_PASSWORD")
	if mysqlPass == "" && redisPass == "" && os.Getenv("IM_LIVE_TESTS") == "" {
		t.Skip("skipping live Stage 4 friends integration test: MYSQL_PASSWORD/REDIS_PASSWORD not set")
	}

	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if v := firstEnv("MYSQL_HOST", "IM_MYSQL_HOST"); v != "" {
		cfg.MySQL.Host = v
	}
	if v := firstEnv("MYSQL_PORT", "IM_MYSQL_PORT"); v != "" {
		if p, convErr := strconv.Atoi(v); convErr == nil {
			cfg.MySQL.Port = p
		}
	}
	if v := firstEnv("MYSQL_USER", "IM_MYSQL_USER"); v != "" {
		cfg.MySQL.User = v
	}
	if v := firstEnv("MYSQL_DATABASE", "IM_MYSQL_DATABASE"); v != "" {
		cfg.MySQL.Database = v
	}
	cfg.MySQL.Password = mysqlPass
	if v := firstEnv("REDIS_ADDR", "IM_REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	cfg.Redis.Password = redisPass

	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	now := time.Now().UTC().Truncate(time.Millisecond)
	uA := model.User{Username: "s4_a_" + suffix, PasswordHash: "hash", Nickname: "Alice", Status: 1, Role: "user"}
	uB := model.User{Username: "s4_b_" + suffix, PasswordHash: "hash", Nickname: "Bob", AvatarURL: "https://cdn.example.com/b.png", Signature: "Hello", LastOnlineAt: &now, Status: 1, Role: "user"}
	uC := model.User{Username: "s4_c_" + suffix, PasswordHash: "hash", Nickname: "Charlie", Status: 1, Role: "user"}
	uD := model.User{Username: "s4_d_" + suffix, PasswordHash: "hash", Nickname: "Dave", Status: 1, Role: "user"}
	for _, u := range []*model.User{&uA, &uB, &uC, &uD} {
		if err := db.WithContext(ctx).Create(u).Error; err != nil {
			t.Fatalf("create user %s: %v", u.Username, err)
		}
	}

	var convABID, convACID, convADID, msgID uint64
	var redisCleanupKeys []string

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		if len(redisCleanupKeys) > 0 {
			_ = rdb.Del(cleanCtx, redisCleanupKeys...).Err()
		}
		if msgID > 0 {
			_ = db.WithContext(cleanCtx).Where("id = ?", msgID).Delete(&model.Message{}).Error
		}
		convIDs := make([]uint64, 0, 3)
		for _, cid := range []uint64{convABID, convACID, convADID} {
			if cid > 0 {
				convIDs = append(convIDs, cid)
			}
		}
		if len(convIDs) > 0 {
			_ = db.WithContext(cleanCtx).Where("conversation_id IN ?", convIDs).Delete(&model.ConversationMember{}).Error
			_ = db.WithContext(cleanCtx).Where("id IN ?", convIDs).Delete(&model.Conversation{}).Error
		}
		userIDs := []uint64{uA.ID, uB.ID, uC.ID, uD.ID}
		_ = db.WithContext(cleanCtx).Where("user_id IN ? OR friend_id IN ?", userIDs, userIDs).Delete(&model.Friendship{}).Error
		_ = db.WithContext(cleanCtx).Where("id IN ?", userIDs).Delete(&model.User{}).Error
	})

	friendRepo := repository.NewFriendshipRepository(db, rdb)
	friendSvc := service.NewFriendService(friendRepo, log)

	err = repository.WithTx(ctx, db, func(tx *gorm.DB) error {
		c1, err := friendSvc.EnsureFriendshipAndConversationTx(ctx, tx, uA.ID, uB.ID)
		if err != nil {
			return err
		}
		convABID = c1.ID
		c2, err := friendSvc.EnsureFriendshipAndConversationTx(ctx, tx, uA.ID, uC.ID)
		if err != nil {
			return err
		}
		convACID = c2.ID
		c3, err := friendSvc.EnsureFriendshipAndConversationTx(ctx, tx, uA.ID, uD.ID)
		if err != nil {
			return err
		}
		convADID = c3.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed friendships and conversations: %v", err)
	}

	// Insert a real message in convAB to assert historical messages are preserved on friend delete.
	sfGen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatalf("snowflake generator: %v", err)
	}
	msgID, err = sfGen.NextID()
	if err != nil {
		t.Fatalf("snowflake NextID: %v", err)
	}
	histMsg := model.Message{
		ID:             msgID,
		ConversationID: convABID,
		Seq:            1,
		SenderID:       uA.ID,
		ClientMsgID:    "stage4-friends-history-" + suffix,
		MsgType:        model.MessageTypeText,
		Content:        "preserved history before delete",
		Status:         model.MessageStatusNormal,
	}
	if err := db.WithContext(ctx).Create(&histMsg).Error; err != nil {
		t.Fatalf("create history message: %v", err)
	}

	// Real Redis presence setup:
	// uB: 2 devices in route set, 1 active conn lease -> online=true
	// uC: stale route set with expired conn lease -> online=false
	// uD: no WS connections -> online=false
	connBKey := "im:presence:conn:live-b-" + suffix
	routeBKey := "im:route:user:" + strconv.FormatUint(uB.ID, 10)
	userBKey := "im:presence:user:" + strconv.FormatUint(uB.ID, 10)
	routeCKey := "im:route:user:" + strconv.FormatUint(uC.ID, 10)
	userCKey := "im:presence:user:" + strconv.FormatUint(uC.ID, 10)
	redisCleanupKeys = append(redisCleanupKeys, connBKey, routeBKey, userBKey, routeCKey, userCKey)

	if err := rdb.HSet(ctx, connBKey, "user_id", uB.ID, "conn_id", "live-b-"+suffix).Err(); err != nil {
		t.Fatalf("redis HSet connBKey: %v", err)
	}
	_ = rdb.Expire(ctx, connBKey, 60*time.Second).Err()
	_ = rdb.SAdd(ctx, routeBKey, "node-1:dead-b-"+suffix, "node-2:live-b-"+suffix).Err()
	_ = rdb.Expire(ctx, routeBKey, 60*time.Second).Err()
	_ = rdb.Set(ctx, userBKey, "1", 60*time.Second).Err()

	_ = rdb.SAdd(ctx, routeCKey, "node-1:expired-c-"+suffix).Err()
	_ = rdb.Expire(ctx, routeCKey, 60*time.Second).Err()
	_ = rdb.Set(ctx, userCKey, "1", 60*time.Second).Err()

	r := api.NewRouter(cfg, log, nil)
	api.RegisterFriendRoutes(r, stubJWTAuthMiddleware(), controller.NewFriendController(friendSvc))

	// 1. Paginated GET /api/v1/friends (page 1 & page 2)
	wPage1 := httptest.NewRecorder()
	reqPage1, _ := http.NewRequest(http.MethodGet, "/api/v1/friends?limit=2", nil)
	reqPage1.Header.Set("Authorization", "Bearer test-token-user-"+strconv.FormatUint(uA.ID, 10))
	r.ServeHTTP(wPage1, reqPage1)
	if wPage1.Code != http.StatusOK {
		t.Fatalf("live GET /api/v1/friends page1 failed: %d body=%s", wPage1.Code, wPage1.Body.String())
	}
	var envPage1 friendListEnvelope
	_ = json.Unmarshal(wPage1.Body.Bytes(), &envPage1)
	if !envPage1.Data.HasMore || envPage1.Data.NextCursor == nil || len(envPage1.Data.Items) != 2 {
		t.Fatalf("unexpected live page1: %+v", envPage1.Data)
	}
	if envPage1.Data.Items[0].FriendID != uD.ID || envPage1.Data.Items[0].Online ||
		envPage1.Data.Items[1].FriendID != uC.ID || envPage1.Data.Items[1].Online {
		t.Fatalf("expected uD and uC offline on page1, got %+v", envPage1.Data.Items)
	}

	wPage2 := httptest.NewRecorder()
	reqPage2, _ := http.NewRequest(http.MethodGet, "/api/v1/friends?limit=2&cursor="+*envPage1.Data.NextCursor, nil)
	reqPage2.Header.Set("Authorization", "Bearer test-token-user-"+strconv.FormatUint(uA.ID, 10))
	r.ServeHTTP(wPage2, reqPage2)
	if wPage2.Code != http.StatusOK {
		t.Fatalf("live GET /api/v1/friends page2 failed: %d body=%s", wPage2.Code, wPage2.Body.String())
	}
	var envPage2 friendListEnvelope
	_ = json.Unmarshal(wPage2.Body.Bytes(), &envPage2)
	if envPage2.Data.HasMore || envPage2.Data.NextCursor != nil || len(envPage2.Data.Items) != 1 {
		t.Fatalf("unexpected live page2: %+v", envPage2.Data)
	}
	if envPage2.Data.Items[0].FriendID != uB.ID || !envPage2.Data.Items[0].Online ||
		envPage2.Data.Items[0].ConversationID == nil || *envPage2.Data.Items[0].ConversationID != strconv.FormatUint(convABID, 10) {
		t.Fatalf("unexpected uB item on live page2: %+v", envPage2.Data.Items[0])
	}

	// 2. PATCH remark updates only caller side (uA -> uB).
	wPatch := httptest.NewRecorder()
	reqPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/friends/"+strconv.FormatUint(uB.ID, 10), strings.NewReader(`{"remark":"老朋友"}`))
	reqPatch.Header.Set("Authorization", "Bearer test-token-user-"+strconv.FormatUint(uA.ID, 10))
	reqPatch.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wPatch, reqPatch)
	if wPatch.Code != http.StatusOK {
		t.Fatalf("live PATCH failed: %d body=%s", wPatch.Code, wPatch.Body.String())
	}
	rowBA, err := friendRepo.GetFriendship(ctx, uB.ID, uA.ID)
	if err != nil || rowBA.Remark != "" {
		t.Fatalf("expected uB->uA remark empty, got %+v err=%v", rowBA, err)
	}

	// 3. DELETE uB -> deactivates both sides, hides both conversation_members, preserves message history.
	wDel := httptest.NewRecorder()
	reqDel, _ := http.NewRequest(http.MethodDelete, "/api/v1/friends/"+strconv.FormatUint(uB.ID, 10), nil)
	reqDel.Header.Set("Authorization", "Bearer test-token-user-"+strconv.FormatUint(uA.ID, 10))
	r.ServeHTTP(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("live DELETE failed: %d body=%s", wDel.Code, wDel.Body.String())
	}
	var delEnv deleteFriendEnvelope
	_ = json.Unmarshal(wDel.Body.Bytes(), &delEnv)
	if delEnv.Data.ConversationID == nil || *delEnv.Data.ConversationID != strconv.FormatUint(convABID, 10) || delEnv.Data.IsVisible {
		t.Fatalf("unexpected live delete data: %+v", delEnv.Data)
	}

	var storedMsg model.Message
	if err := db.WithContext(ctx).Where("id = ?", msgID).First(&storedMsg).Error; err != nil {
		t.Fatalf("expected preserved message history after delete, got err=%v", err)
	}
	var members []model.ConversationMember
	if err := db.WithContext(ctx).Where("conversation_id = ?", convABID).Find(&members).Error; err != nil {
		t.Fatalf("query conversation members: %v", err)
	}
	if len(members) != 2 || members[0].IsVisible || members[1].IsVisible {
		t.Fatalf("expected both conversation_members hidden and preserved, got %+v", members)
	}

	// 4. Re-adding restores original conversation ID and sets is_visible=true.
	var restoredConvID uint64
	err = repository.WithTx(ctx, db, func(tx *gorm.DB) error {
		conv, err := friendSvc.EnsureFriendshipAndConversationTx(ctx, tx, uB.ID, uA.ID)
		if err != nil {
			return err
		}
		restoredConvID = conv.ID
		return nil
	})
	if err != nil || restoredConvID != convABID {
		t.Fatalf("expected restored convABID=%d, got %d err=%v", convABID, restoredConvID, err)
	}

	// 5. Real unreachable Redis client triggers 503 + 19002 without mocking Redis.
	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	defer func() { _ = deadRedis.Close() }()
	degradedRouter := api.NewRouter(cfg, log, nil)
	degradedSvc := service.NewFriendService(repository.NewFriendshipRepository(db, deadRedis), log)
	api.RegisterFriendRoutes(degradedRouter, stubJWTAuthMiddleware(), controller.NewFriendController(degradedSvc))

	wRedisFail := httptest.NewRecorder()
	reqRedisFail, _ := http.NewRequest(http.MethodGet, "/api/v1/friends", nil)
	reqRedisFail.Header.Set("Authorization", "Bearer test-token-user-"+strconv.FormatUint(uA.ID, 10))
	degradedRouter.ServeHTTP(wRedisFail, reqRedisFail)
	if wRedisFail.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on unreachable redis, got %d body=%s", wRedisFail.Code, wRedisFail.Body.String())
	}
	var errEnv errcode.Envelope
	_ = json.Unmarshal(wRedisFail.Body.Bytes(), &errEnv)
	if errEnv.Code != errcode.ErrRedisUnavailable.Code {
		t.Fatalf("expected business code 19002, got %d", errEnv.Code)
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
