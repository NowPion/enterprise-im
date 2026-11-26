package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

// pipeListener implements net.Listener over in-memory net.Pipe connections so
// http.Server.Serve and Shutdown can be exercised without opening TCP sockets.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return pipeAddr("pipe://stage3-test")
}

func (l *pipeListener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	serverConn, clientConn := net.Pipe()
	select {
	case l.conns <- serverConn:
		return clientConn, nil
	case <-l.closed:
		_ = serverConn.Close()
		_ = clientConn.Close()
		return nil, errors.New("listener closed")
	case <-ctx.Done():
		_ = serverConn.Close()
		_ = clientConn.Close()
		return nil, ctx.Err()
	}
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func TestOrderedGracefulShutdownDrainsInFlightAndRunsStepsInOrder(t *testing.T) {
	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	reqStarted := make(chan struct{})
	releaseReq := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(reqStarted)
		<-releaseReq
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})

	ln := newPipeListener()
	srv := &http.Server{Handler: mux}

	var (
		mu    sync.Mutex
		order []string
	)
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}

	hooks := ShutdownHooks{
		NotifyAndCloseWS: func(context.Context) error {
			record("ws_drain")
			return nil
		},
		DrainMQ: func(context.Context) error {
			record("mq_drain")
			return nil
		},
		CloseDB: func() error {
			record("db_close")
			return nil
		},
		CloseRedis: func() error {
			record("redis_close")
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveAndShutdown(ctx, log, srv, ln, 3*time.Second, hooks)
	}()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: ln.DialContext,
		},
	}

	// Start an in-flight HTTP request before triggering shutdown.
	reqDone := make(chan error, 1)
	go func() {
		resp, reqErr := client.Get("http://stage3.local/slow")
		if reqErr == nil {
			_ = resp.Body.Close()
		}
		reqDone <- reqErr
	}()

	select {
	case <-reqStarted:
	case reqErr := <-reqDone:
		t.Fatalf("request finished before reaching handler: %v", reqErr)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for in-flight request to start")
	}

	cancel()

	// Wait until Step 2 and Step 3 hooks execute while HTTP is still draining,
	// confirming Step 1-3 execute and DB/Redis stay open until Step 5.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// New request after Step 1 must be rejected because listeners are closed.
	rejectClient := &http.Client{
		Transport: &http.Transport{
			DialContext: ln.DialContext,
		},
		Timeout: 500 * time.Millisecond,
	}
	if resp, dialErr := rejectClient.Get("http://stage3.local/slow"); dialErr == nil {
		_ = resp.Body.Close()
		t.Fatal("expected new request after shutdown start to fail")
	}

	mu.Lock()
	for _, s := range order {
		if s == "db_close" || s == "redis_close" {
			mu.Unlock()
			t.Fatalf("db/redis closed before in-flight HTTP finished: %v", order)
		}
	}
	mu.Unlock()

	close(releaseReq)
	select {
	case reqErr := <-reqDone:
		if reqErr != nil {
			t.Fatalf("in-flight HTTP request failed during drain: %v", reqErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for in-flight HTTP request to finish")
	}

	select {
	case shutdownErr := <-done:
		if shutdownErr != nil {
			t.Fatalf("serveAndShutdown returned error: %v", shutdownErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for serveAndShutdown to exit")
	}

	wantOrder := []string{"ws_drain", "mq_drain", "db_close", "redis_close"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("unexpected shutdown hook order: got %v, want %v", order, wantOrder)
	}

	logs := logBuf.String()
	for _, marker := range []string{
		"shutdown_step_1_stop_accepting_http",
		"shutdown_step_2_websocket_closed",
		"shutdown_step_3_mq_drained",
		"shutdown_step_4_http_drained",
		"shutdown_step_5_db_and_redis_closed",
	} {
		if !strings.Contains(logs, marker) {
			t.Fatalf("missing shutdown log marker %q in logs: %s", marker, logs)
		}
	}
}

func TestLoggedNoopSessionRevokeHook(t *testing.T) {
	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "info", Format: "json"}, &logBuf)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	hook := newLoggedNoopSessionRevokeHook(log)
	if err := hook(context.Background(), 10001, []uint64{10, 20}, "logout"); err != nil {
		t.Fatalf("expected nil error from noop hook, got %v", err)
	}

	out := logBuf.String()
	for _, needle := range []string{
		`"auth_session_revoke_hook_noop"`,
		`"user_id":10001`,
		`"session_count":2`,
		`"reason":"logout"`,
		`"no active websocket hub in stage 4"`,
	} {
		if !strings.Contains(out, needle) {
			t.Fatalf("expected %s in hook log output: %s", needle, out)
		}
	}
}

func TestBuildRouterMountsHealthzAndAllStage4Routes(t *testing.T) {
	cfg, err := config.Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Upload.Dir = t.TempDir()
	authCfg, err := config.LoadAuthConfig("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("load auth config: %v", err)
	}
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	idGen, err := snowflake.NewGenerator(cfg.Snowflake.NodeID)
	if err != nil {
		t.Fatalf("snowflake generator: %v", err)
	}
	jwtMgr, err := pkgjwt.NewManager(authCfg.JWT)
	if err != nil {
		t.Fatalf("jwt manager: %v", err)
	}

	db := &gorm.DB{}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = rdb.Close() }()

	if _, err := buildRouter(nil, authCfg, log, db, rdb, idGen, jwtMgr); err == nil {
		t.Fatal("expected buildRouter to fail when cfg is nil")
	}
	if _, err := buildRouter(cfg, nil, log, db, rdb, idGen, jwtMgr); err == nil {
		t.Fatal("expected buildRouter to fail when authCfg is nil")
	}

	router, err := buildRouter(cfg, authCfg, log, db, rdb, idGen, jwtMgr)
	if err != nil {
		t.Fatalf("buildRouter failed: %v", err)
	}
	if router.mqProducer == nil || router.mqManager == nil {
		t.Fatal("expected buildRouter to initialize mqProducer and mqManager")
	}

	registered := make(map[string]bool)
	for _, rt := range router.Routes() {
		registered[rt.Method+" "+rt.Path] = true
	}

	expectedRoutes := []string{
		"GET /healthz",
		"POST /api/v1/auth/register",
		"POST /api/v1/auth/login",
		"POST /api/v1/auth/refresh",
		"POST /api/v1/auth/logout",
		"PUT /api/v1/me/password",
		"GET /api/v1/me",
		"PATCH /api/v1/me",
		"GET /api/v1/users/search",
		"POST /api/v1/friend-requests",
		"GET /api/v1/friend-requests",
		"POST /api/v1/friend-requests/:id/accept",
		"POST /api/v1/friend-requests/:id/reject",
		"POST /api/v1/friend-requests/:id/cancel",
		"GET /api/v1/friends",
		"PATCH /api/v1/friends/:friend_id",
		"DELETE /api/v1/friends/:friend_id",
		"POST /api/v1/uploads",
		"GET /api/v1/uploads/:asset_id",
		"GET /api/v1/uploads/:asset_id/download",
		"DELETE /api/v1/uploads/:asset_id",
		"POST /api/v1/me/avatar",
		"GET /api/v1/conversations",
		"GET /api/v1/conversations/:conversation_id",
		"PATCH /api/v1/conversations/:conversation_id/settings",
		"GET /api/v1/conversations/:conversation_id/members",
		"GET /api/v1/conversations/:conversation_id/messages",
		"GET /api/v1/conversations/:conversation_id/sync",
		"POST /api/v1/conversations/:conversation_id/messages",
		"POST /api/v1/conversations/:conversation_id/messages/:msg_id/recall",
		"POST /api/v1/groups",
		"GET /api/v1/groups",
		"GET /api/v1/groups/:group_id",
		"PATCH /api/v1/groups/:group_id",
		"DELETE /api/v1/groups/:group_id",
		"GET /api/v1/groups/:group_id/members",
		"POST /api/v1/groups/:group_id/members",
		"DELETE /api/v1/groups/:group_id/members/:user_id",
		"PATCH /api/v1/groups/:group_id/members/:user_id",
		"PATCH /api/v1/groups/:group_id/members/:user_id/role",
		"POST /api/v1/groups/:group_id/leave",
		"POST /api/v1/groups/:group_id/transfer",
		"GET /api/v1/groups/:group_id/audit",
	}
	for _, key := range expectedRoutes {
		if !registered[key] {
			t.Fatalf("missing expected route %q in router.Routes()", key)
		}
	}
	for _, forbidden := range []string{
		"POST /api/v1/conversations/:conversation_id/read",
		"POST /api/v1/conversations/:conversation_id/delivered",
		"GET /api/v1/conversations/:conversation_id/messages/:msg_id/receipts",
	} {
		if registered[forbidden] {
			t.Fatalf("stage 7 route %q must not be registered in stage 6", forbidden)
		}
	}

	protectedSamples := [][2]string{
		{http.MethodPost, "/api/v1/auth/logout"},
		{http.MethodPut, "/api/v1/me/password"},
		{http.MethodGet, "/api/v1/me"},
		{http.MethodPatch, "/api/v1/me"},
		{http.MethodGet, "/api/v1/users/search?q=alice"},
		{http.MethodPost, "/api/v1/friend-requests"},
		{http.MethodGet, "/api/v1/friend-requests?direction=received"},
		{http.MethodPost, "/api/v1/friend-requests/1/accept"},
		{http.MethodPost, "/api/v1/friend-requests/1/reject"},
		{http.MethodPost, "/api/v1/friend-requests/1/cancel"},
		{http.MethodGet, "/api/v1/friends"},
		{http.MethodPatch, "/api/v1/friends/1"},
		{http.MethodDelete, "/api/v1/friends/1"},
		{http.MethodPost, "/api/v1/uploads"},
		{http.MethodGet, "/api/v1/uploads/550e8400-e29b-41d4-a716-446655440000"},
		{http.MethodGet, "/api/v1/uploads/550e8400-e29b-41d4-a716-446655440000/download"},
		{http.MethodDelete, "/api/v1/uploads/550e8400-e29b-41d4-a716-446655440000"},
		{http.MethodPost, "/api/v1/me/avatar"},
		{http.MethodGet, "/api/v1/conversations"},
		{http.MethodGet, "/api/v1/conversations/1"},
		{http.MethodPatch, "/api/v1/conversations/1/settings"},
		{http.MethodGet, "/api/v1/conversations/1/members"},
		{http.MethodGet, "/api/v1/conversations/1/messages"},
		{http.MethodGet, "/api/v1/conversations/1/sync"},
		{http.MethodPost, "/api/v1/conversations/1/messages"},
		{http.MethodPost, "/api/v1/conversations/1/messages/1/recall"},
		{http.MethodPost, "/api/v1/groups"},
		{http.MethodGet, "/api/v1/groups"},
		{http.MethodGet, "/api/v1/groups/1"},
		{http.MethodPatch, "/api/v1/groups/1"},
		{http.MethodDelete, "/api/v1/groups/1"},
		{http.MethodGet, "/api/v1/groups/1/members"},
		{http.MethodPost, "/api/v1/groups/1/members"},
		{http.MethodDelete, "/api/v1/groups/1/members/2"},
		{http.MethodPatch, "/api/v1/groups/1/members/me"},
		{http.MethodPatch, "/api/v1/groups/1/members/2/role"},
		{http.MethodPost, "/api/v1/groups/1/leave"},
		{http.MethodPost, "/api/v1/groups/1/transfer"},
		{http.MethodGet, "/api/v1/groups/1/audit"},
	}
	for _, tc := range protectedSamples {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(tc[0], tc[1], nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s expected 401 without token, got %d body=%s", tc[0], tc[1], w.Code, w.Body.String())
		}
		var env errcode.Envelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal 401 envelope: %v", err)
		}
		if env.Code != errcode.ErrAccessTokenInvalid.Code {
			t.Fatalf("%s %s expected 11001, got %d", tc[0], tc[1], env.Code)
		}
	}
}

func TestStage7DefaultChatWiringPublishesStreamWithoutDirectPush(t *testing.T) {
	cfg, err := config.Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Chat.DirectPushFallback {
		t.Fatal("expected chat.direct_push_fallback to default to false in config")
	}

	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = rdb.Close() }()

	adapter := newRedisStreamPublisherAdapter(repository.NewRedisStreamPublisher(rdb))
	if adapter == nil {
		t.Fatal("expected non-nil stream publisher adapter")
	}
	if err := adapter.PublishMessage(context.Background(), service.StreamMessage{
		Kind:           "message",
		MessageID:      1,
		ConversationID: 1,
		Seq:            1,
		SenderID:       1,
		PayloadRef:     "mysql:messages:1",
	}); err == nil {
		t.Fatal("expected PublishMessage against unreachable redis to return error")
	}
	if err := adapter.PublishEvent(context.Background(), service.StreamEvent{
		Kind:           "recall",
		Event:          "message.recalled",
		ConversationID: 1,
		ActorID:        1,
		MessageID:      1,
		Seq:            2,
	}); err == nil {
		t.Fatal("expected PublishEvent against unreachable redis to return error")
	}
}
