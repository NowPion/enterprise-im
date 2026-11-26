package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
)

func TestMiddlewareChainLoggingRedactionAndRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	r := gin.New()
	r.Use(RequestID())
	r.Use(AccessLogger(log))
	r.Use(Recovery(log))
	r.Use(CORS([]string{"http://localhost:5173"}))
	r.Use(MaxBodySize(32))

	r.GET("/ok", func(c *gin.Context) {
		errcode.SetUserID(c, "10001")
		errcode.OK(c, map[string]string{"hello": "world"})
	})
	r.GET("/panic", func(c *gin.Context) {
		panic("boom secret stack detail")
	})

	// 1. Success + header/query redaction + request_id alignment
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/ok?access_token=top-secret-jwt&limit=20", nil)
	req.Header.Set("Authorization", "Bearer super-secret-bearer")
	req.Header.Set("Origin", "http://localhost:5173")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	reqID := w.Header().Get(errcode.HeaderRequestID)
	if !strings.HasPrefix(reqID, "r-") {
		t.Fatalf("expected generated X-Request-ID, got %q", reqID)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatalf("expected CORS Allow-Origin header, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}

	var env errcode.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Code != 0 || env.RequestID != reqID {
		t.Fatalf("envelope mismatch: %+v vs header %q", env, reqID)
	}

	rawLogs := logBuf.String()
	if strings.Contains(rawLogs, "super-secret-bearer") || strings.Contains(rawLogs, "top-secret-jwt") {
		t.Fatalf("secret leaked in access log: %s", rawLogs)
	}
	if !strings.Contains(rawLogs, reqID) || !strings.Contains(rawLogs, `"user_id":"10001"`) || !strings.Contains(rawLogs, `"latency_ms"`) {
		t.Fatalf("missing required access log fields: %s", rawLogs)
	}

	// 2. Panic recovery returns unified JSON 500 with code 19004 and does not leak panic string
	wPanic := httptest.NewRecorder()
	reqPanic, _ := http.NewRequest(http.MethodGet, "/panic", nil)
	reqPanic.Header.Set(errcode.HeaderRequestID, "req_custom_99")
	r.ServeHTTP(wPanic, reqPanic)

	if wPanic.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on panic, got %d", wPanic.Code)
	}
	if strings.Contains(wPanic.Body.String(), "boom secret stack detail") {
		t.Fatalf("panic detail leaked to client: %s", wPanic.Body.String())
	}
	var panicEnv errcode.Envelope
	if err := json.Unmarshal(wPanic.Body.Bytes(), &panicEnv); err != nil {
		t.Fatalf("unmarshal panic envelope: %v", err)
	}
	if panicEnv.Code != errcode.ErrInternalConsistency.Code || panicEnv.RequestID != "req_custom_99" {
		t.Fatalf("unexpected panic envelope: %+v", panicEnv)
	}

	// 3. Payload limit returns 413 with code 10002
	wBig := httptest.NewRecorder()
	reqBig, _ := http.NewRequest(http.MethodPost, "/ok", strings.NewReader(strings.Repeat("a", 64)))
	r.ServeHTTP(wBig, reqBig)
	if wBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", wBig.Code)
	}
	var bigEnv errcode.Envelope
	if err := json.Unmarshal(wBig.Body.Bytes(), &bigEnv); err != nil {
		t.Fatalf("unmarshal 413 envelope: %v", err)
	}
	if bigEnv.Code != errcode.ErrPayloadTooLarge.Code {
		t.Fatalf("expected code 10002, got %d", bigEnv.Code)
	}

	// 4. CORS preflight + disallowed origin
	wOpt := httptest.NewRecorder()
	reqOpt, _ := http.NewRequest(http.MethodOptions, "/ok", nil)
	reqOpt.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(wOpt, reqOpt)
	if wOpt.Code != http.StatusNoContent || wOpt.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed origin should not receive allow-origin: code=%d allow=%q", wOpt.Code, wOpt.Header().Get("Access-Control-Allow-Origin"))
	}
}
