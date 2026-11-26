package tests

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

func TestStage7SnowflakeSeqOrderingAndRecovery(t *testing.T) {
	rig := newStage7Rig(t)

	alice := rig.registerAndLogin("s7_seq_a", "Alice Seq")
	bob := rig.registerAndLogin("s7_seq_b", "Bob Seq")
	convIDNum, convID, sysMsgID := rig.makeFriends(alice, bob)
	_ = sysMsgID // seq=1 is the friend-accept system message

	startMs := time.Now().UnixMilli()

	// Fire 20 concurrent HTTP message sends (10 Alice + 10 Bob) into the same conversation.
	const totalConcurrent = 20
	type sendOutcome struct {
		msgID string
		seq   uint64
	}
	results := make(chan sendOutcome, totalConcurrent)
	errCh := make(chan error, totalConcurrent)

	startGate := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < totalConcurrent; i++ {
		sender := alice
		if i%2 == 1 {
			sender = bob
		}
		clientMsgID := uuid.NewString()
		content := fmt.Sprintf("concurrent-seq-msg-%02d", i)

		wg.Add(1)
		go func(u stage6User, cid, body string) {
			defer wg.Done()
			<-startGate

			for attempt := 0; attempt < 5; attempt++ {
				st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", u.AccessToken, map[string]any{
					"client_msg_id": cid,
					"msg_type":      model.MessageTypeText,
					"content":       body,
					"extra":         map[string]any{},
				})
				if env.Code == errcode.ErrMessageInProgress.Code {
					time.Sleep(30 * time.Millisecond)
					continue
				}
				if (st != http.StatusCreated && st != http.StatusOK) || env.Code != 0 {
					errCh <- fmt.Errorf("concurrent send %s failed: http=%d code=%d msg=%s", cid, st, env.Code, env.Message)
					return
				}
				msg := decodeStage6JSON[struct {
					Message stage6MessageDTO `json:"message"`
				}](t, env.Data).Message
				results <- sendOutcome{
					msgID: msg.MsgID,
					seq:   msg.seqUint64(t),
				}
				return
			}
			errCh <- fmt.Errorf("concurrent send %s exhausted 14007 retries", cid)
		}(sender, clientMsgID, content)
	}
	close(startGate)
	wg.Wait()
	close(results)
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
	endMs := time.Now().UnixMilli()

	seenMsgIDs := make(map[uint64]struct{}, totalConcurrent)
	seenSeqs := make([]uint64, 0, totalConcurrent)
	for out := range results {
		id := assertValidSnowflakeID(t, out.msgID, startMs, endMs)
		if _, dup := seenMsgIDs[id]; dup {
			t.Fatalf("duplicate snowflake msg_id %d", id)
		}
		seenMsgIDs[id] = struct{}{}
		seenSeqs = append(seenSeqs, out.seq)
	}
	if len(seenSeqs) != totalConcurrent {
		t.Fatalf("expected %d outcomes, got %d", totalConcurrent, len(seenSeqs))
	}

	slices.Sort(seenSeqs)
	// Initial friend-accept system message took seq=1, so concurrent sends must occupy [2..21] without gaps or duplicates.
	for i, s := range seenSeqs {
		want := uint64(2 + i)
		if s != want {
			t.Fatalf("expected contiguous seq %d at index %d, got %d (all=%v)", want, i, s, seenSeqs)
		}
	}

	// History & /sync must both return messages strictly ordered ascending by seq.
	histSt, _, histEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?limit=50", alice.AccessToken, nil)
	assertStage6OK(t, "history after concurrent sends", histSt, histEnv, http.StatusOK)
	hist := decodeStage6JSON[stage6MessagePageDTO](t, histEnv.Data)
	if len(hist.Items) != 21 {
		t.Fatalf("expected 21 messages in history, got %d", len(hist.Items))
	}
	verifySeqStrictlyIncreasing(t, hist.Items)
	for _, it := range hist.Items {
		assertValidSnowflakeID(t, it.MsgID, startMs, endMs)
	}

	syncSt, _, syncEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/sync?after_seq=0&limit=50", bob.AccessToken, nil)
	assertStage6OK(t, "sync after concurrent sends", syncSt, syncEnv, http.StatusOK)
	syncRes := decodeStage6JSON[stage6SyncDTO](t, syncEnv.Data)
	if len(syncRes.Items) != 21 || syncRes.FromSeq != "1" || syncRes.ToSeq != "21" {
		t.Fatalf("unexpected sync result: len=%d from=%s to=%s", len(syncRes.Items), syncRes.FromSeq, syncRes.ToSeq)
	}
	verifySeqStrictlyIncreasing(t, syncRes.Items)

	// Verify MySQL conversation.last_seq and Redis conv:seq:{id} match 21.
	var conv model.Conversation
	if err := rig.db.WithContext(t.Context()).First(&conv, convIDNum).Error; err != nil {
		t.Fatalf("query conversation %d: %v", convIDNum, err)
	}
	if conv.LastSeq != 21 {
		t.Fatalf("expected conversations.last_seq=21, got %d", conv.LastSeq)
	}
	seqKey := fmt.Sprintf("conv:seq:%d", convIDNum)
	redisSeq, err := rig.redis.Get(t.Context(), seqKey).Uint64()
	if err != nil || redisSeq != 21 {
		t.Fatalf("expected Redis %s=21, got %d (err=%v)", seqKey, redisSeq, err)
	}

	// Delete Redis conv:seq:{id} to simulate Redis restart/eviction -> next send must calibrate from MySQL MAX(seq)=21 -> seq=22.
	if err := rig.redis.Del(t.Context(), seqKey).Err(); err != nil {
		t.Fatalf("delete %s: %v", seqKey, err)
	}
	aliceWS := rig.dialWS(alice)
	recAck := decodeStage7Ack(t, aliceWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, "post-redis-evict message", map[string]any{}))
	if recAck.Seq != "22" {
		t.Fatalf("expected recovered seq=22 after deleting %s, got %q", seqKey, recAck.Seq)
	}
}

func TestStage7IdempotencyConcurrent10xAndFallback(t *testing.T) {
	rig := newStage7Rig(t)

	alice := rig.registerAndLogin("s7_idem_a", "Alice Idem")
	bob := rig.registerAndLogin("s7_idem_b", "Bob Idem")
	convIDNum, convID, _ := rig.makeFriends(alice, bob)
	bobInitial := getConvMemberDB(t, rig, convIDNum, bob.ID)

	// 1. 10x concurrent HTTP resend with the exact same client_msg_id.
	httpClientID := uuid.NewString()
	httpContent := "stage7 10x concurrent HTTP idempotent message"

	const repeatN = 10
	httpOut := make(chan stage6MessageDTO, repeatN)
	httpErrs := make(chan error, repeatN)
	httpGate := make(chan struct{})
	var httpWG sync.WaitGroup
	for i := 0; i < repeatN; i++ {
		httpWG.Add(1)
		go func() {
			defer httpWG.Done()
			<-httpGate
			for attempt := 0; attempt < 6; attempt++ {
				st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
					"client_msg_id": httpClientID,
					"msg_type":      model.MessageTypeText,
					"content":       httpContent,
					"extra":         map[string]any{},
				})
				if env.Code == errcode.ErrMessageInProgress.Code {
					time.Sleep(30 * time.Millisecond)
					continue
				}
				if (st != http.StatusCreated && st != http.StatusOK) || env.Code != 0 {
					httpErrs <- fmt.Errorf("HTTP idempotent send failed: http=%d code=%d msg=%s", st, env.Code, env.Message)
					return
				}
				msg := decodeStage6JSON[struct {
					Message stage6MessageDTO `json:"message"`
				}](t, env.Data).Message
				httpOut <- msg
				return
			}
			httpErrs <- fmt.Errorf("HTTP 10x send exhausted 14007 retries")
		}()
	}
	close(httpGate)
	httpWG.Wait()
	close(httpOut)
	close(httpErrs)
	for err := range httpErrs {
		t.Fatal(err)
	}

	var firstHTTP stage6MessageDTO
	for m := range httpOut {
		if firstHTTP.MsgID == "" {
			firstHTTP = m
		} else if m.MsgID != firstHTTP.MsgID || m.Seq != firstHTTP.Seq {
			t.Fatalf("HTTP 10x concurrent resend diverged: got (%s,%s) vs (%s,%s)", m.MsgID, m.Seq, firstHTTP.MsgID, firstHTTP.Seq)
		}
	}
	if firstHTTP.Seq != "2" {
		t.Fatalf("expected HTTP 10x message seq=2, got %q", firstHTTP.Seq)
	}
	if n, err := countMessagesByClientID(t.Context(), rig.db, alice.ID, httpClientID); err != nil || n != 1 {
		t.Fatalf("expected 1 DB row for HTTP 10x resend, got %d (err=%v)", n, err)
	}
	if mem := getConvMemberDB(t, rig, convIDNum, bob.ID); mem.UnreadCount != bobInitial.UnreadCount+1 {
		t.Fatalf("expected bob unread_count=%d (initial %d + 1) after HTTP 10x resend, got %d",
			bobInitial.UnreadCount+1, bobInitial.UnreadCount, mem.UnreadCount)
	}

	// Verify Redis im:dedupe:{sender}:{client_msg_id} key exists and parses accurately.
	dedupeVal, err := rig.redis.Get(t.Context(), repository.DedupeKey(alice.ID, httpClientID)).Result()
	if err != nil {
		t.Fatalf("expected Redis dedupe key for HTTP send: %v", err)
	}
	parsedDedupe, ok := repository.ParseDedupeValue(dedupeVal)
	if !ok || strconv.FormatUint(parsedDedupe.MsgID, 10) != firstHTTP.MsgID || parsedDedupe.Seq != 2 {
		t.Fatalf("unexpected Redis dedupe value %q (parsed=%+v ok=%v)", dedupeVal, parsedDedupe, ok)
	}

	// 2. 10x concurrent WS msg.send with a fresh client_msg_id on one WS client (single collector avoids read races).
	aliceWS := rig.dialWS(alice)
	wsClientID := uuid.NewString()
	wsContent := "stage7 10x concurrent WS idempotent message"

	pendingReqIDs := make(map[string]bool, repeatN)
	reqIDList := make([]string, 0, repeatN)
	for i := 0; i < repeatN; i++ {
		rid := fmt.Sprintf("ws-idem-%d-%s", i, uuid.NewString())
		pendingReqIDs[rid] = true
		reqIDList = append(reqIDList, rid)
	}

	wsGate := make(chan struct{})
	var wsWG sync.WaitGroup
	for _, rid := range reqIDList {
		wsWG.Add(1)
		go func(reqID string) {
			defer wsWG.Done()
			<-wsGate
			aliceWS.send("msg.send", reqID, map[string]any{
				"conversation_id": convID,
				"client_msg_id":   wsClientID,
				"msg_type":        model.MessageTypeText,
				"content":         wsContent,
				"extra":           map[string]any{},
			})
		}(rid)
	}
	close(wsGate)
	wsWG.Wait()

	var firstWSAck stage7AckPayload
	retryQueue := make([]string, 0)
	for remaining := repeatN; remaining > 0; {
		frame := aliceWS.awaitFrame("concurrent WS msg.ack or 14007", stage7IOTimeout, func(f stage6WSFrame) bool {
			return (f.Type == "msg.ack" || f.Type == "error") && pendingReqIDs[f.ReqID]
		})
		delete(pendingReqIDs, frame.ReqID)
		remaining--

		if frame.Type == "error" {
			wsErr := decodeWSError(t, frame)
			if wsErr.Code != errcode.ErrMessageInProgress.Code {
				t.Fatalf("WS 10x concurrent send returned non-14007 error: %+v", wsErr)
			}
			retryQueue = append(retryQueue, frame.ReqID)
			continue
		}
		ack := decodeStage7Ack(t, frame)
		if firstWSAck.MsgID == "" {
			firstWSAck = ack
		} else if ack.MsgID != firstWSAck.MsgID || ack.Seq != firstWSAck.Seq {
			t.Fatalf("WS 10x concurrent resend diverged: %+v vs %+v", ack, firstWSAck)
		}
	}

	for _, oldReqID := range retryQueue {
		retryAck := decodeStage7Ack(t, aliceWS.sendMsgAndAwaitAck(convID, wsClientID, model.MessageTypeText, wsContent, map[string]any{}))
		if firstWSAck.MsgID == "" {
			firstWSAck = retryAck
		} else if retryAck.MsgID != firstWSAck.MsgID || retryAck.Seq != firstWSAck.Seq {
			t.Fatalf("WS 10x retry after 14007 (%s) diverged: %+v vs %+v", oldReqID, retryAck, firstWSAck)
		}
	}

	if firstWSAck.Seq != "3" {
		t.Fatalf("expected WS 10x message seq=3 (no seq gap), got %q", firstWSAck.Seq)
	}
	if n, err := countMessagesByClientID(t.Context(), rig.db, alice.ID, wsClientID); err != nil || n != 1 {
		t.Fatalf("expected 1 DB row for WS 10x resend, got %d (err=%v)", n, err)
	}
	if mem := getConvMemberDB(t, rig, convIDNum, bob.ID); mem.UnreadCount != bobInitial.UnreadCount+2 {
		t.Fatalf("expected bob unread_count=%d (initial %d + 2) after HTTP+WS 10x resends, got %d",
			bobInitial.UnreadCount+2, bobInitial.UnreadCount, mem.UnreadCount)
	}

	// 3. Delete Redis dedupe cache -> MySQL unique constraint fallback must still replay exact msg_id/seq.
	if err := rig.redis.Del(t.Context(), repository.DedupeKey(alice.ID, wsClientID)).Err(); err != nil {
		t.Fatalf("delete WS dedupe key: %v", err)
	}
	fallbackAck := decodeStage7Ack(t, aliceWS.sendMsgAndAwaitAck(convID, wsClientID, model.MessageTypeText, wsContent, map[string]any{}))
	if fallbackAck.MsgID != firstWSAck.MsgID || fallbackAck.Seq != firstWSAck.Seq {
		t.Fatalf("MySQL dedupe fallback returned (%s,%s), want (%s,%s)", fallbackAck.MsgID, fallbackAck.Seq, firstWSAck.MsgID, firstWSAck.Seq)
	}
	if n, err := countMessagesByClientID(t.Context(), rig.db, alice.ID, wsClientID); err != nil || n != 1 {
		t.Fatalf("expected still 1 DB row after MySQL dedupe fallback, got %d (err=%v)", n, err)
	}

	// 4. Reusing same client_msg_id with different content must return 14001 on both HTTP and WS.
	confSt, _, confEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": wsClientID,
		"msg_type":      model.MessageTypeText,
		"content":       "mutated content conflict",
		"extra":         map[string]any{},
	})
	assertStage6BizErr(t, "HTTP conflicting client_msg_id", confSt, confEnv, errcode.ErrClientMsgIDConflict.Code)

	wsConf := aliceWS.sendMsgAndAwaitAck(convID, wsClientID, model.MessageTypeText, "mutated WS conflict", map[string]any{})
	if wsConf.Type != "error" || decodeWSError(t, wsConf).Code != errcode.ErrClientMsgIDConflict.Code {
		t.Fatalf("expected WS 14001 for conflicting client_msg_id, got %s %s", wsConf.Type, string(wsConf.Data))
	}
}
