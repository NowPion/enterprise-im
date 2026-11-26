package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/api"
	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/ws"
)

type wireFrame struct {
	Type      string          `json:"type"`
	ReqID     string          `json:"req_id"`
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

func dialPipeServer(t *testing.T, handler http.Handler) *websocket.Dialer {
	t.Helper()
	listener := newPipeListener()
	srv := &http.Server{Handler: handler}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return &websocket.Dialer{NetDialContext: listener.DialContext, HandshakeTimeout: 3 * time.Second}
}

func readWire(t *testing.T, conn *websocket.Conn) wireFrame {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var msg wireFrame
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type == "" || msg.Timestamp <= 0 || len(msg.Data) == 0 {
		t.Fatal("invalid response envelope")
	}
	return msg
}

func expectClose(t *testing.T, conn *websocket.Conn, code int) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	_, _, err := conn.ReadMessage()
	if !websocket.IsCloseError(err, code) {
		t.Fatalf("expected close %d, got %v", code, err)
	}
}

func TestWebSocketRejectsUnauthenticatedUpgrades(t *testing.T) {
	cfg, err := config.Load("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	authCfg, err := config.LoadAuthConfig("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := pkgjwt.NewManager(authCfg.JWT)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	auth := middleware.NewAccessAuthenticator(manager, middleware.NewAuthStateStore(nil, nil))
	hub := ws.NewHub(cfg, auth, nil, log)
	router := api.NewRouter(cfg, log, nil)
	api.RegisterWSRoute(router, cfg, hub)
	handled := make(chan struct{}, 1)
	dialer := dialPipeServer(t, http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		router.ServeHTTP(writer, req)
		handled <- struct{}{}
	}))
	signed, _, _, err := manager.IssueAccessToken(42, 81, "user")
	if err != nil {
		t.Fatal("could not issue test JWT")
	}
	for _, tc := range []struct {
		suffix, bearer string
		code, close    int
	}{
		{code: 18001, close: 1008},
		{suffix: "?access_token=query-marker", code: 18001, close: 1008},
		{bearer: "header-marker", code: 18001, close: 1008},
		{bearer: signed, code: 18007, close: 1011},
	} {
		header := http.Header{"X-Request-ID": []string{"ws-handshake-test"}}
		if tc.bearer != "" {
			header.Set("Authorization", "Bearer "+tc.bearer)
		}
		conn, _, err := dialer.DialContext(context.Background(), "ws://stage5.local/ws"+tc.suffix, header)
		if err != nil {
			t.Fatal("expected completed WS upgrade")
		}
		msg := readWire(t, conn)
		var failure struct {
			Code    int
			Message string
		}
		if json.Unmarshal(msg.Data, &failure) != nil || msg.Type != "error" || msg.ReqID != "ws-handshake-test" || failure.Code != tc.code || (tc.code == 18001 && !strings.Contains(failure.Message, "11001")) {
			t.Fatal("incorrect handshake error", msg.Type, string(msg.Data))
		}
		expectClose(t, conn, tc.close)
		_ = conn.Close()
		<-handled
	}
	for _, credential := range []string{"query-marker", "header-marker", signed} {
		if strings.Contains(logs.String(), credential) {
			t.Fatal("credential present in handshake logs")
		}
	}
}
