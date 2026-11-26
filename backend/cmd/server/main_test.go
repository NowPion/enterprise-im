package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/pkg/logger"
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
