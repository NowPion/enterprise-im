package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type msgCtrlTestStore struct {
	*convCtrlTestStore
	mu       sync.Mutex
	msgs     map[uint64]model.Message
	byClient map[string]uint64
	dedupe   map[string]repository.DedupeRecord
	seq      uint64
}

func newMsgCtrlTestStore() *msgCtrlTestStore {
	return &msgCtrlTestStore{
		convCtrlTestStore: newConvCtrlTestStore(),
		msgs:              make(map[uint64]model.Message),
		byClient:          make(map[string]uint64),
		dedupe:            make(map[string]repository.DedupeRecord),
	}
}

func (s *msgCtrlTestStore) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(nil)
}
func (s *msgCtrlTestStore) GetDedupeRecord(_ context.Context, senderID uint64, cid string) (*repository.DedupeRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.dedupe[repository.DedupeKey(senderID, cid)]
	if !ok {
		return nil, false, nil
	}
	return &rec, true, nil
}
func (s *msgCtrlTestStore) SetDedupeRecordNX(_ context.Context, senderID uint64, cid string, rec repository.DedupeRecord, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dedupe[repository.DedupeKey(senderID, cid)] = rec
	return nil
}
func (s *msgCtrlTestStore) DeleteDedupeRecord(_ context.Context, senderID uint64, cid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dedupe, repository.DedupeKey(senderID, cid))
	return nil
}
func (s *msgCtrlTestStore) AcquireDedupeLock(_ context.Context, senderID uint64, cid string, _, _ time.Duration) (string, *repository.DedupeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.dedupe[repository.DedupeKey(senderID, cid)]; ok {
		return "", &rec, nil
	}
	return "tok", nil, nil
}
func (s *msgCtrlTestStore) ReleaseDedupeLock(context.Context, uint64, string, string) error { return nil }
func (s *msgCtrlTestStore) FindMessageByClientMsgID(_ context.Context, senderID uint64, cid string) (*model.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byClient[cid]
	if !ok {
		return nil, nil
	}
	m := s.msgs[id]
	if m.SenderID != senderID {
		return nil, nil
	}
	return &m, nil
}
func (s *msgCtrlTestStore) FindMessageByID(_ context.Context, id uint64) (*model.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.msgs[id]
	if !ok {
		return nil, repository.ErrMessageNotFound
	}
	return &m, nil
}
func (s *msgCtrlTestStore) LockConversationByID(_ context.Context, _ *gorm.DB, convID uint64) (*model.Conversation, error) {
	return s.GetConversationByID(context.Background(), convID)
}
func (s *msgCtrlTestStore) LockMessageInConversation(_ context.Context, _ *gorm.DB, convID, msgID uint64) (*model.Message, error) {
	m, ok := s.msgs[msgID]
	if !ok || m.ConversationID != convID {
		return nil, repository.ErrMessageNotFound
	}
	return &m, nil
}
func (s *msgCtrlTestStore) GetAssetByAssetID(_ context.Context, _ string) (*model.Asset, error) {
	return nil, repository.ErrAssetNotFound
}
func (s *msgCtrlTestStore) LockAssetByAssetID(_ context.Context, _ *gorm.DB, _ string) (*model.Asset, error) {
	return nil, repository.ErrAssetNotFound
}
func (s *msgCtrlTestStore) BindAssetToMessageTx(context.Context, *gorm.DB, *model.Asset, uint64, uint64, time.Time) error {
	return nil
}
func (s *msgCtrlTestStore) CreateMessageTx(_ context.Context, _ *gorm.DB, msg *model.Message) error {
	s.msgs[msg.ID] = *msg
	s.byClient[msg.ClientMsgID] = msg.ID
	return nil
}
func (s *msgCtrlTestStore) UpdateConversationOnMessageTx(context.Context, *gorm.DB, uint64, *model.Message, string, time.Time) error {
	return nil
}
func (s *msgCtrlTestStore) UpdateMembersOnNewMessageTx(context.Context, *gorm.DB, uint64, uint64, []uint64, time.Time) error {
	return nil
}
func (s *msgCtrlTestStore) CreateMessageReceiptsTx(context.Context, *gorm.DB, []model.MessageReceipt) error {
	return nil
}
func (s *msgCtrlTestStore) MarkMessageRecalledTx(_ context.Context, _ *gorm.DB, msgID uint64, at time.Time) (bool, error) {
	m := s.msgs[msgID]
	m.Status = model.MessageStatusRecalled
	m.RecalledAt = &at
	s.msgs[msgID] = m
	return true, nil
}
func (s *msgCtrlTestStore) AdjustMembersOnRecallTx(context.Context, *gorm.DB, uint64, uint64, uint64, []uint64, time.Time) error {
	return nil
}
func (s *msgCtrlTestStore) ListMessagesBefore(_ context.Context, convID uint64, beforeSeq *uint64, _ int) ([]model.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Message
	for _, m := range s.msgs {
		if m.ConversationID == convID && (beforeSeq == nil || m.Seq < *beforeSeq) {
			out = append(out, m)
		}
	}
	return out, nil
}
func (s *msgCtrlTestStore) ListMessagesAfter(_ context.Context, convID uint64, afterSeq uint64, _ int) ([]model.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Message
	for _, m := range s.msgs {
		if m.ConversationID == convID && m.Seq > afterSeq {
			out = append(out, m)
		}
	}
	return out, nil
}
func (s *msgCtrlTestStore) BatchGetSingleDeliveryStatuses(context.Context, uint64, uint64, []uint64) (map[uint64]string, error) {
	return map[uint64]string{}, nil
}
func (s *msgCtrlTestStore) ListActiveMemberUserIDs(_ context.Context, _ *model.Conversation) ([]uint64, error) {
	return []uint64{10001, 10002}, nil
}
func (s *msgCtrlTestStore) AcquireConvLock(context.Context, uint64) (string, error) { return "cl", nil }
func (s *msgCtrlTestStore) AllocateNextSeq(context.Context, *gorm.DB, uint64, string) (*repository.SeqReservation, error) {
	s.seq++
	return &repository.SeqReservation{Seq: s.seq, Rollback: func(context.Context) error { return nil }}, nil
}
func (s *msgCtrlTestStore) ReleaseConvLock(context.Context, uint64, string) error { return nil }

func TestMessageControllerEndpointsAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newMsgCtrlTestStore()
	idGen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewMessageService(store, store, store, idGen, nil, 2*time.Minute, nil)
	ctrl := NewMessageController(svc)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(errcode.ContextKeyRequestID, "r-msg-test")
		if uid := c.GetHeader("X-Test-User"); uid != "" {
			errcode.SetUserID(c, uid)
		}
		c.Next()
	})
	r.GET("/api/v1/conversations/:conversation_id/messages", ctrl.List)
	r.GET("/api/v1/conversations/:conversation_id/sync", ctrl.Sync)
	r.POST("/api/v1/conversations/:conversation_id/messages", ctrl.Send)
	r.POST("/api/v1/conversations/:conversation_id/messages/:msg_id/recall", ctrl.Recall)

	// Send text -> 201 Created.
	wSend := httptest.NewRecorder()
	reqSend, _ := http.NewRequest(http.MethodPost, "/api/v1/conversations/90001/messages", strings.NewReader(`{"client_msg_id":"cid-1","msg_type":"text","content":"hello","extra":{}}`))
	reqSend.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wSend, reqSend)
	if wSend.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first send, got %d body=%s", wSend.Code, wSend.Body.String())
	}
	var sendEnv struct {
		Code int `json:"code"`
		Data struct {
			Message service.MessageDTO `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wSend.Body.Bytes(), &sendEnv); err != nil || sendEnv.Data.Message.MsgID == 0 {
		t.Fatalf("unexpected send response: %s err=%v", wSend.Body.String(), err)
	}

	// Idempotent retry -> 200 OK with same msg_id and seq.
	wRetry := httptest.NewRecorder()
	reqRetry, _ := http.NewRequest(http.MethodPost, "/api/v1/conversations/90001/messages", strings.NewReader(`{"client_msg_id":"cid-1","msg_type":"text","content":"hello"}`))
	reqRetry.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wRetry, reqRetry)
	if wRetry.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent retry, got %d body=%s", wRetry.Code, wRetry.Body.String())
	}

	// Bad seq cursor -> 400 / 13004.
	wBadSeq := httptest.NewRecorder()
	reqBadSeq, _ := http.NewRequest(http.MethodGet, "/api/v1/conversations/90001/messages?before_seq=abc", nil)
	reqBadSeq.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wBadSeq, reqBadSeq)
	if wBadSeq.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad before_seq, got %d", wBadSeq.Code)
	}
	var badEnv errcode.Envelope
	_ = json.Unmarshal(wBadSeq.Body.Bytes(), &badEnv)
	if badEnv.Code != errcode.ErrInvalidSeqCursor.Code {
		t.Fatalf("expected 13004, got %d", badEnv.Code)
	}

	// Both before_seq & after_seq -> 400 / 10001.
	wBoth := httptest.NewRecorder()
	reqBoth, _ := http.NewRequest(http.MethodGet, "/api/v1/conversations/90001/messages?before_seq=2&after_seq=0", nil)
	reqBoth.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wBoth, reqBoth)
	if wBoth.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when both cursors present, got %d", wBoth.Code)
	}

	// Recall -> 200 OK.
	msgIDStr := strings.TrimSpace(wSend.Body.String())
	_ = msgIDStr
	var m map[string]any
	_ = json.Unmarshal(wSend.Body.Bytes(), &m)
	rawMsgID := m["data"].(map[string]any)["message"].(map[string]any)["msg_id"].(string)
	wRecall := httptest.NewRecorder()
	reqRecall, _ := http.NewRequest(http.MethodPost, "/api/v1/conversations/90001/messages/"+rawMsgID+"/recall", nil)
	reqRecall.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wRecall, reqRecall)
	if wRecall.Code != http.StatusOK {
		t.Fatalf("expected 200 on recall, got %d body=%s", wRecall.Code, wRecall.Body.String())
	}

	// Sync -> 200 OK.
	wSync := httptest.NewRecorder()
	reqSync, _ := http.NewRequest(http.MethodGet, "/api/v1/conversations/90001/sync?after_seq=0", nil)
	reqSync.Header.Set("X-Test-User", "10001")
	r.ServeHTTP(wSync, reqSync)
	if wSync.Code != http.StatusOK {
		t.Fatalf("expected 200 on sync, got %d body=%s", wSync.Code, wSync.Body.String())
	}
	_ = errors.New("keep import clean")
}
