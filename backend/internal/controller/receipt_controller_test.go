package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type receiptCtrlTestStore struct {
	*convCtrlTestStore
	msgs     map[uint64]model.Message
	receipts map[string]model.MessageReceipt
}

func newReceiptCtrlTestStore() *receiptCtrlTestStore {
	s := &receiptCtrlTestStore{
		convCtrlTestStore: newConvCtrlTestStore(),
		msgs:              make(map[uint64]model.Message),
		receipts:          make(map[string]model.MessageReceipt),
	}
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	c := s.convs[90001]
	c.LastSeq = 2
	c.LastMsgID = 1002
	s.convs[90001] = c
	cm2 := s.convMembers[[2]uint64{90001, 10002}]
	cm2.UnreadCount = 2
	s.convMembers[[2]uint64{90001, 10002}] = cm2

	s.msgs[1001] = model.Message{ID: 1001, ConversationID: 90001, Seq: 1, SenderID: 10001, MsgType: model.MessageTypeText, Content: "m1", Status: model.MessageStatusNormal, CreatedAt: now}
	s.msgs[1002] = model.Message{ID: 1002, ConversationID: 90001, Seq: 2, SenderID: 10001, MsgType: model.MessageTypeText, Content: "m2", Status: model.MessageStatusNormal, CreatedAt: now}
	s.receipts["1001:10002"] = model.MessageReceipt{ID: 1, MessageID: 1001, ConversationID: 90001, Seq: 1, UserID: 10002, Status: model.ReceiptStatusSent, CreatedAt: now, UpdatedAt: now}
	s.receipts["1002:10002"] = model.MessageReceipt{ID: 2, MessageID: 1002, ConversationID: 90001, Seq: 2, UserID: 10002, Status: model.ReceiptStatusSent, CreatedAt: now, UpdatedAt: now}
	return s
}

func (s *receiptCtrlTestStore) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error {
	return fn(nil)
}

func (s *receiptCtrlTestStore) LockConversationByIDTx(_ context.Context, _ *gorm.DB, convID uint64) (*model.Conversation, error) {
	return s.GetConversationByID(context.Background(), convID)
}

func (s *receiptCtrlTestStore) HasGroupMemberTx(_ context.Context, _ *gorm.DB, _, _ uint64) (bool, error) {
	return true, nil
}

func (s *receiptCtrlTestStore) LockConversationMemberTx(_ context.Context, _ *gorm.DB, convID, userID uint64) (*model.ConversationMember, error) {
	return s.GetConversationMember(context.Background(), convID, userID)
}

func (s *receiptCtrlTestStore) GetMessageInConversation(_ context.Context, convID, msgID uint64) (*model.Message, error) {
	m, ok := s.msgs[msgID]
	if !ok || m.ConversationID != convID {
		return nil, repository.ErrMessageNotFound
	}
	cp := m
	return &cp, nil
}

func (s *receiptCtrlTestStore) GetMessageInConversationTx(_ context.Context, _ *gorm.DB, convID, msgID uint64) (*model.Message, error) {
	return s.GetMessageInConversation(context.Background(), convID, msgID)
}

func (s *receiptCtrlTestStore) LockMessageReceiptTx(_ context.Context, _ *gorm.DB, msgID, userID uint64) (*model.MessageReceipt, error) {
	rec, ok := s.receipts[fmt.Sprintf("%d:%d", msgID, userID)]
	if !ok {
		return nil, nil
	}
	cp := rec
	return &cp, nil
}

func (s *receiptCtrlTestStore) MarkSingleReceiptDeliveredTx(_ context.Context, _ *gorm.DB, receiptID uint64, now time.Time) (bool, error) {
	ts := now.UTC().Truncate(time.Millisecond)
	for k, rec := range s.receipts {
		if rec.ID == receiptID && rec.Status == model.ReceiptStatusSent {
			rec.Status = model.ReceiptStatusDelivered
			rec.DeliveredAt = &ts
			rec.UpdatedAt = ts
			s.receipts[k] = rec
			return true, nil
		}
	}
	return false, nil
}

func (s *receiptCtrlTestStore) MarkReceiptsDeliveredUpToSeqTx(_ context.Context, _ *gorm.DB, convID, userID, targetSeq uint64, now time.Time) (int64, error) {
	ts := now.UTC().Truncate(time.Millisecond)
	var n int64
	for k, rec := range s.receipts {
		if rec.ConversationID == convID && rec.UserID == userID && rec.Seq <= targetSeq && rec.Status == model.ReceiptStatusSent {
			rec.Status = model.ReceiptStatusDelivered
			rec.DeliveredAt = &ts
			rec.UpdatedAt = ts
			s.receipts[k] = rec
			n++
		}
	}
	return n, nil
}

func (s *receiptCtrlTestStore) MarkReceiptsReadUpToSeqTx(_ context.Context, _ *gorm.DB, convID, userID, targetSeq uint64, now time.Time) (int64, error) {
	ts := now.UTC().Truncate(time.Millisecond)
	var n int64
	for k, rec := range s.receipts {
		if rec.ConversationID == convID && rec.UserID == userID && rec.Seq <= targetSeq &&
			(rec.Status == model.ReceiptStatusSent || rec.Status == model.ReceiptStatusDelivered) {
			rec.Status = model.ReceiptStatusRead
			if rec.DeliveredAt == nil {
				rec.DeliveredAt = &ts
			}
			rec.ReadAt = &ts
			rec.UpdatedAt = ts
			s.receipts[k] = rec
			n++
		}
	}
	return n, nil
}

func (s *receiptCtrlTestStore) UpdateMemberDeliveredWatermarkTx(_ context.Context, _ *gorm.DB, memberID, lastDeliveredSeq uint64, now time.Time) error {
	for k, cm := range s.convMembers {
		if cm.ID == memberID {
			cm.LastDeliveredSeq = lastDeliveredSeq
			cm.UpdatedAt = now
			s.convMembers[k] = cm
			return nil
		}
	}
	return repository.ErrNotConversationMember
}

func (s *receiptCtrlTestStore) UpdateMemberReadWatermarkTx(_ context.Context, _ *gorm.DB, memberID, lastReadSeq, lastDeliveredSeq uint64, now time.Time) error {
	for k, cm := range s.convMembers {
		if cm.ID == memberID {
			cm.LastReadSeq = lastReadSeq
			cm.LastDeliveredSeq = lastDeliveredSeq
			cm.UnreadCount = 0
			cm.UpdatedAt = now
			s.convMembers[k] = cm
			return nil
		}
	}
	return repository.ErrNotConversationMember
}

func (s *receiptCtrlTestStore) ListDistinctSendersInSeqRangeTx(_ context.Context, _ *gorm.DB, _ uint64, _, _ uint64, _ uint64) ([]uint64, error) {
	return []uint64{10001}, nil
}

func (s *receiptCtrlTestStore) QueryMessageReceiptDetails(_ context.Context, conv *model.Conversation, msg *model.Message, statusFilter string, _ uint64, _ int) (repository.ReceiptSummaryCounts, []repository.ReceiptMemberRow, error) {
	var summary repository.ReceiptSummaryCounts
	var rows []repository.ReceiptMemberRow
	for _, cm := range s.convMembers {
		if cm.ConversationID != conv.ID || (msg.SenderID > 0 && cm.UserID == msg.SenderID) {
			continue
		}
		rec := s.receipts[fmt.Sprintf("%d:%d", msg.ID, cm.UserID)]
		status := rec.Status
		if status == "" {
			status = model.ReceiptStatusSent
		}
		summary.TotalMembers++
		if status == model.ReceiptStatusDelivered || status == model.ReceiptStatusRead {
			summary.DeliveredCount++
		}
		if status == model.ReceiptStatusRead {
			summary.ReadCount++
		} else {
			summary.UnreadCount++
		}
		if statusFilter == "" ||
			(statusFilter == "read" && status == model.ReceiptStatusRead) ||
			(statusFilter == "unread" && status != model.ReceiptStatusRead) ||
			(statusFilter == "delivered" && (status == model.ReceiptStatusDelivered || status == model.ReceiptStatusRead)) ||
			(statusFilter == "pending" && status == model.ReceiptStatusSent) {
			rows = append(rows, repository.ReceiptMemberRow{
				CMID:             cm.ID,
				UserID:           cm.UserID,
				DeliveryStatus:   status,
				LastDeliveredSeq: cm.LastDeliveredSeq,
				LastReadSeq:      cm.LastReadSeq,
				DeliveredAt:      rec.DeliveredAt,
				ReadAt:           rec.ReadAt,
			})
		}
	}
	return summary, rows, nil
}

func TestReceiptControllerEndpointsAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newReceiptCtrlTestStore()
	svc := service.NewReceiptService(store, store, nil, nil)
	ctrl := NewReceiptController(svc)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(errcode.ContextKeyRequestID, "r-rcpt-test")
		if uid := c.GetHeader("X-Test-User"); uid != "" {
			errcode.SetUserID(c, uid)
		}
		c.Next()
	})
	r.GET("/api/v1/conversations/:conversation_id/messages/:msg_id/receipts", ctrl.GetMessageReceipts)
	r.POST("/api/v1/conversations/:conversation_id/read", ctrl.MarkRead)
	r.POST("/api/v1/conversations/:conversation_id/delivered", ctrl.MarkDelivered)

	// 1. Missing body / missing field / invalid cursor / exceeding last_seq.
	for _, tc := range []struct {
		name     string
		method   string
		path     string
		body     string
		user     string
		wantCode int
	}{
		{"unauthenticated", http.MethodPost, "/api/v1/conversations/90001/read", `{"last_read_seq":"1"}`, "", errcode.ErrAccessTokenInvalid.Code},
		{"non-member", http.MethodPost, "/api/v1/conversations/90001/read", `{"last_read_seq":"1"}`, "10099", errcode.ErrNotConversationMember.Code},
		{"missing field", http.MethodPost, "/api/v1/conversations/90001/read", `{}`, "10002", errcode.ErrInvalidParams.Code},
		{"bad seq cursor", http.MethodPost, "/api/v1/conversations/90001/read", `{"last_read_seq":"abc"}`, "10002", errcode.ErrInvalidSeqCursor.Code},
		{"exceeds last_seq read", http.MethodPost, "/api/v1/conversations/90001/read", `{"last_read_seq":"99"}`, "10002", errcode.ErrWatermarkExceedsSeq.Code},
		{"exceeds last_seq delivered", http.MethodPost, "/api/v1/conversations/90001/delivered", `{"last_delivered_seq":"99"}`, "10002", errcode.ErrWatermarkExceedsSeq.Code},
		{"invalid status filter", http.MethodGet, "/api/v1/conversations/90001/messages/1001/receipts?status=bogus", "", "10001", errcode.ErrInvalidValue.Code},
		{"invalid limit", http.MethodGet, "/api/v1/conversations/90001/messages/1001/receipts?limit=101", "", "10001", errcode.ErrInvalidValue.Code},
		{"message not found", http.MethodGet, "/api/v1/conversations/90001/messages/9999/receipts", "", "10001", errcode.ErrMessageNotFound.Code},
	} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.user != "" {
			req.Header.Set("X-Test-User", tc.user)
		}
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		var env errcode.Envelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: unmarshal response: %v body=%s", tc.name, err, w.Body.String())
		}
		if env.Code != tc.wantCode {
			t.Fatalf("%s: expected code %d, got %d body=%s", tc.name, tc.wantCode, env.Code, w.Body.String())
		}
	}

	// 2. POST /delivered -> 200 OK.
	wDel := httptest.NewRecorder()
	reqDel, _ := http.NewRequest(http.MethodPost, "/api/v1/conversations/90001/delivered", strings.NewReader(`{"last_delivered_seq":"1"}`))
	reqDel.Header.Set("X-Test-User", "10002")
	reqDel.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("POST /delivered expected 200, got %d body=%s", wDel.Code, wDel.Body.String())
	}

	// 3. POST /read -> 200 OK with last_read_seq="2", unread_count=0, read_at!=empty.
	wRead := httptest.NewRecorder()
	reqRead, _ := http.NewRequest(http.MethodPost, "/api/v1/conversations/90001/read", strings.NewReader(`{"last_read_seq":"2"}`))
	reqRead.Header.Set("X-Test-User", "10002")
	reqRead.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wRead, reqRead)
	if wRead.Code != http.StatusOK {
		t.Fatalf("POST /read expected 200, got %d body=%s", wRead.Code, wRead.Body.String())
	}
	var readEnv struct {
		Code int                         `json:"code"`
		Data service.MarkReadResult `json:"data"`
	}
	if err := json.Unmarshal(wRead.Body.Bytes(), &readEnv); err != nil {
		t.Fatalf("unmarshal read response: %v", err)
	}
	if readEnv.Code != 0 || readEnv.Data.LastReadSeq != 2 || readEnv.Data.UnreadCount != 0 || readEnv.Data.ReadAt == "" {
		t.Fatalf("unexpected POST /read response: %+v", readEnv)
	}

	// 4. GET /messages/1002/receipts -> 200 OK with read_count=1.
	wGet := httptest.NewRecorder()
	reqGet, _ := http.NewRequest(http.MethodGet, "/api/v1/conversations/90001/messages/1002/receipts?status=read", nil)
	reqGet.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wGet, reqGet)
	if wGet.Code != http.StatusOK {
		t.Fatalf("GET /receipts expected 200, got %d body=%s", wGet.Code, wGet.Body.String())
	}
	var listEnv struct {
		Code int                           `json:"code"`
		Data service.MessageReceiptListDTO `json:"data"`
	}
	if err := json.Unmarshal(wGet.Body.Bytes(), &listEnv); err != nil {
		t.Fatalf("unmarshal receipts response: %v", err)
	}
	if listEnv.Code != 0 || listEnv.Data.TotalMembers != 1 || listEnv.Data.ReadCount != 1 || listEnv.Data.UnreadCount != 0 || len(listEnv.Data.Items) != 1 {
		t.Fatalf("unexpected GET /receipts response: %+v", listEnv)
	}
}
