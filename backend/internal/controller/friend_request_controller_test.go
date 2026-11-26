package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

// controllerTestStore is a stateful in-memory repository for HTTP controller tests.
type controllerTestStore struct {
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
	onlineUsers     map[uint64]bool
}

func newControllerTestStore() *controllerTestStore {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &controllerTestStore{
		users: map[uint64]model.User{
			10001: {ID: 10001, Username: "alice", Nickname: "Alice", Status: model.UserStatusActive, CreatedAt: now},
			10002: {ID: 10002, Username: "bob", Nickname: "Bob", Status: model.UserStatusActive, CreatedAt: now},
			10003: {ID: 10003, Username: "charlie", Nickname: "Charlie", Status: model.UserStatusActive, CreatedAt: now},
		},
		requests:        make(map[uint64]model.FriendRequest),
		nextRequestID:   1,
		friendships:     make(map[string]model.Friendship),
		conversations:   make(map[uint64]model.Conversation),
		convByDirectKey: make(map[string]uint64),
		nextConvID:      500,
		members:         make(map[string]model.ConversationMember),
		onlineUsers:     map[uint64]bool{10002: true},
	}
}

func (s *controllerTestStore) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(nil)
}

func (s *controllerTestStore) LockUsersAscending(_ context.Context, _ *gorm.DB, userA, userB uint64) (map[uint64]model.User, error) {
	out := make(map[uint64]model.User, 2)
	for _, id := range []uint64{userA, userB} {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *controllerTestStore) HasActiveFriendship(_ context.Context, _ *gorm.DB, userA, userB uint64) (bool, error) {
	f1 := s.friendships[fmt.Sprintf("%d:%d", userA, userB)]
	f2 := s.friendships[fmt.Sprintf("%d:%d", userB, userA)]
	return f1.Status == model.FriendshipStatusActive || f2.Status == model.FriendshipStatusActive, nil
}

func (s *controllerTestStore) HasPendingFriendRequest(_ context.Context, _ *gorm.DB, userA, userB uint64) (bool, error) {
	for _, fr := range s.requests {
		if fr.Status == model.FriendRequestStatusPending &&
			((fr.FromUserID == userA && fr.ToUserID == userB) || (fr.FromUserID == userB && fr.ToUserID == userA)) {
			return true, nil
		}
	}
	return false, nil
}

func (s *controllerTestStore) CreateFriendRequest(_ context.Context, _ *gorm.DB, req *model.FriendRequest) error {
	req.ID = s.nextRequestID
	s.nextRequestID++
	s.requests[req.ID] = *req
	return nil
}

func (s *controllerTestStore) GetFriendRequestByID(_ context.Context, id uint64) (*model.FriendRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fr, ok := s.requests[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := fr
	return &cp, nil
}

func (s *controllerTestStore) LockFriendRequestByID(_ context.Context, _ *gorm.DB, id uint64) (*model.FriendRequest, error) {
	fr, ok := s.requests[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cp := fr
	return &cp, nil
}

func (s *controllerTestStore) UpdateFriendRequestStatus(_ context.Context, _ *gorm.DB, id uint64, fromStatus, toStatus string, handledAt time.Time) (bool, error) {
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

func (s *controllerTestStore) ListFriendRequests(_ context.Context, filter repository.FriendRequestListFilter) ([]model.FriendRequest, bool, error) {
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

func (s *controllerTestStore) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
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

func (s *controllerTestStore) FindSingleConversationIDByDirectKey(_ context.Context, directKey string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.convByDirectKey[directKey], nil
}

func (s *controllerTestStore) LockSingleConversationByDirectKey(_ context.Context, _ *gorm.DB, directKey string) (*model.Conversation, error) {
	id, ok := s.convByDirectKey[directKey]
	if !ok {
		return nil, nil
	}
	c := s.conversations[id]
	return &c, nil
}

func (s *controllerTestStore) CreateSingleConversation(_ context.Context, _ *gorm.DB, directKey string) (*model.Conversation, error) {
	id := s.nextConvID
	s.nextConvID++
	dk := directKey
	c := model.Conversation{ID: id, Type: model.ConversationTypeSingle, DirectKey: &dk}
	s.conversations[id] = c
	s.convByDirectKey[directKey] = id
	return &c, nil
}

func (s *controllerTestStore) EnsureConversationMembersVisible(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, baselineSeq uint64, now time.Time) error {
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

func (s *controllerTestStore) CreateSystemMessage(_ context.Context, _ *gorm.DB, msg *model.Message) error {
	s.messages = append(s.messages, *msg)
	return nil
}

func (s *controllerTestStore) UpdateConversationLastMessage(_ context.Context, _ *gorm.DB, convID uint64, msg *model.Message) error {
	c := s.conversations[convID]
	c.LastMsgID = msg.ID
	c.LastSeq = msg.Seq
	c.LastMsgPreview = msg.Content
	s.conversations[convID] = c
	return nil
}

func (s *controllerTestStore) IncrementMembersUnreadCount(_ context.Context, _ *gorm.DB, convID uint64, userIDs []uint64, now time.Time) error {
	for _, uid := range userIDs {
		k := fmt.Sprintf("%d:%d", convID, uid)
		m := s.members[k]
		m.UnreadCount++
		m.UpdatedAt = now
		s.members[k] = m
	}
	return nil
}

func (s *controllerTestStore) CreateMessageReceipts(context.Context, *gorm.DB, []model.MessageReceipt) error {
	return nil
}

func (s *controllerTestStore) EnsureBilateralFriendship(_ context.Context, _ *gorm.DB, userA, userB uint64) error {
	s.friendships[fmt.Sprintf("%d:%d", userA, userB)] = model.Friendship{UserID: userA, FriendID: userB, Status: model.FriendshipStatusActive}
	s.friendships[fmt.Sprintf("%d:%d", userB, userA)] = model.Friendship{UserID: userB, FriendID: userA, Status: model.FriendshipStatusActive}
	return nil
}

func (s *controllerTestStore) AcquireConvLock(_ context.Context, convID uint64) (string, error) {
	return fmt.Sprintf("tok-%d", convID), nil
}

func (s *controllerTestStore) AllocateNextSeq(_ context.Context, _ *gorm.DB, convID uint64, _ string) (*repository.SeqReservation, error) {
	var maxSeq uint64
	for _, m := range s.messages {
		if m.ConversationID == convID && m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	return &repository.SeqReservation{Seq: maxSeq + 1, Rollback: func(context.Context) error { return nil }}, nil
}

func (s *controllerTestStore) ReleaseConvLock(context.Context, uint64, string) error {
	return nil
}

func (s *controllerTestStore) BatchIsOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		if s.onlineUsers[id] {
			out[id] = true
		}
	}
	return out, nil
}

func setupControllerTestRouter(t *testing.T) (*gin.Engine, *controllerTestStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := newControllerTestStore()
	gen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatalf("snowflake generator: %v", err)
	}
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	svc := service.NewFriendRequestService(store, store, store, store, gen, log)
	ctrl := NewFriendRequestController(svc)

	stubAuth := func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer user:") {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		uid := strings.TrimPrefix(auth, "Bearer user:")
		if _, parseErr := strconv.ParseUint(uid, 10, 64); parseErr != nil {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		errcode.SetUserID(c, uid)
		c.Next()
	}

	r := gin.New()
	r.Use(middleware.RequestID())
	r.Use(middleware.Recovery(log))
	grp := r.Group("/api/v1/friend-requests")
	grp.Use(stubAuth)
	grp.POST("", ctrl.Create)
	grp.GET("", ctrl.List)
	grp.POST("/:id/accept", ctrl.Accept)
	grp.POST("/:id/reject", ctrl.Reject)
	grp.POST("/:id/cancel", ctrl.Cancel)
	return r, store
}

func doJSONReq(r http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer user:"+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestFriendRequestControllerEndpointsAndEnvelopes(t *testing.T) {
	r, _ := setupControllerTestRouter(t)

	// Unauthenticated request -> 401 + code 11001 + X-Request-ID.
	wNoAuth := doJSONReq(r, http.MethodGet, "/api/v1/friend-requests?direction=received", "", "")
	if wNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d body=%s", wNoAuth.Code, wNoAuth.Body.String())
	}

	// Bad request formats & values -> 400 + 10000 / 10001.
	wBadJSON := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests", "10001", `{bad`)
	if wBadJSON.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on malformed JSON, got %d", wBadJSON.Code)
	}
	wMissingTo := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests", "10001", `{"message":"hi"}`)
	if wMissingTo.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on missing to_user_id, got %d", wMissingTo.Code)
	}
	var missingEnv errcode.Envelope
	_ = json.Unmarshal(wMissingTo.Body.Bytes(), &missingEnv)
	if missingEnv.Code != errcode.ErrInvalidParams.Code {
		t.Fatalf("expected 10000 on missing to_user_id, got %d", missingEnv.Code)
	}
	wInvalidTo := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests", "10001", `{"to_user_id":"0"}`)
	var invalidEnv errcode.Envelope
	_ = json.Unmarshal(wInvalidTo.Body.Bytes(), &invalidEnv)
	if wInvalidTo.Code != http.StatusBadRequest || invalidEnv.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 400/10001 on to_user_id=0, got status=%d code=%d", wInvalidTo.Code, invalidEnv.Code)
	}

	// Valid create (with null message) -> 201 + code 0 + string IDs + X-Request-ID.
	wCreate := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests", "10001", `{"to_user_id":"10002","message":null}`)
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d body=%s", wCreate.Code, wCreate.Body.String())
	}
	var createResp struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
		Data      struct {
			Request struct {
				ID         string  `json:"id"`
				FromUserID string  `json:"from_user_id"`
				ToUserID   string  `json:"to_user_id"`
				Message    string  `json:"message"`
				Status     string  `json:"status"`
				CreatedAt  string  `json:"created_at"`
				HandledAt  *string `json:"handled_at"`
			} `json:"request"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wCreate.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if createResp.Code != 0 || createResp.RequestID == "" || wCreate.Header().Get(errcode.HeaderRequestID) != createResp.RequestID {
		t.Fatalf("unexpected envelope metadata: %+v header=%q", createResp, wCreate.Header().Get(errcode.HeaderRequestID))
	}
	if createResp.Data.Request.ID != "1" || createResp.Data.Request.FromUserID != "10001" || createResp.Data.Request.ToUserID != "10002" || createResp.Data.Request.HandledAt != nil {
		t.Fatalf("unexpected created request payload: %+v", createResp.Data.Request)
	}

	// List with explicit limit=0 -> 400/10001.
	wBadLimit := doJSONReq(r, http.MethodGet, "/api/v1/friend-requests?direction=received&limit=0", "10002", "")
	if wBadLimit.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for explicit limit=0, got %d", wBadLimit.Code)
	}

	// List received for Bob (10002) -> 200 with peer Alice.
	wList := doJSONReq(r, http.MethodGet, "/api/v1/friend-requests?direction=received&status=pending", "10002", "")
	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200 on list, got %d body=%s", wList.Code, wList.Body.String())
	}

	// Invalid path id -> 400/10001.
	wBadID := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests/abc/accept", "10002", "")
	if wBadID.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on non-numeric path id, got %d", wBadID.Code)
	}

	// Accept by Bob -> 200 with decimal string IDs.
	wAccept := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests/1/accept", "10002", "")
	if wAccept.Code != http.StatusOK {
		t.Fatalf("expected 200 on accept, got %d body=%s", wAccept.Code, wAccept.Body.String())
	}
	var acceptResp struct {
		Code int `json:"code"`
		Data struct {
			FriendID       string `json:"friend_id"`
			ConversationID string `json:"conversation_id"`
			SystemMsgID    string `json:"system_msg_id"`
			Request        struct {
				Status    string  `json:"status"`
				HandledAt *string `json:"handled_at"`
			} `json:"request"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wAccept.Body.Bytes(), &acceptResp); err != nil {
		t.Fatalf("unmarshal accept response: %v", err)
	}
	if acceptResp.Data.FriendID != "10001" || acceptResp.Data.ConversationID == "" || acceptResp.Data.SystemMsgID == "" || acceptResp.Data.Request.Status != "accepted" || acceptResp.Data.Request.HandledAt == nil {
		t.Fatalf("unexpected accept payload: %+v", acceptResp.Data)
	}

	// Reject & Cancel endpoints via HTTP.
	wC2 := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests", "10001", `{"to_user_id":"10003","message":"hi charlie"}`)
	if wC2.Code != http.StatusCreated {
		t.Fatalf("expected 201 for req 2, got %d body=%s", wC2.Code, wC2.Body.String())
	}
	wRejForbidden := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests/2/reject", "10001", "")
	if wRejForbidden.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when sender rejects, got %d", wRejForbidden.Code)
	}
	wRej := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests/2/reject", "10003", "")
	if wRej.Code != http.StatusOK {
		t.Fatalf("expected 200 on reject, got %d body=%s", wRej.Code, wRej.Body.String())
	}

	wC3 := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests", "10001", `{"to_user_id":"10003","message":"again"}`)
	if wC3.Code != http.StatusCreated {
		t.Fatalf("expected 201 for req 3, got %d body=%s", wC3.Code, wC3.Body.String())
	}
	wCanForbidden := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests/3/cancel", "10003", "")
	if wCanForbidden.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when recipient cancels, got %d", wCanForbidden.Code)
	}
	wCan := doJSONReq(r, http.MethodPost, "/api/v1/friend-requests/3/cancel", "10001", "")
	if wCan.Code != http.StatusOK {
		t.Fatalf("expected 200 on cancel, got %d body=%s", wCan.Code, wCan.Body.String())
	}
}
