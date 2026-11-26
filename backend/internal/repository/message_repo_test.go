package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/model"
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
}
