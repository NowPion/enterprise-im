package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/snowflake"
)

const (
	stage7IOTimeout     = 8 * time.Second
	stage7QuietTimeout  = 450 * time.Millisecond
	stage7ReclaimWait   = 70 * time.Second
	stage7StreamMsgsKey = "im:stream:messages"
	stage7StreamEvtsKey = "im:stream:events"
	stage7GroupMsgDeliv = "im-msg-deliver"
	stage7GroupEvtDeliv = "im-event-deliver"
)

type stage7AckPayload struct {
	ClientMsgID    string `json:"client_msg_id"`
	MsgID          string `json:"msg_id"`
	ConversationID string `json:"conversation_id"`
	Seq            string `json:"seq"`
	Status         string `json:"status"`
	DeliveryStatus string `json:"delivery_status"`
}

type stage7ReceiptPushDTO struct {
	PushType         string  `json:"push_type"`
	ConversationID   string  `json:"conversation_id"`
	UserID           string  `json:"user_id"`
	DeliveryStatus   string  `json:"delivery_status"`
	MsgID            *string `json:"msg_id"`
	Seq              *string `json:"seq"`
	LastDeliveredSeq string  `json:"last_delivered_seq"`
	LastReadSeq      string  `json:"last_read_seq"`
	UpdatedAt        string  `json:"updated_at"`
}

type stage7WatermarkRespDTO struct {
	ConversationID   string  `json:"conversation_id"`
	LastDeliveredSeq string  `json:"last_delivered_seq"`
	LastReadSeq      string  `json:"last_read_seq"`
	UnreadCount      *int    `json:"unread_count"`
	ReadAt           *string `json:"read_at"`
	DeliveredAt      *string `json:"delivered_at"`
}

type stage7ReceiptUserDTO struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
}

type stage7MessageReceiptItemDTO struct {
	User             stage7ReceiptUserDTO `json:"user"`
	Role             string               `json:"role"`
	DeliveryStatus   string               `json:"delivery_status"`
	LastDeliveredSeq string               `json:"last_delivered_seq"`
	LastReadSeq      string               `json:"last_read_seq"`
	DeliveredAt      *string              `json:"delivered_at"`
	ReadAt           *string              `json:"read_at"`
}

type stage7MessageReceiptsDTO struct {
	MsgID          string                        `json:"msg_id"`
	ConversationID string                        `json:"conversation_id"`
	Seq            string                        `json:"seq"`
	TotalMembers   int                           `json:"total_members"`
	DeliveredCount int                           `json:"delivered_count"`
	ReadCount      int                           `json:"read_count"`
	UnreadCount    int                           `json:"unread_count"`
	Items          []stage7MessageReceiptItemDTO `json:"items"`
}

func newStage7Rig(t *testing.T) *stage6Rig {
	t.Helper()
	rig := newStage6Rig(t)
	expectDBAvailable(t, rig.db)
	if rig.redis == nil {
		t.Fatal("expected live Redis client in Stage 7 test rig")
	}
	return rig
}

func decodeStage7Ack(t *testing.T, frame stage6WSFrame) stage7AckPayload {
	t.Helper()
	if frame.Type != "msg.ack" {
		t.Fatalf("expected msg.ack frame, got %s: %s", frame.Type, string(frame.Data))
	}
	return decodeStage6JSON[stage7AckPayload](t, frame.Data)
}

func decodeStage7ReceiptPush(frame stage6WSFrame) (stage7ReceiptPushDTO, bool) {
	if frame.Type != "msg.push" {
		return stage7ReceiptPushDTO{}, false
	}
	var out stage7ReceiptPushDTO
	if err := json.Unmarshal(frame.Data, &out); err != nil {
		return stage7ReceiptPushDTO{}, false
	}
	if out.PushType != "receipt" {
		return stage7ReceiptPushDTO{}, false
	}
	return out, true
}

func sendDeliveredAckWS(t *testing.T, ws *stage6WSClient, convID, msgID, seq string) *stage6WSErrorData {
	t.Helper()
	reqID := "deliv-" + uuid.NewString()
	ws.send("msg.delivered_ack", reqID, map[string]any{
		"msg_id":          msgID,
		"conversation_id": convID,
		"seq":             seq,
	})
	return awaitWSCommandSettled(t, ws, reqID)
}

func sendReadAckWS(t *testing.T, ws *stage6WSClient, convID, lastReadSeq string) *stage6WSErrorData {
	t.Helper()
	reqID := "read-" + uuid.NewString()
	ws.send("msg.read_ack", reqID, map[string]any{
		"conversation_id": convID,
		"last_read_seq":   lastReadSeq,
	})
	return awaitWSCommandSettled(t, ws, reqID)
}

// awaitWSCommandSettled sends a ping immediately after a fire-and-forget WS receipt frame
// so any synchronous error frame for reqID arrives before pong.
func awaitWSCommandSettled(t *testing.T, ws *stage6WSClient, reqID string) *stage6WSErrorData {
	t.Helper()
	pingID := "ping-" + uuid.NewString()
	ws.send("ping", pingID, map[string]string{"nonce": pingID})

	var sawErr *stage6WSErrorData
	ws.awaitFrame("pong after "+reqID, stage7IOTimeout, func(f stage6WSFrame) bool {
		if f.Type == "error" && f.ReqID == reqID {
			e := decodeWSError(t, f)
			sawErr = &e
			return false
		}
		return f.Type == "pong" && f.ReqID == pingID
	})
	return sawErr
}

func awaitReceiptPush(t *testing.T, ws *stage6WSClient, convID, peerUserID, wantDeliveryStatus string, minWatermark uint64) stage7ReceiptPushDTO {
	t.Helper()
	var matched stage7ReceiptPushDTO
	ws.awaitFrame(
		fmt.Sprintf("receipt push conv=%s user=%s status=%s seq>=%d", convID, peerUserID, wantDeliveryStatus, minWatermark),
		stage7IOTimeout,
		func(f stage6WSFrame) bool {
			rcpt, ok := decodeStage7ReceiptPush(f)
			if !ok {
				return false
			}
			if rcpt.ConversationID != convID || rcpt.UserID != peerUserID || rcpt.DeliveryStatus != wantDeliveryStatus {
				return false
			}
			var seqStr string
			if wantDeliveryStatus == model.ReceiptStatusRead {
				seqStr = rcpt.LastReadSeq
			} else {
				seqStr = rcpt.LastDeliveredSeq
			}
			seqVal, err := strconv.ParseUint(seqStr, 10, 64)
			if err != nil || seqVal < minWatermark {
				return false
			}
			matched = rcpt
			return true
		},
	)
	return matched
}

func createGroupWithMembers(t *testing.T, rig *stage6Rig, owner stage6User, name string, memberIDs ...string) (string, string) {
	t.Helper()
	st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/groups", owner.AccessToken, map[string]any{
		"name":       name,
		"member_ids": memberIDs,
	})
	assertStage6OK(t, "create group "+name, st, env, http.StatusCreated)
	grp, convID := extractGroupCreateData(t, env.Data)
	rig.trackGroup(mustParseUint64(t, grp.ID))
	rig.trackConv(mustParseUint64(t, convID))
	return grp.ID, convID
}

func getConvDetail(t *testing.T, rig *stage6Rig, user stage6User, convID string) stage6ConversationDTO {
	t.Helper()
	st, _, env, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID, user.AccessToken, nil)
	assertStage6OK(t, "get conversation detail "+convID, st, env, http.StatusOK)
	return extractConversationFromDetail(t, env.Data)
}

func getConvMemberDB(t *testing.T, rig *stage6Rig, convID, userID uint64) model.ConversationMember {
	t.Helper()
	var m model.ConversationMember
	if err := rig.db.WithContext(t.Context()).
		Where("conversation_id = ? AND user_id = ?", convID, userID).
		Take(&m).Error; err != nil {
		t.Fatalf("query conversation_member conv=%d user=%d: %v", convID, userID, err)
	}
	return m
}

func getMessageReceiptDB(t *testing.T, rig *stage6Rig, msgID, userID uint64) model.MessageReceipt {
	t.Helper()
	var r model.MessageReceipt
	if err := rig.db.WithContext(t.Context()).
		Where("message_id = ? AND user_id = ?", msgID, userID).
		Take(&r).Error; err != nil {
		t.Fatalf("query message_receipt msg=%d user=%d: %v", msgID, userID, err)
	}
	return r
}

func getMessageReceiptsHTTP(t *testing.T, rig *stage6Rig, user stage6User, convID, msgID, statusFilter string) stage7MessageReceiptsDTO {
	t.Helper()
	path := "/api/v1/conversations/" + convID + "/messages/" + msgID + "/receipts"
	if statusFilter != "" {
		path += "?status=" + statusFilter
	}
	st, _, env, _ := rig.doRequest(http.MethodGet, path, user.AccessToken, nil)
	assertStage6OK(t, "GET "+path, st, env, http.StatusOK)
	return decodeStage6JSON[stage7MessageReceiptsDTO](t, env.Data)
}

func findReceiptItemByUser(items []stage7MessageReceiptItemDTO, userID string) (stage7MessageReceiptItemDTO, bool) {
	for _, it := range items {
		if it.User.ID == userID {
			return it, true
		}
	}
	return stage7MessageReceiptItemDTO{}, false
}

func assertValidSnowflakeID(t *testing.T, rawID string, minTimeMs, maxTimeMs int64) uint64 {
	t.Helper()
	id := mustParseUint64(t, rawID)
	if id == 0 {
		t.Fatal("snowflake msg_id must not be 0")
	}
	tsShift := 22 // nodeBits(10) + sequenceBits(12)
	embeddedMs := int64(id>>tsShift) + snowflake.CustomEpochMillis
	if embeddedMs < minTimeMs-5000 || embeddedMs > maxTimeMs+5000 {
		t.Fatalf("msg_id %s embedded timestamp %d outside expected test window [%d, %d]", rawID, embeddedMs, minTimeMs, maxTimeMs)
	}
	return id
}

func streamValueString(values map[string]any, key string) string {
	v, ok := values[key]
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func findStreamEntriesByField(ctx context.Context, rdb *redis.Client, streamKey, field, wantVal string) ([]redis.XMessage, error) {
	msgs, err := rdb.XRange(ctx, streamKey, "-", "+").Result()
	if err != nil {
		return nil, err
	}
	var out []redis.XMessage
	for _, m := range msgs {
		if streamValueString(m.Values, field) == wantVal {
			out = append(out, m)
		}
	}
	return out, nil
}

func findStreamGroup(ctx context.Context, rdb *redis.Client, streamKey, groupName string) (*redis.XInfoGroup, error) {
	groups, err := rdb.XInfoGroups(ctx, streamKey).Result()
	if err != nil {
		return nil, err
	}
	for i := range groups {
		if groups[i].Name == groupName {
			return &groups[i], nil
		}
	}
	return nil, nil
}

func assertWatermarkRollbackSafe(t *testing.T, op string, status int, env stage6Envelope) {
	t.Helper()
	if status == http.StatusOK && env.Code == 0 {
		return
	}
	assertStage6BizErr(t, op, status, env, errcode.ErrWatermarkRollback.Code)
}

func assertWSWatermarkRollbackSafe(t *testing.T, op string, wsErr *stage6WSErrorData) {
	t.Helper()
	if wsErr == nil {
		return
	}
	if wsErr.Code != errcode.ErrWatermarkRollback.Code {
		t.Fatalf("%s returned unexpected WS error %+v (want nil or 13005)", op, *wsErr)
	}
}
