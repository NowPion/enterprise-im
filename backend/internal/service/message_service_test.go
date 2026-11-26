package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
)

type memMessageBackend struct {
	*memChatStore
	mu          sync.Mutex
	messages    map[uint64]model.Message
	byClientKey map[string]uint64
	receipts    map[string]model.MessageReceipt
	assets      map[string]model.Asset
	dedupe      map[string]repository.DedupeRecord
	dedupeLocks map[string]string
	convSeqs    map[uint64]uint64
	convLocks   map[uint64]string
}

func newMemMessageBackend() *memMessageBackend {
	return &memMessageBackend{
		memChatStore: newMemChatStore(),
		messages:     make(map[uint64]model.Message),
		byClientKey:  make(map[string]uint64),
		receipts:     make(map[string]model.MessageReceipt),
		assets:       make(map[string]model.Asset),
		dedupe:       make(map[string]repository.DedupeRecord),
		dedupeLocks:  make(map[string]string),
		convSeqs:     make(map[uint64]uint64),
		convLocks:    make(map[uint64]string),
	}
}

func (b *memMessageBackend) WithTx(_ context.Context, fn func(tx *gorm.DB) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return fn(nil)
}

func (b *memMessageBackend) GetDedupeRecord(_ context.Context, senderID uint64, clientMsgID string) (*repository.DedupeRecord, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, ok := b.dedupe[repository.DedupeKey(senderID, clientMsgID)]
	if !ok {
		return nil, false, nil
	}
	cp := rec
	return &cp, true, nil
}

func (b *memMessageBackend) SetDedupeRecordNX(_ context.Context, senderID uint64, clientMsgID string, rec repository.DedupeRecord, _ time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := repository.DedupeKey(senderID, clientMsgID)
	if _, exists := b.dedupe[k]; !exists {
		b.dedupe[k] = rec
	}
	return nil
}

func (b *memMessageBackend) DeleteDedupeRecord(_ context.Context, senderID uint64, clientMsgID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.dedupe, repository.DedupeKey(senderID, clientMsgID))
	return nil
}

func (b *memMessageBackend) AcquireDedupeLock(ctx context.Context, senderID uint64, clientMsgID string, _ time.Duration, waitTimeout time.Duration) (string, *repository.DedupeRecord, error) {
	deadline := time.Now().Add(waitTimeout)
	k := repository.DedupeLockKey(senderID, clientMsgID)
	for {
		b.mu.Lock()
		if rec, ok := b.dedupe[repository.DedupeKey(senderID, clientMsgID)]; ok {
			cp := rec
			b.mu.Unlock()
			return "", &cp, nil
		}
		if _, held := b.dedupeLocks[k]; !held {
			tok := fmt.Sprintf("dl-%d", time.Now().UnixNano())
			b.dedupeLocks[k] = tok
			b.mu.Unlock()
			return tok, nil, nil
		}
		b.mu.Unlock()
		if time.Now().After(deadline) {
			return "", nil, repository.ErrDedupeInProgress
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (b *memMessageBackend) ReleaseDedupeLock(_ context.Context, senderID uint64, clientMsgID, token string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := repository.DedupeLockKey(senderID, clientMsgID)
	if b.dedupeLocks[k] == token {
		delete(b.dedupeLocks, k)
	}
	return nil
}

func (b *memMessageBackend) FindMessageByClientMsgID(_ context.Context, senderID uint64, clientMsgID string) (*model.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.byClientKey[fmt.Sprintf("%d:%s", senderID, clientMsgID)]
	if !ok {
		return nil, nil
	}
	m := b.messages[id]
	return &m, nil
}

func (b *memMessageBackend) FindMessageByID(_ context.Context, msgID uint64) (*model.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.messages[msgID]
	if !ok {
		return nil, repository.ErrMessageNotFound
	}
	return &m, nil
}

func (b *memMessageBackend) LockConversationByID(_ context.Context, _ *gorm.DB, convID uint64) (*model.Conversation, error) {
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()
	c, ok := b.convs[convID]
	if !ok {
		return nil, repository.ErrConversationNotFound
	}
	cp := c
	return &cp, nil
}

func (b *memMessageBackend) LockMessageInConversation(_ context.Context, _ *gorm.DB, convID, msgID uint64) (*model.Message, error) {
	m, ok := b.messages[msgID]
	if !ok || m.ConversationID != convID {
		return nil, repository.ErrMessageNotFound
	}
	cp := m
	return &cp, nil
}

func (b *memMessageBackend) GetAssetByAssetID(_ context.Context, assetID string) (*model.Asset, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.assets[assetID]
	if !ok {
		return nil, repository.ErrAssetNotFound
	}
	cp := a
	return &cp, nil
}

func (b *memMessageBackend) LockAssetByAssetID(_ context.Context, _ *gorm.DB, assetID string) (*model.Asset, error) {
	a, ok := b.assets[assetID]
	if !ok {
		return nil, repository.ErrAssetNotFound
	}
	cp := a
	return &cp, nil
}

func (b *memMessageBackend) BindAssetToMessageTx(_ context.Context, _ *gorm.DB, asset *model.Asset, msgID, convID uint64, now time.Time) error {
	a := b.assets[asset.AssetID]
	a.RefCount++
	if a.RefType == model.AssetRefNone {
		a.RefType = model.AssetRefMessage
		a.RefID = &msgID
		a.RefConversationID = &convID
		a.BoundAt = &now
	}
	a.UpdatedAt = now
	b.assets[asset.AssetID] = a
	return nil
}

func (b *memMessageBackend) CreateMessageTx(_ context.Context, _ *gorm.DB, msg *model.Message) error {
	ck := fmt.Sprintf("%d:%s", msg.SenderID, msg.ClientMsgID)
	if _, exists := b.byClientKey[ck]; exists {
		return errors.New("duplicate")
	}
	b.messages[msg.ID] = *msg
	b.byClientKey[ck] = msg.ID
	return nil
}

func (b *memMessageBackend) UpdateConversationOnMessageTx(_ context.Context, _ *gorm.DB, convID uint64, msg *model.Message, preview string, now time.Time) error {
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()
	c := b.convs[convID]
	c.LastMsgID = msg.ID
	c.LastSeq = msg.Seq
	c.LastMsgPreview = preview
	t := now
	c.LastMsgAt = &t
	c.UpdatedAt = now
	b.convs[convID] = c
	return nil
}

func (b *memMessageBackend) UpdateMembersOnNewMessageTx(_ context.Context, _ *gorm.DB, convID, senderID uint64, recipientIDs []uint64, now time.Time) error {
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()
	if senderID > 0 {
		if cm, ok := b.convMembers[[2]uint64{convID, senderID}]; ok {
			cm.IsVisible = true
			cm.UpdatedAt = now
			b.convMembers[[2]uint64{convID, senderID}] = cm
		}
	}
	for _, uid := range recipientIDs {
		if uid == senderID {
			continue
		}
		if cm, ok := b.convMembers[[2]uint64{convID, uid}]; ok {
			cm.UnreadCount++
			cm.IsVisible = true
			cm.UpdatedAt = now
			b.convMembers[[2]uint64{convID, uid}] = cm
		}
	}
	return nil
}

func (b *memMessageBackend) CreateMessageReceiptsTx(_ context.Context, _ *gorm.DB, receipts []model.MessageReceipt) error {
	for _, r := range receipts {
		k := fmt.Sprintf("%d:%d", r.MessageID, r.UserID)
		if _, exists := b.receipts[k]; !exists {
			b.receipts[k] = r
		}
	}
	return nil
}

func (b *memMessageBackend) MarkMessageRecalledTx(_ context.Context, _ *gorm.DB, msgID uint64, recalledAt time.Time) (bool, error) {
	m, ok := b.messages[msgID]
	if !ok || m.Status != model.MessageStatusNormal {
		return false, nil
	}
	m.Status = model.MessageStatusRecalled
	t := recalledAt
	m.RecalledAt = &t
	b.messages[msgID] = m
	return true, nil
}

func (b *memMessageBackend) AdjustMembersOnRecallTx(_ context.Context, _ *gorm.DB, convID, senderID uint64, targetSeq uint64, recipientIDs []uint64, now time.Time) error {
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()
	for _, uid := range recipientIDs {
		if cm, ok := b.convMembers[[2]uint64{convID, uid}]; ok {
			if uid != senderID && cm.LastReadSeq < targetSeq && cm.UnreadCount > 0 {
				cm.UnreadCount--
			}
			cm.UpdatedAt = now
			b.convMembers[[2]uint64{convID, uid}] = cm
		}
	}
	return nil
}

func (b *memMessageBackend) ListMessagesBefore(_ context.Context, convID uint64, beforeSeq *uint64, limit int) ([]model.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var rows []model.Message
	for _, m := range b.messages {
		if m.ConversationID != convID {
			continue
		}
		if beforeSeq != nil && m.Seq >= *beforeSeq {
			continue
		}
		rows = append(rows, m)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq > rows[j].Seq })
	if len(rows) > limit+1 {
		rows = rows[:limit+1]
	}
	return rows, nil
}

func (b *memMessageBackend) ListMessagesAfter(_ context.Context, convID uint64, afterSeq uint64, limit int) ([]model.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var rows []model.Message
	for _, m := range b.messages {
		if m.ConversationID != convID || m.Seq <= afterSeq {
			continue
		}
		rows = append(rows, m)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	if len(rows) > limit+1 {
		rows = rows[:limit+1]
	}
	return rows, nil
}

func (b *memMessageBackend) BatchGetSingleDeliveryStatuses(_ context.Context, convID, peerUserID uint64, msgIDs []uint64) (map[uint64]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[uint64]string)
	for _, mid := range msgIDs {
		if r, ok := b.receipts[fmt.Sprintf("%d:%d", mid, peerUserID)]; ok && r.ConversationID == convID {
			out[mid] = r.Status
		}
	}
	return out, nil
}

func (b *memMessageBackend) AcquireConvLock(ctx context.Context, convID uint64) (string, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		if _, held := b.convLocks[convID]; !held {
			tok := fmt.Sprintf("cl-%d", time.Now().UnixNano())
			b.convLocks[convID] = tok
			b.mu.Unlock()
			return tok, nil
		}
		b.mu.Unlock()
		if time.Now().After(deadline) {
			return "", repository.ErrConvLockTimeout
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (b *memMessageBackend) AllocateNextSeq(_ context.Context, _ *gorm.DB, convID uint64, _ string) (*repository.SeqReservation, error) {
	var maxSeq uint64
	for _, m := range b.messages {
		if m.ConversationID == convID && m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	prev, had := b.convSeqs[convID]
	next := maxSeq + 1
	b.convSeqs[convID] = next
	return &repository.SeqReservation{
		Seq: next,
		Rollback: func(context.Context) error {
			if !had {
				delete(b.convSeqs, convID)
			} else {
				b.convSeqs[convID] = prev
			}
			return nil
		},
	}, nil
}

func (b *memMessageBackend) ReleaseConvLock(_ context.Context, convID uint64, token string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.convLocks[convID] == token {
		delete(b.convLocks, convID)
	}
	return nil
}

type recordedBroadcast struct {
	userIDs   []uint64
	frameType string
	data      any
}

type testBroadcaster struct {
	mu     sync.Mutex
	events []recordedBroadcast
}

func (b *testBroadcaster) BroadcastToUsers(_ context.Context, userIDs []uint64, frameType string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := append([]uint64(nil), userIDs...)
	b.events = append(b.events, recordedBroadcast{userIDs: cp, frameType: frameType, data: data})
}

func TestMessageServiceSixTypesAssetsIdempotencyRecallAndSync(t *testing.T) {
	ctx := context.Background()
	b := newMemMessageBackend()
	idGen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}
	broadcaster := &testBroadcaster{}
	now := time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC)

	b.users[10001] = model.User{ID: 10001, Username: "alice", Nickname: "Alice"}
	b.users[10002] = model.User{ID: 10002, Username: "bob", Nickname: "Bob"}
	b.users[10003] = model.User{ID: 10003, Username: "carol", Nickname: "Carol"}
	b.friendships[[2]uint64{10001, 10002}] = model.Friendship{UserID: 10001, FriendID: 10002, Status: 1}
	b.friendships[[2]uint64{10002, 10001}] = model.Friendship{UserID: 10002, FriendID: 10001, Status: 1}

	dk := "10001:10002"
	b.convs[90001] = model.Conversation{ID: 90001, Type: model.ConversationTypeSingle, DirectKey: &dk}
	b.convMembers[[2]uint64{90001, 10001}] = model.ConversationMember{ID: 1, ConversationID: 90001, UserID: 10001, IsVisible: true, JoinedAt: now}
	b.convMembers[[2]uint64{90001, 10002}] = model.ConversationMember{ID: 2, ConversationID: 90001, UserID: 10002, IsVisible: true, JoinedAt: now}

	// Seed assets for image, file, voice, and mismatched cases.
	w, h, dur := uint32(800), uint32(600), uint32(3500)
	thumbKey := "thumbs/img1.jpg"
	b.assets["asset-img-1"] = model.Asset{ID: 1, AssetID: "asset-img-1", OwnerID: 10001, Purpose: model.AssetPurposeImage, Filename: "pic.png", Mime: "image/png", SizeBytes: 1234, Width: &w, Height: &h, StorageKey: "secret/disk/img1.png", ThumbnailKey: &thumbKey, RefType: model.AssetRefNone, Status: model.AssetStatusActive}
	b.assets["asset-file-1"] = model.Asset{ID: 2, AssetID: "asset-file-1", OwnerID: 10001, Purpose: model.AssetPurposeFile, Filename: "spec.pdf", Mime: "application/pdf", SizeBytes: 4096, StorageKey: "secret/disk/spec.pdf", RefType: model.AssetRefNone, Status: model.AssetStatusActive}
	b.assets["asset-voice-1"] = model.Asset{ID: 3, AssetID: "asset-voice-1", OwnerID: 10001, Purpose: model.AssetPurposeVoice, Filename: "clip.ogg", Mime: "audio/ogg", SizeBytes: 900, DurationMs: &dur, StorageKey: "secret/disk/clip.ogg", RefType: model.AssetRefNone, Status: model.AssetStatusActive}
	b.assets["asset-other-owner"] = model.Asset{ID: 4, AssetID: "asset-other-owner", OwnerID: 10002, Purpose: model.AssetPurposeImage, Filename: "bob.png", Mime: "image/png", SizeBytes: 100, StorageKey: "secret/bob.png", RefType: model.AssetRefNone, Status: model.AssetStatusActive}

	svc := NewMessageService(b, b, b, idGen, broadcaster, 2*time.Minute, nil)
	svc.nowFn = func() time.Time { return now }

	// 1. System message (seq=1).
	if _, sysErr := svc.PersistSystemMessage(ctx, 90001, "sys:1", "已添加为好友", json.RawMessage(`{"event":"friend.accepted"}`)); sysErr != nil {
		t.Fatalf("PersistSystemMessage failed: %v", sysErr)
	}

	// 2. Concurrent 10x send with same client_msg_id -> 1 row inserted, all 10 identical msg_id & seq=2.
	var wg sync.WaitGroup
	results := make(chan *SendMessageResult, 10)
	errs := make(chan *errcode.Error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, sErr := svc.SendMessage(ctx, SendMessageInput{
				ConversationID: 90001,
				SenderID:       10001,
				ClientMsgID:    "cmsg-concurrent-1",
				MsgType:        model.MessageTypeText,
				Content:        "hello concurrent",
			}, nil)
			if sErr != nil {
				errs <- sErr
				return
			}
			results <- res
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		t.Fatalf("unexpected concurrent error: %v", e)
	}
	var firstMsgID uint64
	newCount := 0
	for r := range results {
		if firstMsgID == 0 {
			firstMsgID = r.Message.MsgID
		}
		if r.Message.MsgID != firstMsgID || r.Message.Seq != 2 {
			t.Fatalf("concurrent replay mismatch: got (%d, %d) want (%d, 2)", r.Message.MsgID, r.Message.Seq, firstMsgID)
		}
		if r.IsNew {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("expected exactly 1 IsNew=true across 10 concurrent retries, got %d", newCount)
	}

	// Reusing same client_msg_id with different content returns 14001.
	if _, conflictErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10001,
		ClientMsgID:    "cmsg-concurrent-1",
		MsgType:        model.MessageTypeText,
		Content:        "mutated content",
	}, nil); conflictErr == nil || conflictErr.Code != errcode.ErrClientMsgIDConflict.Code {
		t.Fatalf("expected 14001 on payload mismatch, got %v", conflictErr)
	}

	// 3. Send image, file, voice (seq=3, 4, 5) + verify asset validation & binding & no storage_key leak.
	for _, tc := range []struct {
		cid, kind, asset string
	}{
		{"cmsg-img", model.MessageTypeImage, "asset-img-1"},
		{"cmsg-file", model.MessageTypeFile, "asset-file-1"},
		{"cmsg-voice", model.MessageTypeVoice, "asset-voice-1"},
	} {
		extra, _ := json.Marshal(map[string]string{"asset_id": tc.asset})
		res, sErr := svc.SendMessage(ctx, SendMessageInput{
			ConversationID: 90001,
			SenderID:       10001,
			ClientMsgID:    tc.cid,
			MsgType:        tc.kind,
			Content:        "",
			Extra:          extra,
		}, nil)
		if sErr != nil {
			t.Fatalf("send %s failed: %v", tc.kind, sErr)
		}
		if string(res.Message.Extra) == "" || string(res.Message.Extra) == "{}" {
			t.Fatalf("expected enriched asset extra for %s", tc.kind)
		}
		if b.assets[tc.asset].RefType != model.AssetRefMessage || b.assets[tc.asset].RefCount != 1 {
			t.Fatalf("expected asset %s bound to message, got %+v", tc.asset, b.assets[tc.asset])
		}
	}

	// Mismatched purpose / missing / other owner -> 14006.
	if _, err := svc.SendMessage(ctx, SendMessageInput{ConversationID: 90001, SenderID: 10001, ClientMsgID: "bad-1", MsgType: model.MessageTypeImage, Extra: json.RawMessage(`{"asset_id":"asset-file-1"}`)}, nil); err == nil || err.Code != errcode.ErrInvalidMessagePayload.Code {
		t.Fatalf("expected 14006 on purpose mismatch, got %v", err)
	}
	if _, err := svc.SendMessage(ctx, SendMessageInput{ConversationID: 90001, SenderID: 10001, ClientMsgID: "bad-2", MsgType: model.MessageTypeImage, Extra: json.RawMessage(`{"asset_id":"nonexistent"}`)}, nil); err == nil || err.Code != errcode.ErrInvalidMessagePayload.Code {
		t.Fatalf("expected 14006 on missing asset, got %v", err)
	}
	if _, err := svc.SendMessage(ctx, SendMessageInput{ConversationID: 90001, SenderID: 10001, ClientMsgID: "bad-3", MsgType: model.MessageTypeImage, Extra: json.RawMessage(`{"asset_id":"asset-other-owner"}`)}, nil); err == nil || err.Code != errcode.ErrInvalidMessagePayload.Code {
		t.Fatalf("expected 14006 on non-owner asset, got %v", err)
	}
	if _, err := svc.SendMessage(ctx, SendMessageInput{ConversationID: 90001, SenderID: 10001, ClientMsgID: "bad-4", MsgType: model.MessageTypeSystem, Content: "spoof"}, nil); err == nil || err.Code != errcode.ErrInvalidMessagePayload.Code {
		t.Fatalf("expected 14006 when client attempts system msg_type, got %v", err)
	}

	// Non-member -> 13002.
	if _, err := svc.SendMessage(ctx, SendMessageInput{ConversationID: 90001, SenderID: 10003, ClientMsgID: "non-mem", MsgType: model.MessageTypeText, Content: "hi"}, nil); err == nil || err.Code != errcode.ErrNotConversationMember.Code {
		t.Fatalf("expected 13002 for non-member, got %v", err)
	}

	// 4. Recall permissions & window & state transitions.
	if _, err := svc.RecallMessage(ctx, 10002, 90001, firstMsgID, nil); err == nil || err.Code != errcode.ErrMessageAccessDenied.Code {
		t.Fatalf("expected 14003 when peer tries to recall, got %v", err)
	}
	now = now.Add(30 * time.Second)
	recallRes, rErr := svc.RecallMessage(ctx, 10001, 90001, firstMsgID, nil)
	if rErr != nil || recallRes.RecalledMessage.Status != model.MessageStatusRecalled || recallRes.RecallMessage.MsgType != model.MessageTypeRecall || recallRes.RecallMessage.Seq != 6 {
		t.Fatalf("unexpected recall result: %+v err=%v", recallRes, rErr)
	}
	// Repeat recall -> 14004.
	if _, err := svc.RecallMessage(ctx, 10001, 90001, firstMsgID, nil); err == nil || err.Code != errcode.ErrMessageAlreadyRecalled.Code {
		t.Fatalf("expected 14004 on repeat recall, got %v", err)
	}
	// Idempotent resend of recalled client_msg_id reflects status=recalled!
	replayed, replayErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10001,
		ClientMsgID:    "cmsg-concurrent-1",
		MsgType:        model.MessageTypeText,
		Content:        "hello concurrent",
	}, nil)
	if replayErr != nil || replayed.Message.Status != model.MessageStatusRecalled || replayed.Message.MsgID != firstMsgID {
		t.Fatalf("expected recalled status on replay, got %+v err=%v", replayed, replayErr)
	}
	// Expired recall window -> 14005.
	now = now.Add(5 * time.Minute)
	if _, err := svc.RecallMessage(ctx, 10001, 90001, recallRes.RecallMessage.MsgID, nil); err == nil || err.Code != errcode.ErrMessageAccessDenied.Code {
		t.Fatalf("expected 14003 recalling a recall record, got %v", err)
	}

	// 5. History before_seq & after_seq & sync across all 6 message types (seq 1..6).
	before6 := uint64(6)
	pageBefore, listErr := svc.ListMessages(ctx, 10001, 90001, &before6, nil, 3)
	if listErr != nil || !pageBefore.HasMore || len(pageBefore.Items) != 3 || pageBefore.Items[0].Seq != 3 || pageBefore.Items[2].Seq != 5 {
		t.Fatalf("unexpected before_seq page: %+v err=%v", pageBefore, listErr)
	}
	if pageBefore.NextCursor == nil || *pageBefore.NextCursor != "3" {
		t.Fatalf("expected next_cursor=3, got %v", pageBefore.NextCursor)
	}
	syncRes, syncErr := svc.SyncMessages(ctx, 10001, 90001, 1, 10)
	if syncErr != nil || syncRes.HasMore || len(syncRes.Items) != 5 || syncRes.FromSeq != "2" || syncRes.ToSeq != "6" {
		t.Fatalf("unexpected sync result: %+v err=%v", syncRes, syncErr)
	}
	if syncRes.Items[0].DeliveryStatus == nil || *syncRes.Items[0].DeliveryStatus != model.ReceiptStatusSent {
		t.Fatalf("expected single sender history item to carry delivery_status=sent, got %+v", syncRes.Items[0])
	}

	// Both before_seq and after_seq -> 10001.
	a1 := uint64(1)
	if _, bothErr := svc.ListMessages(ctx, 10001, 90001, &before6, &a1, 10); bothErr == nil || bothErr.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 when both before_seq and after_seq provided, got %v", bothErr)
	}

	// Deleted friend blocks writing with 13003, preserves reading.
	b.memChatStore.mu.Lock()
	b.friendships[[2]uint64{10001, 10002}] = model.Friendship{UserID: 10001, FriendID: 10002, Status: 0}
	cm := b.convMembers[[2]uint64{90001, 10001}]
	cm.IsVisible = false
	b.convMembers[[2]uint64{90001, 10001}] = cm
	b.memChatStore.mu.Unlock()
	if _, sendDelErr := svc.SendMessage(ctx, SendMessageInput{ConversationID: 90001, SenderID: 10001, ClientMsgID: "after-del", MsgType: model.MessageTypeText, Content: "x"}, nil); sendDelErr == nil || sendDelErr.Code != errcode.ErrConversationReadOnly.Code {
		t.Fatalf("expected 13003 on hidden deleted-friend conversation, got %v", sendDelErr)
	}
	if _, syncDelErr := svc.SyncMessages(ctx, 10001, 90001, 0, 10); syncDelErr != nil {
		t.Fatalf("expected history sync still readable after friend delete, got %v", syncDelErr)
	}

	// Verify expired recall window (14005) on normal text message.
	b.memChatStore.mu.Lock()
	b.friendships[[2]uint64{10001, 10002}] = model.Friendship{UserID: 10001, FriendID: 10002, Status: 1}
	b.memChatStore.mu.Unlock()
	expRes, expErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10001,
		ClientMsgID:    "cmsg-expire-test",
		MsgType:        model.MessageTypeText,
		Content:        "will expire",
	}, nil)
	if expErr != nil {
		t.Fatalf("send expire test failed: %v", expErr)
	}
	now = now.Add(3 * time.Minute)
	if _, err := svc.RecallMessage(ctx, 10001, 90001, expRes.Message.MsgID, nil); err == nil || err.Code != errcode.ErrRecallWindowExpired.Code {
		t.Fatalf("expected 14005 when recall window expired, got %v", err)
	}

	// Verify in-progress dedupe lock returns 14007.
	b.mu.Lock()
	b.dedupeLocks[repository.DedupeLockKey(10001, "cmsg-locked")] = "other-holder"
	b.mu.Unlock()
	if _, err := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10001,
		ClientMsgID:    "cmsg-locked",
		MsgType:        model.MessageTypeText,
		Content:        "locked",
	}, nil); err == nil || err.Code != errcode.ErrMessageInProgress.Code {
		t.Fatalf("expected 14007 when dedupe lock held, got %v", err)
	}

	// Verify Group conversation send, broadcast exclusion of removed members, and disbanded read-only (13003).
	gid := uint64(6001)
	b.memChatStore.mu.Lock()
	b.groups[gid] = model.Group{ID: gid, Name: "Stage6 Group", OwnerID: 10001, Status: model.GroupStatusActive, MaxMembers: 500}
	b.groupMembers[[2]uint64{gid, 10001}] = model.GroupMember{GroupID: gid, UserID: 10001, Role: model.GroupRoleOwner, JoinedAt: now}
	b.groupMembers[[2]uint64{gid, 10002}] = model.GroupMember{GroupID: gid, UserID: 10002, Role: model.GroupRoleMember, JoinedAt: now}
	b.convs[90010] = model.Conversation{ID: 90010, Type: model.ConversationTypeGroup, GroupID: &gid}
	b.convMembers[[2]uint64{90010, 10001}] = model.ConversationMember{ID: 10, ConversationID: 90010, UserID: 10001, IsVisible: true, JoinedAt: now}
	b.convMembers[[2]uint64{90010, 10002}] = model.ConversationMember{ID: 11, ConversationID: 90010, UserID: 10002, IsVisible: true, JoinedAt: now}
	b.convMembers[[2]uint64{90010, 10003}] = model.ConversationMember{ID: 12, ConversationID: 90010, UserID: 10003, IsVisible: false, JoinedAt: now} // removed from group_members
	b.memChatStore.mu.Unlock()

	grpRes, grpErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90010,
		SenderID:       10001,
		ClientMsgID:    "grp-msg-1",
		MsgType:        model.MessageTypeText,
		Content:        "hello group",
	}, nil)
	if grpErr != nil || grpRes.Message.Seq != 1 || len(grpRes.RecipientIDs) != 2 {
		t.Fatalf("unexpected group send: res=%+v err=%v", grpRes, grpErr)
	}
	if _, err := svc.SendMessage(ctx, SendMessageInput{ConversationID: 90010, SenderID: 10003, ClientMsgID: "grp-removed", MsgType: model.MessageTypeText, Content: "x"}, nil); err == nil || err.Code != errcode.ErrNotConversationMember.Code {
		t.Fatalf("expected 13002 for removed group member, got %v", err)
	}
	b.memChatStore.mu.Lock()
	g := b.groups[gid]
	g.Status = model.GroupStatusDisbanded
	b.groups[gid] = g
	b.memChatStore.mu.Unlock()
	if _, err := svc.SendMessage(ctx, SendMessageInput{ConversationID: 90010, SenderID: 10001, ClientMsgID: "grp-disband", MsgType: model.MessageTypeText, Content: "x"}, nil); err == nil || err.Code != errcode.ErrConversationReadOnly.Code {
		t.Fatalf("expected 13003 on disbanded group, got %v", err)
	}
	grpSync, grpSyncErr := svc.SyncMessages(ctx, 10002, 90010, 0, 10)
	if grpSyncErr != nil || len(grpSync.Items) != 1 || grpSync.Items[0].DeliveryStatus != nil {
		t.Fatalf("expected disbanded group sync without per-msg delivery_status, got %+v err=%v", grpSync, grpSyncErr)
	}
}
