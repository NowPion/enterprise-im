package api

import (
	"encoding/json"
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

func TestRegisterConversationRoutesFailClosedAndMountCleanly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)

	convCtrl := controller.NewConversationController(service.NewConversationService(nil, nil, log))
	msgCtrl := controller.NewMessageController(service.NewMessageService(nil, nil, nil, nil, nil, 0, log))
	receiptCtrl := controller.NewReceiptController(service.NewReceiptService(nil, nil, nil, log))

	// Nil jwtAuth fails closed with 401/11001.
	closed := NewRouter(&config.Config{}, log, nil)
	RegisterConversationRoutes(closed, nil, convCtrl, msgCtrl)
	RegisterReceiptRoutes(closed, nil, receiptCtrl)

	chatEndpoints := [][2]string{
		{http.MethodGet, "/api/v1/conversations"},
		{http.MethodGet, "/api/v1/conversations/90001"},
		{http.MethodPatch, "/api/v1/conversations/90001/settings"},
		{http.MethodGet, "/api/v1/conversations/90001/members"},
		{http.MethodGet, "/api/v1/conversations/90001/messages"},
		{http.MethodGet, "/api/v1/conversations/90001/sync"},
		{http.MethodPost, "/api/v1/conversations/90001/messages"},
		{http.MethodPost, "/api/v1/conversations/90001/messages/1/recall"},
		{http.MethodGet, "/api/v1/conversations/90001/messages/1/receipts"},
		{http.MethodPost, "/api/v1/conversations/90001/read"},
		{http.MethodPost, "/api/v1/conversations/90001/delivered"},
	}

	for _, ep := range chatEndpoints {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(ep[0], ep[1], nil)
		closed.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s expected 401 without jwtAuth, got %d", ep[0], ep[1], w.Code)
		}
		var env errcode.Envelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != errcode.ErrAccessTokenInvalid.Code {
			t.Fatalf("%s %s expected 11001, got %d", ep[0], ep[1], env.Code)
		}
	}

	// Out-of-scope routes must NOT be registered on conversation router.
	unregistered := [][2]string{
		{http.MethodPost, "/api/v1/groups"},
		{http.MethodPost, "/api/v1/uploads"},
	}
	for _, ep := range unregistered {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(ep[0], ep[1], nil)
		closed.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s should NOT be registered on conversation router, got %d", ep[0], ep[1], w.Code)
		}
	}

	// Verify /api/v1 group mounting avoids duplicate /api/v1/api/v1 prefix.
	grouped := NewRouter(&config.Config{}, log, nil)
	v1 := grouped.Group("/api/v1")
	RegisterConversationRoutes(v1, nil, convCtrl, msgCtrl)
	RegisterReceiptRoutes(v1, nil, receiptCtrl)
	for _, ep := range [][2]string{
		{http.MethodGet, "/api/v1/conversations"},
		{http.MethodPost, "/api/v1/conversations/90001/read"},
		{http.MethodPost, "/api/v1/conversations/90001/delivered"},
		{http.MethodGet, "/api/v1/conversations/90001/messages/1/receipts"},
	} {
		wGrp := httptest.NewRecorder()
		reqGrp, _ := http.NewRequest(ep[0], ep[1], nil)
		grouped.ServeHTTP(wGrp, reqGrp)
		if wGrp.Code != http.StatusUnauthorized {
			t.Fatalf("v1 mount %s %s expected 401 from nil jwtAuth guard, got %d", ep[0], ep[1], wGrp.Code)
		}
	}
}
