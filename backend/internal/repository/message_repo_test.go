package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
)

func TestDedupeValueFormatAndParse(t *testing.T) {
	rec := DedupeRecord{
		MsgID:          188900000000001,
		ConversationID: 90001,
		Seq:            42,
		Status:         model.MessageStatusNormal,
		DeliveryStatus: model.ReceiptStatusSent,
	}
	raw := FormatDedupeValue(rec)
	if raw != "188900000000001|90001|42|normal|sent" {
		t.Fatalf("unexpected dedupe format: %q", raw)
	}
	parsed, ok := ParseDedupeValue(raw)
	if !ok || *parsed != rec {
		t.Fatalf("failed to parse dedupe value: parsed=%+v ok=%v", parsed, ok)
	}

	for _, bad := range []string{
		"",
		"1|2|3|normal",
		"0|2|3|normal|sent",
		"1|2|3|invalid|sent",
		"1|2|3|normal|unknown",
		"abc|2|3|normal|sent",
	} {
		if _, ok := ParseDedupeValue(bad); ok {
			t.Fatalf("expected invalid dedupe value %q to be rejected", bad)
		}
	}
}

func TestIsDuplicateClientMsgError(t *testing.T) {
	dup := &mysqldriver.MySQLError{
		Number:  1062,
		Message: "Duplicate entry '10001-cmsg1' for key 'messages.uk_msg_sender_client'",
	}
	if !IsDuplicateClientMsgError(dup) {
		t.Fatal("expected uk_msg_sender_client 1062 error to match")
	}

	other := &mysqldriver.MySQLError{
		Number:  1062,
		Message: "Duplicate entry '90001-1' for key 'messages.uk_msg_conv_seq'",
	}
	if IsDuplicateClientMsgError(other) {
		t.Fatal("did not expect uk_msg_conv_seq 1062 error to match client_msg_id duplicate")
	}

	if IsDuplicateClientMsgError(errors.New("other error")) {
		t.Fatal("did not expect generic error to match")
	}
}

func TestMessageRepositoryNilAndUnreachableRedisFailFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	nilRepo := NewMessageRepository(nil, nil)
	if _, _, err := nilRepo.GetDedupeRecord(ctx, 1, "c1"); !errors.Is(err, ErrRedisClientNil) {
		t.Fatalf("expected ErrRedisClientNil, got %v", err)
	}
	if _, _, err := nilRepo.AcquireDedupeLock(ctx, 1, "c1", time.Second, 100*time.Millisecond); !errors.Is(err, ErrRedisClientNil) {
		t.Fatalf("expected ErrRedisClientNil, got %v", err)
	}

	deadRedis := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
	})
	defer func() { _ = deadRedis.Close() }()

	deadRepo := NewMessageRepository(nil, deadRedis)
	if _, _, err := deadRepo.GetDedupeRecord(ctx, 1, "c1"); err == nil {
		t.Fatal("expected GetDedupeRecord on unreachable redis to return error")
	}
	if _, _, err := deadRepo.AcquireDedupeLock(ctx, 1, "c1", time.Second, 100*time.Millisecond); err == nil {
		t.Fatal("expected AcquireDedupeLock on unreachable redis to return error")
	}
	if _, err := deadRepo.FindMessageByConvSeq(ctx, 1, 1); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil db FindMessageByConvSeq, got %v", err)
	}

	nilPub := NewRedisStreamPublisher(nil)
	if err := nilPub.PublishMessage(ctx, StreamMessageInput{MessageID: 1, ConversationID: 1, Seq: 1}); !errors.Is(err, ErrRedisClientNil) {
		t.Fatalf("expected ErrRedisClientNil from nil publisher, got %v", err)
	}
	deadPub := NewRedisStreamPublisher(deadRedis)
	if err := deadPub.PublishMessage(ctx, StreamMessageInput{MessageID: 1, ConversationID: 1, Seq: 1}); err == nil {
		t.Fatal("expected PublishMessage on unreachable redis to fail")
	}
	if err := deadPub.PublishEvent(ctx, StreamEventInput{Kind: "recall", ConversationID: 1}); err == nil {
		t.Fatal("expected PublishEvent on unreachable redis to fail")
	}
}

func TestIsDuplicateConvSeqError(t *testing.T) {
	dup := &mysqldriver.MySQLError{
		Number:  1062,
		Message: "Duplicate entry '90001-5' for key 'messages.uk_msg_conv_seq'",
	}
	if !IsDuplicateConvSeqError(dup) {
		t.Fatal("expected uk_msg_conv_seq 1062 error to match")
	}
	other := &mysqldriver.MySQLError{
		Number:  1062,
		Message: "Duplicate entry '10001-cmsg1' for key 'messages.uk_msg_sender_client'",
	}
	if IsDuplicateConvSeqError(other) {
		t.Fatal("did not expect uk_msg_sender_client error to match conv_seq duplicate")
	}
}

func TestLiveMessageRepoDedupeConvSeqAndStreamsIntegration(t *testing.T) {
	mysqlPass, mysqlSet := lookupEnv("MYSQL_PASSWORD", "IM_MYSQL_PASSWORD")
	redisPass, redisSet := lookupEnv("REDIS_PASSWORD", "IM_REDIS_PASSWORD")
	if !mysqlSet && !redisSet && os.Getenv("MYSQL_HOST") == "" && os.Getenv("IM_MYSQL_HOST") == "" {
		t.Skip("skipping live MessageRepository integration test: env credentials not set")
	}

	host := firstNonEmpty(os.Getenv("MYSQL_HOST"), os.Getenv("IM_MYSQL_HOST"), "127.0.0.1")
	port := 3306
	if p := firstNonEmpty(os.Getenv("MYSQL_PORT"), os.Getenv("IM_MYSQL_PORT")); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	user := firstNonEmpty(os.Getenv("MYSQL_USER"), os.Getenv("IM_MYSQL_USER"), "root")
	dbName := firstNonEmpty(os.Getenv("MYSQL_DATABASE"), os.Getenv("IM_MYSQL_DATABASE"), "enterprise_im")
	redisAddr := firstNonEmpty(os.Getenv("REDIS_ADDR"), os.Getenv("IM_REDIS_ADDR"), "127.0.0.1:6379")

	log, err := logger.New(config.LogConfig{Level: "warn", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	db, err := NewMySQL(config.MySQLConfig{
		Host:            host,
		Port:            port,
		User:            user,
		Password:        mysqlPass,
		Database:        dbName,
		Charset:         "utf8mb4",
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		DialTimeout:     3 * time.Second,
	}, log)
	if err != nil {
		t.Fatalf("NewMySQL failed: %v", err)
	}
	defer func() { _ = CloseMySQL(db) }()

	rdb, err := NewRedis(config.RedisConfig{
		Addr:         redisAddr,
		Password:     redisPass,
		DB:           testLiveRedisDB(),
		PoolSize:     5,
		MinIdleConns: 1,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRedis failed: %v", err)
	}
	defer func() { _ = CloseRedis(rdb) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	suffix := uint64(time.Now().UnixNano())
	testMsgsStream := fmt.Sprintf("im:stream:test:repo:messages:%d", suffix)
	testEvtsStream := fmt.Sprintf("im:stream:test:repo:events:%d", suffix)
	repo := NewMessageRepository(db, rdb)
	pub := NewRedisStreamPublisher(rdb, WithStreamKeys(testMsgsStream, testEvtsStream))
	gen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}

	convID := suffix
	senderID := suffix + 1
	clientMsgID := fmt.Sprintf("live-repo-cmsg-%d", suffix)
	msgID, err := gen.NextID()
	if err != nil {
		t.Fatalf("NextID: %v", err)
	}

	var streamMsgIDs []string
	var streamEvtIDs []string
	defer func() {
		cleanupCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = db.WithContext(cleanupCtx).Where("conversation_id = ?", convID).Delete(&model.Message{}).Error
		_ = rdb.Del(cleanupCtx, DedupeKey(senderID, clientMsgID), DedupeLockKey(senderID, clientMsgID)).Err()
		_ = rdb.Del(cleanupCtx, testMsgsStream, testEvtsStream).Err()
		if len(streamMsgIDs) > 0 {
			_ = rdb.XDel(cleanupCtx, testMsgsStream, streamMsgIDs...).Err()
		}
		if len(streamEvtIDs) > 0 {
			_ = rdb.XDel(cleanupCtx, testEvtsStream, streamEvtIDs...).Err()
		}
	}()

	// 1. Verify Dedupe SET NX + TTL 24h + format + lock TTL 10s.
	lockTok, hit, err := repo.AcquireDedupeLock(ctx, senderID, clientMsgID, 0, 200*time.Millisecond)
	if err != nil || hit != nil || lockTok == "" {
		t.Fatalf("AcquireDedupeLock failed: tok=%q hit=%v err=%v", lockTok, hit, err)
	}
	lockTTL, err := rdb.TTL(ctx, DedupeLockKey(senderID, clientMsgID)).Result()
	if err != nil || lockTTL <= 0 || lockTTL > 10*time.Second {
		t.Fatalf("expected dedupe lock TTL in (0, 10s], got %v (err=%v)", lockTTL, err)
	}
	rec := DedupeRecord{
		MsgID:          msgID,
		ConversationID: convID,
		Seq:            1,
		Status:         model.MessageStatusNormal,
		DeliveryStatus: model.ReceiptStatusSent,
	}
	if err := repo.SetDedupeRecordNX(ctx, senderID, clientMsgID, rec, 0); err != nil {
		t.Fatalf("SetDedupeRecordNX failed: %v", err)
	}
	// SET NX must not overwrite existing dedupe entry.
	if err := repo.SetDedupeRecordNX(ctx, senderID, clientMsgID, DedupeRecord{
		MsgID: msgID + 1, ConversationID: convID, Seq: 2, Status: model.MessageStatusRecalled, DeliveryStatus: model.ReceiptStatusRead,
	}, 0); err != nil {
		t.Fatalf("second SetDedupeRecordNX failed: %v", err)
	}
	gotRec, found, err := repo.GetDedupeRecord(ctx, senderID, clientMsgID)
	if err != nil || !found || *gotRec != rec {
		t.Fatalf("unexpected dedupe record: got=%+v found=%v err=%v", gotRec, found, err)
	}
	dedupeTTL, err := rdb.TTL(ctx, DedupeKey(senderID, clientMsgID)).Result()
	if err != nil || dedupeTTL <= 23*time.Hour || dedupeTTL > 24*time.Hour {
		t.Fatalf("expected dedupe TTL ~24h, got %v (err=%v)", dedupeTTL, err)
	}
	if err := repo.ReleaseDedupeLock(ctx, senderID, clientMsgID, lockTok); err != nil {
		t.Fatalf("ReleaseDedupeLock failed: %v", err)
	}

	// 2. Verify FindMessageByConvSeq and uk_msg_conv_seq error detection.
	row := model.Message{
		ID:             msgID,
		ConversationID: convID,
		Seq:            1,
		SenderID:       senderID,
		ClientMsgID:    clientMsgID,
		MsgType:        model.MessageTypeText,
		Content:        "live repo check",
		Extra:          model.NullableJSON("{}"),
		Status:         model.MessageStatusNormal,
		CreatedAt:      time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := db.WithContext(ctx).Create(&row).Error; err != nil {
		t.Fatalf("insert test message: %v", err)
	}
	foundBySeq, err := repo.FindMessageByConvSeq(ctx, convID, 1)
	if err != nil || foundBySeq.ID != msgID {
		t.Fatalf("FindMessageByConvSeq failed: got=%+v err=%v", foundBySeq, err)
	}
	dupRow := row
	dupRow.ID = msgID + 10
	dupRow.ClientMsgID = clientMsgID + "-other"
	dupErr := db.WithContext(ctx).Create(&dupRow).Error
	if !IsDuplicateConvSeqError(dupErr) {
		t.Fatalf("expected IsDuplicateConvSeqError on duplicate (conv, seq), got %v", dupErr)
	}

	// 3. Verify RedisStreamPublisher XADD fields on im:stream:messages and im:stream:events.
	if err := pub.PublishMessage(ctx, StreamMessageInput{
		Kind:           "message",
		MessageID:      msgID,
		ConversationID: convID,
		Seq:            1,
		SenderID:       senderID,
		PayloadRef:     fmt.Sprintf("mysql:messages:%d", msgID),
	}); err != nil {
		t.Fatalf("PublishMessage failed: %v", err)
	}
	msgs, err := rdb.XRevRangeN(ctx, testMsgsStream, "+", "-", 10).Result()
	if err != nil {
		t.Fatalf("XRevRangeN messages: %v", err)
	}
	var matchedMsg bool
	for _, x := range msgs {
		if x.Values["message_id"] == strconv.FormatUint(msgID, 10) {
			streamMsgIDs = append(streamMsgIDs, x.ID)
			if x.Values["kind"] == "message" &&
				x.Values["conversation_id"] == strconv.FormatUint(convID, 10) &&
				x.Values["seq"] == "1" &&
				x.Values["sender_id"] == strconv.FormatUint(senderID, 10) &&
				x.Values["payload_ref"] != "" {
				matchedMsg = true
			}
		}
	}
	if !matchedMsg {
		t.Fatalf("expected published entry in %s for message_id=%d", testMsgsStream, msgID)
	}

	if err := pub.PublishEvent(ctx, StreamEventInput{
		Kind:           "recall",
		Event:          "message.recalled",
		ConversationID: convID,
		ActorID:        senderID,
		MessageID:      msgID,
		Seq:            2,
		TargetUserIDs:  []uint64{senderID, senderID + 10},
		Payload: map[string]any{
			"recalled_msg_id": strconv.FormatUint(msgID, 10),
			"recall_msg_id":   strconv.FormatUint(msgID+1, 10),
			"recall_seq":      "2",
			"recalled_at":     "2026-11-26T12:00:00.000Z",
		},
	}); err != nil {
		t.Fatalf("PublishEvent failed: %v", err)
	}
	evts, err := rdb.XRevRangeN(ctx, testEvtsStream, "+", "-", 10).Result()
	if err != nil {
		t.Fatalf("XRevRangeN events: %v", err)
	}
	var matchedEvt bool
	for _, x := range evts {
		if x.Values["kind"] == "recall" && x.Values["message_id"] == strconv.FormatUint(msgID, 10) {
			streamEvtIDs = append(streamEvtIDs, x.ID)
			if x.Values["event"] == "message.recalled" &&
				x.Values["conversation_id"] == strconv.FormatUint(convID, 10) &&
				x.Values["recalled_msg_id"] == strconv.FormatUint(msgID, 10) {
				matchedEvt = true
			}
		}
	}
	if !matchedEvt {
		t.Fatalf("expected published recall event in %s for message_id=%d", testEvtsStream, msgID)
	}
}
