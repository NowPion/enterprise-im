package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type APIEnvelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	RequestID string          `json:"request_id"`
}

type APIError struct {
	Method     string
	Path       string
	HTTPStatus int
	Code       int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s status=%d code=%d msg=%s", e.Method, e.Path, e.HTTPStatus, e.Code, e.Message)
}

type RegisterData struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

type LoginData struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		SessionID    string `json:"session_id"`
		DeviceID     string `json:"device_id"`
	} `json:"tokens"`
}

type AdminStreamsSummary struct {
	MessagesLen     int64 `json:"messages_len"`
	EventsLen       int64 `json:"events_len"`
	PendingTotal    int64 `json:"pending_total"`
	DeadLetterCount int64 `json:"dead_letter_count"`
}

type AdminMetricsOverview struct {
	OnlineUsers       int64               `json:"online_users"`
	WSConnections     int64               `json:"ws_connections"`
	MessagesPerMinute int64               `json:"messages_per_minute"`
	Streams           AdminStreamsSummary `json:"streams"`
	Goroutines        int                 `json:"goroutines"`
	HeapBytes         uint64              `json:"heap_bytes"`
	Timestamp         string              `json:"timestamp"`
}

type AdminStreamRow struct {
	Stream           string `json:"stream"`
	Group            string `json:"group"`
	Consumer         string `json:"consumer"`
	LastDeliveredID  string `json:"last_delivered_id"`
	PendingCount     int64  `json:"pending_count"`
	OldestPendingAge int64  `json:"oldest_pending_age"`
}

type APIClient struct {
	baseURL string
	http    *http.Client
}

func NewAPIClient(baseURL string, timeout time.Duration) *APIClient {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     60 * time.Second,
	}
	return &APIClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout:   timeout,
			Transport: tr,
		},
	}
}

func (c *APIClient) doJSON(ctx context.Context, method, path, bearer string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("new request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read %s %s response: %w", method, path, err)
	}

	var env APIEnvelope
	if err := json.Unmarshal(rawResp, &env); err != nil {
		return fmt.Errorf("%s %s status=%d invalid json: %w", method, path, resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || env.Code != 0 {
		return &APIError{
			Method:     method,
			Path:       path,
			HTTPStatus: resp.StatusCode,
			Code:       env.Code,
			Message:    env.Message,
		}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode %s %s data: %w", method, path, err)
		}
	}
	return nil
}

func (c *APIClient) Healthz(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/healthz", "", nil, nil)
}

func (c *APIClient) Register(ctx context.Context, username, password, nickname string) (RegisterData, error) {
	var out RegisterData
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"username": username,
		"password": password,
		"nickname": nickname,
	}, &out)
	return out, err
}

func (c *APIClient) Login(ctx context.Context, username, password, deviceID, deviceType string) (LoginData, error) {
	var out LoginData
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username":    username,
		"password":    password,
		"device_id":   deviceID,
		"device_type": deviceType,
	}, &out)
	return out, err
}

func (c *APIClient) SendFriendRequest(ctx context.Context, token, toUserID, message string) (string, error) {
	var out struct {
		Request struct {
			ID string `json:"id"`
		} `json:"request"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/friend-requests", token, map[string]any{
		"to_user_id": toUserID,
		"message":    message,
	}, &out)
	if err != nil {
		return "", err
	}
	if out.Request.ID == "" {
		return "", errors.New("empty friend request id")
	}
	return out.Request.ID, nil
}

func (c *APIClient) AcceptFriendRequest(ctx context.Context, token, requestID string) (string, error) {
	var out struct {
		ConversationID string `json:"conversation_id"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/friend-requests/"+requestID+"/accept", token, map[string]any{}, &out)
	if err != nil {
		return "", err
	}
	if out.ConversationID == "" {
		return "", errors.New("empty conversation_id from friend accept")
	}
	return out.ConversationID, nil
}

func (c *APIClient) CreateGroup(ctx context.Context, token, name string, memberIDs []string) (string, error) {
	var out struct {
		Group struct {
			ID             string `json:"id"`
			ConversationID string `json:"conversation_id"`
		} `json:"group"`
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/groups", token, map[string]any{
		"name":       name,
		"member_ids": memberIDs,
	}, &out)
	if err != nil {
		return "", err
	}
	convID := out.Conversation.ID
	if convID == "" {
		convID = out.Group.ConversationID
	}
	if convID == "" {
		return "", errors.New("empty conversation_id from create group")
	}
	return convID, nil
}

func (c *APIClient) AdminMetricsOverview(ctx context.Context, adminToken string) (AdminMetricsOverview, error) {
	var out AdminMetricsOverview
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/admin/metrics/overview", adminToken, nil, &out)
	return out, err
}

func (c *APIClient) AdminMetricsStreams(ctx context.Context, adminToken string) ([]AdminStreamRow, error) {
	var out struct {
		Items []AdminStreamRow `json:"items"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/admin/metrics/streams", adminToken, nil, &out)
	return out.Items, err
}

type WSEnvelope struct {
	Type      string          `json:"type"`
	ReqID     string          `json:"req_id"`
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type MsgAckData struct {
	ClientMsgID    string `json:"client_msg_id"`
	MsgID          string `json:"msg_id"`
	ConversationID string `json:"conversation_id"`
	Seq            string `json:"seq"`
	Status         string `json:"status"`
	DeliveryStatus string `json:"delivery_status"`
}

type WSErrorData struct {
	Code         int    `json:"code"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable"`
	RelatedType  string `json:"related_type"`
	RelatedReqID string `json:"related_req_id"`
}

type MsgPushData struct {
	PushType       string `json:"push_type"`
	DeliveryStatus string `json:"delivery_status"`
	Message        struct {
		MsgID          string `json:"msg_id"`
		ClientMsgID    string `json:"client_msg_id"`
		ConversationID string `json:"conversation_id"`
		Seq            string `json:"seq"`
		SenderID       string `json:"sender_id"`
		MsgType        string `json:"msg_type"`
		Content        string `json:"content"`
		Status         string `json:"status"`
	} `json:"message"`
}

type pendingReply struct {
	ack   *MsgAckData
	wsErr *WSErrorData
	pong  bool
}

type WSConn struct {
	userID           uint64
	conn             *websocket.Conn
	tracker          *Tracker
	onUnexpectedDrop func()

	writeMu sync.Mutex
	closed  atomic.Bool
	expDone atomic.Bool
	done    chan struct{}

	pendingMu sync.Mutex
	pending   map[string]chan pendingReply
}

func DialWS(
	ctx context.Context,
	wsURL string,
	userID uint64,
	accessToken string,
	tracker *Tracker,
	onUnexpectedDrop func(),
) (*WSConn, time.Duration, error) {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return nil, 0, fmt.Errorf("parse ws url: %w", err)
	}
	q := parsed.Query()
	q.Set("access_token", accessToken)
	parsed.RawQuery = q.Encode()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}
	start := time.Now()
	conn, resp, err := dialer.DialContext(ctx, parsed.String(), nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, 0, err
	}
	dur := time.Since(start)

	w := &WSConn{
		userID:           userID,
		conn:             conn,
		tracker:          tracker,
		onUnexpectedDrop: onUnexpectedDrop,
		done:             make(chan struct{}),
		pending:          make(map[string]chan pendingReply),
	}
	go w.readLoop()
	return w, dur, nil
}

func (w *WSConn) MarkExpectedClose() {
	w.expDone.Store(true)
}

func (w *WSConn) readLoop() {
	defer close(w.done)
	for {
		msgType, raw, err := w.conn.ReadMessage()
		if err != nil {
			if !w.expDone.Load() && w.onUnexpectedDrop != nil {
				w.onUnexpectedDrop()
			}
			w.failAllPending()
			return
		}
		if msgType != websocket.TextMessage {
			continue
		}
		var env WSEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		w.handleEnvelope(env)
	}
}

func (w *WSConn) handleEnvelope(env WSEnvelope) {
	switch env.Type {
	case "pong":
		w.resolvePending(env.ReqID, pendingReply{pong: true})
	case "msg.ack":
		var ack MsgAckData
		if json.Unmarshal(env.Data, &ack) == nil {
			w.resolvePending(env.ReqID, pendingReply{ack: &ack})
		}
	case "error":
		var wsErr WSErrorData
		if json.Unmarshal(env.Data, &wsErr) == nil {
			if w.tracker != nil {
				w.tracker.RecordWSErrorFrame()
			}
			targetReq := wsErr.RelatedReqID
			if targetReq == "" {
				targetReq = env.ReqID
			}
			w.resolvePending(targetReq, pendingReply{wsErr: &wsErr})
		}
	case "msg.push":
		var push MsgPushData
		if json.Unmarshal(env.Data, &push) != nil {
			return
		}
		if push.PushType != "message" {
			return
		}
		msgID, err1 := strconv.ParseUint(push.Message.MsgID, 10, 64)
		convID, err2 := strconv.ParseUint(push.Message.ConversationID, 10, 64)
		seq, err3 := strconv.ParseUint(push.Message.Seq, 10, 64)
		senderID, _ := strconv.ParseUint(push.Message.SenderID, 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || msgID == 0 || convID == 0 || seq == 0 {
			return
		}
		if w.tracker != nil {
			w.tracker.RecordPushDelivery(PushDelivery{
				ReceiverUserID: w.userID,
				ConvID:         convID,
				MsgID:          msgID,
				Seq:            seq,
				SenderID:       senderID,
				ClientMsgID:    push.Message.ClientMsgID,
				ReceivedAt:     time.Now(),
			})
		}
	default:
		// Ignore presence.update, system.notice, etc.
	}
}

func (w *WSConn) resolvePending(reqID string, reply pendingReply) {
	if reqID == "" {
		return
	}
	w.pendingMu.Lock()
	ch, ok := w.pending[reqID]
	if ok {
		delete(w.pending, reqID)
	}
	w.pendingMu.Unlock()
	if ok && ch != nil {
		select {
		case ch <- reply:
		default:
		}
	}
}

func (w *WSConn) failAllPending() {
	w.pendingMu.Lock()
	pending := w.pending
	w.pending = make(map[string]chan pendingReply)
	w.pendingMu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}

func (w *WSConn) sendEnvelope(reqID, frameType string, data any) (chan pendingReply, error) {
	if w.closed.Load() {
		return nil, errors.New("ws connection closed")
	}
	ch := make(chan pendingReply, 1)
	w.pendingMu.Lock()
	w.pending[reqID] = ch
	w.pendingMu.Unlock()

	payload := map[string]any{
		"type":      frameType,
		"req_id":    reqID,
		"timestamp": time.Now().UnixMilli(),
		"data":      data,
	}

	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.closed.Load() {
		w.removePending(reqID)
		return nil, errors.New("ws connection closed")
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := w.conn.WriteJSON(payload); err != nil {
		w.removePending(reqID)
		return nil, err
	}
	return ch, nil
}

func (w *WSConn) removePending(reqID string) {
	w.pendingMu.Lock()
	delete(w.pending, reqID)
	w.pendingMu.Unlock()
}

func (w *WSConn) Ping(ctx context.Context, timeout time.Duration) error {
	reqID := "ping-" + uuid.NewString()
	if w.tracker != nil {
		w.tracker.RecordHeartbeatPing()
	}
	start := time.Now()
	ch, err := w.sendEnvelope(reqID, "ping", map[string]any{
		"nonce": reqID,
	})
	if err != nil {
		return err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reply, ok := <-ch:
		if !ok {
			return errors.New("ws closed while awaiting pong")
		}
		if reply.wsErr != nil {
			return fmt.Errorf("ping error code=%d msg=%s", reply.wsErr.Code, reply.wsErr.Message)
		}
		if reply.pong {
			if w.tracker != nil {
				w.tracker.RecordHeartbeatPong(time.Since(start))
			}
			return nil
		}
		return errors.New("unexpected reply to ping")
	case <-timer.C:
		w.removePending(reqID)
		return fmt.Errorf("pong timeout after %s", timeout)
	case <-ctx.Done():
		w.removePending(reqID)
		return ctx.Err()
	}
}

func (w *WSConn) SendTextMessage(
	ctx context.Context,
	convID, clientMsgID, content string,
	timeout time.Duration,
) (MsgAckData, *WSErrorData, error) {
	reqID := "send-" + uuid.NewString()
	sentAt := time.Now()
	if w.tracker != nil {
		w.tracker.RecordSendStart(w.userID, clientMsgID, sentAt)
	}
	ch, err := w.sendEnvelope(reqID, "msg.send", map[string]any{
		"conversation_id": convID,
		"client_msg_id":   clientMsgID,
		"msg_type":        "text",
		"content":         content,
		"extra":           map[string]any{},
	})
	if err != nil {
		return MsgAckData{}, nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reply, ok := <-ch:
		if !ok {
			return MsgAckData{}, nil, errors.New("ws closed while awaiting msg.ack")
		}
		if reply.wsErr != nil {
			return MsgAckData{}, reply.wsErr, nil
		}
		if reply.ack == nil {
			return MsgAckData{}, nil, errors.New("missing msg.ack payload")
		}
		if w.tracker != nil {
			w.tracker.RecordAckLatency(time.Since(sentAt))
		}
		return *reply.ack, nil, nil
	case <-timer.C:
		w.removePending(reqID)
		return MsgAckData{}, nil, fmt.Errorf("msg.ack timeout after %s", timeout)
	case <-ctx.Done():
		w.removePending(reqID)
		return MsgAckData{}, nil, ctx.Err()
	}
}

func (w *WSConn) CloseGracefully(timeout time.Duration) error {
	w.MarkExpectedClose()
	if !w.closed.CompareAndSwap(false, true) {
		select {
		case <-w.done:
		case <-time.After(timeout):
		}
		return nil
	}

	w.writeMu.Lock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = w.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "loadtest complete"), time.Now().Add(2*time.Second))
	w.writeMu.Unlock()

	select {
	case <-w.done:
		return w.conn.Close()
	case <-time.After(timeout):
		err := w.conn.Close()
		<-w.done
		return err
	}
}
