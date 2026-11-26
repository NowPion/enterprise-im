package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/pkg/errcode"
)

func loadLiveRedisForMQTest(t *testing.T) (*redis.Client, *config.Config) {
	t.Helper()
	cfg, err := config.Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Redis.Password == "" && os.Getenv("IM_RUN_LIVE_TESTS") == "" && os.Getenv("IM_LIVE_TESTS") == "" {
		t.Skip("live Redis test requires REDIS_PASSWORD (or IM_RUN_LIVE_TESTS)")
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.ReadTimeout,
		WriteTimeout: cfg.Redis.WriteTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		t.Fatalf("ping live redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, cfg
}

func TestStreamIDHelpersAndSanitization(t *testing.T) {
	if compareStreamID("999-0", "1000-0") >= 0 {
		t.Fatal("expected numeric comparison 999-0 < 1000-0")
	}
	if compareStreamID("1000-9", "1000-10") >= 0 {
		t.Fatal("expected numeric comparison 1000-9 < 1000-10")
	}
	if compareStreamID("1000-5", "1000-5") != 0 {
		t.Fatal("expected equal stream IDs")
	}
	if minStreamID("1000-2", "999-99") != "999-99" {
		t.Fatal("unexpected minStreamID")
	}
	if nextStreamID("1000-5") != "1000-6" {
		t.Fatalf("unexpected nextStreamID: %s", nextStreamID("1000-5"))
	}
	ts, ok := streamIDTime("1764120000123-0")
	if !ok || ts.UnixMilli() != 1764120000123 {
		t.Fatalf("unexpected streamIDTime: %v %v", ts, ok)
	}

	ids := parseTargetUserIDs(map[string]any{
		"target_user_ids": "10001, 10002, 10001, 0",
		"target_user_id":  "10003",
	})
	if len(ids) != 3 || ids[0] != 10001 || ids[1] != 10002 || ids[2] != 10003 {
		t.Fatalf("unexpected parsed CSV target IDs: %v", ids)
	}
	jsonIDs := parseTargetUserIDs(map[string]any{
		"target_user_ids": `["20001","20002"]`,
	})
	if len(jsonIDs) != 2 || jsonIDs[0] != 20001 || jsonIDs[1] != 20002 {
		t.Fatalf("unexpected parsed JSON target IDs: %v", jsonIDs)
	}

	sanitized := sanitizeErrorSummary(errors.New("dial tcp redis://:supersecret@127.0.0.1:6379/0 failed password=abc"))
	if strings.Contains(sanitized, "supersecret") || strings.Contains(sanitized, "abc") {
		t.Fatalf("sanitized error leaked secret: %s", sanitized)
	}

	if PendingReclaimKey(StreamMessages, GroupMsgPersist) != "im:stream:pending:reclaim:messages:im-msg-persist" {
		t.Fatal("unexpected PendingReclaimKey for messages")
	}
	if PendingReclaimKey(StreamEvents, GroupEventAudit) != "im:stream:pending:reclaim:events:im-event-audit" {
		t.Fatal("unexpected PendingReclaimKey for events")
	}
	if DeadLetterStream(StreamMessages) != "im:stream:dead:messages" || DeadLetterStream(StreamEvents) != "im:stream:dead:events" {
		t.Fatal("unexpected DeadLetterStream")
	}
}

func TestProducerValidationAndFailClosedErrors(t *testing.T) {
	// Validation errors before hitting Redis.
	pNil := NewProducer(nil, config.MQConfig{}, nil)
	if _, err := pNil.PublishMessage(context.Background(), MessageRecord{}); !errors.Is(err, ErrInvalidStreamPayload) {
		t.Fatalf("expected ErrInvalidStreamPayload for empty MessageRecord, got %v", err)
	}
	if _, err := pNil.PublishMessage(context.Background(), MessageRecord{MessageID: 1, ConversationID: 2, Seq: 3}); !errors.Is(err, ErrStreamPublishFailed) {
		t.Fatalf("expected ErrStreamPublishFailed with nil redis, got %v", err)
	}
	if !IsStreamUnavailable(ErrStreamPublishFailed) {
		t.Fatal("expected IsStreamUnavailable true")
	}
	if biz := AsStreamError(fmt.Errorf("wrap: %w", ErrStreamPublishFailed)); biz == nil || biz.Code != errcode.ErrStreamUnavailable.Code {
		t.Fatalf("expected 14008 ErrStreamUnavailable, got %v", biz)
	}
	if _, err := pNil.PublishEvent(context.Background(), EventRecord{Kind: "bogus"}); !errors.Is(err, ErrInvalidStreamPayload) {
		t.Fatalf("expected ErrInvalidStreamPayload for invalid kind, got %v", err)
	}
	if _, err := pNil.PublishEvent(context.Background(), EventRecord{Kind: KindDelivered, Payload: json.RawMessage(`{bad`)}); !errors.Is(err, ErrInvalidStreamPayload) {
		t.Fatalf("expected ErrInvalidStreamPayload for invalid JSON payload, got %v", err)
	}

	// Unreachable Redis client must NOT pretend success and must not leak password.
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		Password:    "unit-test-secret-password",
		DialTimeout: 50 * time.Millisecond,
	})
	defer func() { _ = rdb.Close() }()

	p := NewProducer(rdb, config.MQConfig{}, nil)
	_, err := p.PublishMessage(context.Background(), MessageRecord{
		MessageID:      1001,
		ConversationID: 2001,
		Seq:            1,
		SenderID:       3001,
	})
	if err == nil || !errors.Is(err, ErrStreamPublishFailed) {
		t.Fatalf("expected ErrStreamPublishFailed on unreachable redis, got %v", err)
	}
	if strings.Contains(err.Error(), "unit-test-secret-password") {
		t.Fatalf("error leaked redis password: %v", err)
	}
	if biz := AsStreamError(err); biz.Code != 14008 {
		t.Fatalf("expected error code 14008, got %d", biz.Code)
	}
}

func TestLiveProducerXAddAndSafeRetentionTrim(t *testing.T) {
	rdb, cfg := loadLiveRedisForMQTest(t)
	ctx := context.Background()

	// 1. Test canonical XADD fields on frozen StreamMessages and StreamEvents.
	p := NewProducer(rdb, cfg.MQ, nil)
	msgID := uint64(time.Now().UnixNano())
	entryID, err := p.PublishMessage(ctx, MessageRecord{
		MessageID:      msgID,
		ConversationID: 99001,
		Seq:            7,
		SenderID:       10001,
		PayloadRef:     fmt.Sprintf("messages:%d", msgID),
	})
	if err != nil {
		t.Fatalf("PublishMessage failed: %v", err)
	}
	t.Cleanup(func() { _ = rdb.XDel(context.Background(), StreamMessages, entryID).Err() })

	entries, err := rdb.XRange(ctx, StreamMessages, entryID, entryID).Result()
	if err != nil || len(entries) != 1 {
		t.Fatalf("read back message stream entry: %v len=%d", err, len(entries))
	}
	vals := entries[0].Values
	if vals["kind"] != KindMessage ||
		vals["message_id"] != fmt.Sprintf("%d", msgID) ||
		vals["conversation_id"] != "99001" ||
		vals["seq"] != "7" ||
		vals["sender_id"] != "10001" ||
		vals["payload_ref"] != fmt.Sprintf("messages:%d", msgID) {
		t.Fatalf("unexpected im:stream:messages fields: %+v", vals)
	}

	evtID := "evt-" + uuid.NewString()
	evtEntryID, err := p.PublishEvent(ctx, EventRecord{
		Kind:             KindRead,
		EventID:          evtID,
		ConversationID:   99001,
		ActorID:          10002,
		TargetUserIDs:    []uint64{10001},
		LastDeliveredSeq: 7,
		LastReadSeq:      7,
		OccurredAt:       time.Now().UTC(),
		Payload:          json.RawMessage(`{"source":"unit"}`),
	})
	if err != nil {
		t.Fatalf("PublishEvent failed: %v", err)
	}
	t.Cleanup(func() { _ = rdb.XDel(context.Background(), StreamEvents, evtEntryID).Err() })

	evtEntries, err := rdb.XRange(ctx, StreamEvents, evtEntryID, evtEntryID).Result()
	if err != nil || len(evtEntries) != 1 {
		t.Fatalf("read back event stream entry: %v len=%d", err, len(evtEntries))
	}
	ev := evtEntries[0].Values
	if ev["kind"] != KindRead || ev["event_id"] != evtID || ev["conversation_id"] != "99001" ||
		ev["last_delivered_seq"] != "7" || ev["last_read_seq"] != "7" || ev["target_user_ids"] != "10001" {
		t.Fatalf("unexpected im:stream:events fields: %+v", ev)
	}

	// 2. Test safe retention trimming on a dedicated test stream (never touching global stream entries).
	testStream := "im:stream:test:trim:" + uuid.NewString()
	t.Cleanup(func() { _ = rdb.Del(context.Background(), testStream).Err() })

	groups := []string{"g-persist", "g-deliver"}
	for _, g := range groups {
		if err := rdb.XGroupCreateMkStream(ctx, testStream, g, "0").Err(); err != nil {
			t.Fatalf("create group %s: %v", g, err)
		}
	}

	// Add 5 entries; leave group g-deliver unconsumed at 0-0 -> trim MUST NOT delete anything.
	trimCfg := config.MQConfig{
		StreamMaxLen:    2,
		StreamRetention: time.Hour,
	}
	now := time.Now().UTC()
	trimProducer := NewProducer(rdb, trimCfg, nil, WithProducerClock(func() time.Time { return now }), WithExactTrim(true))

	var ids []string
	for i := 1; i <= 5; i++ {
		id, addErr := rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: testStream,
			ID:     fmt.Sprintf("%d-0", now.Add(-2*time.Hour).UnixMilli()+int64(i)),
			Values: map[string]any{"n": i},
		}).Result()
		if addErr != nil {
			t.Fatalf("xadd test entry %d: %v", i, addErr)
		}
		ids = append(ids, id)
	}

	// Only g-persist reads and ACKs all 5; g-deliver has NOT read yet -> TrimStream trims 0.
	if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    "g-persist",
		Consumer: "c1",
		Streams:  []string{testStream, ">"},
		Count:    10,
	}).Result(); err != nil {
		t.Fatalf("xreadgroup g-persist: %v", err)
	}
	if err := rdb.XAck(ctx, testStream, "g-persist", ids...).Err(); err != nil {
		t.Fatalf("xack g-persist: %v", err)
	}
	if trimmed, err := trimProducer.TrimStream(ctx, testStream, groups); err != nil || trimmed != 0 {
		t.Fatalf("expected 0 trimmed while g-deliver has unread entries, got trimmed=%d err=%v", trimmed, err)
	}

	// Now g-deliver reads all 5, ACKs #1 and #2, leaves #3..#5 pending -> only #1 and #2 may be trimmed!
	if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    "g-deliver",
		Consumer: "c2",
		Streams:  []string{testStream, ">"},
		Count:    10,
	}).Result(); err != nil {
		t.Fatalf("xreadgroup g-deliver: %v", err)
	}
	if err := rdb.XAck(ctx, testStream, "g-deliver", ids[0], ids[1]).Err(); err != nil {
		t.Fatalf("xack g-deliver #1,#2: %v", err)
	}

	trimmed, err := trimProducer.TrimStream(ctx, testStream, groups)
	if err != nil {
		t.Fatalf("TrimStream with partial PEL failed: %v", err)
	}
	if trimmed != 2 {
		t.Fatalf("expected exactly 2 trimmed (#1 and #2), got %d", trimmed)
	}
	remaining, err := rdb.XRange(ctx, testStream, "-", "+").Result()
	if err != nil || len(remaining) != 3 || remaining[0].ID != ids[2] {
		t.Fatalf("expected #3..#5 preserved while #3 is pending, got len=%d first=%v err=%v", len(remaining), remaining, err)
	}

	// ACK #3..#5; all entries are older than 1h StreamRetention -> remaining 3 are trimmed cleanly.
	if err := rdb.XAck(ctx, testStream, "g-deliver", ids[2], ids[3], ids[4]).Err(); err != nil {
		t.Fatalf("xack remaining: %v", err)
	}
	trimmed2, err := trimProducer.TrimStream(ctx, testStream, groups)
	if err != nil || trimmed2 != 3 {
		t.Fatalf("expected remaining 3 old entries trimmed, got trimmed=%d err=%v", trimmed2, err)
	}
}
