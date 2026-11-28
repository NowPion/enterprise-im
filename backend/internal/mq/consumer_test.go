package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/ws"
)

type spyHubDeliverer struct {
	mu         sync.Mutex
	deliveries []spyDelivery
	failErr    error
}

type spyDelivery struct {
	UserIDs   []uint64
	FrameType string
	Data      any
}

func (s *spyHubDeliverer) DeliverToUsers(_ context.Context, targetUserIDs []uint64, frameType string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failErr != nil {
		return s.failErr
	}
	s.deliveries = append(s.deliveries, spyDelivery{
		UserIDs:   append([]uint64(nil), targetUserIDs...),
		FrameType: frameType,
		Data:      data,
	})
	return nil
}

func (s *spyHubDeliverer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deliveries)
}

func (s *spyHubDeliverer) all() []spyDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]spyDelivery(nil), s.deliveries...)
}

type stubMessageStore struct {
	mu       sync.Mutex
	messages map[uint64]CommittedMessage
	convs    map[uint64]ConversationMeta
	members  map[uint64][]uint64
	receipts []model.MessageReceipt
}

func (s *stubMessageStore) GetCommittedMessage(_ context.Context, msgID uint64) (*CommittedMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[msgID]
	if !ok {
		return nil, ErrMessageNotFound
	}
	cp := m
	return &cp, nil
}

func (s *stubMessageStore) ListConversationActiveRecipients(_ context.Context, convID uint64) (*ConversationMeta, []uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, ok := s.convs[convID]
	if !ok {
		return nil, nil, repository.ErrConversationNotFound
	}
	return &meta, append([]uint64(nil), s.members[convID]...), nil
}

func (s *stubMessageStore) InitDelayedGroupReceipts(_ context.Context, convID, msgID, seq uint64, recipientIDs []uint64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, uid := range recipientIDs {
		s.receipts = append(s.receipts, model.MessageReceipt{
			MessageID:      msgID,
			ConversationID: convID,
			Seq:            seq,
			UserID:         uid,
			Status:         model.ReceiptStatusSent,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
	}
	return nil
}

func (s *stubMessageStore) EnsureSingleReceipt(_ context.Context, convID, msgID, seq, peerID uint64, now time.Time) error {
	return s.InitDelayedGroupReceipts(context.Background(), convID, msgID, seq, []uint64{peerID}, now)
}

type stubAuditStore struct {
	mu      sync.Mutex
	entries map[string]model.AuditLog
	inserts int
}

func newStubAuditStore() *stubAuditStore {
	return &stubAuditStore{entries: make(map[string]model.AuditLog)}
}

func (s *stubAuditStore) AppendAuditLog(_ context.Context, entry *model.AuditLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserts++
	if _, exists := s.entries[entry.EventID]; !exists {
		s.entries[entry.EventID] = *entry
	}
	return nil
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

func TestConsumerUnitBuildFramesAndAuditEntry(t *testing.T) {
	// Delivered receipt frame
	ft, data, err := buildEventWSFrame(KindDelivered, "", "evt-1", 90001, 10002, 0, 188901, 42, 42, 40, "2026-11-26T04:00:05.000Z", `{}`)
	if err != nil || ft != "msg.push" {
		t.Fatalf("unexpected delivered frame: %q %v", ft, err)
	}
	rp := data.(ReceiptPushPayload)
	if rp.PushType != "receipt" || rp.DeliveryStatus != "delivered" || rp.MsgID == nil || *rp.MsgID != "188901" || rp.Seq == nil || *rp.Seq != "42" {
		t.Fatalf("unexpected delivered ReceiptPushPayload: %+v", rp)
	}

	// Read receipt frame (msg_id & seq must be null)
	ft, data, err = buildEventWSFrame(KindRead, "", "evt-2", 90001, 10002, 0, 0, 0, 42, 42, "2026-11-26T04:00:05.000Z", `{}`)
	if err != nil || ft != "msg.push" {
		t.Fatalf("unexpected read frame: %q %v", ft, err)
	}
	rpRead := data.(ReceiptPushPayload)
	if rpRead.PushType != "receipt" || rpRead.DeliveryStatus != "read" || rpRead.MsgID != nil || rpRead.Seq != nil {
		t.Fatalf("unexpected read ReceiptPushPayload: %+v", rpRead)
	}

	// Recall system.notice frame
	ft, data, err = buildEventWSFrame(KindRecall, "message.recalled", "evt-3", 90001, 10001, 0, 188901, 43, 0, 0, "2026-11-26T04:01:00.000Z", `{"recalled_msg_id":"188901","recall_msg_id":"188902","recall_seq":"43"}`)
	if err != nil || ft != "system.notice" {
		t.Fatalf("unexpected recall notice: %q %v", ft, err)
	}
	sn := data.(SystemNoticePayload)
	if sn.Event != "message.recalled" || sn.ConversationID == nil || *sn.ConversationID != "90001" || sn.TargetUserID != nil {
		t.Fatalf("unexpected SystemNoticePayload: %+v", sn)
	}

	// Audit mapping: read without explicit AuditRecord is metrics-only; kick writes audit row.
	if _, persist, err := buildAuditLogEntry("evt-read", KindRead, map[string]any{"kind": KindRead}, time.Now().UTC()); err != nil || persist {
		t.Fatalf("expected read without audit payload to skip audit_logs insert: persist=%v err=%v", persist, err)
	}
	entry, persist, err := buildAuditLogEntry("evt-kick-1", KindKick, map[string]any{
		"kind":           KindKick,
		"event":          "account.disabled",
		"actor_id":       "1",
		"target_user_id": "10002",
		"audit":          `{"scope":"admin","action":"user.disable","actor_role":"admin","target_type":"user","target_id":"10002","reason":"policy"}`,
	}, time.Now().UTC())
	if err != nil || !persist || entry.Scope != model.AuditScopeAdmin || entry.Action != "user.disable" || entry.TargetID != 10002 {
		t.Fatalf("unexpected audit entry: %+v persist=%v err=%v", entry, persist, err)
	}

	// Nil redis start guard & no-op drain when unstarted.
	cm := NewConsumerManager(config.MQConfig{}, "node-test", nil, nil, nil, nil)
	if err := cm.Start(context.Background()); err == nil {
		t.Fatal("expected Start with nil redis to fail")
	}
	if err := cm.Drain(context.Background()); err != nil {
		t.Fatalf("expected unstarted Drain to return nil, got %v", err)
	}
}

func TestLiveConsumerDeliveryIdempotencyReclaimDeadLetterAndDrain(t *testing.T) {
	rdb, cfg := loadLiveRedisForMQTest(t)
	ctx := context.Background()

	// Helper to ensure fresh clean state for a test stream without touching other tests' entries.
	newTestMQ := func(t *testing.T, stream, group, nodeID string, mqCfg config.MQConfig, opts ...ConsumerOption) (*ConsumerManager, string) {
		t.Helper()
		allOpts := append([]ConsumerOption{WithGroups(StreamGroup{Stream: stream, Group: group})}, opts...)
		cm := NewConsumerManager(mqCfg, nodeID, rdb, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), allOpts...)
		return cm, stream
	}

	// 1. Live Stream -> im-msg-deliver -> HubDeliverer receives msg.push, plus idempotent redelivery.
	t.Run("DeliveryAndIdempotentRedelivery", func(t *testing.T) {
		stream := "im:stream:test:deliver:" + uuid.NewString()
		t.Cleanup(func() { _ = rdb.Del(context.Background(), stream).Err() })

		msgID := uint64(time.Now().UnixNano())
		store := &stubMessageStore{
			messages: map[uint64]CommittedMessage{
				msgID: {
					MsgID:          msgID,
					ClientMsgID:    "cid-" + uuid.NewString(),
					ConversationID: 90011,
					Seq:            12,
					SenderID:       50001,
					MsgType:        model.MessageTypeText,
					Content:        "delivered via stream",
					Extra:          json.RawMessage(`{}`),
					Status:         model.MessageStatusNormal,
					CreatedAt:      "2026-11-26T04:00:00.000Z",
				},
			},
			convs: map[uint64]ConversationMeta{
				90011: {ID: 90011, Type: model.ConversationTypeSingle},
			},
			members: map[uint64][]uint64{
				90011: {50001, 50002},
			},
		}

		hubSpy := &spyHubDeliverer{}
		mqCfg := cfg.MQ
		mqCfg.BlockTimeout = 200 * time.Millisecond
		mqCfg.ReclaimInterval = 10 * time.Second
		cm := NewConsumerManager(
			mqCfg,
			"mq-deliver-"+uuid.NewString(),
			rdb,
			nil,
			hubSpy,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			WithGroups(StreamGroup{Stream: stream, Group: GroupMsgDeliver}),
			WithMessageStore(store),
		)
		if err := cm.Start(ctx); err != nil {
			t.Fatalf("start mq consumer: %v", err)
		}
		t.Cleanup(func() {
			dCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = cm.Drain(dCtx)
			_ = rdb.Del(context.Background(), doneKey(GroupMsgDeliver, "msg:"+strconv.FormatUint(msgID, 10))).Err()
		})

		entryID, err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: stream,
			ID:     "*",
			Values: map[string]any{
				"kind":            KindMessage,
				"message_id":      strconv.FormatUint(msgID, 10),
				"conversation_id": "90011",
				"seq":             "12",
				"sender_id":       "50001",
				"payload_ref":     fmt.Sprintf("messages:%d", msgID),
			},
		}).Result()
		if err != nil {
			t.Fatalf("xadd: %v", err)
		}

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if hubSpy.count() >= 1 {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		if hubSpy.count() != 1 {
			t.Fatalf("expected 1 hub delivery, got %d", hubSpy.count())
		}
		d := hubSpy.all()[0]
		push := d.Data.(MessagePushPayload)
		if d.FrameType != "msg.push" || push.PushType != "message" ||
			push.Message.MsgID != msgID || push.Message.Content != "delivered via stream" ||
			push.DeliveryStatus != model.ReceiptStatusSent {
			t.Fatalf("unexpected msg.push delivery: %+v", d)
		}

		// Re-publish same message_id (idempotent redelivery): Hub MUST NOT receive a second msg.push!
		dupID, err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: stream,
			ID:     "*",
			Values: map[string]any{
				"kind":            KindMessage,
				"message_id":      strconv.FormatUint(msgID, 10),
				"conversation_id": "90011",
				"seq":             "12",
				"sender_id":       "50001",
				"payload_ref":     fmt.Sprintf("messages:%d", msgID),
			},
		}).Result()
		if err != nil {
			t.Fatalf("xadd duplicate: %v", err)
		}
		t.Cleanup(func() {
			_ = rdb.Del(context.Background(),
				doneKey(GroupMsgDeliver, "sid:"+entryID),
				doneKey(GroupMsgDeliver, "sid:"+dupID),
			).Err()
		})

		deadline = time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pInfo, _ := rdb.XPending(ctx, stream, GroupMsgDeliver).Result()
			if pInfo.Count == 0 {
				if done, _ := rdb.Exists(ctx, doneKey(GroupMsgDeliver, "sid:"+dupID)).Result(); done > 0 {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if hubSpy.count() != 1 {
			t.Fatalf("expected NO duplicate hub broadcast on idempotent redelivery, got %d", hubSpy.count())
		}
		if pInfo, err := rdb.XPending(ctx, stream, GroupMsgDeliver).Result(); err != nil || pInfo.Count != 0 {
			t.Fatalf("expected PEL=0 after idempotent ACK, got count=%d err=%v", pInfo.Count, err)
		}
	})

	// 2. XAUTOCLAIM reclaim + observability Hash + startup pending recovery before `>`.
	t.Run("XAutoClaimAndStartupPendingRecovery", func(t *testing.T) {
		stream := "im:stream:test:reclaim:" + uuid.NewString()
		group := GroupEventDeliver
		t.Cleanup(func() {
			_ = rdb.Del(context.Background(), stream, PendingReclaimKey(stream, group)).Err()
		})

		if err := rdb.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
			t.Fatal(err)
		}

		evtID := "evt-reclaim-" + uuid.NewString()
		entryID, err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: stream,
			ID:     "*",
			Values: map[string]any{
				"kind":               KindDelivered,
				"event_id":           evtID,
				"conversation_id":    "88001",
				"actor_id":           "10002",
				"target_user_ids":    "10001",
				"message_id":         "777001",
				"seq":                "5",
				"last_delivered_seq": "5",
				"last_read_seq":      "3",
				"occurred_at":        "2026-11-26T04:00:05.000Z",
			},
		}).Result()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = rdb.Del(context.Background(),
				doneKey(group, "evt:"+evtID),
				doneKey(group, "sid:"+entryID),
			).Err()
		})

		// Dead consumer claims the entry via XREADGROUP and crashes without XACK.
		if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: "dead-node-w1",
			Streams:  []string{stream, ">"},
			Count:    1,
		}).Result(); err != nil {
			t.Fatal(err)
		}

		time.Sleep(120 * time.Millisecond)

		hubSpy := &spyHubDeliverer{}
		mqCfg := cfg.MQ
		mqCfg.ReclaimMinIdle = 50 * time.Millisecond
		mqCfg.PendingReclaimTTL = 24 * time.Hour

		cm, _ := newTestMQ(t, stream, group, "node-reclaimer", mqCfg)
		cm.hub = hubSpy

		res, err := cm.ReclaimGroupOnce(ctx, stream, group)
		if err != nil {
			t.Fatalf("ReclaimGroupOnce failed: %v", err)
		}
		if res.ClaimedCount != 1 || res.AckedCount != 1 || res.FailedCount != 0 || res.DeadLetterCount != 0 {
			t.Fatalf("unexpected reclaim result: %+v", res)
		}
		if hubSpy.count() != 1 || hubSpy.all()[0].FrameType != "msg.push" {
			t.Fatalf("expected 1 receipt msg.push delivery on reclaim, got %+v", hubSpy.all())
		}

		// Verify observability Hash im:stream:pending:reclaim:messages|events:{group} and daily TTL.
		obsKey := PendingReclaimKey(stream, group)
		obs, err := rdb.HGetAll(ctx, obsKey).Result()
		if err != nil || obs["start_id"] != "0-0" || obs["claimed_count"] != "1" || obs["consumer"] != "reclaim-node-reclaimer" {
			t.Fatalf("unexpected reclaim observability hash %s: %+v err=%v", obsKey, obs, err)
		}
		if ttl, err := rdb.TTL(ctx, obsKey).Result(); err != nil || ttl <= 23*time.Hour {
			t.Fatalf("expected ~24h TTL on %s, got %v err=%v", obsKey, ttl, err)
		}

		// Startup recovery test: pending entry for THIS consumer is processed before `>`.
		streamRec := "im:stream:test:startup:" + uuid.NewString()
		t.Cleanup(func() { _ = rdb.Del(context.Background(), streamRec).Err() })
		if err := rdb.XGroupCreateMkStream(ctx, streamRec, group, "0").Err(); err != nil {
			t.Fatal(err)
		}
		evtPending := "evt-pend-" + uuid.NewString()
		evtFresh := "evt-fresh-" + uuid.NewString()
		t.Cleanup(func() {
			_ = rdb.Del(context.Background(), doneKey(group, "evt:"+evtPending), doneKey(group, "evt:"+evtFresh)).Err()
		})
		idPend, _ := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: streamRec, ID: "*",
			Values: map[string]any{"kind": KindRead, "event_id": evtPending, "conversation_id": "88002", "actor_id": "2", "target_user_ids": "1", "last_delivered_seq": "1", "last_read_seq": "1"},
		}).Result()
		// Read by exact consumer name of worker 1 (`node-rec-event-deliver-1`) and don't ACK.
		if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: "node-rec-" + workerShortName(group) + "-1",
			Streams:  []string{streamRec, ">"},
			Count:    1,
		}).Result(); err != nil {
			t.Fatal(err)
		}
		// Now add a fresh entry after the pending entry.
		idFresh, _ := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: streamRec, ID: "*",
			Values: map[string]any{"kind": KindRead, "event_id": evtFresh, "conversation_id": "88002", "actor_id": "2", "target_user_ids": "1", "last_delivered_seq": "2", "last_read_seq": "2"},
		}).Result()
		t.Cleanup(func() {
			_ = rdb.Del(context.Background(), doneKey(group, "sid:"+idPend), doneKey(group, "sid:"+idFresh)).Err()
		})

		recSpy := &spyHubDeliverer{}
		recCfg := cfg.MQ
		recCfg.BlockTimeout = 200 * time.Millisecond
		recCfg.ReclaimInterval = 10 * time.Second
		recCM, _ := newTestMQ(t, streamRec, group, "node-rec", recCfg)
		recCM.hub = recSpy
		if err := recCM.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() {
			dCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = recCM.Drain(dCtx)
		}()

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if recSpy.count() >= 2 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		deliveries := recSpy.all()
		if len(deliveries) != 2 {
			t.Fatalf("expected 2 deliveries (pending first then >), got %d", len(deliveries))
		}
		firstSeq := deliveries[0].Data.(ReceiptPushPayload).LastReadSeq
		secondSeq := deliveries[1].Data.(ReceiptPushPayload).LastReadSeq
		if firstSeq != "1" || secondSeq != "2" {
			t.Fatalf("expected pending seq=1 before fresh seq=2, got %s then %s", firstSeq, secondSeq)
		}
	})

	// 3. Dead-letter queue after 5 failures AND after >15 minutes pending age.
	t.Run("DeadLetterOn5FailuresAnd15MinAge", func(t *testing.T) {
		// 3a. 5 failures on messages test stream -> isolated dead-letter stream + XACK original.
		dlq5ID := uuid.NewString()
		stream5 := "im:stream:test:dlq5:" + dlq5ID
		group5 := GroupMsgDeliver
		deadStreamMsg := "im:stream:test:dead:dlq5:" + dlq5ID
		t.Cleanup(func() {
			_ = rdb.Del(context.Background(), stream5, deadStreamMsg, PendingReclaimKey(stream5, group5)).Err()
		})
		if err := rdb.XGroupCreateMkStream(ctx, stream5, group5, "0").Err(); err != nil {
			t.Fatal(err)
		}

		msgID := uint64(time.Now().UnixNano())
		entryID, err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: stream5,
			ID:     "*",
			Values: map[string]any{
				"kind":            KindMessage,
				"message_id":      strconv.FormatUint(msgID, 10),
				"conversation_id": "99555",
				"seq":             "9",
				"sender_id":       "10001",
				"payload_ref":     fmt.Sprintf("messages:%d", msgID),
			},
		}).Result()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = rdb.Del(context.Background(), retryMetaKey(stream5, group5, entryID)).Err()
		})

		// Initial read by worker-1 (failure #1) without XACK.
		msgs, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    group5,
			Consumer: "node-dlq-msg-deliver-1",
			Streams:  []string{stream5, ">"},
			Count:    1,
		}).Result()
		if err != nil || len(msgs) != 1 || len(msgs[0].Messages) != 1 {
			t.Fatalf("initial xreadgroup failed: %v", err)
		}

		var attempts atomic.Int32
		dlqCfg := cfg.MQ
		dlqCfg.MaxRetries = 5
		dlqCfg.MaxPendingAge = 15 * time.Minute
		dlqCfg.ReclaimMinIdle = 20 * time.Millisecond

		cm5, _ := newTestMQ(
			t, stream5, group5, "node-dlq", dlqCfg,
			WithConsumerStreams(stream5, "", deadStreamMsg, ""),
			WithCustomGroupHandler(group5, func(context.Context, string, string, redis.XMessage) error {
				attempts.Add(1)
				return errors.New("simulated hub delivery failure")
			}),
		)
		// Run attempt 1 directly as initial worker failure.
		acked, dead, _ := cm5.handleEntry(ctx, stream5, group5, "node-dlq-msg-deliver-1", msgs[0].Messages[0], 1)
		if acked || dead {
			t.Fatalf("expected failure 1 to remain in PEL, got acked=%v dead=%v", acked, dead)
		}

		// Failures 2, 3, 4 stay in PEL; failure 5 dead-letters into im:stream:dead:messages and XACKs.
		for round := 2; round <= 5; round++ {
			time.Sleep(30 * time.Millisecond)
			res, recErr := cm5.ReclaimGroupOnce(ctx, stream5, group5)
			if recErr != nil {
				t.Fatalf("reclaim round %d error: %v", round, recErr)
			}
			if round < 5 && (res.DeadLetterCount != 0 || res.FailedCount != 1) {
				t.Fatalf("round %d expected failed=1 dead=0, got %+v", round, res)
			}
			if round == 5 && (res.DeadLetterCount != 1 || res.ClaimedCount != 1) {
				t.Fatalf("round 5 expected dead=1, got %+v", res)
			}
		}
		if attempts.Load() != 5 {
			t.Fatalf("expected 5 total attempts, got %d", attempts.Load())
		}
		if pInfo, err := rdb.XPending(ctx, stream5, group5).Result(); err != nil || pInfo.Count != 0 {
			t.Fatalf("expected original entry XACKed after dead-lettering, got pending=%d err=%v", pInfo.Count, err)
		}

		// Verify dead letter entry in im:stream:dead:messages (and clean up ONLY our entry).
		deadEntries, err := rdb.XRevRangeN(ctx, deadStreamMsg, "+", "-", 20).Result()
		if err != nil {
			t.Fatalf("read %s: %v", deadStreamMsg, err)
		}
		var foundDead *redis.XMessage
		for i := range deadEntries {
			if deadEntries[i].Values["original_stream_id"] == entryID {
				foundDead = &deadEntries[i]
				t.Cleanup(func() { _ = rdb.XDel(context.Background(), deadStreamMsg, foundDead.ID).Err() })
				break
			}
		}
		if foundDead == nil {
			t.Fatalf("dead-letter entry for %s not found in %s", entryID, deadStreamMsg)
		}
		if foundDead.Values["last_consumer"] != "reclaim-node-dlq" ||
			foundDead.Values["failure_count"] != "5" ||
			foundDead.Values["message_id"] != strconv.FormatUint(msgID, 10) ||
			!strings.Contains(fmt.Sprint(foundDead.Values["error_summary"]), "simulated hub delivery failure") {
			t.Fatalf("unexpected dead-letter fields: %+v", foundDead.Values)
		}

		// 3b. >15 minutes age -> dead-lettered on first failure into isolated dead-letter events stream.
		groupAge := GroupEventAudit
		ageTestID := uuid.NewString()
		// Note: XADD requires ID > last-generated-id on existing stream, so test age with WithConsumerClock(+16m)!
		streamAge := "im:stream:test:age:" + ageTestID
		deadStreamEvt := "im:stream:test:dead:events:" + ageTestID
		t.Cleanup(func() {
			_ = rdb.Del(context.Background(), streamAge, deadStreamEvt).Err()
		})
		if err := rdb.XGroupCreateMkStream(ctx, streamAge, groupAge, "0").Err(); err != nil && !isBusyGroupErr(err) {
			t.Fatal(err)
		}
		evtAgeID := "evt-age-" + uuid.NewString()
		ageEntryID, err := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: streamAge,
			ID:     "*",
			Values: map[string]any{
				"kind":     KindKick,
				"event_id": evtAgeID,
				"event":    "account.kicked",
				"actor_id": "1",
			},
		}).Result()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = rdb.Del(context.Background(), retryMetaKey(streamAge, groupAge, ageEntryID), PendingReclaimKey(streamAge, groupAge)).Err()
		})

		// Consume the event once with a temp consumer, leave in PEL, then advance clock +16m and reclaim.
		if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    groupAge,
			Consumer: "c-age-init",
			Streams:  []string{streamAge, ">"},
			Count:    100,
		}).Result(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)

		futureNow := time.Now().UTC().Add(16 * time.Minute)
		ageCfg := cfg.MQ
		ageCfg.MaxRetries = 5
		ageCfg.MaxPendingAge = 15 * time.Minute
		ageCfg.ReclaimMinIdle = 20 * time.Millisecond
		cmAge, _ := newTestMQ(
			t, streamAge, groupAge, "node-age", ageCfg,
			WithConsumerStreams("", streamAge, "", deadStreamEvt),
			WithConsumerClock(func() time.Time { return futureNow }),
			WithCustomGroupHandler(groupAge, func(_ context.Context, _ string, _ string, msg redis.XMessage) error {
				if msg.ID == ageEntryID {
					return errors.New("db unavailable after 16m")
				}
				return nil
			}),
		)
		if _, err := cmAge.ReclaimGroupOnce(ctx, streamAge, groupAge); err != nil {
			t.Fatalf("ReclaimGroupOnce age failed: %v", err)
		}

		deadEvts, err := rdb.XRevRangeN(ctx, deadStreamEvt, "+", "-", 20).Result()
		if err != nil {
			t.Fatalf("read %s: %v", deadStreamEvt, err)
		}
		var foundAgeDead *redis.XMessage
		for i := range deadEvts {
			if deadEvts[i].Values["original_stream_id"] == ageEntryID {
				foundAgeDead = &deadEvts[i]
				break
			}
		}
		if foundAgeDead == nil {
			t.Fatalf("expected >15m entry %s in %s", ageEntryID, deadStreamEvt)
		}
		if foundAgeDead.Values["failure_count"] != "2" || foundAgeDead.Values["event_id"] != evtAgeID {
			t.Fatalf("unexpected >15m dead-letter entry: %+v", foundAgeDead.Values)
		}
	})

	// 4. Graceful shutdown (Drain): finishes in-flight, stops claiming new entries, and leaves unacked PEL on timeout.
	t.Run("GracefulDrainAndTimeoutLeavesPEL", func(t *testing.T) {
		stream := "im:stream:test:drain:" + uuid.NewString()
		group := GroupMsgPersist
		t.Cleanup(func() { _ = rdb.Del(context.Background(), stream).Err() })

		started := make(chan string, 2)
		release := make(chan struct{})
		var processed atomic.Int32

		mqCfg := cfg.MQ
		mqCfg.ReadCount = 1 // Claim 1 entry per XREADGROUP so entry #2 waits in stream!
		mqCfg.BlockTimeout = 100 * time.Millisecond
		mqCfg.ReclaimInterval = 10 * time.Second

		cm, _ := newTestMQ(t, stream, group, "node-drain-ok", mqCfg, WithCustomGroupHandler(group, func(_ context.Context, _ string, _ string, msg redis.XMessage) error {
			processed.Add(1)
			started <- msg.ID
			<-release
			return nil
		}))
		if err := cm.Start(ctx); err != nil {
			t.Fatal(err)
		}

		id1, _ := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, ID: "*", Values: map[string]any{"kind": KindMessage, "message_id": "1", "conversation_id": "1", "seq": "1"}}).Result()
		<-started // Wait until entry 1 is in-flight before adding entry 2 and draining.
		id2, _ := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, ID: "*", Values: map[string]any{"kind": KindMessage, "message_id": "2", "conversation_id": "1", "seq": "2"}}).Result()
		_ = id2

		drainDone := make(chan error, 1)
		drainCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		go func() {
			drainDone <- cm.Drain(drainCtx)
		}()

		time.Sleep(50 * time.Millisecond)
		close(release) // Let in-flight entry 1 finish and XACK!

		if err := <-drainDone; err != nil {
			t.Fatalf("expected Drain to succeed, got %v", err)
		}
		if processed.Load() != 1 {
			t.Fatalf("expected only in-flight entry 1 processed (new entry 2 NOT claimed), got %d", processed.Load())
		}
		pInfo, err := rdb.XPending(ctx, stream, group).Result()
		if err != nil || pInfo.Count != 0 {
			t.Fatalf("expected in-flight entry %s ACKed (PEL=0), got count=%d err=%v", id1, pInfo.Count, err)
		}

		// Timeout drain case: slow handler exceeds Drain timeout -> unacked entry remains in PEL for next consumer!
		streamTO := "im:stream:test:drainto:" + uuid.NewString()
		t.Cleanup(func() { _ = rdb.Del(context.Background(), streamTO).Err() })

		toStarted := make(chan struct{}, 1)
		cmTO, _ := newTestMQ(t, streamTO, group, "node-drain-to", mqCfg, WithCustomGroupHandler(group, func(hCtx context.Context, _ string, _ string, _ redis.XMessage) error {
			select {
			case toStarted <- struct{}{}:
			default:
			}
			<-hCtx.Done()
			return hCtx.Err()
		}))
		if err := cmTO.Start(ctx); err != nil {
			t.Fatal(err)
		}
		_, _ = rdb.XAdd(ctx, &redis.XAddArgs{Stream: streamTO, ID: "*", Values: map[string]any{"kind": KindMessage, "message_id": "3", "conversation_id": "1", "seq": "3"}}).Result()
		<-toStarted

		shortCtx, shortCancel := context.WithTimeout(ctx, 40*time.Millisecond)
		defer shortCancel()
		if err := cmTO.Drain(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded on slow drain, got %v", err)
		}
		if pInfo, err := rdb.XPending(ctx, streamTO, group).Result(); err != nil || pInfo.Count != 1 {
			t.Fatalf("expected timed-out in-flight entry left in PEL (count=1), got %d err=%v", pInfo.Count, err)
		}
	})
}

func TestLiveMySQLAndRedisEndToEndAllFourConsumerGroups(t *testing.T) {
	rdb, cfg := loadLiveRedisForMQTest(t)
	if cfg.MySQL.Password == "" && os.Getenv("IM_RUN_LIVE_TESTS") == "" && os.Getenv("IM_LIVE_TESTS") == "" {
		t.Skip("live MySQL+Redis MQ test requires MYSQL_PASSWORD and REDIS_PASSWORD")
	}
	db, err := repository.NewMySQL(cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect mysql: %v", err)
	}
	defer func() { _ = repository.CloseMySQL(db) }()

	ctx := context.Background()
	idGen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}

	// Create 3 real users + 1 group conversation + 1 message in MySQL (no direct Hub push!).
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	users := []model.User{
		{Username: "mq_u1_" + suffix, PasswordHash: "hash", Nickname: "U1"},
		{Username: "mq_u2_" + suffix, PasswordHash: "hash", Nickname: "U2"},
		{Username: "mq_u3_" + suffix, PasswordHash: "hash", Nickname: "U3"},
	}
	for i := range users {
		if err := db.WithContext(ctx).Create(&users[i]).Error; err != nil {
			t.Fatalf("create user %d: %v", i, err)
		}
	}
	u1, u2, u3 := users[0].ID, users[1].ID, users[2].ID

	grp := model.Group{Name: "MQ Live Group " + suffix, OwnerID: u1, MaxMembers: 500, Status: model.GroupStatusActive}
	if err := db.WithContext(ctx).Create(&grp).Error; err != nil {
		t.Fatal(err)
	}
	gid := grp.ID
	conv := model.Conversation{Type: model.ConversationTypeGroup, GroupID: &gid}
	if err := db.WithContext(ctx).Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	for i, uid := range []uint64{u1, u2, u3} {
		role := model.GroupRoleMember
		if i == 0 {
			role = model.GroupRoleOwner
		}
		_ = db.WithContext(ctx).Create(&model.GroupMember{GroupID: gid, UserID: uid, Role: role}).Error
		_ = db.WithContext(ctx).Create(&model.ConversationMember{ConversationID: conv.ID, UserID: uid, IsVisible: true}).Error
	}

	msgID, _ := idGen.NextID()
	now := time.Now().UTC().Truncate(time.Millisecond)
	msg := model.Message{
		ID:             msgID,
		ConversationID: conv.ID,
		Seq:            1,
		SenderID:       u1,
		ClientMsgID:    "mq-live-" + uuid.NewString(),
		MsgType:        model.MessageTypeText,
		Content:        "live mq persist+deliver",
		Extra:          model.NullableJSON("{}"),
		Status:         model.MessageStatusNormal,
		CreatedAt:      now,
	}
	if err := db.WithContext(ctx).Create(&msg).Error; err != nil {
		t.Fatal(err)
	}

	// Wire a REAL *ws.Hub + real PresenceRepository(db, rdb) + real WebSocket client for u2,
	// alongside all 4 Consumer Groups running on real MySQL & real Redis!
	jwtMgr, err := pkgjwt.NewManager(config.JWTConfig{
		Secret:     "mq-e2e-hub-test-secret-32-bytes!",
		Issuer:     "test",
		AccessTTL:  time.Hour,
		RefreshTTL: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	u2Session := model.UserSession{
		UserID:           u2,
		DeviceID:         "web-mq-" + suffix,
		DeviceType:       model.DeviceTypeWeb,
		RefreshTokenHash: fmt.Sprintf("%064x", u2),
		AccessJTI:        "jti-" + uuid.NewString(),
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
		LastActiveAt:     time.Now().UTC(),
	}
	if err := db.WithContext(ctx).Create(&u2Session).Error; err != nil {
		t.Fatalf("create u2 session: %v", err)
	}
	tok, jti, _, err := jwtMgr.IssueAccessToken(u2, u2Session.ID, model.UserRoleUser)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.WithContext(ctx).Model(&model.UserSession{}).Where("id = ?", u2Session.ID).Update("access_jti", jti).Error

	wsCfg := *cfg
	wsCfg.App.NodeID = "live-node-" + suffix
	wsCfg.WS.HeartbeatTimeout = 30 * time.Second
	wsCfg.WS.WriteTimeout = 5 * time.Second
	presence := repository.NewPresenceRepository(db, rdb, 30*time.Second)
	authState := middleware.NewAuthStateStore(repository.NewAuthRepository(db), repository.NewSessionRepository(db, rdb))
	hub := ws.NewHub(&wsCfg, middleware.NewAccessAuthenticator(jwtMgr, authState), presence, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := hub.Start(ctx); err != nil {
		t.Fatalf("start hub: %v", err)
	}
	t.Cleanup(func() {
		dCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hub.Shutdown(dCtx)
	})

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, failure := hub.Authenticate(r.Context(), r.URL.Query().Get("access_token"))
		if failure != nil {
			http.Error(w, failure.Error(), http.StatusUnauthorized)
			return
		}
		conn, upErr := upgrader.Upgrade(w, r, nil)
		if upErr != nil {
			return
		}
		hub.Serve(r.Context(), conn, id, "req-mq-e2e", nil)
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "?access_token=" + url.QueryEscape(tok)
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws for u2: %v", err)
	}
	t.Cleanup(func() { _ = wsConn.Close() })

	// Wait until u2's connection lease is registered in Redis.
	leaseDeadline := time.Now().Add(3 * time.Second)
	u2Str := strconv.FormatUint(u2, 10)
	for time.Now().Before(leaseDeadline) {
		if val, _ := rdb.Get(ctx, "im:presence:user:"+u2Str).Result(); val == "1" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	evtID := "evt-live-audit-" + uuid.NewString()
	t.Cleanup(func() {
		c := context.Background()
		_ = db.WithContext(c).Where("event_id = ?", evtID).Delete(&model.AuditLog{}).Error
		_ = db.WithContext(c).Where("conversation_id = ?", conv.ID).Delete(&model.MessageReceipt{}).Error
		_ = db.WithContext(c).Where("conversation_id = ?", conv.ID).Delete(&model.Message{}).Error
		_ = db.WithContext(c).Where("conversation_id = ?", conv.ID).Delete(&model.ConversationMember{}).Error
		_ = db.WithContext(c).Where("id = ?", conv.ID).Delete(&model.Conversation{}).Error
		_ = db.WithContext(c).Where("group_id = ?", gid).Delete(&model.GroupMember{}).Error
		_ = db.WithContext(c).Where("id = ?", gid).Delete(&model.Group{}).Error
		_ = db.WithContext(c).Where("user_id IN ?", []uint64{u1, u2, u3}).Delete(&model.UserSession{}).Error
		_ = db.WithContext(c).Where("id IN ?", []uint64{u1, u2, u3}).Delete(&model.User{}).Error
		_ = rdb.Del(c,
			"im:presence:user:"+u2Str,
			"im:route:user:"+u2Str,
			"im:route:session:"+strconv.FormatUint(u2Session.ID, 10),
			repository.DedupeKey(u1, msg.ClientMsgID),
			"im:mq:offline:conv:"+strconv.FormatUint(conv.ID, 10),
			"im:mq:offline:user:"+strconv.FormatUint(u2, 10),
			"im:mq:offline:user:"+strconv.FormatUint(u3, 10),
			doneKey(GroupMsgPersist, "msg:"+strconv.FormatUint(msgID, 10)),
			doneKey(GroupMsgDeliver, "msg:"+strconv.FormatUint(msgID, 10)),
			doneKey(GroupEventDeliver, "evt:"+evtID),
			doneKey(GroupEventAudit, "evt:"+evtID),
		).Err()
	})

	mqCfg := cfg.MQ
	mqCfg.BlockTimeout = 200 * time.Millisecond
	mqCfg.ReclaimInterval = 10 * time.Second

	testMsgsStream := "im:stream:test:mq:messages:" + suffix
	testEvtsStream := "im:stream:test:mq:events:" + suffix
	testDeadMsgsStream := "im:stream:test:mq:dead:messages:" + suffix
	testDeadEvtsStream := "im:stream:test:mq:dead:events:" + suffix
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), testMsgsStream, testEvtsStream, testDeadMsgsStream, testDeadEvtsStream).Err()
	})

	cm := NewConsumerManager(
		mqCfg, wsCfg.App.NodeID, rdb, db, hub, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithConsumerStreams(testMsgsStream, testEvtsStream, testDeadMsgsStream, testDeadEvtsStream),
	)
	if err := cm.Start(ctx); err != nil {
		t.Fatalf("start all 4 consumer groups: %v", err)
	}
	t.Cleanup(func() {
		dCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = cm.Drain(dCtx)
	})

	producer := NewProducer(rdb, mqCfg, nil, WithProducerStreams(testMsgsStream, testEvtsStream))
	msgStreamID, err := producer.PublishMessage(ctx, MessageRecord{
		MessageID:      msgID,
		ConversationID: conv.ID,
		Seq:            1,
		SenderID:       u1,
	})
	if err != nil {
		t.Fatalf("PublishMessage: %v", err)
	}
	evtStreamID, err := producer.PublishEvent(ctx, EventRecord{
		Kind:           KindGroup,
		EventID:        evtID,
		Event:          "group.updated",
		ConversationID: conv.ID,
		ActorID:        u1,
		TargetUserIDs:  []uint64{u1, u2, u3},
		OccurredAt:     now,
		Payload:        json.RawMessage(`{"name":"MQ Live Group"}`),
		Audit: &AuditRecord{
			Scope:          model.AuditScopeGroup,
			Action:         "group.update",
			ActorRole:      model.GroupRoleOwner,
			TargetType:     model.AuditTargetGroup,
			TargetID:       gid,
			GroupID:        &gid,
			ConversationID: &conv.ID,
			RequestID:      "req-live-audit",
		},
	})
	if err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_ = rdb.XDel(c, testMsgsStream, msgStreamID).Err()
		_ = rdb.XDel(c, testEvtsStream, evtStreamID).Err()
		_ = rdb.Del(c,
			doneKey(GroupMsgPersist, "sid:"+msgStreamID),
			doneKey(GroupMsgDeliver, "sid:"+msgStreamID),
			doneKey(GroupEventDeliver, "sid:"+evtStreamID),
			doneKey(GroupEventAudit, "sid:"+evtStreamID),
		).Err()
	})

	// Receive BOTH real WS frames on u2's WebSocket connection: msg.push(message) + system.notice(group.updated).
	var gotMsgPush, gotSysNotice bool
	_ = wsConn.SetReadDeadline(time.Now().Add(6 * time.Second))
	for !(gotMsgPush && gotSysNotice) {
		var frame struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := wsConn.ReadJSON(&frame); err != nil {
			t.Fatalf("read WS frame on u2 (gotMsgPush=%v gotSysNotice=%v): %v", gotMsgPush, gotSysNotice, err)
		}
		switch frame.Type {
		case "msg.push":
			var p MessagePushPayload
			if json.Unmarshal(frame.Data, &p) == nil && p.PushType == "message" && p.Message.MsgID == msgID {
				gotMsgPush = true
			}
		case "system.notice":
			var n SystemNoticePayload
			if json.Unmarshal(frame.Data, &n) == nil && n.Event == "group.updated" {
				gotSysNotice = true
			}
		}
	}

	// Wait until persist & audit DB writes settle too.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var recCount, auditCount int64
		_ = db.WithContext(ctx).Model(&model.MessageReceipt{}).Where("message_id = ?", msgID).Count(&recCount).Error
		_ = db.WithContext(ctx).Model(&model.AuditLog{}).Where("event_id = ?", evtID).Count(&auditCount).Error
		if recCount == 2 && auditCount == 1 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	var msgCount, recCount, auditCount int64
	_ = db.WithContext(ctx).Model(&model.Message{}).Where("id = ?", msgID).Count(&msgCount).Error
	_ = db.WithContext(ctx).Model(&model.MessageReceipt{}).Where("message_id = ?", msgID).Count(&recCount).Error
	_ = db.WithContext(ctx).Model(&model.AuditLog{}).Where("event_id = ?", evtID).Count(&auditCount).Error
	if msgCount != 1 {
		t.Fatalf("expected im-msg-persist NOT to duplicate message row, got %d", msgCount)
	}
	if recCount != 2 {
		t.Fatalf("expected 2 delayed group receipts (u2, u3), got %d", recCount)
	}
	if auditCount != 1 {
		t.Fatalf("expected 1 immutable audit_logs row for %s, got %d", evtID, auditCount)
	}
	var cmU2 model.ConversationMember
	if err := db.WithContext(ctx).Where("conversation_id = ? AND user_id = ?", conv.ID, u2).Take(&cmU2).Error; err != nil || cmU2.UnreadCount != 0 {
		t.Fatalf("expected im-msg-persist NOT to increment unread_count, got %d err=%v", cmU2.UnreadCount, err)
	}
	// u3 is offline, so offline compensation index must record seq=1 for u3.
	if idxSeq, err := rdb.HGet(ctx, "im:mq:offline:user:"+strconv.FormatUint(u3, 10), strconv.FormatUint(conv.ID, 10)).Result(); err != nil || idxSeq != "1" {
		t.Fatalf("expected offline compensation index seq=1 for offline user u3, got %q err=%v", idxSeq, err)
	}

	// 5. Verify recall notice delivery + idempotent audit redelivery (no duplicate audit_logs or metric double-count).
	recallEvtID := "evt-live-recall-" + uuid.NewString()
	t.Cleanup(func() {
		c := context.Background()
		_ = db.WithContext(c).Where("event_id = ?", recallEvtID).Delete(&model.AuditLog{}).Error
		_ = rdb.Del(c,
			doneKey(GroupEventDeliver, "evt:"+recallEvtID),
			doneKey(GroupEventAudit, "evt:"+recallEvtID),
		).Err()
	})
	recStreamID1, err := producer.PublishEvent(ctx, EventRecord{
		Kind:           KindRecall,
		EventID:        recallEvtID,
		Event:          "message.recalled",
		ConversationID: conv.ID,
		ActorID:        u1,
		TargetUserIDs:  []uint64{u2},
		MessageID:      msgID,
		Seq:            2,
		OccurredAt:     now,
		Payload: json.RawMessage(fmt.Sprintf(
			`{"recalled_msg_id":"%d","recall_msg_id":"%d","recall_seq":"2","recalled_at":"%s"}`,
			msgID, msgID+1, now.Format(utcMillisLayout),
		)),
	})
	if err != nil {
		t.Fatalf("PublishEvent recall: %v", err)
	}
	recStreamID2, err := producer.PublishEvent(ctx, EventRecord{
		Kind:           KindRecall,
		EventID:        recallEvtID, // same event_id -> idempotent redelivery!
		Event:          "message.recalled",
		ConversationID: conv.ID,
		ActorID:        u1,
		TargetUserIDs:  []uint64{u2},
		MessageID:      msgID,
		Seq:            2,
		OccurredAt:     now,
	})
	if err != nil {
		t.Fatalf("PublishEvent duplicate recall: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_ = rdb.XDel(c, testEvtsStream, recStreamID1, recStreamID2).Err()
		_ = rdb.Del(c,
			doneKey(GroupEventDeliver, "sid:"+recStreamID1),
			doneKey(GroupEventDeliver, "sid:"+recStreamID2),
			doneKey(GroupEventAudit, "sid:"+recStreamID1),
			doneKey(GroupEventAudit, "sid:"+recStreamID2),
		).Err()
	})

	_ = wsConn.SetReadDeadline(time.Now().Add(4 * time.Second))
	var recallNoticeCount int
	for {
		var frame struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := wsConn.ReadJSON(&frame); err != nil {
			t.Fatalf("read recall notice on u2: %v", err)
		}
		if frame.Type == "system.notice" {
			var n SystemNoticePayload
			if json.Unmarshal(frame.Data, &n) == nil && n.Event == "message.recalled" {
				recallNoticeCount++
				break
			}
		}
	}
	// Briefly wait to confirm duplicate recall event_id does NOT cause a second notice!
	_ = wsConn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	var extraRecall json.RawMessage
	if readErr := wsConn.ReadJSON(&extraRecall); readErr == nil {
		t.Fatalf("expected no duplicate recall notice on idempotent event_id, got %s", string(extraRecall))
	}
	if recallNoticeCount != 1 {
		t.Fatalf("expected exactly 1 recall notice, got %d", recallNoticeCount)
	}
	var recallAuditCount int64
	auditDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(auditDeadline) {
		_ = db.WithContext(ctx).Model(&model.AuditLog{}).Where("event_id = ?", recallEvtID).Count(&recallAuditCount).Error
		if recallAuditCount == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if recallAuditCount != 1 {
		t.Fatalf("expected 1 recall audit_logs row for idempotent event_id, got %d", recallAuditCount)
	}
}
