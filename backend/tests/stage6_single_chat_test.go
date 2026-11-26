package tests

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
)

func TestStage6SingleChatConversationAndMessaging(t *testing.T) {
	rig := newStage6Rig(t)
	expectDBAvailable(t, rig.db)

	alice := rig.registerAndLogin("s6_sc_a", "Alice SC")
	bob := rig.registerAndLogin("s6_sc_b", "Bob SC")

	convIDNum, convID, acceptSysMsgID := rig.makeFriends(alice, bob)

	// 1. Conversation appears in both users' conversation lists and detail endpoint.
	for _, actor := range []struct {
		name string
		user stage6User
		peer stage6User
	}{
		{"alice", alice, bob},
		{"bob", bob, alice},
	} {
		st, _, env, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations", actor.user.AccessToken, nil)
		assertStage6OK(t, actor.name+" list conversations", st, env, http.StatusOK)
		page := decodeStage6JSON[stage6ConversationPageDTO](t, env.Data)

		var found *stage6ConversationDTO
		for i := range page.Items {
			if page.Items[i].ID == convID {
				found = &page.Items[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("%s conversation list missing single conversation %s: %s", actor.name, convID, string(env.Data))
		}
		if found.Type != model.ConversationTypeSingle || !found.IsVisible || found.GroupID != nil {
			t.Fatalf("%s unexpected conversation summary: %+v", actor.name, *found)
		}
		if found.CanSend != nil && !*found.CanSend {
			t.Fatalf("%s expected can_send=true on single chat, got false", actor.name)
		}
		var peerObj struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		}
		if err := decodeJSONInto(found.Peer, &peerObj); err != nil || peerObj.ID != actor.peer.IDStr {
			t.Fatalf("%s conversation peer mismatch: got %s want %s", actor.name, string(found.Peer), actor.peer.IDStr)
		}

		detSt, _, detEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID, actor.user.AccessToken, nil)
		assertStage6OK(t, actor.name+" get conversation detail", detSt, detEnv, http.StatusOK)
		det := extractConversationFromDetail(t, detEnv.Data)
		if det.ID != convID || det.Type != model.ConversationTypeSingle {
			t.Fatalf("%s conversation detail mismatch: %+v", actor.name, det)
		}
	}

	// 2. Friend-accept system message is persisted and readable in conversation history.
	histSt, _, histEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, nil)
	assertStage6OK(t, "initial single chat history", histSt, histEnv, http.StatusOK)
	initPage := decodeStage6JSON[stage6MessagePageDTO](t, histEnv.Data)
	sysMsg, ok := findMessageInPage(initPage.Items, acceptSysMsgID)
	if !ok || sysMsg.MsgType != model.MessageTypeSystem || sysMsg.Seq != "1" || sysMsg.SenderID != "0" {
		t.Fatalf("expected friend-accept system message %s seq=1 in history, got %+v", acceptSysMsgID, initPage.Items)
	}

	// 3. Connect both Alice and Bob over WS.
	aliceWS := rig.dialWS(alice)
	bobWS := rig.dialWS(bob)

	// 4. Send text via HTTP POST -> persisted, delivery_status=sent, Bob receives real-time msg.push.
	httpClientMsgID := uuid.NewString()
	httpText := "hello over HTTP from Alice"
	sendSt, _, sendEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": httpClientMsgID,
		"msg_type":      model.MessageTypeText,
		"content":       httpText,
		"extra":         map[string]any{},
	})
	assertStage6OK(t, "HTTP send text", sendSt, sendEnv, http.StatusCreated)
	httpSendWrap := decodeStage6JSON[struct {
		Message stage6MessageDTO `json:"message"`
	}](t, sendEnv.Data)
	httpMsg := httpSendWrap.Message
	if httpMsg.MsgID == "" || httpMsg.ClientMsgID != httpClientMsgID || httpMsg.ConversationID != convID ||
		httpMsg.SenderID != alice.IDStr || httpMsg.MsgType != model.MessageTypeText || httpMsg.Content != httpText ||
		httpMsg.Status != model.MessageStatusNormal || httpMsg.DeliveryStatus != model.ReceiptStatusSent ||
		httpMsg.seqUint64(t) != 2 || !isValidRFC3339Millis(httpMsg.CreatedAt) {
		t.Fatalf("unexpected HTTP sent message payload: %+v", httpMsg)
	}

	bobPushHTTP := bobWS.awaitFrame("bob msg.push for HTTP message", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == httpMsg.MsgID
	})
	_, pushedHTTPMsg := decodeMsgPushMessage(t, bobPushHTTP)
	if pushedHTTPMsg.Seq != httpMsg.Seq || pushedHTTPMsg.Content != httpText || pushedHTTPMsg.SenderID != alice.IDStr {
		t.Fatalf("bob msg.push mismatch for HTTP message: %+v", pushedHTTPMsg)
	}

	// 5. Send text via WS msg.send -> Bob gets msg.ack (delivery_status=sent), Alice gets msg.push, persisted in MySQL.
	wsClientMsgID := uuid.NewString()
	wsText := "hello over WS from Bob"
	ackFrame := bobWS.sendMsgAndAwaitAck(convID, wsClientMsgID, model.MessageTypeText, wsText, map[string]any{})
	if ackFrame.Type != "msg.ack" {
		t.Fatalf("expected msg.ack for WS msg.send, got %s: %s", ackFrame.Type, string(ackFrame.Data))
	}
	var ackData struct {
		ClientMsgID    string `json:"client_msg_id"`
		MsgID          string `json:"msg_id"`
		ConversationID string `json:"conversation_id"`
		Seq            string `json:"seq"`
		Status         string `json:"status"`
		DeliveryStatus string `json:"delivery_status"`
	}
	ackData = decodeStage6JSON[struct {
		ClientMsgID    string `json:"client_msg_id"`
		MsgID          string `json:"msg_id"`
		ConversationID string `json:"conversation_id"`
		Seq            string `json:"seq"`
		Status         string `json:"status"`
		DeliveryStatus string `json:"delivery_status"`
	}](t, ackFrame.Data)
	if ackData.MsgID == "" || ackData.ClientMsgID != wsClientMsgID || ackData.ConversationID != convID ||
		ackData.Seq != "3" || ackData.Status != model.MessageStatusNormal || ackData.DeliveryStatus != model.ReceiptStatusSent {
		t.Fatalf("unexpected msg.ack payload: %+v", ackData)
	}

	alicePushWS := aliceWS.awaitFrame("alice msg.push for WS message", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == ackData.MsgID
	})
	_, pushedWSMsg := decodeMsgPushMessage(t, alicePushWS)
	if pushedWSMsg.Seq != "3" || pushedWSMsg.Content != wsText || pushedWSMsg.SenderID != bob.IDStr {
		t.Fatalf("alice msg.push mismatch for WS message: %+v", pushedWSMsg)
	}

	var storedCount int64
	if err := rig.db.WithContext(t.Context()).Model(&model.Message{}).
		Where("conversation_id = ? AND id IN ?", convIDNum, []uint64{mustParseUint64(t, httpMsg.MsgID), mustParseUint64(t, ackData.MsgID)}).
		Count(&storedCount).Error; err != nil || storedCount != 2 {
		t.Fatalf("expected 2 persisted messages in MySQL, got %d (err=%v)", storedCount, err)
	}

	// 6. Idempotency: retrying HTTP & WS with same (sender_id, client_msg_id) does NOT duplicate DB rows,
	//    returns identical msg_id and seq; concurrent retries also collapse to 1 row; different payload returns 14001.
	retrySt, _, retryEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": httpClientMsgID,
		"msg_type":      model.MessageTypeText,
		"content":       httpText,
		"extra":         map[string]any{},
	})
	assertStage6OK(t, "HTTP idempotent retry", retrySt, retryEnv, http.StatusOK, http.StatusCreated)
	retryMsg := decodeStage6JSON[struct {
		Message stage6MessageDTO `json:"message"`
	}](t, retryEnv.Data).Message
	if retryMsg.MsgID != httpMsg.MsgID || retryMsg.Seq != httpMsg.Seq {
		t.Fatalf("HTTP retry returned different id/seq: got %s/%s want %s/%s", retryMsg.MsgID, retryMsg.Seq, httpMsg.MsgID, httpMsg.Seq)
	}

	wsRetryAck := bobWS.sendMsgAndAwaitAck(convID, wsClientMsgID, model.MessageTypeText, wsText, map[string]any{})
	if wsRetryAck.Type != "msg.ack" {
		t.Fatalf("expected msg.ack on WS retry, got %s: %s", wsRetryAck.Type, string(wsRetryAck.Data))
	}
	wsRetryData := decodeStage6JSON[struct {
		MsgID string `json:"msg_id"`
		Seq   string `json:"seq"`
	}](t, wsRetryAck.Data)
	if wsRetryData.MsgID != ackData.MsgID || wsRetryData.Seq != ackData.Seq {
		t.Fatalf("WS retry returned different id/seq: got %s/%s want %s/%s", wsRetryData.MsgID, wsRetryData.Seq, ackData.MsgID, ackData.Seq)
	}

	// Concurrent 5x retries with a fresh client_msg_id.
	concClientID := uuid.NewString()
	concContent := "concurrent idempotent message"
	var wg sync.WaitGroup
	concResults := make(chan stage6MessageDTO, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
				"client_msg_id": concClientID,
				"msg_type":      model.MessageTypeText,
				"content":       concContent,
				"extra":         map[string]any{},
			})
			assertStage6OK(t, "concurrent HTTP send", st, env, http.StatusOK, http.StatusCreated)
			msg := decodeStage6JSON[struct {
				Message stage6MessageDTO `json:"message"`
			}](t, env.Data).Message
			concResults <- msg
		}()
	}
	wg.Wait()
	close(concResults)

	var firstConc stage6MessageDTO
	for msg := range concResults {
		if firstConc.MsgID == "" {
			firstConc = msg
		} else if msg.MsgID != firstConc.MsgID || msg.Seq != firstConc.Seq {
			t.Fatalf("concurrent retries diverged: %+v vs %+v", msg, firstConc)
		}
	}
	if firstConc.Seq != "4" {
		t.Fatalf("expected concurrent retry message seq=4 (no seq gap), got %q", firstConc.Seq)
	}
	if n, err := countMessagesByClientID(t.Context(), rig.db, alice.ID, concClientID); err != nil || n != 1 {
		t.Fatalf("expected 1 row in MySQL for concurrent retries, got %d (err=%v)", n, err)
	}
	if n, err := countMessagesByClientID(t.Context(), rig.db, alice.ID, httpClientMsgID); err != nil || n != 1 {
		t.Fatalf("expected 1 row in MySQL for HTTP retry, got %d (err=%v)", n, err)
	}
	if n, err := countMessagesByClientID(t.Context(), rig.db, bob.ID, wsClientMsgID); err != nil || n != 1 {
		t.Fatalf("expected 1 row in MySQL for WS retry, got %d (err=%v)", n, err)
	}

	// Reusing same client_msg_id with DIFFERENT payload -> 14001 ErrClientMsgIDConflict.
	conflictSt, _, conflictEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": httpClientMsgID,
		"msg_type":      model.MessageTypeText,
		"content":       "mutated payload with old client_msg_id",
		"extra":         map[string]any{},
	})
	assertStage6BizErr(t, "conflicting client_msg_id reuse", conflictSt, conflictEnv, errcode.ErrClientMsgIDConflict.Code)

	// 7. Seq cursor pagination (before_seq / after_seq) and /sync (strictly seq > after_seq).
	// Append 4 more messages so we have seqs 1..8.
	for i := 5; i <= 8; i++ {
		st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
			"client_msg_id": uuid.NewString(),
			"msg_type":      model.MessageTypeText,
			"content":       fmt.Sprintf("seq-page-msg-%d", i),
			"extra":         map[string]any{},
		})
		assertStage6OK(t, fmt.Sprintf("send pagination msg %d", i), st, env, http.StatusCreated)
	}

	// Latest page (no cursor, limit=3) -> seqs 6, 7, 8 in ascending order, has_more=true.
	p1St, _, p1Env, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?limit=3", bob.AccessToken, nil)
	assertStage6OK(t, "messages page 1", p1St, p1Env, http.StatusOK)
	p1 := decodeStage6JSON[stage6MessagePageDTO](t, p1Env.Data)
	if len(p1.Items) != 3 || !p1.HasMore {
		t.Fatalf("expected 3 items with has_more=true on page 1, got len=%d has_more=%v", len(p1.Items), p1.HasMore)
	}
	verifySeqStrictlyIncreasing(t, p1.Items)
	if p1.Items[0].Seq != "6" || p1.Items[2].Seq != "8" {
		t.Fatalf("expected page 1 seqs [6..8], got [%s..%s]", p1.Items[0].Seq, p1.Items[2].Seq)
	}

	// Older page via before_seq=6, limit=3 -> seqs 3, 4, 5 in ASCENDING order!
	p2St, _, p2Env, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?before_seq=6&limit=3", bob.AccessToken, nil)
	assertStage6OK(t, "messages before_seq=6", p2St, p2Env, http.StatusOK)
	p2 := decodeStage6JSON[stage6MessagePageDTO](t, p2Env.Data)
	if len(p2.Items) != 3 || !p2.HasMore {
		t.Fatalf("expected 3 items with has_more=true on before_seq=6, got len=%d has_more=%v", len(p2.Items), p2.HasMore)
	}
	verifySeqStrictlyIncreasing(t, p2.Items)
	if p2.Items[0].Seq != "3" || p2.Items[2].Seq != "5" {
		t.Fatalf("expected before_seq=6 seqs [3,4,5] ascending, got [%s..%s]", p2.Items[0].Seq, p2.Items[2].Seq)
	}

	// Oldest page via before_seq=3, limit=3 -> seqs 1, 2 in ascending order, has_more=false.
	p3St, _, p3Env, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?before_seq=3&limit=3", bob.AccessToken, nil)
	assertStage6OK(t, "messages before_seq=3", p3St, p3Env, http.StatusOK)
	p3 := decodeStage6JSON[stage6MessagePageDTO](t, p3Env.Data)
	if len(p3.Items) != 2 || p3.HasMore {
		t.Fatalf("expected 2 items with has_more=false on before_seq=3, got len=%d has_more=%v", len(p3.Items), p3.HasMore)
	}
	verifySeqStrictlyIncreasing(t, p3.Items)
	if p3.Items[0].Seq != "1" || p3.Items[1].Seq != "2" {
		t.Fatalf("expected oldest page seqs [1,2], got [%s,%s]", p3.Items[0].Seq, p3.Items[1].Seq)
	}

	// Forward pagination via after_seq=5, limit=2 -> seqs 6, 7, has_more=true.
	fwdSt, _, fwdEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?after_seq=5&limit=2", bob.AccessToken, nil)
	assertStage6OK(t, "messages after_seq=5", fwdSt, fwdEnv, http.StatusOK)
	fwd := decodeStage6JSON[stage6MessagePageDTO](t, fwdEnv.Data)
	if len(fwd.Items) != 2 || !fwd.HasMore || fwd.Items[0].Seq != "6" || fwd.Items[1].Seq != "7" {
		t.Fatalf("unexpected after_seq=5 page: %+v", fwd)
	}

	// Sync endpoint strictly returns seq > after_seq.
	syncSt, _, syncEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/sync?after_seq=5&limit=10", bob.AccessToken, nil)
	assertStage6OK(t, "sync after_seq=5", syncSt, syncEnv, http.StatusOK)
	syncData := decodeStage6JSON[stage6SyncDTO](t, syncEnv.Data)
	if len(syncData.Items) != 3 || syncData.HasMore || syncData.Items[0].Seq != "6" || syncData.Items[2].Seq != "8" {
		t.Fatalf("unexpected sync after_seq=5 payload: %+v", syncData)
	}
	verifySeqStrictlyIncreasing(t, syncData.Items)

	syncEmptySt, _, syncEmptyEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/sync?after_seq=8&limit=10", bob.AccessToken, nil)
	assertStage6OK(t, "sync caught up", syncEmptySt, syncEmptyEnv, http.StatusOK)
	syncEmpty := decodeStage6JSON[stage6SyncDTO](t, syncEmptyEnv.Data)
	if len(syncEmpty.Items) != 0 || syncEmpty.HasMore {
		t.Fatalf("expected empty sync at seq=8, got %+v", syncEmpty)
	}

	// Invalid cursor parameters rejected.
	badCurSt, _, badCurEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?before_seq=not-a-num", bob.AccessToken, nil)
	assertStage6BizErr(t, "invalid before_seq cursor", badCurSt, badCurEnv, errcode.ErrInvalidSeqCursor.Code, errcode.ErrInvalidValue.Code)

	bothCurSt, _, bothCurEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?before_seq=5&after_seq=1", bob.AccessToken, nil)
	assertStage6BizErr(t, "mutually exclusive before_seq & after_seq", bothCurSt, bothCurEnv, errcode.ErrInvalidParams.Code, errcode.ErrInvalidValue.Code, errcode.ErrInvalidSeqCursor.Code)

	// 8. Conversation summary reflects latest seq/msg_id and conversation members & settings work.
	afterListSt, _, afterListEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations", bob.AccessToken, nil)
	assertStage6OK(t, "list conversations after messages", afterListSt, afterListEnv, http.StatusOK)
	afterList := decodeStage6JSON[stage6ConversationPageDTO](t, afterListEnv.Data)
	var updatedConv *stage6ConversationDTO
	for i := range afterList.Items {
		if afterList.Items[i].ID == convID {
			updatedConv = &afterList.Items[i]
			break
		}
	}
	if updatedConv == nil || updatedConv.LastSeq != "8" || updatedConv.LastMsgID == "" || updatedConv.LastMsgPreview == "" {
		t.Fatalf("expected updated conversation last_seq=8 & preview, got %+v", updatedConv)
	}

	memSt, _, memEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/members", alice.AccessToken, nil)
	assertStage6OK(t, "list single chat members", memSt, memEnv, http.StatusOK)
	var memPage struct {
		Items []struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"items"`
	}
	memPage = decodeStage6JSON[struct {
		Items []struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"items"`
	}](t, memEnv.Data)
	if len(memPage.Items) != 2 {
		t.Fatalf("expected 2 members in single chat, got %d (%s)", len(memPage.Items), string(memEnv.Data))
	}

	setSt, _, setEnv, _ := rig.doRequest(http.MethodPatch, "/api/v1/conversations/"+convID+"/settings", alice.AccessToken, map[string]bool{
		"is_pinned": true,
	})
	assertStage6OK(t, "pin conversation", setSt, setEnv, http.StatusOK)
}

func TestStage6ReceiptFramesStageBoundaryCompatibility(t *testing.T) {
	rig := newStage6Rig(t)
	expectDBAvailable(t, rig.db)

	alice := rig.registerAndLogin("s6_rcpt_a", "Alice Rcpt")
	bob := rig.registerAndLogin("s6_rcpt_b", "Bob Rcpt")
	_, convID, _ := rig.makeFriends(alice, bob)

	aliceWS := rig.dialWS(alice)
	bobWS := rig.dialWS(bob)

	// Send one message so seq >= 2 exists.
	st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": uuid.NewString(),
		"msg_type":      model.MessageTypeText,
		"content":       "receipt compatibility probe",
		"extra":         map[string]any{},
	})
	assertStage6OK(t, "send probe message", st, env, http.StatusCreated)
	msg := decodeStage6JSON[struct {
		Message stage6MessageDTO `json:"message"`
	}](t, env.Data).Message

	bobWS.awaitFrame("bob msg.push for receipt probe", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == msg.MsgID
	})

	// Stage 7 contract: msg.delivered_ack must succeed (no 18004), notify sender via msg.push(receipt=delivered),
	// and advance receiver last_delivered_seq.
	delivReqID := "rcpt-deliv-" + uuid.NewString()
	bobWS.send("msg.delivered_ack", delivReqID, map[string]any{
		"msg_id":          msg.MsgID,
		"conversation_id": convID,
		"seq":             msg.Seq,
	})
	pingDelivID := "ping-" + uuid.NewString()
	bobWS.send("ping", pingDelivID, map[string]string{"nonce": pingDelivID})
	bobWS.awaitFrame("pong without error after msg.delivered_ack", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type == "error" && f.ReqID == delivReqID {
			t.Fatalf("msg.delivered_ack failed unexpectedly (Stage 7 receipt handler missing/broken): %s", string(f.Data))
		}
		return f.Type == "pong" && f.ReqID == pingDelivID
	})

	aliceWS.awaitFrame("alice receipt delivered push", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		var rcpt struct {
			PushType         string `json:"push_type"`
			ConversationID   string `json:"conversation_id"`
			UserID           string `json:"user_id"`
			DeliveryStatus   string `json:"delivery_status"`
			LastDeliveredSeq string `json:"last_delivered_seq"`
		}
		if decodeJSONInto(f.Data, &rcpt) != nil {
			return false
		}
		return rcpt.PushType == "receipt" && rcpt.ConversationID == convID &&
			rcpt.UserID == bob.IDStr && rcpt.DeliveryStatus == model.ReceiptStatusDelivered &&
			rcpt.LastDeliveredSeq == msg.Seq
	})

	// Stage 7 contract: msg.read_ack must succeed (no 18004), clear unread_count,
	// and notify sender via msg.push(receipt=read).
	readReqID := "rcpt-read-" + uuid.NewString()
	bobWS.send("msg.read_ack", readReqID, map[string]any{
		"conversation_id": convID,
		"last_read_seq":   msg.Seq,
	})
	pingReadID := "ping-" + uuid.NewString()
	bobWS.send("ping", pingReadID, map[string]string{"nonce": pingReadID})
	bobWS.awaitFrame("pong without error after msg.read_ack", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type == "error" && f.ReqID == readReqID {
			t.Fatalf("msg.read_ack failed unexpectedly (Stage 7 receipt handler missing/broken): %s", string(f.Data))
		}
		return f.Type == "pong" && f.ReqID == pingReadID
	})

	aliceWS.awaitFrame("alice receipt read push", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		var rcpt struct {
			PushType       string `json:"push_type"`
			ConversationID string `json:"conversation_id"`
			UserID         string `json:"user_id"`
			DeliveryStatus string `json:"delivery_status"`
			LastReadSeq    string `json:"last_read_seq"`
		}
		if decodeJSONInto(f.Data, &rcpt) != nil {
			return false
		}
		return rcpt.PushType == "receipt" && rcpt.ConversationID == convID &&
			rcpt.UserID == bob.IDStr && rcpt.DeliveryStatus == model.ReceiptStatusRead &&
			rcpt.LastReadSeq == msg.Seq
	})

	detSt, _, detEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID, bob.AccessToken, nil)
	assertStage6OK(t, "bob conversation detail after read_ack", detSt, detEnv, http.StatusOK)
	det := extractConversationFromDetail(t, detEnv.Data)
	if det.LastDeliveredSeq != msg.Seq || det.LastReadSeq != msg.Seq || det.UnreadCount != 0 {
		t.Fatalf("expected bob watermarks=(%s,%s) unread=0 after receipts, got %+v", msg.Seq, msg.Seq, det)
	}
}
