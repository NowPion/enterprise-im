package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type ctrlStubUserStore struct {
	users map[uint64]model.User
}

func (s *ctrlStubUserStore) GetByID(_ context.Context, userID uint64) (*model.User, error) {
	u, ok := s.users[userID]
	if !ok {
		return nil, repository.ErrUserDisabled
	}
	cp := u
	return &cp, nil
}

func (s *ctrlStubUserStore) UpdateProfile(_ context.Context, userID uint64, updates map[string]any) (*model.User, error) {
	u, ok := s.users[userID]
	if !ok {
		return nil, repository.ErrUserDisabled
	}
	if nick, ok := updates["nickname"].(string); ok {
		u.Nickname = nick
	}
	if sig, ok := updates["signature"].(string); ok {
		u.Signature = sig
	}
	s.users[userID] = u
	cp := u
	return &cp, nil
}

func (s *ctrlStubUserStore) Search(_ context.Context, params repository.UserSearchParams) ([]model.User, error) {
	var res []model.User
	for _, u := range s.users {
		if u.Status != model.UserStatusActive {
			continue
		}
		if params.Cursor > 0 && u.ID >= params.Cursor {
			continue
		}
		val := u.Username
		if params.Field == repository.UserSearchFieldNickname {
			val = u.Nickname
		}
		if (params.Match == repository.UserSearchMatchExact && val == params.Query) ||
			(params.Match == repository.UserSearchMatchPrefix && strings.HasPrefix(val, params.Query)) {
			res = append(res, u)
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].ID > res[j].ID })
	if params.Limit > 0 && len(res) > params.Limit {
		res = res[:params.Limit]
	}
	return res, nil
}

type ctrlStubPresence struct {
	online map[uint64]bool
}

func (p *ctrlStubPresence) IsUserOnline(_ context.Context, userID uint64) (bool, error) {
	return p.online[userID], nil
}

func (p *ctrlStubPresence) GetUsersOnline(_ context.Context, userIDs []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(userIDs))
	for _, id := range userIDs {
		out[id] = p.online[id]
	}
	return out, nil
}

func newControllerTestRouter(ctrl *UserController, uid any, sid any, did string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(errcode.ContextKeyRequestID, "r-ctrl-test")
		if uid != nil {
			c.Set(errcode.ContextKeyUserID, uid)
		}
		if sid != nil {
			c.Set(ContextKeySessionID, sid)
		}
		if did != "" {
			c.Set(ContextKeyDeviceID, did)
		}
		c.Next()
	})
	r.GET("/api/v1/me", ctrl.GetMe)
	r.PATCH("/api/v1/me", ctrl.PatchMe)
	r.GET("/api/v1/users/search", ctrl.SearchUsers)
	return r
}

func TestUserControllerHandlers(t *testing.T) {
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	store := &ctrlStubUserStore{
		users: map[uint64]model.User{
			10001: {
				ID:        10001,
				Username:  "alice",
				Nickname:  "Alice",
				Signature: "hi",
				Role:      model.UserRoleUser,
				Status:    model.UserStatusActive,
			},
			10002: {
				ID:       10002,
				Username: "alice_two",
				Nickname: "Alice Two",
				Role:     model.UserRoleUser,
				Status:   model.UserStatusActive,
			},
		},
	}
	svc := service.NewUserService(store, &ctrlStubPresence{online: map[uint64]bool{10001: true}}, log)
	ctrl := NewUserController(svc)

	// 1. Unauthenticated context -> 401 + 11001
	rUnauth := newControllerTestRouter(ctrl, nil, nil, "")
	wUnauth := httptest.NewRecorder()
	reqUnauth, _ := http.NewRequest(http.MethodGet, "/api/v1/me", nil)
	rUnauth.ServeHTTP(wUnauth, reqUnauth)
	if wUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", wUnauth.Code, wUnauth.Body.String())
	}

	// 2. Numeric context types supported cleanly for user_id/session_id
	rAuth := newControllerTestRouter(ctrl, uint64(10001), uint64(5001), "dev-1")
	wMe := httptest.NewRecorder()
	reqMe, _ := http.NewRequest(http.MethodGet, "/api/v1/me", nil)
	rAuth.ServeHTTP(wMe, reqMe)
	if wMe.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /me, got %d body=%s", wMe.Code, wMe.Body.String())
	}
	var meEnv struct {
		Code int                 `json:"code"`
		Data service.GetMeResult `json:"data"`
	}
	if err := json.Unmarshal(wMe.Body.Bytes(), &meEnv); err != nil {
		t.Fatalf("unmarshal GET /me: %v", err)
	}
	if meEnv.Code != 0 || meEnv.Data.SessionID != "5001" || meEnv.Data.DeviceID != "dev-1" || !meEnv.Data.User.Online {
		t.Fatalf("unexpected GET /me envelope: %+v", meEnv)
	}

	// 3. PATCH /api/v1/me rejects null field and empty JSON body with 400 + 10000
	for _, badBody := range []string{``, `{}`, `{"nickname":null}`, `{"signature":null}`, `{"nickname":123}`, `not-json`} {
		wBad := httptest.NewRecorder()
		reqBad, _ := http.NewRequest(http.MethodPatch, "/api/v1/me", strings.NewReader(badBody))
		reqBad.Header.Set("Content-Type", "application/json")
		rAuth.ServeHTTP(wBad, reqBad)
		if wBad.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for body %q, got %d body=%s", badBody, wBad.Code, wBad.Body.String())
		}
		var errEnv errcode.Envelope
		_ = json.Unmarshal(wBad.Body.Bytes(), &errEnv)
		if errEnv.Code != errcode.ErrInvalidParams.Code {
			t.Fatalf("expected code 10000 for body %q, got %d", badBody, errEnv.Code)
		}
	}

	// 4. PATCH /api/v1/me clears signature with empty string
	wPatch := httptest.NewRecorder()
	reqPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/me", strings.NewReader(`{"signature":""}`))
	reqPatch.Header.Set("Content-Type", "application/json")
	rAuth.ServeHTTP(wPatch, reqPatch)
	if wPatch.Code != http.StatusOK {
		t.Fatalf("expected 200 on PATCH /me, got %d body=%s", wPatch.Code, wPatch.Body.String())
	}
	var patchEnv struct {
		Code int                    `json:"code"`
		Data service.UpdateMeResult `json:"data"`
	}
	if err := json.Unmarshal(wPatch.Body.Bytes(), &patchEnv); err != nil {
		t.Fatalf("unmarshal PATCH /me: %v", err)
	}
	if patchEnv.Data.User.Signature != "" || patchEnv.Data.User.Nickname != "Alice" {
		t.Fatalf("unexpected PATCH /me result: %+v", patchEnv.Data)
	}

	// 5. GET /api/v1/users/search rejects non-numeric/out-of-range limit with 400 + 10001
	for _, badLimit := range []string{"0", "-1", "101", "abc"} {
		wLim := httptest.NewRecorder()
		reqLim, _ := http.NewRequest(http.MethodGet, "/api/v1/users/search?q=alice&limit="+badLimit, nil)
		rAuth.ServeHTTP(wLim, reqLim)
		if wLim.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for limit=%s, got %d", badLimit, wLim.Code)
		}
		var errEnv errcode.Envelope
		_ = json.Unmarshal(wLim.Body.Bytes(), &errEnv)
		if errEnv.Code != errcode.ErrInvalidValue.Code {
			t.Fatalf("expected code 10001 for limit=%s, got %d", badLimit, errEnv.Code)
		}
	}

	// 6. GET /api/v1/users/search valid request
	wSearch := httptest.NewRecorder()
	reqSearch, _ := http.NewRequest(http.MethodGet, "/api/v1/users/search?q=alice&field=username&match=prefix&limit=1", nil)
	rAuth.ServeHTTP(wSearch, reqSearch)
	if wSearch.Code != http.StatusOK {
		t.Fatalf("expected 200 on search, got %d body=%s", wSearch.Code, wSearch.Body.String())
	}
	var searchEnv struct {
		Code int                    `json:"code"`
		Data service.UserSearchPage `json:"data"`
	}
	if err := json.Unmarshal(wSearch.Body.Bytes(), &searchEnv); err != nil {
		t.Fatalf("unmarshal search response: %v", err)
	}
	if !searchEnv.Data.HasMore || searchEnv.Data.NextCursor == nil || len(searchEnv.Data.Items) != 1 || searchEnv.Data.Items[0].ID != "10002" {
		t.Fatalf("unexpected search page: %+v", searchEnv.Data)
	}
}
