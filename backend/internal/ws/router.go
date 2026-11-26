package ws

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
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
	case "msg.send":
		hub.handleMsgSend(client, env)
	case "msg.recall":
		hub.handleMsgRecall(client, env)
	case "msg.delivered_ack", "msg.read_ack":
		client.enqueue(errorFrame(env, errcode.ErrWSStateConflict.WithMessage("本阶段不处理发送/回执/撤回，消息与回执状态未变更")))
	case "msg.ack", "msg.push", "presence.update", "pong", "system.notice":
		client.enqueue(errorFrame(env, errcode.ErrWSStateConflict.WithMessage("frame type is server-to-client only")))
	default:
		client.enqueue(errorFrame(env, errcode.ErrWSUnknownType))
	}
}

type wsMsgSendData struct {
	ConversationID string          `json:"conversation_id"`
	ClientMsgID    string          `json:"client_msg_id"`
	MsgType        string          `json:"msg_type"`
	Content        *string         `json:"content"`
	Extra          json.RawMessage `json:"extra"`
}

type wsMsgRecallData struct {
	ConversationID string `json:"conversation_id"`
	MsgID          string `json:"msg_id"`
}

func (hub *Hub) handleMsgSend(client *Client, env envelope) {
	chat := hub.chatService()
	if chat == nil {
		client.enqueue(errorFrame(env, errcode.ErrWSStateConflict.WithMessage("本阶段不处理发送/回执/撤回，消息与回执状态未变更")))
		return
	}

	var payload wsMsgSendData
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		client.enqueue(errorFrame(env, errcode.ErrWSInvalidEnvelope))
		return
	}
	convID, convOK := parsePositiveWSUint64(payload.ConversationID)
	if !convOK || strings.TrimSpace(payload.ClientMsgID) == "" || strings.TrimSpace(payload.MsgType) == "" {
		client.enqueue(errorFrame(env, errcode.ErrWSInvalidEnvelope))
		return
	}
	if payload.MsgType == model.MessageTypeText && payload.Content == nil {
		client.enqueue(errorFrame(env, errcode.ErrWSInvalidEnvelope))
		return
	}
	content := ""
	if payload.Content != nil {
		content = *payload.Content
	}

	timeout := hub.cfg.WriteTimeout
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(client.ctx, timeout)
	defer cancel()

	_, bizErr := chat.SendMessage(ctx, service.SendMessageInput{
		ConversationID: convID,
		SenderID:       client.lease.UserID,
		ClientMsgID:    payload.ClientMsgID,
		MsgType:        payload.MsgType,
		Content:        content,
		Extra:          payload.Extra,
	}, func(res *service.SendMessageResult) {
		delivery := model.ReceiptStatusSent
		if res.Message.DeliveryStatus != nil && *res.Message.DeliveryStatus != "" {
			delivery = *res.Message.DeliveryStatus
		}
		client.enqueue(outgoing("msg.ack", env.ReqID, service.MsgAckPayload{
			ClientMsgID:    res.Message.ClientMsgID,
			MsgID:          strconv.FormatUint(res.Message.MsgID, 10),
			ConversationID: strconv.FormatUint(res.Message.ConversationID, 10),
			Seq:            strconv.FormatUint(res.Message.Seq, 10),
			Status:         res.Message.Status,
			DeliveryStatus: delivery,
		}))
	})
	if bizErr != nil {
		client.enqueue(errorFrame(env, mapWSChatError(bizErr)))
	}
}

func (hub *Hub) handleMsgRecall(client *Client, env envelope) {
	chat := hub.chatService()
	if chat == nil {
		client.enqueue(errorFrame(env, errcode.ErrWSStateConflict.WithMessage("本阶段不处理发送/回执/撤回，消息与回执状态未变更")))
		return
	}

	var payload wsMsgRecallData
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		client.enqueue(errorFrame(env, errcode.ErrWSInvalidEnvelope))
		return
	}
	convID, convOK := parsePositiveWSUint64(payload.ConversationID)
	msgID, msgOK := parsePositiveWSUint64(payload.MsgID)
	if !convOK || !msgOK {
		client.enqueue(errorFrame(env, errcode.ErrWSInvalidEnvelope))
		return
	}

	timeout := hub.cfg.WriteTimeout
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(client.ctx, timeout)
	defer cancel()

	_, bizErr := chat.RecallMessage(ctx, client.lease.UserID, convID, msgID, func(res *service.RecallMessageResult) {
		recalledAt := ""
		if res.RecalledMessage.RecalledAt != nil {
			recalledAt = *res.RecalledMessage.RecalledAt
		}
		client.enqueue(outgoing("msg.recall", env.ReqID, service.MsgRecallAckPayload{
			ConversationID: strconv.FormatUint(res.RecallMessage.ConversationID, 10),
			RecalledMsgID:  strconv.FormatUint(res.RecalledMessage.MsgID, 10),
			RecallMsgID:    strconv.FormatUint(res.RecallMessage.MsgID, 10),
			Seq:            strconv.FormatUint(res.RecallMessage.Seq, 10),
			RecalledAt:     recalledAt,
		}))
	})
	if bizErr != nil {
		client.enqueue(errorFrame(env, mapWSChatError(bizErr)))
	}
}

func parsePositiveWSUint64(raw string) (uint64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed[0] < '1' || trimmed[0] > '9' {
		return 0, false
	}
	for i := 1; i < len(trimmed); i++ {
		if trimmed[i] < '0' || trimmed[i] > '9' {
			return 0, false
		}
	}
	val, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil || val == 0 {
		return 0, false
	}
	return val, true
}

func mapWSChatError(err *errcode.Error) *errcode.Error {
	if err == nil {
		return errcode.ErrWSInternalError
	}
	if err.Code == errcode.ErrNotConversationMember.Code {
		return errcode.ErrWSConvAccessDenied
	}
	return err
}
