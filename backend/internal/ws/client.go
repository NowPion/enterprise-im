package ws

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

type termination struct {
	code   int
	reason string
	frames []frame
}

func (end termination) Error() string { return end.reason }

func failureClose(env envelope, failure *errcode.Error, code int) termination {
	return termination{code: code, reason: strconv.Itoa(failure.Code), frames: []frame{errorFrame(env, failure)}}
}

type Client struct {
	auth  middleware.AuthContext
	lease repository.ConnLease
	conn  *websocket.Conn
	ctx   context.Context
	stop  context.CancelCauseFunc
	send  chan frame
}

func (client *Client) setReadDeadline(heartbeat time.Duration) error {
	deadline := time.Now().Add(heartbeat)
	for _, expiry := range []time.Time{client.auth.ExpiresAt, client.auth.SessionExpiresAt} {
		if expiry.Before(deadline) {
			deadline = expiry
		}
	}
	return client.conn.SetReadDeadline(deadline)
}

func (client *Client) readPump(hub *Hub) {
	client.conn.SetCloseHandler(func(code int, _ string) error {
		client.stop(termination{code: code})
		return nil
	})
	checkControl := func(payload string, pong bool) error {
		if _, ok := hub.authorize(client, envelope{}); ok && pong {
			client.enqueue(frame{controlPong: &payload})
		}
		return client.ctx.Err()
	}
	client.conn.SetPingHandler(func(payload string) error { return checkControl(payload, true) })
	client.conn.SetPongHandler(func(payload string) error { return checkControl(payload, false) })
	if err := client.setReadDeadline(hub.cfg.HeartbeatTimeout); err != nil {
		client.stop(err)
		return
	}
	for client.ctx.Err() == nil {
		opcode, reader, err := client.conn.NextReader()
		if err != nil {
			client.readFailed(err)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(reader, hub.cfg.MaxFrameBytes+1))
		if err != nil {
			client.readFailed(err)
			return
		}
		if int64(len(raw)) > hub.cfg.MaxFrameBytes {
			env := frameReference(raw)
			if _, ok := hub.authorize(client, env); ok {
				client.stop(failureClose(env, errcode.ErrPayloadTooLarge, websocket.ClosePolicyViolation))
			}
			return
		}
		env, failure := decodeEnvelope(raw)
		state, ok := hub.authorize(client, env)
		if !ok {
			return
		}
		if opcode != websocket.TextMessage {
			failure = errcode.ErrWSInvalidEnvelope
		}
		if failure != nil {
			client.enqueue(errorFrame(env, failure))
			continue
		}
		hub.dispatch(client, env, state.SessionExpiresAt)
	}
}

func (client *Client) readFailed(err error) {
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		if !client.auth.ExpiresAt.After(time.Now()) || !client.auth.SessionExpiresAt.After(time.Now()) {
			client.revoke(envelope{}, "session.revoked")
		} else {
			client.stop(termination{code: websocket.ClosePolicyViolation, reason: "heartbeat timeout"})
		}
		return
	}
	client.stop(err)
}

func (client *Client) revoke(env envelope, event string) {
	end := failureClose(env, errcode.ErrWSSessionRevoked, websocket.ClosePolicyViolation)
	end.frames = append(end.frames, noticeFrame(client.lease.UserID, client.lease.SessionID, event))
	client.stop(end)
}

func (client *Client) drain() {
	client.stop(termination{code: websocket.CloseNormalClosure, reason: "server.draining", frames: []frame{noticeFrame(client.lease.UserID, client.lease.SessionID, "server.draining")}})
}

func newClient(parent context.Context, conn *websocket.Conn) *Client {
	ctx, cancel := context.WithCancelCause(parent)
	return &Client{ctx: ctx, stop: cancel, conn: conn, send: make(chan frame, 256)}
}

func (client *Client) enqueue(msg frame) {
	if client.ctx.Err() != nil {
		return
	}
	select {
	case client.send <- msg:
	default:
		client.stop(failureClose(envelope{}, errcode.ErrWSInternalError.WithMessage("send queue full; reconnect and sync via cursor"), websocket.CloseInternalServerErr))
	}
}

func (client *Client) writePump(timeout time.Duration) {
	defer client.conn.Close()
	for client.ctx.Err() == nil {
		select {
		case <-client.ctx.Done():
		case msg := <-client.send:
			if client.ctx.Err() == nil {
				if err := client.write(msg, time.Now().Add(timeout)); err != nil {
					client.stop(err)
					return
				}
			}
		}
	}
	end, ok := context.Cause(client.ctx).(termination)
	if !ok {
		return
	}
	deadline := time.Now().Add(timeout)
	for _, msg := range end.frames {
		if err := client.write(msg, deadline); err != nil {
			return
		}
	}
	_ = client.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(end.code, end.reason), deadline)
}

func (client *Client) write(msg frame, deadline time.Time) error {
	if msg.controlPong != nil {
		return client.conn.WriteControl(websocket.PongMessage, []byte(*msg.controlPong), deadline)
	}
	if err := client.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return client.conn.WriteJSON(msg)
}
