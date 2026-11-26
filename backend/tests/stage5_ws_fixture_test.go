package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/api"
	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/ws"
)

const stage5IO = 5 * time.Second

type stage5Listener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (listener *stage5Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-listener.connections:
		return conn, nil
	case <-listener.closed:
		return nil, net.ErrClosed
	}
}

func (listener *stage5Listener) Close() error {
	listener.once.Do(func() { close(listener.closed) })
	return nil
}

func (*stage5Listener) Addr() net.Addr { return &net.UnixAddr{Name: "stage5", Net: "pipe"} }

type stage5ObservedConn struct {
	net.Conn
	nextWrite <-chan chan struct{}
}

func (conn stage5ObservedConn) Write(data []byte) (int, error) {
	select {
	case started := <-conn.nextWrite:
		close(started)
	default:
	}
	return conn.Conn.Write(data)
}

type stage5Rig struct {
	listener *stage5Listener
	node     string
	manager  *pkgjwt.Manager
	hub      *ws.Hub
	store    *repository.PresenceRepository
	db       *gorm.DB
	redis    *redis.Client
	cleanup  []func(context.Context) error
}

func newStage5Rig(t *testing.T, live bool, heartbeat time.Duration) *stage5Rig {
	t.Helper()
	cfg, err := config.Load("../configs/config.yaml")
	stage5Check(t, err)
	if live && (cfg.MySQL.Password == "" || cfg.Redis.Password == "") {
		t.Skip("live Stage 5 requires MYSQL_PASSWORD and REDIS_PASSWORD (or IM_ equivalents)")
	}
	cfg.App.NodeID = "stage5-" + uuid.NewString()
	if heartbeat > 0 {
		cfg.WS.HeartbeatTimeout = heartbeat
	}
	cfg.WS.WriteTimeout = time.Minute
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rig := &stage5Rig{node: cfg.App.NodeID}
	rig.manager, err = pkgjwt.NewManager(config.JWTConfig{
		Secret: uuid.NewString(), Issuer: rig.node, AccessTTL: time.Hour, RefreshTTL: 2 * time.Hour,
	})
	stage5Check(t, err)
	if live {
		rig.db, err = repository.NewMySQL(cfg.MySQL, log)
		stage5Check(t, err)
		t.Cleanup(func() { stage5Check(t, repository.CloseMySQL(rig.db)) })
		rig.redis, err = repository.NewRedis(cfg.Redis)
		stage5Check(t, err)
		t.Cleanup(func() { stage5Check(t, rig.redis.Close()) })
		rig.store = repository.NewPresenceRepository(rig.db, rig.redis, cfg.WS.HeartbeatTimeout)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			for _, cleanup := range slices.Backward(rig.cleanup) {
				if err := cleanup(ctx); err != nil {
					t.Errorf("clean up Stage 5 fixture: %v", err)
				}
			}
		})
	}
	state := middleware.NewAuthStateStore(repository.NewAuthRepository(rig.db), repository.NewSessionRepository(rig.db, rig.redis))
	rig.hub = ws.NewHub(cfg, middleware.NewAccessAuthenticator(rig.manager, state), rig.store, log)
	if live {
		stage5Check(t, rig.hub.Start(t.Context()))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), stage5IO)
		defer cancel()
		stage5Check(t, rig.hub.Shutdown(ctx))
	})
	router := api.NewRouter(cfg, log, nil)
	api.RegisterWSRoute(router, cfg, rig.hub)
	rig.listener = &stage5Listener{connections: make(chan net.Conn), closed: make(chan struct{})}
	server := &http.Server{Handler: router}
	done := make(chan error, 1)
	go func() { done <- server.Serve(rig.listener) }()
	t.Cleanup(func() {
		stage5Check(t, server.Close())
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve Stage 5 route: %v", err)
		}
	})
	return rig
}

func (rig *stage5Rig) dial(t *testing.T, token string, bearer bool, nextWrite <-chan chan struct{}) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: stage5IO, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		server, client := net.Pipe()
		select {
		case rig.listener.connections <- stage5ObservedConn{Conn: server, nextWrite: nextWrite}:
			return client, nil
		case <-ctx.Done():
		case <-rig.listener.closed:
		}
		_ = server.Close()
		_ = client.Close()
		return nil, net.ErrClosed
	}}
	address := "ws://stage5.local/ws"
	header := http.Header{}
	if bearer {
		header.Set("Authorization", "Bearer "+token)
	} else if token != "" {
		address += "?access_token=" + url.QueryEscape(token)
	}
	conn, response, err := dialer.DialContext(t.Context(), address, header)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
			t.Fatalf("/ws upgrade returned HTTP %d", response.StatusCode)
		}
		t.Fatal("/ws did not complete the WebSocket handshake")
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

type stage5Frame struct {
	Type      string          `json:"type"`
	ReqID     string          `json:"req_id"`
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

func stage5Check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func stage5Send(t *testing.T, conn *websocket.Conn, kind, reqID string, data any) {
	t.Helper()
	stage5Check(t, conn.SetWriteDeadline(time.Now().Add(stage5IO)))
	stage5Check(t, conn.WriteJSON(map[string]any{"type": kind, "req_id": reqID, "timestamp": time.Now().UnixMilli(), "data": data}))
}

func stage5Await(t *testing.T, conn *websocket.Conn, kind string, matches func(stage5Frame) bool) stage5Frame {
	t.Helper()
	stage5Check(t, conn.SetReadDeadline(time.Now().Add(stage5IO)))
	for {
		var frame stage5Frame
		stage5Check(t, conn.ReadJSON(&frame))
		if frame.Timestamp < 1_000_000_000_000 || len(frame.Data) == 0 || frame.Data[0] != '{' {
			t.Fatalf("invalid %q response envelope", frame.Type)
		}
		if frame.Type == kind && (matches == nil || matches(frame)) {
			return frame
		}
		if frame.Type != "presence.update" {
			t.Fatalf("waiting for %s, received %s (req_id %q)", kind, frame.Type, frame.ReqID)
		}
	}
}

func stage5Ping(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	reqID := uuid.NewString()
	stage5Send(t, conn, "ping", reqID, map[string]string{"nonce": reqID})
	frame := stage5Await(t, conn, "pong", nil)
	var pong struct {
		Nonce            string    `json:"nonce"`
		ServerTime       int64     `json:"server_time"`
		SessionExpiresAt time.Time `json:"session_expires_at"`
	}
	stage5Check(t, json.Unmarshal(frame.Data, &pong))
	if frame.ReqID != reqID || pong.Nonce != reqID || pong.ServerTime < 1_000_000_000_000 || pong.SessionExpiresAt.IsZero() {
		t.Fatalf("pong does not confirm heartbeat %s: %+v", reqID, pong)
	}
}

func stage5ExpectClose(t *testing.T, conn *websocket.Conn, codes ...int) {
	t.Helper()
	stage5Check(t, conn.SetReadDeadline(time.Now().Add(stage5IO)))
	for {
		var frame stage5Frame
		err := conn.ReadJSON(&frame)
		if err != nil {
			if !websocket.IsCloseError(err, codes...) {
				t.Fatalf("expected WS close %v, got %v", codes, err)
			}
			return
		}
		switch frame.Type {
		case "presence.update", "system.notice", "error":
		default:
			t.Fatalf("unexpected %q before close %v", frame.Type, codes)
		}
	}
}

func stage5Eventually(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(stage5IO)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
