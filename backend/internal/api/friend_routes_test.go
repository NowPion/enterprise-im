package api

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
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type apiFriendStoreStub struct {
	users       map[uint64]model.User
	friendships map[[2]uint64]model.Friendship
	convs       map[string]model.Conversation
}

func (s *apiFriendStoreStub) ListActiveFriendships(_ context.Context, userID uint64, _ uint64, _ int) ([]model.Friendship, error) {
	var out []model.Friendship
	for _, f := range s.friendships {
		if f.UserID == userID && f.Status == model.FriendshipStatusActive {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *apiFriendStoreStub) BatchGetUsersByIDs(_ context.Context, ids []uint64) (map[uint64]model.User, error) {
	out := make(map[uint64]model.User, len(ids))
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (s *apiFriendStoreStub) BatchGetSingleConversationsByDirectKeys(_ context.Context, keys []string) (map[string]model.Conversation, error) {
	out := make(map[string]model.Conversation, len(keys))
	for _, k := range keys {
		if c, ok := s.convs[k]; ok {
			out[k] = c
		}
	}
	return out, nil
}

func (s *apiFriendStoreStub) GetOnlineStatusBatch(_ context.Context, ids []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		out[id] = false
	}
	return out, nil
}

func (s *apiFriendStoreStub) AreActiveFriends(_ context.Context, a, b uint64) (bool, error) {
	return s.friendships[[2]uint64{a, b}].Status == 1 && s.friendships[[2]uint64{b, a}].Status == 1, nil
}

func (s *apiFriendStoreStub) UpdateRemark(_ context.Context, userID, friendID uint64, remark string) error {
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

func (s *apiFriendStoreStub) DeactivateBidirectionalFriendship(_ context.Context, userID, friendID uint64) (repository.DeleteFriendshipOutcome, error) {
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

func (s *apiFriendStoreStub) FindSingleConversationByDirectKey(_ context.Context, dk string) (*model.Conversation, error) {
	c, ok := s.convs[dk]
	if !ok {
		return nil, errors.New("not found")
	}
	return &c, nil
}

func TestRegisterFriendRoutesFailClosedWithoutJWTAndDispatchWithStub(t *testing.T) {
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	dk := "10001:10002"
	store := &apiFriendStoreStub{
		users: map[uint64]model.User{
			10002: {ID: 10002, Username: "bob", Nickname: "Bob"},
		},
		friendships: map[[2]uint64]model.Friendship{
			{10001, 10002}: {ID: 1, UserID: 10001, FriendID: 10002, Status: 1},
			{10002, 10001}: {ID: 2, UserID: 10002, FriendID: 10001, Status: 1},
		},
		convs: map[string]model.Conversation{
			dk: {ID: 90001, Type: model.ConversationTypeSingle, DirectKey: &dk},
		},
	}
	ctrl := controller.NewFriendController(service.NewFriendService(store, log))

	// 1. Nil jwtAuth fails closed with 401 + 11001.
	rClosed := NewRouter(&config.Config{}, log, nil)
	RegisterFriendRoutes(rClosed, nil, ctrl)

	wClosed := httptest.NewRecorder()
	reqClosed, _ := http.NewRequest(http.MethodGet, "/api/v1/friends", nil)
	rClosed.ServeHTTP(wClosed, reqClosed)
	if wClosed.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when jwtAuth is nil, got %d body=%s", wClosed.Code, wClosed.Body.String())
	}
	var envClosed errcode.Envelope
	_ = json.Unmarshal(wClosed.Body.Bytes(), &envClosed)
	if envClosed.Code != errcode.ErrAccessTokenInvalid.Code {
		t.Fatalf("expected 11001, got %d", envClosed.Code)
	}

	// 2. Stub jwtAuth enables GET/PATCH/DELETE /api/v1/friends* and keeps friend-requests unregistered.
	r := NewRouter(&config.Config{}, log, nil)
	jwtStub := func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer user-") {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		errcode.SetUserID(c, strings.TrimPrefix(auth, "Bearer user-"))
		c.Next()
	}
	RegisterFriendRoutes(r, jwtStub, ctrl)

	wList := httptest.NewRecorder()
	reqList, _ := http.NewRequest(http.MethodGet, "/api/v1/friends", nil)
	reqList.Header.Set("Authorization", "Bearer user-10001")
	r.ServeHTTP(wList, reqList)
	if wList.Code != http.StatusOK || wList.Header().Get(errcode.HeaderRequestID) == "" {
		t.Fatalf("GET /api/v1/friends failed: status=%d body=%s", wList.Code, wList.Body.String())
	}

	wPatch := httptest.NewRecorder()
	reqPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/friends/10002", strings.NewReader(`{"remark":"Buddy"}`))
	reqPatch.Header.Set("Authorization", "Bearer user-10001")
	reqPatch.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wPatch, reqPatch)
	if wPatch.Code != http.StatusOK {
		t.Fatalf("PATCH /api/v1/friends/10002 failed: status=%d body=%s", wPatch.Code, wPatch.Body.String())
	}

	wDel := httptest.NewRecorder()
	reqDel, _ := http.NewRequest(http.MethodDelete, "/api/v1/friends/10002", nil)
	reqDel.Header.Set("Authorization", "Bearer user-10001")
	r.ServeHTTP(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("DELETE /api/v1/friends/10002 failed: status=%d body=%s", wDel.Code, wDel.Body.String())
	}

	wNotReg := httptest.NewRecorder()
	reqNotReg, _ := http.NewRequest(http.MethodGet, "/api/v1/friend-requests", nil)
	reqNotReg.Header.Set("Authorization", "Bearer user-10001")
	r.ServeHTTP(wNotReg, reqNotReg)
	if wNotReg.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unregistered /api/v1/friend-requests, got %d", wNotReg.Code)
	}
}
