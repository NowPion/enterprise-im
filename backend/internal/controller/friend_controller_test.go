package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type controllerFriendStore struct {
	users       map[uint64]model.User
	friendships map[[2]uint64]model.Friendship
	convs       map[string]model.Conversation
	members     map[[2]uint64]model.ConversationMember
}

func newControllerFriendStore() *controllerFriendStore {
	return &controllerFriendStore{
		users:       make(map[uint64]model.User),
		friendships: make(map[[2]uint64]model.Friendship),
		convs:       make(map[string]model.Conversation),
		members:     make(map[[2]uint64]model.ConversationMember),
	}
}

func (s *controllerFriendStore) ListActiveFriendships(_ context.Context, userID uint64, _ uint64, _ int) ([]model.Friendship, error) {
	var out []model.Friendship
	for _, f := range s.friendships {
		if f.UserID == userID && f.Status == model.FriendshipStatusActive {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *controllerFriendStore) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
	out := make(map[uint64]model.User, len(ids))
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *controllerFriendStore) BatchGetSingleConversationsByDirectKeys(_ context.Context, keys []string) (map[string]model.Conversation, error) {
	out := make(map[string]model.Conversation, len(keys))
	for _, k := range keys {
		if c, ok := s.convs[k]; ok {
			out[k] = c
		}
	}
	return out, nil
}

func (s *controllerFriendStore) GetOnlineStatusBatch(_ context.Context, ids []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		out[id] = false
	}
	return out, nil
}

func (s *controllerFriendStore) AreActiveFriends(_ context.Context, a, b uint64) (bool, error) {
	return s.friendships[[2]uint64{a, b}].Status == 1 && s.friendships[[2]uint64{b, a}].Status == 1, nil
}

func (s *controllerFriendStore) UpdateRemark(_ context.Context, userID, friendID uint64, remark string) error {
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

func (s *controllerFriendStore) DeactivateBidirectionalFriendship(_ context.Context, userID, friendID uint64) (repository.DeleteFriendshipOutcome, error) {
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
	return repository.DeleteFriendshipOutcome{
		FriendID:       friendID,
		ConversationID: &convID,
		IsVisible:      false,
	}, nil
}

func (s *controllerFriendStore) FindSingleConversationByDirectKey(_ context.Context, dk string) (*model.Conversation, error) {
	c, ok := s.convs[dk]
	if !ok {
		return nil, errors.New("not found")
	}
	return &c, nil
}

func TestFriendControllerValidationAndResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	store := newControllerFriendStore()
	dk := "10001:10002"
	store.users[10002] = model.User{ID: 10002, Username: "bob", Nickname: "Bob"}
	store.friendships[[2]uint64{10001, 10002}] = model.Friendship{ID: 10, UserID: 10001, FriendID: 10002, Status: 1}
	store.friendships[[2]uint64{10002, 10001}] = model.Friendship{ID: 11, UserID: 10002, FriendID: 10001, Status: 1}
	store.convs[dk] = model.Conversation{ID: 90001, Type: model.ConversationTypeSingle, DirectKey: &dk}

	ctrl := NewFriendController(service.NewFriendService(store, log))

	r := gin.New()
	r.Use(middleware.RequestID())
	auth := func(c *gin.Context) {
		if uid := c.GetHeader("X-Test-User"); uid != "" {
			errcode.SetUserID(c, uid)
		}
		c.Next()
	}
	g := r.Group("/api/v1/friends", auth)
	g.GET("", ctrl.List)
	g.PATCH("/:friend_id", ctrl.UpdateRemark)
	g.DELETE("/:friend_id", ctrl.Delete)

	// Missing auth -> 401 + 11001
	wNoAuth := httptest.NewRecorder()
	reqNoAuth, _ := http.NewRequest(http.MethodGet, "/api/v1/friends", nil)
	r.ServeHTTP(wNoAuth, reqNoAuth)
	if wNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without user_id, got %d", wNoAuth.Code)
	}

	// Invalid limit -> 400 + 10001
	wBadLimit := httptest.NewRecorder()
	reqBadLimit, _ := http.NewRequest(http.MethodGet, "/api/v1/friends?limit=101", nil)
	reqBadLimit.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wBadLimit, reqBadLimit)
	if wBadLimit.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for limit=101, got %d body=%s", wBadLimit.Code, wBadLimit.Body.String())
	}
	var badEnv errcode.Envelope
	_ = json.Unmarshal(wBadLimit.Body.Bytes(), &badEnv)
	if badEnv.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected code 10001, got %d", badEnv.Code)
	}

	// PATCH missing remark field -> 400 + 10000
	wMissingRemark := httptest.NewRecorder()
	reqMissingRemark, _ := http.NewRequest(http.MethodPatch, "/api/v1/friends/10002", strings.NewReader(`{}`))
	reqMissingRemark.Header.Set("X-Test-User", "10001")
	reqMissingRemark.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wMissingRemark, reqMissingRemark)
	if wMissingRemark.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty patch body, got %d", wMissingRemark.Code)
	}
	var missingEnv errcode.Envelope
	_ = json.Unmarshal(wMissingRemark.Body.Bytes(), &missingEnv)
	if missingEnv.Code != errcode.ErrInvalidParams.Code {
		t.Fatalf("expected 10000 for missing remark, got %d", missingEnv.Code)
	}

	// PATCH invalid friend_id -> 400 + 10001
	wBadID := httptest.NewRecorder()
	reqBadID, _ := http.NewRequest(http.MethodPatch, "/api/v1/friends/not-a-number", strings.NewReader(`{"remark":"ok"}`))
	reqBadID.Header.Set("X-Test-User", "10001")
	reqBadID.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wBadID, reqBadID)
	if wBadID.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-numeric friend_id, got %d", wBadID.Code)
	}

	// PATCH success -> 200
	wPatch := httptest.NewRecorder()
	reqPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/friends/10002", strings.NewReader(`{"remark":"Teammate"}`))
	reqPatch.Header.Set("X-Test-User", "10001")
	reqPatch.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wPatch, reqPatch)
	if wPatch.Code != http.StatusOK || store.friendships[[2]uint64{10001, 10002}].Remark != "Teammate" {
		t.Fatalf("patch failed: status=%d body=%s", wPatch.Code, wPatch.Body.String())
	}

	// DELETE success -> 200
	wDel := httptest.NewRecorder()
	reqDel, _ := http.NewRequest(http.MethodDelete, "/api/v1/friends/10002", nil)
	reqDel.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("delete failed: status=%d body=%s", wDel.Code, wDel.Body.String())
	}
}
