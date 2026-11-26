package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/service"
)

func TestRegisterFriendRequestRoutesMountTargetsAndJWTStub(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg, err := config.Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	ctrl := controller.NewFriendRequestController(service.NewFriendRequestService(nil, nil, nil, nil, nil, log))
	jwtStub := func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		errcode.SetUserID(c, "10001")
		c.Next()
	}

	// 1. Mounted directly on root *gin.Engine.
	rootEngine := NewRouter(cfg, log, nil)
	RegisterFriendRequestRoutes(rootEngine, jwtStub, ctrl)

	expected := [][2]string{
		{http.MethodPost, "/api/v1/friend-requests"},
		{http.MethodGet, "/api/v1/friend-requests"},
		{http.MethodPost, "/api/v1/friend-requests/42/accept"},
		{http.MethodPost, "/api/v1/friend-requests/42/reject"},
		{http.MethodPost, "/api/v1/friend-requests/42/cancel"},
	}

	for _, tc := range expected {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(tc[0], tc[1], nil)
		rootEngine.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("root mount %s %s expected 401 from jwtStub, got %d body=%s", tc[0], tc[1], w.Code, w.Body.String())
		}
	}

	// 2. Mounted on /api/v1 group (verifies no double /api/v1/api/v1 prefix).
	groupedEngine := NewRouter(cfg, log, nil)
	v1 := groupedEngine.Group("/api/v1")
	RegisterFriendRequestRoutes(v1, jwtStub, ctrl)

	for _, tc := range expected {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(tc[0], tc[1], nil)
		groupedEngine.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("v1 group mount %s %s expected 401 from jwtStub, got %d body=%s", tc[0], tc[1], w.Code, w.Body.String())
		}
	}
}
