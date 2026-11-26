package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/api"
	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/repository"
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
		suffix, bearer, subCode string
		status, code            int
	}{
		{status: http.StatusUnauthorized, code: 18001, subCode: "11001"},
		{suffix: "?access_token=query-marker", status: http.StatusUnauthorized, code: 18001, subCode: "11001"},
		{bearer: "header-marker", status: http.StatusUnauthorized, code: 18001, subCode: "11001"},
		{bearer: signed, status: http.StatusInternalServerError, code: 18007},
	} {
		header := http.Header{"X-Request-ID": []string{"ws-handshake-test"}}
		if tc.bearer != "" {
			header.Set("Authorization", "Bearer "+tc.bearer)
		}
		conn, resp, err := dialer.DialContext(context.Background(), "ws://stage5.local/ws"+tc.suffix, header)
		if conn != nil {
			_ = conn.Close()
			t.Fatalf("expected handshake rejection before upgrade, got open WS connection")
		}
		if !errors.Is(err, websocket.ErrBadHandshake) || resp == nil {
			t.Fatalf("expected websocket.ErrBadHandshake with HTTP response, got conn=%v resp=%v err=%v", conn, resp, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read handshake error body: %v", readErr)
		}
		if resp.StatusCode != tc.status {
			t.Fatalf("expected HTTP status %d, got %d body=%s", tc.status, resp.StatusCode, string(body))
		}
		if resp.Header.Get(errcode.HeaderRequestID) != "ws-handshake-test" {
			t.Fatalf("expected X-Request-ID ws-handshake-test, got %q", resp.Header.Get(errcode.HeaderRequestID))
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("expected application/json response, got %q", resp.Header.Get("Content-Type"))
		}
		var env errcode.Envelope
		if json.Unmarshal(body, &env) != nil || env.Code != tc.code || env.RequestID != "ws-handshake-test" || (tc.subCode != "" && !strings.Contains(env.Message, tc.subCode)) {
			t.Fatalf("incorrect handshake error envelope: status=%d body=%s", resp.StatusCode, string(body))
		}
		<-handled
	}
	for _, credential := range []string{"query-marker", "header-marker", signed} {
		if strings.Contains(logs.String(), credential) {
			t.Fatal("credential present in handshake logs")
		}
	}
}

type stubAuthStateStore struct {
	status  int8
	role    string
	userErr error

	blacklisted bool
	blockReason string
	blockErr    error

	session    repository.SessionAuthState
	sessionErr error
}

func (s stubAuthStateStore) GetUserAuthStatus(context.Context, uint64) (int8, string, error) {
	return s.status, s.role, s.userErr
}

func (s stubAuthStateStore) IsAccessJTIBlacklisted(context.Context, string) (bool, string, error) {
	return s.blacklisted, s.blockReason, s.blockErr
}

func (s stubAuthStateStore) GetSessionAuthState(context.Context, uint64) (repository.SessionAuthState, error) {
	return s.session, s.sessionErr
}

func TestWebSocketRejectsRevokedDisabledAndBlacklistedBeforeUpgrade(t *testing.T) {
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
	signed, jti, _, err := manager.IssueAccessToken(42, 81, "user")
	if err != nil {
		t.Fatal("could not issue test JWT")
	}

	cases := []struct {
		name    string
		store   stubAuthStateStore
		subCode string
	}{
		{
			name:    "disabled account",
			store:   stubAuthStateStore{status: model.UserStatusDisabled, role: "user"},
			subCode: "11004",
		},
		{
			name:    "blacklisted jti",
			store:   stubAuthStateStore{status: model.UserStatusActive, role: "user", blacklisted: true, blockReason: "logout"},
			subCode: "11002",
		},
		{
			name: "revoked session",
			store: stubAuthStateStore{
				status: model.UserStatusActive,
				role:   "user",
				session: repository.SessionAuthState{
					ID:        81,
					UserID:    42,
					DeviceID:  "web-1",
					AccessJTI: jti,
					ExpiresAt: time.Now().Add(time.Hour),
					RevokedAt: func() *time.Time { now := time.Now(); return &now }(),
				},
			},
			subCode: "11005",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			auth := middleware.NewAccessAuthenticator(manager, tc.store)
			hub := ws.NewHub(cfg, auth, nil, log)
			router := api.NewRouter(cfg, log, nil)
			api.RegisterWSRoute(router, cfg, hub)
			handled := make(chan struct{}, 1)
			dialer := dialPipeServer(t, http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
				router.ServeHTTP(writer, req)
				handled <- struct{}{}
			}))

			header := http.Header{
				"X-Request-ID":  []string{"ws-auth-state"},
				"Authorization": []string{"Bearer " + signed},
			}
			conn, resp, dialErr := dialer.DialContext(context.Background(), "ws://stage5.local/ws", header)
			if conn != nil {
				_ = conn.Close()
				t.Fatal("expected handshake rejection before upgrade")
			}
			if !errors.Is(dialErr, websocket.ErrBadHandshake) || resp == nil {
				t.Fatalf("expected ErrBadHandshake with response, got err=%v resp=%v", dialErr, resp)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d body=%s", resp.StatusCode, string(body))
			}
			var env errcode.Envelope
			if json.Unmarshal(body, &env) != nil || env.Code != errcode.ErrWSAuthFailed.Code || !strings.Contains(env.Message, tc.subCode) {
				t.Fatalf("unexpected envelope: %s", string(body))
			}
			<-handled
			if strings.Contains(logs.String(), signed) {
				t.Fatal("credential present in handshake logs")
			}
		})
	}
}

func TestWebSocketPostUpgradeOpenFailureClosesWith18007And1011(t *testing.T) {
	cfg, err := config.Load("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfgCopy := *cfg
	cfgCopy.WS.WriteTimeout = 200 * time.Millisecond
	authCfg, err := config.LoadAuthConfig("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := pkgjwt.NewManager(authCfg.JWT)
	if err != nil {
		t.Fatal(err)
	}
	signed, jti, _, err := manager.IssueAccessToken(42, 81, "user")
	if err != nil {
		t.Fatal("could not issue test JWT")
	}
	store := stubAuthStateStore{
		status: model.UserStatusActive,
		role:   "user",
		session: repository.SessionAuthState{
			ID:        81,
			UserID:    42,
			DeviceID:  "web-1",
			AccessJTI: jti,
			ExpiresAt: time.Now().Add(time.Hour),
		},
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	defer func() { _ = rdb.Close() }()
	leases := repository.NewPresenceRepository(nil, rdb, cfgCopy.WS.HeartbeatTimeout)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	hub := ws.NewHub(&cfgCopy, middleware.NewAccessAuthenticator(manager, store), leases, log)
	router := api.NewRouter(&cfgCopy, log, nil)
	api.RegisterWSRoute(router, &cfgCopy, hub)
	handled := make(chan struct{}, 1)
	dialer := dialPipeServer(t, http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		router.ServeHTTP(writer, req)
		handled <- struct{}{}
	}))

	conn, resp, err := dialer.DialContext(context.Background(), "ws://stage5.local/ws?access_token="+signed, http.Header{"X-Request-ID": []string{"ws-open-fail"}})
	if err != nil || resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101 upgrade when auth passes, got resp=%v err=%v", resp, err)
	}
	msg := readWire(t, conn)
	var failure struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(msg.Data, &failure) != nil || msg.Type != "error" || msg.ReqID != "ws-open-fail" || failure.Code != 18007 {
		t.Fatalf("expected 18007 error frame, got type=%s data=%s", msg.Type, string(msg.Data))
	}
	expectClose(t, conn, websocket.CloseInternalServerErr)
	_ = conn.Close()
	<-handled
	if strings.Contains(logs.String(), signed) {
		t.Fatal("credential present in logs")
	}
}
