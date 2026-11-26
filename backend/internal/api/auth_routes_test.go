package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type routeTestStore struct{}

func (routeTestStore) WithTx(_ context.Context, _ func(tx service.AuthTxStore) error) error {
	return nil
}
func (routeTestStore) CreateUser(context.Context, *model.User) error { return nil }
func (routeTestStore) FindUserByUsername(context.Context, string) (*model.User, error) {
	return nil, repository.ErrAuthUserNotFound
}
func (routeTestStore) FindUserByID(context.Context, uint64) (*model.User, error) {
	return nil, repository.ErrAuthUserNotFound
}
func (routeTestStore) FindSessionByRefreshHash(context.Context, string) (*model.UserSession, error) {
	return nil, repository.ErrSessionNotFound
}
func (routeTestStore) BlacklistAccessJTIs(context.Context, []string, string, time.Duration) error {
	return nil
}
func (routeTestStore) AcquireRefreshLock(context.Context, string, time.Duration) (string, bool, error) {
	return "", true, nil
}
func (routeTestStore) IsRefreshLockHeld(context.Context, string) (bool, error) { return false, nil }
func (routeTestStore) ReleaseRefreshLock(context.Context, string, string) error { return nil }
func (routeTestStore) IsUserOnline(context.Context, uint64) (bool, error)       { return false, nil }

func TestRegisterAuthRoutesMountsExpectedPathsAndGuardsMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)

	jwtCfg := config.JWTConfig{
		Secret:     "routes-unit-test-secret-32-bytes!",
		Issuer:     "enterprise-im-test",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}
	mgr, err := pkgjwt.NewManager(jwtCfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	authSvc := service.NewAuthService(routeTestStore{}, mgr, &config.AuthConfig{
		JWT:                jwtCfg,
		MaxSessionsPerUser: 5,
		BcryptCost:         10,
	}, log, nil)
	authCtrl := controller.NewAuthController(authSvc)

	// Panic when jwtAuth is nil so protected routes can never be mounted unprotected.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic when RegisterAuthRoutes called with nil jwtAuth")
			}
		}()
		r := gin.New()
		RegisterAuthRoutes(r, authCtrl, nil)
	}()

	r := NewRouter(nil, log, nil)
	jwtRejectMW := func(c *gin.Context) {
		errcode.Fail(c, errcode.ErrAccessTokenInvalid)
		c.Abort()
	}
	RegisterAuthRoutes(r, authCtrl, jwtRejectMW)

	// Protected routes invoke jwtAuth middleware.
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/auth/logout"},
		{http.MethodPut, "/api/v1/me/password"},
	} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s expected 401 from jwtAuth, got %d", tc.method, tc.path, w.Code)
		}
	}

	// Public routes reach controller validation (400/10000 on missing fields) without JWT.
	for _, path := range []string{"/api/v1/auth/register", "/api/v1/auth/login", "/api/v1/auth/refresh"} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("POST %s expected 400 on empty JSON, got %d body=%s", path, w.Code, w.Body.String())
		}
	}
}
