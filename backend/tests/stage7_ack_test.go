package tests

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
)

func TestStage7FourStageAckSingleChat(t *testing.T) {
	rig := newStage7Rig(t)

	alice := rig.registerAndLogin("s7_ack_sc_a", "Alice AckSC")
	bob := rig.registerAndLogin("s7_ack_sc_b", "Bob AckSC")
	outsider := rig.registerAndLogin("s7_ack_sc_out", "Outsider AckSC")

	convIDNum, convID, _ := rig.makeFriends(alice, bob)

	aliceWS := rig.dialWS(alice)
	bobWS := rig.dialWS(bob)

	// Stage 0 -> Stage 1: Alice sends M1 over WS -> msg.ack(delivery_status=sent).
	// Receipt in MySQL is "sent"; Bob's watermarks have not moved yet.
	m1ClientID := uuid.NewString()
	m1Ack := decodeStage7Ack(t, aliceWS.sendMsgAndAwaitAck(convID, m1ClientID, model.MessageTypeText, "stage7 4-phase msg 1", map[string]any{}))
	if m1Ack.DeliveryStatus != model.ReceiptStatusSent || m1Ack.Status != model.MessageStatusNormal || m1Ack.ClientMsgID != m1ClientID {
		t.Fatalf("expected msg.ack delivery_status=sent status=normal, got %+v", m1Ack)
	}
	m1ID := mustParseUint64(t, m1Ack.MsgID)
	m1Seq := mustParseUint64(t, m1Ack.Seq)

	// Bob receives msg.push(push_type=message) over WS.
	bobWS.awaitFrame("bob receives M1 msg.push", stage7IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == m1Ack.MsgID && m.Seq == m1Ack.Seq
	})

	// Before Bob sends delivered_ack: receipt must still be "sent", delivered_at/read_at nil, watermarks 0.
	rcptBeforeDeliv := getMessageReceiptDB(t, rig, m1ID, bob.ID)
	if rcptBeforeDeliv.Status != model.ReceiptStatusSent || rcptBeforeDeliv.DeliveredAt != nil || rcptBeforeDeliv.ReadAt != nil {
		t.Fatalf("msg.ack must not mark message delivered: %+v", rcptBeforeDeliv)
	}
	bobMemBefore := getConvMemberDB(t, rig, convIDNum, bob.ID)
	if bobMemBefore.LastDeliveredSeq >= m1Seq || bobMemBefore.LastReadSeq >= m1Seq || bobMemBefore.UnreadCount < 1 {
		t.Fatalf("expected bob watermarks < %d and unread >= 1 before acks, got %+v", m1Seq, bobMemBefore)
	}

	// Sender history shows delivery_status=sent before delivered_ack.
	histSentSt, _, histSentEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?limit=20", alice.AccessToken, nil)
	assertStage6OK(t, "sender history before delivered_ack", histSentSt, histSentEnv, http.StatusOK)
	histSent := decodeStage6JSON[stage6MessagePageDTO](t, histSentEnv.Data)
	if hm, ok := findMessageInPage(histSent.Items, m1Ack.MsgID); !ok || hm.DeliveryStatus != model.ReceiptStatusSent {
		t.Fatalf("expected sender history delivery_status=sent for M1, got ok=%v msg=%+v", ok, hm)
	}

	// Stage 1 -> Stage 2 (WS): Bob sends msg.delivered_ack -> Alice gets msg.push(receipt, delivered).
	if wsErr := sendDeliveredAckWS(t, bobWS, convID, m1Ack.MsgID, m1Ack.Seq); wsErr != nil {
		t.Fatalf("WS msg.delivered_ack failed: %+v", *wsErr)
	}
	delivPush := awaitReceiptPush(t, aliceWS, convID, bob.IDStr, model.ReceiptStatusDelivered, m1Seq)
	if !isValidRFC3339Millis(delivPush.UpdatedAt) {
		t.Fatalf("receipt delivered push missing valid updated_at: %+v", delivPush)
	}

	rcptAfterDeliv := getMessageReceiptDB(t, rig, m1ID, bob.ID)
	if rcptAfterDeliv.Status != model.ReceiptStatusDelivered || rcptAfterDeliv.DeliveredAt == nil || rcptAfterDeliv.ReadAt != nil {
		t.Fatalf("expected receipt delivered with delivered_at and nil read_at, got %+v", rcptAfterDeliv)
	}
	bobMemAfterDeliv := getConvMemberDB(t, rig, convIDNum, bob.ID)
	if bobMemAfterDeliv.LastDeliveredSeq != m1Seq || bobMemAfterDeliv.LastReadSeq >= m1Seq || bobMemAfterDeliv.UnreadCount < 1 {
		t.Fatalf("expected last_delivered_seq=%d unread>=1 after delivered_ack, got %+v", m1Seq, bobMemAfterDeliv)
	}

	// GET .../receipts reflects delivered state for M1.
	rcptDetailDeliv := getMessageReceiptsHTTP(t, rig, alice, convID, m1Ack.MsgID, "")
	if rcptDetailDeliv.MsgID != m1Ack.MsgID || rcptDetailDeliv.Seq != m1Ack.Seq ||
		rcptDetailDeliv.DeliveredCount < 1 || rcptDetailDeliv.ReadCount != 0 {
		t.Fatalf("unexpected receipts detail after delivered_ack: %+v", rcptDetailDeliv)
	}
	if item, ok := findReceiptItemByUser(rcptDetailDeliv.Items, bob.IDStr); !ok ||
		item.DeliveryStatus != model.ReceiptStatusDelivered || item.DeliveredAt == nil || item.ReadAt != nil {
		t.Fatalf("expected bob item delivered in GET /receipts, got ok=%v item=%+v", ok, item)
	}

	// Stage 2 -> Stage 3 (WS): Bob sends msg.read_ack -> unread_count=0, Alice gets msg.push(receipt, read).
	if wsErr := sendReadAckWS(t, bobWS, convID, m1Ack.Seq); wsErr != nil {
		t.Fatalf("WS msg.read_ack failed: %+v", *wsErr)
	}
	readPush := awaitReceiptPush(t, aliceWS, convID, bob.IDStr, model.ReceiptStatusRead, m1Seq)
	if !isValidRFC3339Millis(readPush.UpdatedAt) {
		t.Fatalf("receipt read push missing valid updated_at: %+v", readPush)
	}

	rcptAfterRead := getMessageReceiptDB(t, rig, m1ID, bob.ID)
	if rcptAfterRead.Status != model.ReceiptStatusRead || rcptAfterRead.DeliveredAt == nil || rcptAfterRead.ReadAt == nil {
		t.Fatalf("expected receipt read with delivered_at & read_at, got %+v", rcptAfterRead)
	}
	bobMemAfterRead := getConvMemberDB(t, rig, convIDNum, bob.ID)
	if bobMemAfterRead.LastReadSeq != m1Seq || bobMemAfterRead.LastDeliveredSeq != m1Seq || bobMemAfterRead.UnreadCount != 0 {
		t.Fatalf("expected watermarks=%d and unread_count=0 after read_ack, got %+v", m1Seq, bobMemAfterRead)
	}

	histReadSt, _, histReadEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?limit=20", alice.AccessToken, nil)
	assertStage6OK(t, "sender history after read_ack", histReadSt, histReadEnv, http.StatusOK)
	histRead := decodeStage6JSON[stage6MessagePageDTO](t, histReadEnv.Data)
	if hm, ok := findMessageInPage(histRead.Items, m1Ack.MsgID); !ok || hm.DeliveryStatus != model.ReceiptStatusRead {
		t.Fatalf("expected sender history delivery_status=read for M1, got ok=%v msg=%+v", ok, hm)
	}

	rcptDetailRead := getMessageReceiptsHTTP(t, rig, alice, convID, m1Ack.MsgID, "")
	if rcptDetailRead.ReadCount < 1 || rcptDetailRead.UnreadCount != 0 {
		t.Fatalf("unexpected receipts detail after read_ack: %+v", rcptDetailRead)
	}
	if item, ok := findReceiptItemByUser(rcptDetailRead.Items, bob.IDStr); !ok ||
		item.DeliveryStatus != model.ReceiptStatusRead || item.ReadAt == nil {
		t.Fatalf("expected bob item read in GET /receipts, got ok=%v item=%+v", ok, item)
	}

	// HTTP parity: Alice sends M2 & M3 via HTTP; Bob uses POST .../delivered for M2, then POST .../read for M3.
	var m2, m3 stage6MessageDTO
	for idx, dst := range []*stage6MessageDTO{&m2, &m3} {
		st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
			"client_msg_id": uuid.NewString(),
			"msg_type":      model.MessageTypeText,
			"content":       "http receipt parity msg " + strconv.Itoa(idx+2),
			"extra":         map[string]any{},
		})
		assertStage6OK(t, "send parity message", st, env, http.StatusCreated)
		*dst = decodeStage6JSON[struct {
			Message stage6MessageDTO `json:"message"`
		}](t, env.Data).Message
	}
	m2Seq := m2.seqUint64(t)
	m3Seq := m3.seqUint64(t)

	httpDelivSt, _, httpDelivEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/delivered", bob.AccessToken, map[string]string{
		"last_delivered_seq": m2.Seq,
	})
	assertStage6OK(t, "HTTP POST /delivered", httpDelivSt, httpDelivEnv, http.StatusOK)
	delivResp := decodeStage6JSON[stage7WatermarkRespDTO](t, httpDelivEnv.Data)
	if delivResp.LastDeliveredSeq != m2.Seq {
		t.Fatalf("POST /delivered returned last_delivered_seq=%q, want %q", delivResp.LastDeliveredSeq, m2.Seq)
	}
	awaitReceiptPush(t, aliceWS, convID, bob.IDStr, model.ReceiptStatusDelivered, m2Seq)

	// M2 is delivered while M3 remains sent.
	if r2 := getMessageReceiptDB(t, rig, mustParseUint64(t, m2.MsgID), bob.ID); r2.Status != model.ReceiptStatusDelivered {
		t.Fatalf("expected M2 receipt delivered after POST /delivered, got %+v", r2)
	}
	if r3 := getMessageReceiptDB(t, rig, mustParseUint64(t, m3.MsgID), bob.ID); r3.Status != model.ReceiptStatusSent {
		t.Fatalf("expected M3 receipt still sent, got %+v", r3)
	}

	httpReadSt, _, httpReadEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/read", bob.AccessToken, map[string]string{
		"last_read_seq": m3.Seq,
	})
	assertStage6OK(t, "HTTP POST /read", httpReadSt, httpReadEnv, http.StatusOK)
	readResp := decodeStage6JSON[stage7WatermarkRespDTO](t, httpReadEnv.Data)
	if readResp.LastReadSeq != m3.Seq || (readResp.UnreadCount != nil && *readResp.UnreadCount != 0) {
		t.Fatalf("POST /read returned unexpected payload: %+v", readResp)
	}
	awaitReceiptPush(t, aliceWS, convID, bob.IDStr, model.ReceiptStatusRead, m3Seq)

	for _, mid := range []string{m2.MsgID, m3.MsgID} {
		if rr := getMessageReceiptDB(t, rig, mustParseUint64(t, mid), bob.ID); rr.Status != model.ReceiptStatusRead || rr.ReadAt == nil {
			t.Fatalf("expected M%s receipt batch-updated to read, got %+v", mid, rr)
		}
	}
	if mem := getConvMemberDB(t, rig, convIDNum, bob.ID); mem.LastReadSeq != m3Seq || mem.LastDeliveredSeq < m3Seq || mem.UnreadCount != 0 {
		t.Fatalf("expected bob watermarks >= %d and unread=0 after POST /read, got %+v", m3Seq, mem)
	}

	// Non-regression / rollback guards:
	// 1. Sending delivered_ack or read_ack for an older/duplicate seq must NOT roll back watermarks or receipt status.
	assertWSWatermarkRollbackSafe(t, "duplicate delivered_ack on read M1", sendDeliveredAckWS(t, bobWS, convID, m1Ack.MsgID, m1Ack.Seq))
	assertWSWatermarkRollbackSafe(t, "duplicate read_ack on older M2", sendReadAckWS(t, bobWS, convID, m2.Seq))

	dupDelivSt, _, dupDelivEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/delivered", bob.AccessToken, map[string]string{
		"last_delivered_seq": m1Ack.Seq,
	})
	assertWatermarkRollbackSafe(t, "HTTP duplicate smaller /delivered", dupDelivSt, dupDelivEnv)

	dupReadSt, _, dupReadEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/read", bob.AccessToken, map[string]string{
		"last_read_seq": m2.Seq,
	})
	assertWatermarkRollbackSafe(t, "HTTP duplicate smaller /read", dupReadSt, dupReadEnv)

	if r1 := getMessageReceiptDB(t, rig, m1ID, bob.ID); r1.Status != model.ReceiptStatusRead {
		t.Fatalf("duplicate delivered_ack regressed M1 status to %s", r1.Status)
	}
	if mem := getConvMemberDB(t, rig, convIDNum, bob.ID); mem.LastReadSeq != m3Seq || mem.LastDeliveredSeq < m3Seq || mem.UnreadCount != 0 {
		t.Fatalf("duplicate/smaller ack regressed watermarks: %+v", mem)
	}

	// 2. Exceeding conversation last_seq MUST be rejected with 13006 and MUST NOT mutate DB.
	futureSeq := strconv.FormatUint(m3Seq+10, 10)
	if wsErr := sendDeliveredAckWS(t, bobWS, convID, m3.MsgID, futureSeq); wsErr == nil || wsErr.Code != errcode.ErrWatermarkExceedsSeq.Code {
		t.Fatalf("expected WS 13006 for delivered_ack > last_seq, got %+v", wsErr)
	}
	if wsErr := sendReadAckWS(t, bobWS, convID, futureSeq); wsErr == nil || wsErr.Code != errcode.ErrWatermarkExceedsSeq.Code {
		t.Fatalf("expected WS 13006 for read_ack > last_seq, got %+v", wsErr)
	}

	excDelivSt, _, excDelivEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/delivered", bob.AccessToken, map[string]string{
		"last_delivered_seq": futureSeq,
	})
	assertStage6BizErr(t, "HTTP /delivered > last_seq", excDelivSt, excDelivEnv, errcode.ErrWatermarkExceedsSeq.Code)

	excReadSt, _, excReadEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/read", bob.AccessToken, map[string]string{
		"last_read_seq": futureSeq,
	})
	assertStage6BizErr(t, "HTTP /read > last_seq", excReadSt, excReadEnv, errcode.ErrWatermarkExceedsSeq.Code)

	if mem := getConvMemberDB(t, rig, convIDNum, bob.ID); mem.LastReadSeq != m3Seq || mem.LastDeliveredSeq != m3Seq {
		t.Fatalf("rejected future seq mutated DB watermarks: %+v", mem)
	}

	// 3. Outsider access to receipts / delivered / read must fail closed.
	outRcptSt, _, outRcptEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages/"+m3.MsgID+"/receipts", outsider.AccessToken, nil)
	assertStage6BizErr(t, "outsider GET /receipts", outRcptSt, outRcptEnv, errcode.ErrNotConversationMember.Code)

	outReadSt, _, outReadEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/read", outsider.AccessToken, map[string]string{"last_read_seq": m3.Seq})
	assertStage6BizErr(t, "outsider POST /read", outReadSt, outReadEnv, errcode.ErrNotConversationMember.Code)
}

func TestStage7FourStageAckGroupChat(t *testing.T) {
	rig := newStage7Rig(t)

	alice := rig.registerAndLogin("s7_ack_grp_a", "Alice AckGrp")
	bob := rig.registerAndLogin("s7_ack_grp_b", "Bob AckGrp")
	charlie := rig.registerAndLogin("s7_ack_grp_c", "Charlie AckGrp")

	_, convID := createGroupWithMembers(t, rig, alice, "Stage7 ACK Group", bob.IDStr, charlie.IDStr)
	convIDNum := mustParseUint64(t, convID)

	aliceWS := rig.dialWS(alice)
	bobWS := rig.dialWS(bob)
	charlieWS := rig.dialWS(charlie)

	// Alice sends group message -> msg.ack(delivery_status=sent); Bob & Charlie receive msg.push.
	ack := decodeStage7Ack(t, aliceWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, "group 4-phase ack message", map[string]any{}))
	if ack.DeliveryStatus != model.ReceiptStatusSent {
		t.Fatalf("expected group msg.ack delivery_status=sent, got %+v", ack)
	}
	msgSeq := mustParseUint64(t, ack.Seq)

	for _, peer := range []struct {
		name string
		ws   *stage6WSClient
	}{
		{"bob", bobWS},
		{"charlie", charlieWS},
	} {
		peer.ws.awaitFrame(peer.name+" group msg.push", stage7IOTimeout, func(f stage6WSFrame) bool {
			if f.Type != "msg.push" {
				return false
			}
			pt, m := decodeMsgPushMessage(t, f)
			return pt == "message" && m.MsgID == ack.MsgID
		})
	}

	// Bob sends WS msg.delivered_ack -> Alice receives delivered receipt push for Bob.
	if wsErr := sendDeliveredAckWS(t, bobWS, convID, ack.MsgID, ack.Seq); wsErr != nil {
		t.Fatalf("bob group WS delivered_ack failed: %+v", *wsErr)
	}
	awaitReceiptPush(t, aliceWS, convID, bob.IDStr, model.ReceiptStatusDelivered, msgSeq)

	rcpt1 := getMessageReceiptsHTTP(t, rig, alice, convID, ack.MsgID, "")
	if bItem, ok := findReceiptItemByUser(rcpt1.Items, bob.IDStr); !ok || bItem.DeliveryStatus != model.ReceiptStatusDelivered {
		t.Fatalf("expected bob delivered in group receipts, got ok=%v item=%+v", ok, bItem)
	}

	// Charlie reports delivered via HTTP POST /delivered -> Alice receives delivered receipt push for Charlie.
	chDelivSt, _, chDelivEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/delivered", charlie.AccessToken, map[string]string{
		"last_delivered_seq": ack.Seq,
	})
	assertStage6OK(t, "charlie group POST /delivered", chDelivSt, chDelivEnv, http.StatusOK)
	awaitReceiptPush(t, aliceWS, convID, charlie.IDStr, model.ReceiptStatusDelivered, msgSeq)

	rcpt2 := getMessageReceiptsHTTP(t, rig, alice, convID, ack.MsgID, "")
	if rcpt2.DeliveredCount < 2 || rcpt2.ReadCount != 0 {
		t.Fatalf("expected delivered_count>=2 and read_count=0, got %+v", rcpt2)
	}

	// Bob sends WS msg.read_ack -> Bob unread=0, Charlie still unread, filter ?status=read/unread works.
	if wsErr := sendReadAckWS(t, bobWS, convID, ack.Seq); wsErr != nil {
		t.Fatalf("bob group WS read_ack failed: %+v", *wsErr)
	}
	awaitReceiptPush(t, aliceWS, convID, bob.IDStr, model.ReceiptStatusRead, msgSeq)

	if bobMem := getConvMemberDB(t, rig, convIDNum, bob.ID); bobMem.UnreadCount != 0 || bobMem.LastReadSeq != msgSeq {
		t.Fatalf("expected bob unread=0 last_read_seq=%d, got %+v", msgSeq, bobMem)
	}

	readOnly := getMessageReceiptsHTTP(t, rig, alice, convID, ack.MsgID, "read")
	if _, ok := findReceiptItemByUser(readOnly.Items, bob.IDStr); !ok {
		t.Fatalf("expected bob in ?status=read receipts, got %+v", readOnly.Items)
	}
	if _, ok := findReceiptItemByUser(readOnly.Items, charlie.IDStr); ok {
		t.Fatalf("charlie should not be in ?status=read yet: %+v", readOnly.Items)
	}

	unreadOnly := getMessageReceiptsHTTP(t, rig, alice, convID, ack.MsgID, "unread")
	if _, ok := findReceiptItemByUser(unreadOnly.Items, charlie.IDStr); !ok {
		t.Fatalf("expected charlie in ?status=unread receipts, got %+v", unreadOnly.Items)
	}

	// Charlie reports read via HTTP POST /read -> Charlie unread=0, Alice gets read push for Charlie.
	chReadSt, _, chReadEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/read", charlie.AccessToken, map[string]string{
		"last_read_seq": ack.Seq,
	})
	assertStage6OK(t, "charlie group POST /read", chReadSt, chReadEnv, http.StatusOK)
	awaitReceiptPush(t, aliceWS, convID, charlie.IDStr, model.ReceiptStatusRead, msgSeq)

	if chMem := getConvMemberDB(t, rig, convIDNum, charlie.ID); chMem.UnreadCount != 0 || chMem.LastReadSeq != msgSeq {
		t.Fatalf("expected charlie unread=0 last_read_seq=%d, got %+v", msgSeq, chMem)
	}
	rcpt3 := getMessageReceiptsHTTP(t, rig, alice, convID, ack.MsgID, "")
	if rcpt3.ReadCount < 2 || rcpt3.UnreadCount != 0 {
		t.Fatalf("expected both peers read and unread_count=0, got %+v", rcpt3)
	}

	// Group boundary: exceeding last_seq fails with 13006 and does not alter DB.
	tooHigh := strconv.FormatUint(msgSeq+5, 10)
	excSt, _, excEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/read", bob.AccessToken, map[string]string{
		"last_read_seq": tooHigh,
	})
	assertStage6BizErr(t, "group POST /read > last_seq", excSt, excEnv, errcode.ErrWatermarkExceedsSeq.Code)
	if bobMem := getConvMemberDB(t, rig, convIDNum, bob.ID); bobMem.LastReadSeq != msgSeq {
		t.Fatalf("rejected group watermark mutated DB: %+v", bobMem)
	}
}
