package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

func TestAdminControllerAuthorizationAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctrl := NewAdminController(service.NewAdminService(nil, nil, nil, nil, nil, 0, nil))

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(errcode.ContextKeyRequestID, "req-test")
		if role := c.GetHeader("X-Test-Role"); role != "" {
			c.Set(middleware.ContextKeyAuthContext, middleware.AuthContext{
				UserID: 1,
				Role:   role,
			})
		}
		c.Next()
	})
	r.GET("/api/v1/admin/users", ctrl.ListUsers)
	r.PATCH("/api/v1/admin/users/:user_id/status", ctrl.UpdateUserStatus)
	r.GET("/api/v1/admin/messages/audit", ctrl.ListMessageAudit)

	// Non-admin user must be rejected with 17001 / 403
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	req1.Header.Set("X-Test-Role", model.UserRoleUser)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin, got %d body=%s", w1.Code, w1.Body.String())
	}
	var env1 errcode.Envelope
	if err := json.Unmarshal(w1.Body.Bytes(), &env1); err != nil || env1.Code != errcode.ErrAdminRequired.Code {
		t.Fatalf("expected code 17001, got %s", w1.Body.String())
	}

	// Invalid status query -> 10001
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users?status=9", nil)
	req2.Header.Set("X-Test-Role", model.UserRoleAdmin)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid status query, got %d body=%s", w2.Code, w2.Body.String())
	}

	// Missing status body field -> 10000
	req3 := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/users/2/status", bytes.NewBufferString(`{"reason":"test"}`))
	req3.Header.Set("X-Test-Role", model.UserRoleAdmin)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing status field, got %d body=%s", w3.Code, w3.Body.String())
	}

	// Invalid audit timestamp -> 17005
	req4 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/messages/audit?from=not-a-date", nil)
	req4.Header.Set("X-Test-Role", model.UserRoleAdmin)
	w4 := httptest.NewRecorder()
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid audit from filter, got %d body=%s", w4.Code, w4.Body.String())
	}
	var env4 errcode.Envelope
	if err := json.Unmarshal(w4.Body.Bytes(), &env4); err != nil || env4.Code != errcode.ErrInvalidAuditFilter.Code {
		t.Fatalf("expected code 17005, got %s", w4.Body.String())
	}
}
