package ws

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

type pingData struct {
	Nonce   string            `json:"nonce"`
	LastSeq map[string]string `json:"last_seq"`
}

func parsePing(data json.RawMessage) (pingData, bool) {
	var ping pingData
	if json.Unmarshal(data, &ping) != nil || strings.TrimSpace(ping.Nonce) == "" {
		return ping, false
	}
	for conversation, seq := range ping.LastSeq {
		id, idErr := strconv.ParseUint(conversation, 10, 64)
		_, seqErr := strconv.ParseUint(seq, 10, 64)
		if idErr != nil || id == 0 || seqErr != nil {
			return ping, false
		}
	}
	return ping, true
}

func (hub *Hub) dispatch(client *Client, env envelope, sessionExpiry time.Time) {
	switch env.Type {
	case "ping":
		ping, ok := parsePing(env.Data)
		if !ok {
			client.enqueue(errorFrame(env, errcode.ErrWSInvalidEnvelope))
			return
		}
		ctx, cancel := context.WithTimeout(client.ctx, hub.cfg.WriteTimeout)
		defer cancel()
		err := hub.store.Renew(ctx, client.lease)
		if errors.Is(err, repository.ErrLeaseLost) {
			client.stop(termination{code: websocket.ClosePolicyViolation, reason: "heartbeat lease expired"})
			return
		}
		if err != nil || client.setReadDeadline(hub.cfg.HeartbeatTimeout) != nil {
			client.stop(failureClose(env, errcode.ErrWSInternalError, websocket.CloseInternalServerErr))
			return
		}
		client.enqueue(outgoing("pong", env.ReqID, map[string]any{"nonce": ping.Nonce, "server_time": time.Now().UnixMilli(), "session_expires_at": sessionExpiry.UTC().Format(time.RFC3339Nano)}))
	case "error":
		var failure frameError
		if json.Unmarshal(env.Data, &failure) != nil || failure.Code <= 0 || failure.Message == "" {
			client.enqueue(errorFrame(env, errcode.ErrWSInvalidEnvelope))
			return
		}
		hub.log.InfoContext(client.ctx, "ws_peer_error", "request_id", env.ReqID, "user_id", client.lease.UserID, "code", failure.Code)
	case "msg.send", "msg.delivered_ack", "msg.read_ack", "msg.recall":
		client.enqueue(errorFrame(env, errcode.ErrWSStateConflict.WithMessage("本阶段不处理发送/回执/撤回，消息与回执状态未变更")))
	case "msg.ack", "msg.push", "presence.update", "pong", "system.notice":
		client.enqueue(errorFrame(env, errcode.ErrWSStateConflict.WithMessage("frame type is server-to-client only")))
	default:
		client.enqueue(errorFrame(env, errcode.ErrWSUnknownType))
	}
}
