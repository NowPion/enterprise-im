package ws

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

type Hub struct {
	peers       registry
	cfg         config.WSConfig
	node        string
	auth        *middleware.AccessAuthenticator
	store       *repository.PresenceRepository
	log         *slog.Logger
	gate        sync.RWMutex
	draining    bool
	stopWorkers context.CancelFunc
	active      sync.WaitGroup
}

func NewHub(cfg *config.Config, auth *middleware.AccessAuthenticator, store *repository.PresenceRepository, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{cfg: cfg.WS, node: cfg.App.NodeID, auth: auth, store: store, log: log}
}

func (hub *Hub) Authenticate(ctx context.Context, token string) (middleware.AuthContext, *errcode.Error) {
	check, cancel := context.WithTimeout(ctx, hub.cfg.WriteTimeout)
	defer cancel()
	return hub.auth.Authenticate(check, token)
}

func (hub *Hub) admit(client *Client) bool {
	hub.gate.RLock()
	defer hub.gate.RUnlock()
	if hub.draining {
		return false
	}
	hub.active.Add(1)
	hub.peers.add(client.lease.UserID, client.lease.ConnID, client)
	return true
}

func (hub *Hub) Serve(ctx context.Context, conn *websocket.Conn, identity middleware.AuthContext, reqID string, failure *errcode.Error) {
	client := newClient(ctx, conn)
	client.auth = identity
	client.lease = repository.ConnLease{UserID: identity.UserID, SessionID: identity.SessionID, DeviceID: identity.DeviceID, NodeID: hub.node, ConnID: uuid.NewString()}
	ref := envelope{ReqID: reqID}
	log := hub.log.With("request_id", reqID, "user_id", identity.UserID, "conn_id", client.lease.ConnID)
	if failure != nil {
		end := failureClose(ref, errcode.ErrWSAuthFailed.WithMessage(failure.Error()), websocket.ClosePolicyViolation)
		if failure.HTTPStatus >= 500 {
			end = failureClose(ref, errcode.ErrWSInternalError, websocket.CloseInternalServerErr)
		}
		log.WarnContext(ctx, "ws_handshake_denied", "code", failure.Code)
		client.stop(end)
		client.writePump(hub.cfg.WriteTimeout)
		return
	}
	if !hub.admit(client) {
		client.drain()
		client.writePump(hub.cfg.WriteTimeout)
		return
	}
	defer hub.active.Done()
	defer func() {
		hub.peers.remove(identity.UserID, client.lease.ConnID, client)
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), hub.cfg.WriteTimeout)
		defer cancel()
		if err := hub.store.Release(cleanup, client.lease); err != nil {
			log.ErrorContext(cleanup, "ws_lease_release_failed", "error", err)
		}
		log.InfoContext(cleanup, "ws_disconnected")
	}()
	openCtx, cancel := context.WithTimeout(client.ctx, hub.cfg.WriteTimeout)
	err := hub.store.Open(openCtx, client.lease)
	cancel()
	if err != nil {
		client.stop(failureClose(ref, errcode.ErrWSInternalError, websocket.CloseInternalServerErr))
	} else if _, ok := hub.authorize(client, ref); ok {
		log.InfoContext(ctx, "ws_connected", "session_id", identity.SessionID, "node_id", hub.node)
	}
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); client.writePump(hub.cfg.WriteTimeout) }()
	if client.ctx.Err() == nil {
		client.readPump(hub)
	}
	<-writerDone
}

func (hub *Hub) authorize(client *Client, env envelope) (middleware.AuthContext, bool) {
	ctx, cancel := context.WithTimeout(client.ctx, hub.cfg.WriteTimeout)
	defer cancel()
	state, reason, failure := hub.auth.Check(ctx, client.auth)
	if failure == nil {
		return state, true
	}
	if failure.HTTPStatus >= 500 {
		client.stop(failureClose(env, errcode.ErrWSInternalError, websocket.CloseInternalServerErr))
	} else {
		client.revoke(env, revokeEvent(reason))
	}
	return state, false
}

func (hub *Hub) Start(ctx context.Context) error {
	sub, err := hub.store.Subscribe(ctx, hub.node)
	if err != nil {
		return err
	}
	hub.gate.Lock()
	defer hub.gate.Unlock()
	if hub.draining || hub.stopWorkers != nil {
		_ = sub.Close()
		return errors.New("websocket hub already started or draining")
	}
	runCtx, cancel := context.WithCancel(ctx)
	hub.stopWorkers = cancel
	hub.active.Go(func() { defer sub.Close(); hub.listen(runCtx, sub) })
	hub.active.Go(func() {
		ticker := time.NewTicker(min(time.Second, max(time.Millisecond, hub.cfg.HeartbeatTimeout/2)))
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := hub.store.ReapExpired(runCtx); err != nil && runCtx.Err() == nil {
					hub.log.ErrorContext(runCtx, "ws_expired_leases_failed", "error", err)
				}
			}
		}
	})
	return nil
}

func (hub *Hub) Shutdown(ctx context.Context) error {
	hub.gate.Lock()
	hub.draining = true
	if hub.stopWorkers != nil {
		hub.stopWorkers()
	}
	hub.gate.Unlock()
	for _, client := range hub.peers.all() {
		client.drain()
	}
	done := make(chan struct{})
	go func() { hub.active.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
