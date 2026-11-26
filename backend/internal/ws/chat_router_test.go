package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type stubWSChatService struct {
	mu          sync.Mutex
	broadcaster service.RealtimeChatBroadcaster
	nextSeq     uint64
	nextMsgID   uint64
	sentByCID   map[string]service.MessageDTO
	members     map[uint64]map[uint64]bool
}

func newStubWSChatService(b service.RealtimeChatBroadcaster) *stubWSChatService {
	return &stubWSChatService{
		broadcaster: b,
		nextMsgID:   188900000000000,
		sentByCID:   make(map[string]service.MessageDTO),
		members: map[uint64]map[uint64]bool{
			90001: {10001: true, 10002: true},
		},
	}
}

func (s *stubWSChatService) SendMessage(ctx context.Context, input service.SendMessageInput, beforePush func(res *service.SendMessageResult)) (*service.SendMessageResult, *errcode.Error) {
	s.mu.Lock()
	if !s.members[input.ConversationID][input.SenderID] {
		s.mu.Unlock()
		return nil, errcode.ErrNotConversationMember
	}
	key := strconv.FormatUint(input.SenderID, 10) + ":" + input.ClientMsgID
	if existing, ok := s.sentByCID[key]; ok {
		s.mu.Unlock()
		res := &service.SendMessageResult{Message: existing, IsNew: false}
		if beforePush != nil {
			beforePush(res)
		}
		return res, nil
	}
	s.nextSeq++
	s.nextMsgID++
	sent := model.ReceiptStatusSent
	dto := service.MessageDTO{
		MsgID:          s.nextMsgID,
		ClientMsgID:    input.ClientMsgID,
		ConversationID: input.ConversationID,
		Seq:            s.nextSeq,
		SenderID:       input.SenderID,
		MsgType:        input.MsgType,
		Content:        input.Content,
		Extra:          json.RawMessage(`{}`),
		Status:         model.MessageStatusNormal,
		CreatedAt:      "2026-11-26T04:00:00.000Z",
		DeliveryStatus: &sent,
	}
	s.sentByCID[key] = dto
	recipients := make([]uint64, 0, len(s.members[input.ConversationID]))
	for uid := range s.members[input.ConversationID] {
		recipients = append(recipients, uid)
	}
	s.mu.Unlock()

	res := &service.SendMessageResult{Message: dto, IsNew: true, RecipientIDs: recipients}
	if beforePush != nil {
		beforePush(res)
	}
	pushMsg := dto
	pushMsg.DeliveryStatus = nil
	s.broadcaster.BroadcastToUsers(ctx, recipients, "msg.push", service.MessagePushPayload{
		PushType:       "message",
		Message:        pushMsg,
		DeliveryStatus: model.ReceiptStatusSent,
	})
	return res, nil
}

func (s *stubWSChatService) RecallMessage(ctx context.Context, userID uint64, convID uint64, msgID uint64, beforePush func(res *service.RecallMessageResult)) (*service.RecallMessageResult, *errcode.Error) {
	s.mu.Lock()
	if !s.members[convID][userID] {
		s.mu.Unlock()
		return nil, errcode.ErrNotConversationMember
	}
	var target *service.MessageDTO
	var targetKey string
	for k, m := range s.sentByCID {
		if m.ConversationID == convID && m.MsgID == msgID {
			cp := m
			target = &cp
			targetKey = k
			break
		}
	}
	if target == nil {
		s.mu.Unlock()
		return nil, errcode.ErrMessageNotFound
	}
	if target.SenderID != userID {
		s.mu.Unlock()
		return nil, errcode.ErrMessageAccessDenied
	}
	if target.Status == model.MessageStatusRecalled {
		s.mu.Unlock()
		return nil, errcode.ErrMessageAlreadyRecalled
	}
	recalledAt := "2026-11-26T04:01:00.000Z"
	target.Status = model.MessageStatusRecalled
	target.RecalledAt = &recalledAt
	s.sentByCID[targetKey] = *target

	s.nextSeq++
	s.nextMsgID++
	recallDTO := service.MessageDTO{
		MsgID:          s.nextMsgID,
		ClientMsgID:    "recall:" + strconv.FormatUint(msgID, 10),
		ConversationID: convID,
		Seq:            s.nextSeq,
		SenderID:       userID,
		MsgType:        model.MessageTypeRecall,
		Content:        "撤回了一条消息",
		Extra:          json.RawMessage(`{}`),
		Status:         model.MessageStatusNormal,
		CreatedAt:      recalledAt,
	}
	recipients := make([]uint64, 0, len(s.members[convID]))
	for uid := range s.members[convID] {
		recipients = append(recipients, uid)
	}
	s.mu.Unlock()

	res := &service.RecallMessageResult{
		RecalledMessage: *target,
		RecallMessage:   recallDTO,
		RecipientIDs:    recipients,
	}
	if beforePush != nil {
		beforePush(res)
	}
	s.broadcaster.BroadcastToUsers(ctx, recipients, "msg.push", service.MessagePushPayload{
		PushType:       "message",
		Message:        recallDTO,
		DeliveryStatus: model.ReceiptStatusSent,
	})
	convStr := strconv.FormatUint(convID, 10)
	actorStr := strconv.FormatUint(userID, 10)
	s.broadcaster.BroadcastToUsers(ctx, recipients, "system.notice", service.SystemNoticePayload{
		Event:          "message.recalled",
		NoticeID:       "n_test",
		ConversationID: &convStr,
		ActorID:        &actorStr,
		Payload: map[string]any{
			"recalled_msg_id": strconv.FormatUint(target.MsgID, 10),
			"recall_msg_id":   strconv.FormatUint(recallDTO.MsgID, 10),
			"recall_seq":      strconv.FormatUint(recallDTO.Seq, 10),
			"recalled_at":     recalledAt,
		},
	})
	return res, nil
}

func TestWSRouterMsgSendRecallAndStage7Deferred(t *testing.T) {
	cfg := &config.Config{
		App: config.AppConfig{NodeID: "test-node"},
		WS:  config.WSConfig{HeartbeatTimeout: time.Minute, WriteTimeout: 5 * time.Second, MaxFrameBytes: 1 << 20},
	}
	hub := NewHub(cfg, nil, nil, nil)
	chatSvc := newStubWSChatService(hub)
	hub.SetChatService(chatSvc)

	senderConn := newClient(context.Background(), nil)
	senderConn.lease = repository.ConnLease{UserID: 10001, SessionID: 1, DeviceID: "web", NodeID: "test-node", ConnID: "c1"}
	peerConn := newClient(context.Background(), nil)
	peerConn.lease = repository.ConnLease{UserID: 10002, SessionID: 2, DeviceID: "desktop", NodeID: "test-node", ConnID: "c2"}
	outsiderConn := newClient(context.Background(), nil)
	outsiderConn.lease = repository.ConnLease{UserID: 10003, SessionID: 3, DeviceID: "mobile", NodeID: "test-node", ConnID: "c3"}

	hub.peers.add(10001, "c1", senderConn)
	hub.peers.add(10002, "c2", peerConn)
	hub.peers.add(10003, "c3", outsiderConn)

	// 1. Valid msg.send produces msg.ack (with req_id) followed by msg.push to online members.
	hub.dispatch(senderConn, envelope{
		Type:  "msg.send",
		ReqID: "req-send-1",
		Data:  json.RawMessage(`{"conversation_id":"90001","client_msg_id":"cid-ws-1","msg_type":"text","content":"hello ws","extra":{}}`),
	}, time.Now().Add(time.Hour))

	ack := <-senderConn.send
	if ack.Type != "msg.ack" || ack.ReqID != "req-send-1" {
		t.Fatalf("expected msg.ack first, got %+v", ack)
	}
	ackPayload := ack.Data.(service.MsgAckPayload)
	if ackPayload.ClientMsgID != "cid-ws-1" || ackPayload.Seq != "1" || ackPayload.Status != "normal" || ackPayload.DeliveryStatus != "sent" {
		t.Fatalf("unexpected msg.ack payload: %+v", ackPayload)
	}

	senderPush := <-senderConn.send
	peerPush := <-peerConn.send
	if senderPush.Type != "msg.push" || peerPush.Type != "msg.push" {
		t.Fatalf("expected msg.push on online members, got sender=%s peer=%s", senderPush.Type, peerPush.Type)
	}
	select {
	case unexpected := <-outsiderConn.send:
		t.Fatalf("outsider must not receive conversation msg.push: %+v", unexpected)
	default:
	}

	// 2. Idempotent retry returns msg.ack with same msg_id/seq and NO duplicate msg.push.
	hub.dispatch(senderConn, envelope{
		Type:  "msg.send",
		ReqID: "req-send-retry",
		Data:  json.RawMessage(`{"conversation_id":"90001","client_msg_id":"cid-ws-1","msg_type":"text","content":"hello ws"}`),
	}, time.Now().Add(time.Hour))
	retryAck := <-senderConn.send
	if retryAck.Type != "msg.ack" || retryAck.ReqID != "req-send-retry" || retryAck.Data.(service.MsgAckPayload).MsgID != ackPayload.MsgID {
		t.Fatalf("unexpected retry ack: %+v", retryAck)
	}
	select {
	case extra := <-peerConn.send:
		t.Fatalf("retry must not send duplicate msg.push: %+v", extra)
	default:
	}

	// 3. Non-member msg.send returns 18005.
	hub.dispatch(outsiderConn, envelope{
		Type:  "msg.send",
		ReqID: "req-denied",
		Data:  json.RawMessage(`{"conversation_id":"90001","client_msg_id":"cid-outsider","msg_type":"text","content":"x"}`),
	}, time.Now().Add(time.Hour))
	denied := <-outsiderConn.send
	if denied.Type != "error" || denied.Data.(frameError).Code != errcode.ErrWSConvAccessDenied.Code {
		t.Fatalf("expected 18005 for non-member send, got %+v", denied)
	}

	// 4. msg.recall returns msg.recall ack then broadcasts msg.push(recall) + system.notice(message.recalled).
	hub.dispatch(senderConn, envelope{
		Type:  "msg.recall",
		ReqID: "req-recall-1",
		Data:  json.RawMessage(`{"conversation_id":"90001","msg_id":"` + ackPayload.MsgID + `"}`),
	}, time.Now().Add(time.Hour))
	recallAck := <-senderConn.send
	if recallAck.Type != "msg.recall" || recallAck.ReqID != "req-recall-1" {
		t.Fatalf("expected msg.recall ack, got %+v", recallAck)
	}
	recallPayload := recallAck.Data.(service.MsgRecallAckPayload)
	if recallPayload.RecalledMsgID != ackPayload.MsgID || recallPayload.Seq != "2" {
		t.Fatalf("unexpected msg.recall payload: %+v", recallPayload)
	}
	if p1 := <-peerConn.send; p1.Type != "msg.push" {
		t.Fatalf("expected peer msg.push for recall record, got %+v", p1)
	}
	if p2 := <-peerConn.send; p2.Type != "system.notice" || p2.Data.(service.SystemNoticePayload).Event != "message.recalled" {
		t.Fatalf("expected peer system.notice(message.recalled), got %+v", p2)
	}
	_, _ = <-senderConn.send, <-senderConn.send

	// 5. Stage 7 frames (msg.delivered_ack, msg.read_ack) MUST continue returning 18004.
	for _, kind := range []string{"msg.delivered_ack", "msg.read_ack"} {
		hub.dispatch(peerConn, envelope{Type: kind, ReqID: "req-" + kind, Data: json.RawMessage(`{}`)}, time.Now().Add(time.Hour))
		resp := <-peerConn.send
		if resp.Type != "error" || resp.Data.(frameError).Code != errcode.ErrWSStateConflict.Code {
			t.Fatalf("expected 18004 for Stage 7 frame %s, got %+v", kind, resp)
		}
	}

	// 6. Retryable flag covers 14007, 14008, 19001, 19002, 18007 only.
	if !errorFrame(envelope{Type: "msg.send", ReqID: "r"}, errcode.ErrMessageInProgress).Data.(frameError).Retryable {
		t.Fatal("expected 14007 to be retryable")
	}
	if errorFrame(envelope{Type: "msg.send", ReqID: "r"}, errcode.ErrClientMsgIDConflict).Data.(frameError).Retryable {
		t.Fatal("expected 14001 not to be retryable")
	}
}

type stubWSAuthState struct {
	sessions map[uint64]repository.SessionAuthState
}

func (s *stubWSAuthState) GetUserAuthStatus(_ context.Context, _ uint64) (int8, string, error) {
	return model.UserStatusActive, model.UserRoleUser, nil
}
func (s *stubWSAuthState) IsAccessJTIBlacklisted(context.Context, string) (bool, string, error) {
	return false, "", nil
}
func (s *stubWSAuthState) GetSessionAuthState(_ context.Context, sid uint64) (repository.SessionAuthState, error) {
	return s.sessions[sid], nil
}

func TestWSLiveSocketSendAndRecallPushFlow(t *testing.T) {
	jwtMgr, err := pkgjwt.NewManager(config.JWTConfig{
		Secret:     "ws-chat-live-socket-test-secret-32b",
		Issuer:     "test",
		AccessTTL:  time.Hour,
		RefreshTTL: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	tok1, jti1, _, err := jwtMgr.IssueAccessToken(10001, 1, model.UserRoleUser)
	if err != nil {
		t.Fatal(err)
	}
	tok2, jti2, _, err := jwtMgr.IssueAccessToken(10002, 2, model.UserRoleUser)
	if err != nil {
		t.Fatal(err)
	}
	authState := &stubWSAuthState{
		sessions: map[uint64]repository.SessionAuthState{
			1: {ID: 1, UserID: 10001, DeviceID: "web-1", AccessJTI: jti1, ExpiresAt: time.Now().Add(time.Hour)},
			2: {ID: 2, UserID: 10002, DeviceID: "web-2", AccessJTI: jti2, ExpiresAt: time.Now().Add(time.Hour)},
		},
	}
	cfg := &config.Config{
		App: config.AppConfig{NodeID: "ws-node-1"},
		WS:  config.WSConfig{HeartbeatTimeout: 30 * time.Second, WriteTimeout: 5 * time.Second, MaxFrameBytes: 1 << 20},
	}
	auth := middleware.NewAccessAuthenticator(jwtMgr, authState)
	hub := NewHub(cfg, auth, nil, nil)
	hub.SetChatService(newStubWSChatService(hub))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hub.Shutdown(ctx)
	}()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("access_token")
		id, failure := hub.Authenticate(r.Context(), token)
		if failure != nil {
			http.Error(w, failure.Error(), http.StatusUnauthorized)
			return
		}
		conn, upErr := upgrader.Upgrade(w, r, nil)
		if upErr != nil {
			return
		}
		// Register peer directly via hub.peers and run pumps without Redis store.
		c := newClient(r.Context(), conn)
		c.auth = id
		c.lease = repository.ConnLease{UserID: id.UserID, SessionID: id.SessionID, DeviceID: id.DeviceID, NodeID: cfg.App.NodeID, ConnID: "conn-" + strconv.FormatUint(id.UserID, 10)}
		if !hub.admit(c) {
			c.drain()
			c.writePump(cfg.WS.WriteTimeout)
			return
		}
		defer hub.active.Done()
		defer hub.peers.remove(id.UserID, c.lease.ConnID, c)
		done := make(chan struct{})
		go func() { defer close(done); c.writePump(cfg.WS.WriteTimeout) }()
		c.readPump(hub)
		<-done
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c1, _, err := websocket.DefaultDialer.Dial(wsURL+"?access_token="+tok1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, _, err := websocket.DefaultDialer.Dial(wsURL+"?access_token="+tok2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	// Ensure both connections have completed admission before sending.
	for _, conn := range []*websocket.Conn{c1, c2} {
		_ = conn.WriteJSON(map[string]any{
			"type": "error", "req_id": "sync", "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"code": 18002, "message": "sync"},
		})
	}
	time.Sleep(30 * time.Millisecond)

	if err := c1.WriteJSON(map[string]any{
		"type":      "msg.send",
		"req_id":    "live-req-1",
		"timestamp": time.Now().UnixMilli(),
		"data": map[string]any{
			"conversation_id": "90001",
			"client_msg_id":   "live-cid-1",
			"msg_type":        "text",
			"content":         "real socket hello",
			"extra":           map[string]any{},
		},
	}); err != nil {
		t.Fatal(err)
	}

	_ = c1.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ackFrame envelope
	if err := c1.ReadJSON(&ackFrame); err != nil || ackFrame.Type != "msg.ack" || ackFrame.ReqID != "live-req-1" {
		t.Fatalf("expected msg.ack on c1, got %+v err=%v", ackFrame, err)
	}

	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	var pushFrame envelope
	if err := c2.ReadJSON(&pushFrame); err != nil || pushFrame.Type != "msg.push" {
		t.Fatalf("expected msg.push on c2, got %+v err=%v", pushFrame, err)
	}
}
