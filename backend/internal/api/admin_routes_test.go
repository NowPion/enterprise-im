package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

func TestRegisterAdminRoutesGuardsNonAdminAndUnauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	jwtStub := func(c *gin.Context) {
		role := c.GetHeader("X-Test-Role")
		if role == "" {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		c.Set(middleware.ContextKeyAuthContext, middleware.AuthContext{UserID: 2, Role: role})
		c.Next()
	}

	ctrl := controller.NewAdminController(service.NewAdminService(nil, nil, nil, nil, nil, 0, nil))
	RegisterAdminRoutes(r, jwtStub, ctrl)

	// Unauthenticated -> 401 / 11001
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/api/v1/admin/metrics/overview", nil))
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w1.Code)
	}

	// Normal user -> 403 / 17001
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/metrics/overview", nil)
	req2.Header.Set("X-Test-Role", model.UserRoleUser)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w2.Code)
	}
	var env errcode.Envelope
	if err := json.Unmarshal(w2.Body.Bytes(), &env); err != nil || env.Code != errcode.ErrAdminRequired.Code {
		t.Fatalf("expected 17001, got %s", w2.Body.String())
	}

	// Admin reaches handler (nil repo yields 500 internal consistency rather than 401/403/404)
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/metrics/overview", nil)
	req3.Header.Set("X-Test-Role", model.UserRoleAdmin)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code == http.StatusUnauthorized || w3.Code == http.StatusForbidden || w3.Code == http.StatusNotFound {
		t.Fatalf("expected admin request to route to controller, got %d", w3.Code)
	}
}
