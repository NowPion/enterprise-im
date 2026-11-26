package tests

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/model"
)

func TestStage7RedisStreamsProductionDeliveryAndIdempotency(t *testing.T) {
	rig := newStage7Rig(t)

	alice := rig.registerAndLogin("s7_mq_a", "Alice MQ")
	bob := rig.registerAndLogin("s7_mq_b", "Bob MQ")
	convIDNum, convID, _ := rig.makeFriends(alice, bob)

	aliceWS := rig.dialWS(alice)
	bobWS := rig.dialWS(bob)

	// 1. Send M1 via WS -> Bob receives exactly 1 msg.push (no double push), entry exists in im:stream:messages.
	m1Ack := decodeStage7Ack(t, aliceWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, "stream msg 1 ws", map[string]any{}))
	bobWS.awaitFrame("bob receives M1 via consumer group", stage7IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == m1Ack.MsgID
	})
	bobWS.expectNoFrame("bob must not receive duplicate msg.push (no double push) for M1", stage7QuietTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == m1Ack.MsgID
	})

	// 2. Send M2 via HTTP -> Bob receives exactly 1 msg.push.
	m2St, _, m2Env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": uuid.NewString(),
		"msg_type":      model.MessageTypeText,
		"content":       "stream msg 2 http",
		"extra":         map[string]any{},
	})
	assertStage6OK(t, "HTTP send M2 for stream test", m2St, m2Env, http.StatusCreated)
	m2 := decodeStage6JSON[struct {
		Message stage6MessageDTO `json:"message"`
	}](t, m2Env.Data).Message

	bobWS.awaitFrame("bob receives M2 via consumer group", stage7IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == m2.MsgID
	})
	bobWS.expectNoFrame("bob must not receive duplicate msg.push (no double push) for M2", stage7QuietTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == m2.MsgID
	})

	// Verify both M1 and M2 are recorded in im:stream:messages.
	ctx := t.Context()
	m1Entries, err := findStreamEntriesByField(ctx, rig.redis, stage7StreamMsgsKey, "message_id", m1Ack.MsgID)
	if err != nil {
		t.Fatalf("read %s for M1: %v", stage7StreamMsgsKey, err)
	}
	if len(m1Entries) != 1 {
		t.Fatalf("expected 1 stream entry in %s for M1 (%s), got %d", stage7StreamMsgsKey, m1Ack.MsgID, len(m1Entries))
	}
	if gotConv := streamValueString(m1Entries[0].Values, "conversation_id"); gotConv != convID {
		t.Fatalf("stream M1 conversation_id=%q, want %q", gotConv, convID)
	}
	if gotSeq := streamValueString(m1Entries[0].Values, "seq"); gotSeq != m1Ack.Seq {
		t.Fatalf("stream M1 seq=%q, want %q", gotSeq, m1Ack.Seq)
	}

	m2Entries, err := findStreamEntriesByField(ctx, rig.redis, stage7StreamMsgsKey, "message_id", m2.MsgID)
	if err != nil {
		t.Fatalf("read %s for M2: %v", stage7StreamMsgsKey, err)
	}
	if len(m2Entries) != 1 {
		t.Fatalf("expected 1 stream entry in %s for M2 (%s), got %d", stage7StreamMsgsKey, m2.MsgID, len(m2Entries))
	}

	// Verify Consumer Group im-msg-deliver exists and has processed & XACKed the entries.
	msgGrp, err := findStreamGroup(ctx, rig.redis, stage7StreamMsgsKey, stage7GroupMsgDeliv)
	if err != nil || msgGrp == nil {
		t.Fatalf("expected consumer group %s on %s, got grp=%+v err=%v", stage7GroupMsgDeliv, stage7StreamMsgsKey, msgGrp, err)
	}
	deadline := time.Now().Add(stage7IOTimeout)
	for msgGrp.Pending > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		msgGrp, err = findStreamGroup(ctx, rig.redis, stage7StreamMsgsKey, stage7GroupMsgDeliv)
		if err != nil || msgGrp == nil {
			t.Fatalf("poll %s group failed: grp=%+v err=%v", stage7GroupMsgDeliv, msgGrp, err)
		}
	}
	if msgGrp.Pending != 0 {
		t.Fatalf("expected %s pending=0 after delivery, got %d", stage7GroupMsgDeliv, msgGrp.Pending)
	}

	// 3. Receipt ACK triggers im:stream:events and im-event-deliver consumer group.
	if wsErr := sendDeliveredAckWS(t, bobWS, convID, m1Ack.MsgID, m1Ack.Seq); wsErr != nil {
		t.Fatalf("delivered_ack failed in stream test: %+v", *wsErr)
	}
	awaitReceiptPush(t, aliceWS, convID, bob.IDStr, model.ReceiptStatusDelivered, mustParseUint64(t, m1Ack.Seq))

	evtGrp, err := findStreamGroup(ctx, rig.redis, stage7StreamEvtsKey, stage7GroupEvtDeliv)
	if err != nil || evtGrp == nil {
		t.Fatalf("expected consumer group %s on %s, got grp=%+v err=%v", stage7GroupEvtDeliv, stage7StreamEvtsKey, evtGrp, err)
	}

	// 4. Consumer idempotency: re-XADD M1's exact stream payload into im:stream:messages.
	// Must NOT increment Bob's unread_count, must NOT broadcast duplicate msg.push, and must XACK cleanly.
	bobBeforeRedeliv := getConvMemberDB(t, rig, convIDNum, bob.ID)

	redelivID, err := rig.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: stage7StreamMsgsKey,
		Values: m1Entries[0].Values,
	}).Result()
	if err != nil {
		t.Fatalf("re-XADD duplicate entry to %s: %v", stage7StreamMsgsKey, err)
	}
	t.Cleanup(func() {
		_ = rig.redis.XAck(context.Background(), stage7StreamMsgsKey, stage7GroupMsgDeliv, redelivID).Err()
	})

	// Wait until consumer group finishes consuming & ACKing the redelivered entry.
	ackDeadline := time.Now().Add(stage7IOTimeout)
	for time.Now().Before(ackDeadline) {
		grp, gErr := findStreamGroup(ctx, rig.redis, stage7StreamMsgsKey, stage7GroupMsgDeliv)
		if gErr == nil && grp != nil && grp.Pending == 0 && grp.LastDeliveredID >= redelivID {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	bobWS.expectNoFrame("consumer idempotency must suppress duplicate msg.push on redelivery", stage7QuietTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == m1Ack.MsgID
	})

	bobAfterRedeliv := getConvMemberDB(t, rig, convIDNum, bob.ID)
	if bobAfterRedeliv.UnreadCount != bobBeforeRedeliv.UnreadCount {
		t.Fatalf("stream redelivery mutated unread_count: before=%d after=%d", bobBeforeRedeliv.UnreadCount, bobAfterRedeliv.UnreadCount)
	}
	finalGrp, err := findStreamGroup(ctx, rig.redis, stage7StreamMsgsKey, stage7GroupMsgDeliv)
	if err != nil || finalGrp == nil || finalGrp.Pending != 0 {
		t.Fatalf("expected %s pending=0 after idempotent redelivery, got %+v (err=%v)", stage7GroupMsgDeliv, finalGrp, err)
	}
}

func TestStage7RedisStreamsPendingReclaimAndDeadLetter(t *testing.T) {
	t.Run("LiveServerPendingReclaim", func(t *testing.T) {
		rig := newStage7Rig(t)
		ctx := t.Context()

		alice := rig.registerAndLogin("s7_recl_a", "Alice Reclaim")
		bob := rig.registerAndLogin("s7_recl_b", "Bob Reclaim")
		convIDNum, convID, _ := rig.makeFriends(alice, bob)

		st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
			"client_msg_id": uuid.NewString(),
			"msg_type":      model.MessageTypeText,
			"content":       "seed message for pending reclaim",
			"extra":         map[string]any{},
		})
		assertStage6OK(t, "seed M1 on rig server", st, env, http.StatusCreated)
		m1 := decodeStage6JSON[struct {
			Message stage6MessageDTO `json:"message"`
		}](t, env.Data).Message

		m1Entries, err := findStreamEntriesByField(ctx, rig.redis, stage7StreamMsgsKey, "message_id", m1.MsgID)
		if err != nil || len(m1Entries) == 0 {
			t.Fatalf("expected M1 in %s, got len=%d err=%v", stage7StreamMsgsKey, len(m1Entries), err)
		}
		grp, err := findStreamGroup(ctx, rig.redis, stage7StreamMsgsKey, stage7GroupMsgDeliv)
		if err != nil || grp == nil {
			t.Fatalf("expected consumer group %s on %s, got grp=%+v err=%v", stage7GroupMsgDeliv, stage7StreamMsgsKey, grp, err)
		}

		// Wait for rig's server to XACK M1.
		settleDeadline := time.Now().Add(stage7IOTimeout)
		for grp.Pending > 0 && time.Now().Before(settleDeadline) {
			time.Sleep(50 * time.Millisecond)
			grp, _ = findStreamGroup(ctx, rig.redis, stage7StreamMsgsKey, stage7GroupMsgDeliv)
		}

		// Record Bob's unread count before injecting pending redelivery.
		bobBefore := getConvMemberDB(t, rig, convIDNum, bob.ID)

		// Atomically XADD + XREADGROUP via Redis Lua so deadConsumer holds a genuine Pending entry
		// in Redis PEL without racing against the live server's BLOCK consumer loop.
		deadConsumer := "dead-qa-consumer-" + uuid.NewString()
		script := redis.NewScript(`
			local id = redis.call("XADD", KEYS[1], "*", unpack(ARGV, 3))
			redis.call("XREADGROUP", "GROUP", ARGV[1], ARGV[2], "COUNT", "10", "STREAMS", KEYS[1], ">")
			return id
		`)
		args := make([]any, 0, 2+len(m1Entries[0].Values)*2)
		args = append(args, stage7GroupMsgDeliv, deadConsumer)
		for k, v := range m1Entries[0].Values {
			args = append(args, k, v)
		}
		res, err := script.Run(ctx, rig.redis, []string{stage7StreamMsgsKey}, args...).Result()
		if err != nil {
			t.Fatalf("atomically XADD + XREADGROUP pending entry: %v", err)
		}
		injectedID, ok := res.(string)
		if !ok || injectedID == "" {
			t.Fatalf("unexpected injected stream id: %#v", res)
		}
		t.Cleanup(func() {
			_ = rig.redis.XAck(context.Background(), stage7StreamMsgsKey, stage7GroupMsgDeliv, injectedID).Err()
		})

		// Verify real Pending entry is in Redis PEL owned by deadConsumer.
		pendExt, err := rig.redis.XPendingExt(ctx, &redis.XPendingExtArgs{
			Stream:   stage7StreamMsgsKey,
			Group:    stage7GroupMsgDeliv,
			Start:    injectedID,
			End:      injectedID,
			Count:    1,
			Consumer: deadConsumer,
		}).Result()
		if err != nil || len(pendExt) != 1 || pendExt[0].ID != injectedID {
			t.Fatalf("expected 1 pending entry %s for %s in Redis PEL, got %+v (err=%v)", injectedID, deadConsumer, pendExt, err)
		}

		// Wait for the live backend's XAUTOCLAIM worker to reclaim the stale Pending entry and XACK it.
		reclaimDeadline := time.Now().Add(stage7ReclaimWait)
		reclaimed := false
		for time.Now().Before(reclaimDeadline) {
			rem, pErr := rig.redis.XPendingExt(ctx, &redis.XPendingExtArgs{
				Stream: stage7StreamMsgsKey,
				Group:  stage7GroupMsgDeliv,
				Start:  injectedID,
				End:    injectedID,
				Count:  1,
			}).Result()
			if pErr == nil && len(rem) == 0 {
				reclaimed = true
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if !reclaimed {
			t.Fatalf("live server XAUTOCLAIM did not reclaim & XACK pending entry %s within %s", injectedID, stage7ReclaimWait)
		}

		// Reclaimed duplicate message must NOT double-increment Bob's unread_count.
		bobAfter := getConvMemberDB(t, rig, convIDNum, bob.ID)
		if bobAfter.UnreadCount != bobBefore.UnreadCount {
			t.Fatalf("reclaimed pending message incremented unread_count: before=%d after=%d", bobBefore.UnreadCount, bobAfter.UnreadCount)
		}
	})

	t.Run("ControlledRealRedisXAutoclaimAndDeadLetter", func(t *testing.T) {
		rig := newStage7Rig(t)
		ctx := t.Context()

		suffix := uuid.NewString()
		streamKey := "im:stream:qa-reclaim:" + suffix
		deadKey := "im:stream:dead:qa:" + suffix
		groupName := "im-msg-deliver"
		t.Cleanup(func() {
			_ = rig.redis.Del(context.Background(), streamKey, deadKey).Err()
		})

		if err := rig.redis.XGroupCreateMkStream(ctx, streamKey, groupName, "0").Err(); err != nil {
			t.Fatalf("XGROUP CREATE: %v", err)
		}
		entryID, err := rig.redis.XAdd(ctx, &redis.XAddArgs{
			Stream: streamKey,
			Values: map[string]any{
				"kind":            "message",
				"message_id":      "188900000000001",
				"conversation_id": "90001",
				"seq":             "1",
				"sender_id":       "10001",
			},
		}).Result()
		if err != nil {
			t.Fatalf("XADD test stream entry: %v", err)
		}

		// Consumer 1 reads without ACK -> creates Pending entry with RetryCount=1.
		readRes, err := rig.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    groupName,
			Consumer: "crashed-node-1-worker-0",
			Streams:  []string{streamKey, ">"},
			Count:    1,
		}).Result()
		if err != nil || len(readRes) != 1 || len(readRes[0].Messages) != 1 || readRes[0].Messages[0].ID != entryID {
			t.Fatalf("XREADGROUP initial consume failed: res=%+v err=%v", readRes, err)
		}

		pendBefore, err := rig.redis.XPending(ctx, streamKey, groupName).Result()
		if err != nil || pendBefore.Count != 1 || pendBefore.Consumers["crashed-node-1-worker-0"] != 1 {
			t.Fatalf("unexpected XPENDING after unacked consume: %+v (err=%v)", pendBefore, err)
		}

		// Wait past 50ms idle threshold and reclaim via real XAUTOCLAIM.
		time.Sleep(80 * time.Millisecond)
		claimed, _, err := rig.redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   streamKey,
			Group:    groupName,
			Consumer: "reclaim-node-2",
			MinIdle:  50 * time.Millisecond,
			Start:    "0-0",
			Count:    10,
		}).Result()
		if err != nil || len(claimed) != 1 || claimed[0].ID != entryID {
			t.Fatalf("XAUTOCLAIM failed: claimed=%+v err=%v", claimed, err)
		}

		// Simulate 5 failed delivery attempts -> write to dead-letter stream and XACK original entry.
		for attempt := 2; attempt <= 5; attempt++ {
			time.Sleep(60 * time.Millisecond)
			if _, err := rig.redis.XClaim(ctx, &redis.XClaimArgs{
				Stream:   streamKey,
				Group:    groupName,
				Consumer: "reclaim-node-2",
				MinIdle:  50 * time.Millisecond,
				Messages: []string{entryID},
			}).Result(); err != nil {
				t.Fatalf("XCLAIM retry attempt %d: %v", attempt, err)
			}
		}

		pendExt, err := rig.redis.XPendingExt(ctx, &redis.XPendingExtArgs{
			Stream: streamKey,
			Group:  groupName,
			Start:  entryID,
			End:    entryID,
			Count:  1,
		}).Result()
		if err != nil || len(pendExt) != 1 || pendExt[0].RetryCount < 5 || pendExt[0].Consumer != "reclaim-node-2" {
			t.Fatalf("expected RetryCount>=5 on reclaim-node-2, got %+v (err=%v)", pendExt, err)
		}

		if _, err := rig.redis.XAdd(ctx, &redis.XAddArgs{
			Stream: deadKey,
			Values: map[string]any{
				"orig_stream_id": entryID,
				"group":          groupName,
				"last_consumer":  pendExt[0].Consumer,
				"attempts":       pendExt[0].RetryCount,
				"error_summary":  "exceeded max retry attempts",
				"message_id":     "188900000000001",
			},
		}).Result(); err != nil {
			t.Fatalf("XADD dead-letter entry: %v", err)
		}
		if err := rig.redis.XAck(ctx, streamKey, groupName, entryID).Err(); err != nil {
			t.Fatalf("XACK after dead-letter move: %v", err)
		}

		pendAfter, err := rig.redis.XPending(ctx, streamKey, groupName).Result()
		if err != nil || pendAfter.Count != 0 {
			t.Fatalf("expected XPENDING count=0 after dead-letter XACK, got %+v (err=%v)", pendAfter, err)
		}
		deadMsgs, err := rig.redis.XRange(ctx, deadKey, "-", "+").Result()
		if err != nil || len(deadMsgs) != 1 || streamValueString(deadMsgs[0].Values, "orig_stream_id") != entryID {
			t.Fatalf("expected 1 dead-letter entry for %s, got %+v (err=%v)", entryID, deadMsgs, err)
		}
	})
}
