package tests

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/model"
)

func TestStage7DisconnectCompensationReceiverSyncAndAck(t *testing.T) {
	rig := newStage7Rig(t)

	alice := rig.registerAndLogin("s7_dc_a", "Alice DC")
	bob := rig.registerAndLogin("s7_dc_b", "Bob DC")
	convIDNum, convID, _ := rig.makeFriends(alice, bob)
	bobInitial := getConvMemberDB(t, rig, convIDNum, bob.ID)

	aliceWS := rig.dialWS(alice)
	bobWS := rig.dialWS(bob)

	// 1. Online baseline: Alice sends M1 (seq=2); Bob receives msg.push (local last_seq=2).
	m1Ack := decodeStage7Ack(t, aliceWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, "online baseline M1", map[string]any{}))
	bobWS.awaitFrame("bob receives online baseline M1", stage7IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == m1Ack.MsgID
	})

	localLastSeq := m1Ack.Seq // "2"

	// 2. Bob disconnects WS.
	bobWS.close()

	// 3. While Bob is offline, Alice sends 5 messages (mix of WS and HTTP) -> seq 3..7.
	sentIDs := make([]string, 0, 5)
	for i := 3; i <= 7; i++ {
		if i%2 == 1 {
			ack := decodeStage7Ack(t, aliceWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, fmt.Sprintf("offline-msg-%d", i), map[string]any{}))
			sentIDs = append(sentIDs, ack.MsgID)
		} else {
			st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
				"client_msg_id": uuid.NewString(),
				"msg_type":      model.MessageTypeText,
				"content":       fmt.Sprintf("offline-msg-%d", i),
				"extra":         map[string]any{},
			})
			assertStage6OK(t, fmt.Sprintf("send offline HTTP msg %d", i), st, env, http.StatusCreated)
			m := decodeStage6JSON[struct {
				Message stage6MessageDTO `json:"message"`
			}](t, env.Data).Message
			sentIDs = append(sentIDs, m.MsgID)
		}
	}

	bobOfflineMem := getConvMemberDB(t, rig, convIDNum, bob.ID)
	wantOfflineUnread := bobInitial.UnreadCount + 1 + 5
	if bobOfflineMem.UnreadCount != wantOfflineUnread || bobOfflineMem.LastDeliveredSeq != 0 || bobOfflineMem.LastReadSeq != 0 {
		t.Fatalf("expected bob offline unread=%d watermarks=(0,0), got %+v", wantOfflineUnread, bobOfflineMem)
	}

	// 4. Bob reconnects WS, sends heartbeat ping with last_seq hint, and runs incremental /sync?after_seq=2.
	bobWS2 := rig.dialWS(bob)
	pingID := "ping-sync-" + uuid.NewString()
	bobWS2.send("ping", pingID, map[string]any{
		"nonce":    pingID,
		"last_seq": map[string]string{convID: localLastSeq},
	})
	bobWS2.awaitFrame("pong with last_seq hint", stage7IOTimeout, func(f stage6WSFrame) bool {
		return f.Type == "pong" && f.ReqID == pingID
	})

	// Paginated incremental sync: page 1 (after_seq=2, limit=3 -> seq 3,4,5, has_more=true), page 2 (after_seq=5 -> seq 6,7).
	s1St, _, s1Env, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/sync?after_seq="+localLastSeq+"&limit=3", bob.AccessToken, nil)
	assertStage6OK(t, "sync page 1 after reconnect", s1St, s1Env, http.StatusOK)
	s1 := decodeStage6JSON[stage6SyncDTO](t, s1Env.Data)
	if len(s1.Items) != 3 || !s1.HasMore || s1.FromSeq != "3" || s1.ToSeq != "5" {
		t.Fatalf("unexpected sync page 1: %+v", s1)
	}

	s2St, _, s2Env, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/sync?after_seq="+s1.ToSeq+"&limit=10", bob.AccessToken, nil)
	assertStage6OK(t, "sync page 2 after reconnect", s2St, s2Env, http.StatusOK)
	s2 := decodeStage6JSON[stage6SyncDTO](t, s2Env.Data)
	if len(s2.Items) != 2 || s2.HasMore || s2.FromSeq != "6" || s2.ToSeq != "7" {
		t.Fatalf("unexpected sync page 2: %+v", s2)
	}

	combined := append(s1.Items, s2.Items...)
	verifySeqStrictlyIncreasing(t, combined)
	for i, item := range combined {
		if item.MsgID != sentIDs[i] || item.seqUint64(t) != uint64(3+i) {
			t.Fatalf("synced item[%d] mismatch: got (%s,%s) want (%s,%d)", i, item.MsgID, item.Seq, sentIDs[i], 3+i)
		}
	}

	// Caught-up sync returns 0 items (no duplicates).
	s3St, _, s3Env, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/sync?after_seq=7&limit=10", bob.AccessToken, nil)
	assertStage6OK(t, "sync caught up", s3St, s3Env, http.StatusOK)
	s3 := decodeStage6JSON[stage6SyncDTO](t, s3Env.Data)
	if len(s3.Items) != 0 || s3.HasMore {
		t.Fatalf("expected empty sync after seq=7, got %+v", s3)
	}

	// 5. Bob reports delivered & read after sync compensation -> unread=0, Alice gets read push & history shows read.
	if wsErr := sendDeliveredAckWS(t, bobWS2, convID, s2.Items[1].MsgID, s2.Items[1].Seq); wsErr != nil {
		t.Fatalf("post-sync delivered_ack failed: %+v", *wsErr)
	}
	awaitReceiptPush(t, aliceWS, convID, bob.IDStr, model.ReceiptStatusDelivered, 7)

	if wsErr := sendReadAckWS(t, bobWS2, convID, "7"); wsErr != nil {
		t.Fatalf("post-sync read_ack failed: %+v", *wsErr)
	}
	awaitReceiptPush(t, aliceWS, convID, bob.IDStr, model.ReceiptStatusRead, 7)

	bobFinalMem := getConvMemberDB(t, rig, convIDNum, bob.ID)
	if bobFinalMem.UnreadCount != 0 || bobFinalMem.LastDeliveredSeq != 7 || bobFinalMem.LastReadSeq != 7 {
		t.Fatalf("expected bob final unread=0 watermarks=(7,7), got %+v", bobFinalMem)
	}

	histSt, _, histEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?limit=20", alice.AccessToken, nil)
	assertStage6OK(t, "alice history after bob sync+read", histSt, histEnv, http.StatusOK)
	hist := decodeStage6JSON[stage6MessagePageDTO](t, histEnv.Data)
	for _, id := range sentIDs {
		hm, ok := findMessageInPage(hist.Items, id)
		if !ok || hm.DeliveryStatus != model.ReceiptStatusRead {
			t.Fatalf("expected offline msg %s delivery_status=read after bob sync+read, got ok=%v msg=%+v", id, ok, hm)
		}
	}
}

func TestStage7DisconnectSenderUnackedRetrySameClientMsgID(t *testing.T) {
	rig := newStage7Rig(t)

	alice := rig.registerAndLogin("s7_sretry_a", "Alice SRetry")
	bob := rig.registerAndLogin("s7_sretry_b", "Bob SRetry")
	convIDNum, convID, _ := rig.makeFriends(alice, bob)
	bobInitial := getConvMemberDB(t, rig, convIDNum, bob.ID)

	bobWS := rig.dialWS(bob)

	// Case A: Alice sends msg.send over WS and immediately closes connection before reading msg.ack.
	aliceWS1 := rig.dialWS(alice)
	clientMsgID := uuid.NewString()
	content := "unacked message retried after reconnect"

	aliceWS1.send("msg.send", "req-drop-"+uuid.NewString(), map[string]any{
		"conversation_id": convID,
		"client_msg_id":   clientMsgID,
		"msg_type":        model.MessageTypeText,
		"content":         content,
		"extra":           map[string]any{},
	})
	// Close immediately without awaiting msg.ack, simulating connection loss mid-flight.
	aliceWS1.close()

	// Brief pause so if the first frame reached the server before TCP FIN, it finishes committing.
	time.Sleep(100 * time.Millisecond)

	// Reconnect on a new WS connection and resend using the ORIGINAL client_msg_id.
	aliceWS2 := rig.dialWS(alice)
	ack := decodeStage7Ack(t, aliceWS2.sendMsgAndAwaitAck(convID, clientMsgID, model.MessageTypeText, content, map[string]any{}))
	if ack.ClientMsgID != clientMsgID || ack.Seq != "2" || ack.DeliveryStatus != model.ReceiptStatusSent {
		t.Fatalf("unexpected msg.ack on reconnect retry: %+v", ack)
	}

	// Retry once more via HTTP with the same original client_msg_id -> must return identical msg_id and seq.
	httpSt, _, httpEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": clientMsgID,
		"msg_type":      model.MessageTypeText,
		"content":       content,
		"extra":         map[string]any{},
	})
	assertStage6OK(t, "HTTP retry after WS reconnect", httpSt, httpEnv, http.StatusOK, http.StatusCreated)
	httpMsg := decodeStage6JSON[struct {
		Message stage6MessageDTO `json:"message"`
	}](t, httpEnv.Data).Message
	if httpMsg.MsgID != ack.MsgID || httpMsg.Seq != ack.Seq {
		t.Fatalf("HTTP retry diverged from WS ack: got (%s,%s) want (%s,%s)", httpMsg.MsgID, httpMsg.Seq, ack.MsgID, ack.Seq)
	}

	// Bob receives the message exactly once, MySQL has only 1 row, and Bob's unread_count is 1.
	bobWS.awaitFrame("bob receives unacked-retried message", stage7IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == ack.MsgID
	})
	bobWS.expectNoFrame("bob must not receive duplicate msg.push after sender reconnect retry", stage7QuietTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == ack.MsgID
	})

	if n, err := countMessagesByClientID(t.Context(), rig.db, alice.ID, clientMsgID); err != nil || n != 1 {
		t.Fatalf("expected 1 DB row for sender unacked retry, got %d (err=%v)", n, err)
	}
	if mem := getConvMemberDB(t, rig, convIDNum, bob.ID); mem.UnreadCount != bobInitial.UnreadCount+1 {
		t.Fatalf("expected bob unread_count=%d (initial %d + 1) after sender unacked retry, got %d",
			bobInitial.UnreadCount+1, bobInitial.UnreadCount, mem.UnreadCount)
	}
}
