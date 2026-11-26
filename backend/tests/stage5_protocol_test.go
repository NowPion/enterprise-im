package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

func stage5Failure(t *testing.T, conn *websocket.Conn, want *errcode.Error, kind, reqID string) {
	t.Helper()
	frame := stage5Await(t, conn, "error", nil)
	var failure struct {
		Code         int    `json:"code"`
		Message      string `json:"message"`
		Retryable    *bool  `json:"retryable"`
		RelatedType  string `json:"related_type"`
		RelatedReqID string `json:"related_req_id"`
	}
	stage5Check(t, json.Unmarshal(frame.Data, &failure))
	if failure.Code != want.Code || failure.Message == "" || failure.Retryable == nil || *failure.Retryable {
		t.Fatalf("%s error code=%d, want a non-retryable %d with a message", kind, failure.Code, want.Code)
	}
	if reqID != "" && (frame.ReqID != reqID || failure.RelatedReqID != reqID || failure.RelatedType != kind) {
		t.Fatalf("error for %s/%s referenced %s/%s (envelope %q)", kind, reqID, failure.RelatedType, failure.RelatedReqID, frame.ReqID)
	}
}

func stage5HandshakeDenied(t *testing.T, rig *stage5Rig, token string, bearer bool) {
	t.Helper()
	response, body := rig.dialDenied(t, token, bearer)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/ws handshake returned HTTP %d, want %d; body=%s", response.StatusCode, http.StatusUnauthorized, string(body))
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("/ws handshake Content-Type=%q, want application/json", contentType)
	}
	reqID := response.Header.Get(errcode.HeaderRequestID)
	if reqID == "" {
		t.Fatalf("/ws handshake missing %s header", errcode.HeaderRequestID)
	}
	var env errcode.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode /ws handshake error envelope (%v): %s", err, string(body))
	}
	if env.Code != errcode.ErrWSAuthFailed.Code || env.Message == "" || env.Data != nil || env.RequestID != reqID {
		t.Fatalf("unexpected /ws handshake error envelope: %+v (header request_id=%q, body=%s)", env, reqID, string(body))
	}
}

func TestStage5WSHandshakeInvalidJWT(t *testing.T) {
	rig := newStage5Rig(t, false, time.Minute)
	token, _, _, err := rig.manager.IssueAccessToken(1, 1, model.UserRoleUser)
	stage5Check(t, err)
	parts := strings.Split(token, ".")
	replacement := byte('A')
	if parts[2][0] == replacement {
		replacement = 'B'
	}
	parts[2] = string(replacement) + parts[2][1:]
	for _, tc := range []struct{ name, token string }{
		{"missing", ""}, {"malformed", "invalid"}, {"bad_signature", strings.Join(parts, ".")},
	} {
		t.Run(tc.name, func(t *testing.T) { stage5HandshakeDenied(t, rig, tc.token, false) })
	}
}

func TestStage5WSHandshakeLiveState(t *testing.T) {
	rig := newStage5Rig(t, true, time.Minute)
	for _, change := range []string{"blacklisted_jti", "revoked_session", "disabled_user"} {
		t.Run(change, func(t *testing.T) {
			session := rig.session(t, rig.user(t))
			control := rig.dial(t, session.token, true, nil)
			stage5Ping(t, control)
			ctx := t.Context()
			switch change {
			case "blacklisted_jti":
				stage5Check(t, rig.redis.Set(ctx, repository.TokenBlacklistKey(session.AccessJTI), "logout", time.Minute).Err())
			case "revoked_session":
				stage5Check(t, rig.db.WithContext(ctx).Model(&session.UserSession).Update("revoked_at", time.Now()).Error)
			case "disabled_user":
				stage5Check(t, rig.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", session.UserID).Update("status", model.UserStatusDisabled).Error)
			}
			stage5HandshakeDenied(t, rig, session.token, true)
			rig.count(t, session.UserID, 1)
			reqID := uuid.NewString()
			stage5Send(t, control, "ping", reqID, map[string]string{"nonce": reqID})
			stage5Failure(t, control, errcode.ErrWSSessionRevoked, "ping", reqID)
			stage5ExpectClose(t, control, websocket.ClosePolicyViolation)
			rig.count(t, session.UserID, 0)
			got, err := rig.redis.SCard(ctx, stage5SessionKey(session.ID)).Result()
			stage5Check(t, err)
			if got != 0 {
				t.Fatalf("rejected handshake left %d session routes", got)
			}
		})
	}
}

func TestStage5WSInvalidFrames(t *testing.T) {
	rig := newStage5Rig(t, true, time.Minute)
	conn := rig.connect(t, rig.session(t, rig.user(t)))
	for _, tc := range []struct {
		name, raw, kind, reqID string
		code                   *errcode.Error
	}{
		{"bare_value", `42`, "", "", errcode.ErrWSInvalidEnvelope},
		{"missing_data", fmt.Sprintf(`{"type":"ping","req_id":"no-data","timestamp":%d}`, time.Now().UnixMilli()), "ping", "no-data", errcode.ErrWSInvalidEnvelope},
		{"bad_timestamp", `{"type":"ping","req_id":"bad-time","timestamp":"yesterday","data":{}}`, "ping", "bad-time", errcode.ErrWSInvalidEnvelope},
		{"unknown_type", fmt.Sprintf(`{"type":"qa.unknown","req_id":"unknown","timestamp":%d,"data":{}}`, time.Now().UnixMilli()), "qa.unknown", "unknown", errcode.ErrWSUnknownType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage5Check(t, conn.SetWriteDeadline(time.Now().Add(stage5IO)))
			stage5Check(t, conn.WriteMessage(websocket.TextMessage, []byte(tc.raw)))
			stage5Failure(t, conn, tc.code, tc.kind, tc.reqID)
		})
	}
	stage5Ping(t, conn)
}

func TestStage5WSMsgSendRejectsWithoutPersistence(t *testing.T) {
	rig := newStage5Rig(t, true, time.Minute)
	sender, peer := rig.user(t), rig.user(t)
	rig.friends(t, sender, peer)
	directKey := fmt.Sprintf("%d:%d", min(sender, peer), max(sender, peer))
	conv := model.Conversation{Type: model.ConversationTypeSingle, DirectKey: &directKey}
	stage5Check(t, rig.db.WithContext(t.Context()).Create(&conv).Error)
	rig.cleanup = append(rig.cleanup, func(ctx context.Context) error {
		db := rig.db.WithContext(ctx)
		return errors.Join(db.Where("conversation_id = ?", conv.ID).Delete(&model.MessageReceipt{}).Error,
			db.Where("conversation_id = ?", conv.ID).Delete(&model.Message{}).Error,
			db.Where("conversation_id = ?", conv.ID).Delete(&model.ConversationMember{}).Error, db.Delete(&conv).Error)
	})
	members := []model.ConversationMember{{ConversationID: conv.ID, UserID: sender}, {ConversationID: conv.ID, UserID: peer}}
	stage5Check(t, rig.db.WithContext(t.Context()).Create(&members).Error)
	conn := rig.connect(t, rig.session(t, sender))
	reqID, clientID := uuid.NewString(), uuid.NewString()
	stage5Send(t, conn, "msg.send", reqID, map[string]any{"conversation_id": strconv.FormatUint(conv.ID, 10),
		"client_msg_id": clientID, "msg_type": "text", "content": "stage5", "extra": map[string]any{}})
	stage5Failure(t, conn, errcode.ErrWSStateConflict, "msg.send", reqID)
	stage5Ping(t, conn)
	var count int64
	stage5Check(t, rig.db.WithContext(t.Context()).Model(&model.Message{}).
		Where("conversation_id = ? OR (sender_id = ? AND client_msg_id = ?)", conv.ID, sender, clientID).Count(&count).Error)
	stage5Check(t, rig.db.WithContext(t.Context()).First(&conv, conv.ID).Error)
	if count != 0 || conv.LastSeq != 0 || conv.LastMsgID != 0 {
		t.Fatalf("rejected msg.send left %d messages, last_seq=%d, last_msg_id=%d", count, conv.LastSeq, conv.LastMsgID)
	}
}
