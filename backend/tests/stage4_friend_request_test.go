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

type stage4Store struct {
	mu              sync.Mutex
	users           map[uint64]model.User
	requests        map[uint64]model.FriendRequest
	nextRequestID   uint64
	friendships     map[string]model.Friendship
	conversations   map[uint64]model.Conversation
	convByDirectKey map[string]uint64
	nextConvID      uint64
	members         map[string]model.ConversationMember
	messages        []model.Message
	receipts        []model.MessageReceipt
	onlineUsers     map[uint64]bool
}

func newStage4Store() *stage4Store {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &stage4Store{
		users: map[uint64]model.User{
			20001: {ID: 20001, Username: "u20001", Nickname: "Alice", Status: model.UserStatusActive, CreatedAt: now},
			20002: {ID: 20002, Username: "u20002", Nickname: "Bob", Status: model.UserStatusActive, CreatedAt: now},
		},
		requests:        make(map[uint64]model.FriendRequest),
		nextRequestID:   1,
		friendships:     make(map[string]model.Friendship),
		conversations:   make(map[uint64]model.Conversation),
		convByDirectKey: make(map[string]uint64),
		nextConvID:      1001,
		members:         make(map[string]model.ConversationMember),
		onlineUsers:     map[uint64]bool{20001: true},
	}
}

func (s *stage4Store) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(nil)
}

func (s *stage4Store) LockUsersAscending(_ context.Context, _ *gorm.DB, userA, userB uint64) (map[uint64]model.User, error) {
	out := make(map[uint64]model.User, 2)
	for _, id := range []uint64{userA, userB} {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *stage4Store) HasActiveFriendship(_ context.Context, _ *gorm.DB, userA, userB uint64) (bool, error) {
	f1 := s.friendships[fmt.Sprintf("%d:%d", userA, userB)]
	f2 := s.friendships[fmt.Sprintf("%d:%d", userB, userA)]
	return f1.Status == model.FriendshipStatusActive || f2.Status == model.FriendshipStatusActive, nil
}

func (s *stage4Store) HasPendingFriendRequest(_ context.Context, _ *gorm.DB, userA, userB uint64) (bool, error) {
	for _, fr := range s.requests {
		if fr.Status == model.FriendRequestStatusPending &&
			((fr.FromUserID == userA && fr.ToUserID == userB) || (fr.FromUserID == userB && fr.ToUserID == userA)) {
			return true, nil
		}
	}
	return false, nil
}

func (s *stage4Store) CreateFriendRequest(_ context.Context, _ *gorm.DB, req *model.FriendRequest) error {
	req.ID = s.nextRequestID
	s.nextRequestID++
	s.requests[req.ID] = *req
	return nil
}

func (s *stage4Store) GetFriendRequestByID(_ context.Context, id uint64) (*model.FriendRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fr, ok := s.requests[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := fr
	return &cp, nil
}

func (s *stage4Store) LockFriendRequestByID(_ context.Context, _ *gorm.DB, id uint64) (*model.FriendRequest, error) {
	fr, ok := s.requests[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := fr
	return &cp, nil
}

func (s *stage4Store) UpdateFriendRequestStatus(_ context.Context, _ *gorm.DB, id uint64, fromStatus, toStatus string, handledAt time.Time) (bool, error) {
	fr, ok := s.requests[id]
	if !ok || fr.Status != fromStatus {
		return false, nil
	}
	fr.Status = toStatus
	ht := handledAt
	fr.HandledAt = &ht
	fr.UpdatedAt = handledAt
	s.requests[id] = fr
	return true, nil
}

func (s *stage4Store) ListFriendRequests(_ context.Context, filter repository.FriendRequestListFilter) ([]model.FriendRequest, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.FriendRequest
	for _, fr := range s.requests {
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

func (s *stage4Store) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uint64]model.User, len(ids))
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *stage4Store) FindSingleConversationIDByDirectKey(_ context.Context, directKey string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.convByDirectKey[directKey], nil
}

func (s *stage4Store) LockSingleConversationByDirectKey(_ context.Context, _ *gorm.DB, directKey string) (*model.Conversation, error) {
	id, ok := s.convByDirectKey[directKey]
	if !ok {
		return nil, nil
	}
	c := s.conversations[id]
	return &c, nil
}

func (s *stage4Store) CreateSingleConversation(_ context.Context, _ *gorm.DB, directKey string) (*model.Conversation, error) {
	id := s.nextConvID
	s.nextConvID++
	dk := directKey
	c := model.Conversation{ID: id, Type: model.ConversationTypeSingle, DirectKey: &dk}
	s.conversations[id] = c
	s.convByDirectKey[directKey] = id
	return &c, nil
}

func (s *stage4Store) EnsureConversationMembersVisible(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, baselineSeq uint64, now time.Time) error {
	for _, uid := range userIDs {
		k := fmt.Sprintf("%d:%d", convID, uid)
		m := s.members[k]
		m.ConversationID = convID
		m.UserID = uid
		m.IsVisible = true
		if m.JoinedAt.IsZero() {
			m.LastDeliveredSeq = baselineSeq
			m.LastReadSeq = baselineSeq
			m.JoinedAt = now
		}
		m.UpdatedAt = now
		s.members[k] = m
	}
	return nil
}

func (s *stage4Store) CreateSystemMessage(_ context.Context, _ *gorm.DB, msg *model.Message) error {
	s.messages = append(s.messages, *msg)
	return nil
}

func (s *stage4Store) UpdateConversationLastMessage(_ context.Context, _ *gorm.DB, convID uint64, msg *model.Message) error {
	c := s.conversations[convID]
	c.LastMsgID = msg.ID
	c.LastSeq = msg.Seq
	c.LastMsgPreview = msg.Content
	s.conversations[convID] = c
	return nil
}

func (s *stage4Store) IncrementMembersUnreadCount(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, now time.Time) error {
	for _, uid := range userIDs {
		k := fmt.Sprintf("%d:%d", convID, uid)
		m := s.members[k]
		m.UnreadCount++
		m.UpdatedAt = now
		s.members[k] = m
	}
	return nil
}

func (s *stage4Store) CreateMessageReceipts(_ context.Context, _ *gorm.DB, receipts []model.MessageReceipt) error {
	s.receipts = append(s.receipts, receipts...)
	return nil
}

func (s *stage4Store) EnsureBilateralFriendship(_ context.Context, _ *gorm.DB, userA, userB uint64) error {
	s.friendships[fmt.Sprintf("%d:%d", userA, userB)] = model.Friendship{UserID: userA, FriendID: userB, Status: model.FriendshipStatusActive}
	s.friendships[fmt.Sprintf("%d:%d", userB, userA)] = model.Friendship{UserID: userB, FriendID: userA, Status: model.FriendshipStatusActive}
	return nil
}

func (s *stage4Store) AcquireConvLock(_ context.Context, convID uint64) (string, error) {
	return fmt.Sprintf("lock-%d", convID), nil
}

func (s *stage4Store) AllocateNextSeq(_ context.Context, _ *gorm.DB, convID uint64, _ string) (*repository.SeqReservation, error) {
	var maxSeq uint64
	for _, m := range s.messages {
		if m.ConversationID == convID && m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	return &repository.SeqReservation{Seq: maxSeq + 1, Rollback: func(context.Context) error { return nil }}, nil
}

func (s *stage4Store) ReleaseConvLock(context.Context, uint64, string) error {
	return nil
}

func (s *stage4Store) BatchIsOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		if s.onlineUsers[id] {
			out[id] = true
		}
	}
	return out, nil
}

func testStubJWTAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer stub-") {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		uid := strings.TrimPrefix(auth, "Bearer stub-")
		if _, err := strconv.ParseUint(uid, 10, 64); err != nil {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		errcode.SetUserID(c, uid)
		c.Next()
	}
}

func TestStage4FriendRequestHTTPFlowAndLogRedaction(t *testing.T) {
	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	gen, err := snowflake.NewGenerator(cfg.Snowflake.NodeID)
	if err != nil {
		t.Fatalf("snowflake generator: %v", err)
	}

	store := newStage4Store()
	svc := service.NewFriendRequestService(store, store, store, store, gen, log)
	ctrl := controller.NewFriendRequestController(svc)

	router := api.NewRouter(cfg, log, nil)
	api.RegisterFriendRequestRoutes(router, testStubJWTAuth(), ctrl)

	doReq := func(method, path, uid, body string) *httptest.ResponseRecorder {
		req, _ := http.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if uid != "" {
			req.Header.Set("Authorization", "Bearer stub-"+uid)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 1. Create -> Reject -> Re-create -> Cancel -> Re-create -> List -> Accept -> Repeat Accept.
	w1 := doReq(http.MethodPost, "/api/v1/friend-requests", "20001", `{"to_user_id":"20002","message":"hello bob"}`)
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", w1.Code, w1.Body.String())
	}

	// Opposite direction duplicate blocked -> 409 / 12004.
	wDup := doReq(http.MethodPost, "/api/v1/friend-requests", "20002", `{"to_user_id":"20001"}`)
	if wDup.Code != http.StatusConflict {
		t.Fatalf("expected 409 on reverse duplicate, got %d", wDup.Code)
	}

	// Reject request 1.
	wRej := doReq(http.MethodPost, "/api/v1/friend-requests/1/reject", "20002", "")
	if wRej.Code != http.StatusOK {
		t.Fatalf("expected 200 on reject, got %d", wRej.Code)
	}

	// Re-create & Cancel request 2.
	w2 := doReq(http.MethodPost, "/api/v1/friend-requests", "20001", `{"to_user_id":"20002","message":"retry"}`)
	if w2.Code != http.StatusCreated {
		t.Fatalf("expected 201 for req 2, got %d", w2.Code)
	}
	wCan := doReq(http.MethodPost, "/api/v1/friend-requests/2/cancel", "20001", "")
	if wCan.Code != http.StatusOK {
		t.Fatalf("expected 200 on cancel, got %d", wCan.Code)
	}

	// Re-create request 3 -> list -> accept.
	w3 := doReq(http.MethodPost, "/api/v1/friend-requests", "20001", `{"to_user_id":"20002","message":"third time"}`)
	if w3.Code != http.StatusCreated {
		t.Fatalf("expected 201 for req 3, got %d", w3.Code)
	}

	wList := doReq(http.MethodGet, "/api/v1/friend-requests?direction=received&limit=2", "20002", "")
	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200 on list, got %d body=%s", wList.Code, wList.Body.String())
	}
	var listEnv struct {
		Code int `json:"code"`
		Data struct {
			Items []struct {
				ID   string `json:"id"`
				Peer struct {
					ID     string `json:"id"`
					Online bool   `json:"online"`
				} `json:"peer"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
			HasMore    bool    `json:"has_more"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listEnv); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listEnv.Data.Items) != 2 || !listEnv.Data.HasMore || listEnv.Data.NextCursor == nil {
		t.Fatalf("unexpected page 1: %+v", listEnv.Data)
	}
	if listEnv.Data.Items[0].Peer.ID != "20001" || !listEnv.Data.Items[0].Peer.Online {
		t.Fatalf("expected peer 20001 online=true, got %+v", listEnv.Data.Items[0].Peer)
	}

	wAcc := doReq(http.MethodPost, "/api/v1/friend-requests/3/accept", "20002", "")
	if wAcc.Code != http.StatusOK {
		t.Fatalf("expected 200 on accept, got %d body=%s", wAcc.Code, wAcc.Body.String())
	}

	// Repeat accept -> 409 / 12006.
	wAccAgain := doReq(http.MethodPost, "/api/v1/friend-requests/3/accept", "20002", "")
	if wAccAgain.Code != http.StatusConflict {
		t.Fatalf("expected 409 on repeat accept, got %d", wAccAgain.Code)
	}

	// Access logs must redact Authorization stub token.
	if strings.Contains(logBuf.String(), "stub-20001") || strings.Contains(logBuf.String(), "stub-20002") {
		t.Fatalf("access log leaked authorization token: %s", logBuf.String())
	}
}

func TestStage4FriendRequestLiveMySQLAndRedisIntegration(t *testing.T) {
	mysqlPass, mysqlSet := lookupStage4Env("MYSQL_PASSWORD", "IM_MYSQL_PASSWORD")
	redisPass, redisSet := lookupStage4Env("REDIS_PASSWORD", "IM_REDIS_PASSWORD")
	if !mysqlSet && !redisSet && os.Getenv("MYSQL_HOST") == "" && os.Getenv("IM_MYSQL_HOST") == "" {
		t.Skip("skipping live Stage 4 friend request integration test: env credentials not set")
	}

	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if v := firstStage4NonEmpty(os.Getenv("MYSQL_HOST"), os.Getenv("IM_MYSQL_HOST")); v != "" {
		cfg.MySQL.Host = v
	}
	if v := firstStage4NonEmpty(os.Getenv("MYSQL_PORT"), os.Getenv("IM_MYSQL_PORT")); v != "" {
		if port, convErr := strconv.Atoi(v); convErr == nil {
			cfg.MySQL.Port = port
		}
	}
	if v := firstStage4NonEmpty(os.Getenv("MYSQL_USER"), os.Getenv("IM_MYSQL_USER")); v != "" {
		cfg.MySQL.User = v
	}
	if v := firstStage4NonEmpty(os.Getenv("MYSQL_DATABASE"), os.Getenv("IM_MYSQL_DATABASE")); v != "" {
		cfg.MySQL.Database = v
	}
	cfg.MySQL.Password = mysqlPass
	if v := firstStage4NonEmpty(os.Getenv("REDIS_ADDR"), os.Getenv("IM_REDIS_ADDR")); v != "" {
		cfg.Redis.Addr = v
	}
	cfg.Redis.Password = redisPass

	log, err := logger.New(config.LogConfig{Level: "warn", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
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

	gen, err := snowflake.NewGenerator(cfg.Snowflake.NodeID)
	if err != nil {
		t.Fatalf("snowflake generator: %v", err)
	}

	repo := repository.NewGormFriendRequestRepository(db)
	writer := repository.NewGormRelationWriter()
	seqAlloc := repository.NewRedisSeqAllocator(rdb, 5*time.Second)
	presence := repository.NewRedisPresenceChecker(rdb)
	svc := service.NewFriendRequestService(repo, writer, seqAlloc, presence, gen, log)
	ctrl := controller.NewFriendRequestController(svc)

	router := api.NewRouter(cfg, log, nil)
	api.RegisterFriendRequestRoutes(router, testStubJWTAuth(), ctrl)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	suffix := time.Now().UnixNano()
	alice := model.User{
		Username:     fmt.Sprintf("stage4_fr_alice_%d", suffix),
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     "AliceLive",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	bob := model.User{
		Username:     fmt.Sprintf("stage4_fr_bob_%d", suffix),
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     "BobLive",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	if err := db.WithContext(ctx).Create(&alice).Error; err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := db.WithContext(ctx).Create(&bob).Error; err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	var convID uint64
	defer func() {
		cctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if convID > 0 {
			_ = db.WithContext(cctx).Where("conversation_id = ?", convID).Delete(&model.MessageReceipt{}).Error
			_ = db.WithContext(cctx).Where("conversation_id = ?", convID).Delete(&model.Message{}).Error
			_ = db.WithContext(cctx).Where("conversation_id = ?", convID).Delete(&model.ConversationMember{}).Error
			_ = db.WithContext(cctx).Where("id = ?", convID).Delete(&model.Conversation{}).Error
			_ = rdb.Del(cctx, fmt.Sprintf("conv:seq:%d", convID), fmt.Sprintf("conv:lock:%d", convID)).Err()
		}
		_ = db.WithContext(cctx).Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", alice.ID, bob.ID, bob.ID, alice.ID).Delete(&model.Friendship{}).Error
		_ = db.WithContext(cctx).Where("(from_user_id = ? AND to_user_id = ?) OR (from_user_id = ? AND to_user_id = ?)", alice.ID, bob.ID, bob.ID, alice.ID).Delete(&model.FriendRequest{}).Error
		_ = db.WithContext(cctx).Where("id IN ?", []uint64{alice.ID, bob.ID}).Delete(&model.User{}).Error
		_ = rdb.Del(cctx, fmt.Sprintf("im:presence:user:%d", alice.ID), fmt.Sprintf("im:presence:user:%d", bob.ID)).Err()
	}()

	if err := rdb.Set(ctx, fmt.Sprintf("im:presence:user:%d", alice.ID), "1", 30*time.Second).Err(); err != nil {
		t.Fatalf("set presence in live redis: %v", err)
	}

	doHTTP := func(method, path string, actorID uint64, body string) *httptest.ResponseRecorder {
		req, _ := http.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer stub-%d", actorID))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 1. Alice -> Bob create (201)
	wCreate := doHTTP(http.MethodPost, "/api/v1/friend-requests", alice.ID, fmt.Sprintf(`{"to_user_id":"%d","message":"hello live"}`, bob.ID))
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("live create failed: status=%d body=%s", wCreate.Code, wCreate.Body.String())
	}
	var createEnv struct {
		Data struct {
			Request struct {
				ID string `json:"id"`
			} `json:"request"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wCreate.Body.Bytes(), &createEnv)
	req1ID := createEnv.Data.Request.ID

	// 2. Concurrent HTTP accept by Bob on live MySQL + Redis -> 1 wins (200), 1 gets 12006 (409).
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	bodies := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := doHTTP(http.MethodPost, "/api/v1/friend-requests/"+req1ID+"/accept", bob.ID, "")
			statuses <- w.Code
			bodies <- w.Body.String()
		}()
	}
	wg.Wait()
	close(statuses)
	close(bodies)

	ok200, conflict409 := 0, 0
	for code := range statuses {
		switch code {
		case http.StatusOK:
			ok200++
		case http.StatusConflict:
			conflict409++
		default:
			t.Fatalf("unexpected concurrent accept status %d", code)
		}
	}
	if ok200 != 1 || conflict409 != 1 {
		t.Fatalf("expected 1x200 and 1x409 on concurrent accept, got 200=%d 409=%d", ok200, conflict409)
	}
	for b := range bodies {
		var acc struct {
			Code int `json:"code"`
			Data struct {
				ConversationID string `json:"conversation_id"`
				SystemMsgID    string `json:"system_msg_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(b), &acc); err == nil && acc.Code == 0 {
			convID, _ = strconv.ParseUint(acc.Data.ConversationID, 10, 64)
		}
	}
	if convID == 0 {
		t.Fatal("expected live conversation_id to be populated")
	}

	// Verify Redis conv:seq == "1" and conv:lock is released.
	seqStr, err := rdb.Get(ctx, fmt.Sprintf("conv:seq:%d", convID)).Result()
	if err != nil || seqStr != "1" {
		t.Fatalf("expected live Redis conv:seq=1, got %q err=%v", seqStr, err)
	}
	if _, err := rdb.Get(ctx, fmt.Sprintf("conv:lock:%d", convID)).Result(); err != redis.Nil {
		t.Fatalf("expected conv:lock to be released, got err=%v", err)
	}

	// Simulate unfriend & hidden conversation, then Bob sends to Alice and Alice accepts -> seq=2 & same convID!
	if err := db.WithContext(ctx).Model(&model.Friendship{}).
		Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", alice.ID, bob.ID, bob.ID, alice.ID).
		Update("status", model.FriendshipStatusDeleted).Error; err != nil {
		t.Fatalf("soft delete friendship: %v", err)
	}
	if err := db.WithContext(ctx).Model(&model.ConversationMember{}).
		Where("conversation_id = ?", convID).
		Update("is_visible", false).Error; err != nil {
		t.Fatalf("hide conversation members: %v", err)
	}

	wReCreate := doHTTP(http.MethodPost, "/api/v1/friend-requests", bob.ID, fmt.Sprintf(`{"to_user_id":"%d","message":"welcome back"}`, alice.ID))
	if wReCreate.Code != http.StatusCreated {
		t.Fatalf("re-create failed: %d body=%s", wReCreate.Code, wReCreate.Body.String())
	}
	var reEnv struct {
		Data struct {
			Request struct {
				ID string `json:"id"`
			} `json:"request"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wReCreate.Body.Bytes(), &reEnv)

	wReAccept := doHTTP(http.MethodPost, "/api/v1/friend-requests/"+reEnv.Data.Request.ID+"/accept", alice.ID, "")
	if wReAccept.Code != http.StatusOK {
		t.Fatalf("re-accept failed: %d body=%s", wReAccept.Code, wReAccept.Body.String())
	}
	var reAccEnv struct {
		Data struct {
			FriendID       string `json:"friend_id"`
			ConversationID string `json:"conversation_id"`
			SystemMsgID    string `json:"system_msg_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wReAccept.Body.Bytes(), &reAccEnv)
	if reAccEnv.Data.ConversationID != strconv.FormatUint(convID, 10) {
		t.Fatalf("expected restored conversation %d, got %s", convID, reAccEnv.Data.ConversationID)
	}
	seqStr2, err := rdb.Get(ctx, fmt.Sprintf("conv:seq:%d", convID)).Result()
	if err != nil || seqStr2 != "2" {
		t.Fatalf("expected live Redis conv:seq=2 after re-accept, got %q err=%v", seqStr2, err)
	}
}

func lookupStage4Env(primary, fallback string) (string, bool) {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		return v, true
	}
	if v, ok := os.LookupEnv(fallback); ok && v != "" {
		return v, true
	}
	return "", false
}

func firstStage4NonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
