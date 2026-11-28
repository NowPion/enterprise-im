package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
)

type memMessageBackend struct {
	*memChatStore
	mu           sync.Mutex
	messages     map[uint64]model.Message
	byClientKey  map[string]uint64
	byConvSeq    map[string]uint64
	receipts     map[string]model.MessageReceipt
	assets       map[string]model.Asset
	dedupe       map[string]repository.DedupeRecord
	dedupeLocks  map[string]string
	convSeqs     map[uint64]uint64
	convLocks    map[uint64]string
	createHookFn func(msg *model.Message) error
}

func newMemMessageBackend() *memMessageBackend {
	return &memMessageBackend{
		memChatStore: newMemChatStore(),
		messages:     make(map[uint64]model.Message),
		byClientKey:  make(map[string]uint64),
		byConvSeq:    make(map[string]uint64),
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

func (b *memMessageBackend) FindMessageByConvSeq(_ context.Context, convID, seq uint64) (*model.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.byConvSeq[fmt.Sprintf("%d:%d", convID, seq)]
	if !ok {
		return nil, repository.ErrMessageNotFound
	}
	m := b.messages[id]
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
	if b.createHookFn != nil {
		if err := b.createHookFn(msg); err != nil {
			return err
		}
	}
	ck := fmt.Sprintf("%d:%s", msg.SenderID, msg.ClientMsgID)
	if _, exists := b.byClientKey[ck]; exists {
		return errors.New("duplicate")
	}
	cs := fmt.Sprintf("%d:%d", msg.ConversationID, msg.Seq)
	if _, exists := b.byConvSeq[cs]; exists {
		return &mysqldriver.MySQLError{
			Number:  1062,
			Message: "Duplicate entry for key 'messages.uk_msg_conv_seq'",
		}
	}
	b.messages[msg.ID] = *msg
	b.byClientKey[ck] = msg.ID
	b.byConvSeq[cs] = msg.ID
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

type recordedStreamPublisher struct {
	mu          sync.Mutex
	messages    []StreamMessage
	events      []StreamEvent
	failMsgErr  error
	failEvtErr  error
	onPublishFn func()
}

func (p *recordedStreamPublisher) PublishMessage(_ context.Context, msg StreamMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.onPublishFn != nil {
		p.onPublishFn()
	}
	if p.failMsgErr != nil {
		return p.failMsgErr
	}
	p.messages = append(p.messages, msg)
	return nil
}

func (p *recordedStreamPublisher) PublishEvent(_ context.Context, evt StreamEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failEvtErr != nil {
		return p.failEvtErr
	}
	p.events = append(p.events, evt)
	return nil
}

func TestMessageServiceStreamPublishOrderNoDirectPushAnd14008(t *testing.T) {
	ctx := context.Background()
	b := newMemMessageBackend()
	idGen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}
	broadcaster := &testBroadcaster{}
	pub := &recordedStreamPublisher{}
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)

	b.users[10001] = model.User{ID: 10001, Username: "alice", Nickname: "Alice"}
	b.users[10002] = model.User{ID: 10002, Username: "bob", Nickname: "Bob"}
	b.friendships[[2]uint64{10001, 10002}] = model.Friendship{UserID: 10001, FriendID: 10002, Status: 1}
	b.friendships[[2]uint64{10002, 10001}] = model.Friendship{UserID: 10002, FriendID: 10001, Status: 1}
	dk := "10001:10002"
	b.convs[90001] = model.Conversation{ID: 90001, Type: model.ConversationTypeSingle, DirectKey: &dk}
	b.convMembers[[2]uint64{90001, 10001}] = model.ConversationMember{ID: 1, ConversationID: 90001, UserID: 10001, IsVisible: true, JoinedAt: now}
	b.convMembers[[2]uint64{90001, 10002}] = model.ConversationMember{ID: 2, ConversationID: 90001, UserID: 10002, IsVisible: true, JoinedAt: now}

	svc := NewMessageService(b, b, b, idGen, broadcaster, 2*time.Minute, nil)
	svc.nowFn = func() time.Time { return now }
	svc.SetStreamPublisher(pub)
	// Even if fallback flag is toggled on, StreamPublisher must prevent dual push.
	svc.SetDirectPushFallback(true)

	// 1. Verify order: MySQL commit -> beforePush (msg.ack) -> PublishMessage, and ZERO hub direct push.
	var timeline []string
	pub.onPublishFn = func() {
		// Inside PublishMessage: message must already be committed in backend store.
		if len(b.messages) == 0 {
			t.Error("expected MySQL message persisted before PublishMessage")
		}
		timeline = append(timeline, "publish")
	}
	res1, bizErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10001,
		ClientMsgID:    "s7-cmsg-1",
		MsgType:        model.MessageTypeText,
		Content:        "hello stream",
	}, func(res *SendMessageResult) {
		timeline = append(timeline, "msg.ack:"+strconv.FormatUint(res.Message.Seq, 10))
	})
	if bizErr != nil || !res1.IsNew || res1.Message.Seq != 1 {
		t.Fatalf("unexpected SendMessage result: res=%+v err=%v", res1, bizErr)
	}
	if len(timeline) != 2 || timeline[0] != "msg.ack:1" || timeline[1] != "publish" {
		t.Fatalf("expected commit -> msg.ack -> publish order, got %v", timeline)
	}
	if len(broadcaster.events) != 0 {
		t.Fatalf("expected ZERO direct hub broadcasts when StreamPublisher is set, got %+v", broadcaster.events)
	}
	if len(pub.messages) != 1 || pub.messages[0].Kind != "message" || pub.messages[0].MessageID != res1.Message.MsgID || pub.messages[0].PayloadRef == "" {
		t.Fatalf("unexpected published stream message: %+v", pub.messages)
	}

	// 2. Stream failure on SendMessage -> returns 14008, does NOT roll back DB, retry recovers idempotently.
	pub.onPublishFn = nil
	pub.failMsgErr = errors.New("redis stream down")
	_, failErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10001,
		ClientMsgID:    "s7-cmsg-14008",
		MsgType:        model.MessageTypeText,
		Content:        "persisted despite stream outage",
	}, nil)
	if failErr == nil || failErr.Code != errcode.ErrStreamUnavailable.Code {
		t.Fatalf("expected 14008 ErrStreamUnavailable, got %v", failErr)
	}
	persistedRow, findErr := b.FindMessageByClientMsgID(ctx, 10001, "s7-cmsg-14008")
	if findErr != nil || persistedRow == nil || persistedRow.Seq != 2 {
		t.Fatalf("expected committed message seq=2 preserved after 14008, got %+v err=%v", persistedRow, findErr)
	}

	// Recover stream and advance receipt to delivered before retry; retry must return same msg_id/seq and latest delivery_status.
	pub.failMsgErr = nil
	b.mu.Lock()
	r := b.receipts[fmt.Sprintf("%d:%d", persistedRow.ID, 10002)]
	r.Status = model.ReceiptStatusDelivered
	b.receipts[fmt.Sprintf("%d:%d", persistedRow.ID, 10002)] = r
	b.mu.Unlock()

	var ackAfterRetry MsgAckPayload
	retryRes, retryErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10001,
		ClientMsgID:    "s7-cmsg-14008",
		MsgType:        model.MessageTypeText,
		Content:        "persisted despite stream outage",
	}, func(res *SendMessageResult) {
		ackAfterRetry = MsgAckPayload{
			ClientMsgID:    res.Message.ClientMsgID,
			MsgID:          strconv.FormatUint(res.Message.MsgID, 10),
			Seq:            strconv.FormatUint(res.Message.Seq, 10),
			Status:         res.Message.Status,
			DeliveryStatus: *res.Message.DeliveryStatus,
		}
	})
	if retryErr != nil || retryRes.IsNew || retryRes.Message.MsgID != persistedRow.ID || retryRes.Message.Seq != 2 {
		t.Fatalf("expected idempotent recovery after 14008, got res=%+v err=%v", retryRes, retryErr)
	}
	if retryRes.Message.DeliveryStatus == nil || *retryRes.Message.DeliveryStatus != model.ReceiptStatusDelivered || ackAfterRetry.DeliveryStatus != model.ReceiptStatusDelivered {
		t.Fatalf("expected latest delivery_status=delivered on replay, got %+v ack=%+v", retryRes.Message.DeliveryStatus, ackAfterRetry)
	}
	if len(pub.messages) != 2 {
		t.Fatalf("expected retry after 14008 to publish once to stream (total 2), got %d", len(pub.messages))
	}

	// Subsequent normal retry hits im:dedupe and must NOT duplicate stream XADD.
	retry2, retry2Err := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10001,
		ClientMsgID:    "s7-cmsg-14008",
		MsgType:        model.MessageTypeText,
		Content:        "persisted despite stream outage",
	}, nil)
	if retry2Err != nil || retry2.IsNew || len(pub.messages) != 2 {
		t.Fatalf("expected dedupe hit without extra stream publish, got res=%+v err=%v pubCount=%d", retry2, retry2Err, len(pub.messages))
	}

	// 3. PersistSystemMessage publishes to im:stream:messages and surfaces 14008 without rollback.
	pub.failMsgErr = errors.New("stream temporarily unavailable")
	if _, sysFail := svc.PersistSystemMessage(ctx, 90001, "sys:s7:1", "系统提示", nil); sysFail == nil || sysFail.Code != errcode.ErrStreamUnavailable.Code {
		t.Fatalf("expected 14008 on PersistSystemMessage stream failure, got %v", sysFail)
	}
	if sysRow, _ := b.FindMessageByClientMsgID(ctx, 0, "sys:s7:1"); sysRow == nil || sysRow.Seq != 3 {
		t.Fatalf("expected system message seq=3 preserved after 14008, got %+v", sysRow)
	}
	pub.failMsgErr = nil

	// 4. RecallMessage publishes recall record to im:stream:messages + message.recalled to im:stream:events(kind=recall), zero direct push.
	var recallAcked bool
	recallRes, recallErr := svc.RecallMessage(ctx, 10001, 90001, res1.Message.MsgID, func(res *RecallMessageResult) {
		recallAcked = true
	})
	if recallErr != nil || !recallAcked || recallRes.RecallMessage.Seq != 4 {
		t.Fatalf("unexpected RecallMessage result: res=%+v err=%v acked=%v", recallRes, recallErr, recallAcked)
	}
	if len(broadcaster.events) != 0 {
		t.Fatalf("expected zero direct broadcasts on recall with StreamPublisher, got %+v", broadcaster.events)
	}
	if len(pub.messages) != 3 || pub.messages[2].MessageID != recallRes.RecallMessage.MsgID {
		t.Fatalf("expected recall message published to im:stream:messages, got %+v", pub.messages)
	}
	if len(pub.events) != 1 || pub.events[0].Kind != "recall" || pub.events[0].Event != "message.recalled" {
		t.Fatalf("expected recall event published to im:stream:events, got %+v", pub.events)
	}
	if pub.events[0].Payload["recalled_msg_id"] != strconv.FormatUint(res1.Message.MsgID, 10) ||
		pub.events[0].Payload["recall_msg_id"] != strconv.FormatUint(recallRes.RecallMessage.MsgID, 10) ||
		pub.events[0].Payload["recall_seq"] != "4" {
		t.Fatalf("unexpected recall event payload: %+v", pub.events[0].Payload)
	}

	// Recall event stream failure returns 14008 without rolling back recall.
	pub.failEvtErr = errors.New("events stream down")
	if _, r14008 := svc.RecallMessage(ctx, 10001, 90001, persistedRow.ID, nil); r14008 == nil || r14008.Code != errcode.ErrStreamUnavailable.Code {
		t.Fatalf("expected 14008 when recall event publish fails, got %v", r14008)
	}
	if reloaded, _ := b.FindMessageByID(ctx, persistedRow.ID); reloaded.Status != model.MessageStatusRecalled {
		t.Fatalf("expected recalled status preserved on 14008, got %+v", reloaded)
	}

	// 5. Transition switch check: without StreamPublisher, default directPushFallback=false does NOT push;
	//    explicitly enabling SetDirectPushFallback(true) restores legacy direct push for tests.
	svcFallback := NewMessageService(b, b, b, idGen, broadcaster, 2*time.Minute, nil)
	svcFallback.nowFn = func() time.Time { return now }
	if _, err := svcFallback.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001, SenderID: 10001, ClientMsgID: "s7-no-fallback", MsgType: model.MessageTypeText, Content: "silent",
	}, nil); err != nil {
		t.Fatalf("SendMessage without publisher failed: %v", err)
	}
	if len(broadcaster.events) != 0 {
		t.Fatalf("expected directPushFallback=false by default to suppress hub push, got %d", len(broadcaster.events))
	}
	svcFallback.SetDirectPushFallback(true)
	if _, err := svcFallback.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001, SenderID: 10001, ClientMsgID: "s7-with-fallback", MsgType: model.MessageTypeText, Content: "legacy",
	}, nil); err != nil {
		t.Fatalf("SendMessage with fallback enabled failed: %v", err)
	}
	if len(broadcaster.events) != 1 || broadcaster.events[0].frameType != "msg.push" {
		t.Fatalf("expected explicit transition fallback to broadcast msg.push, got %+v", broadcaster.events)
	}
}

func TestMessageServiceConvSeqConflictAndSyncInvariants(t *testing.T) {
	ctx := context.Background()
	b := newMemMessageBackend()
	idGen, err := snowflake.NewGenerator(2)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)

	b.users[10001] = model.User{ID: 10001, Username: "alice", Nickname: "Alice"}
	b.users[10002] = model.User{ID: 10002, Username: "bob", Nickname: "Bob"}
	b.friendships[[2]uint64{10001, 10002}] = model.Friendship{UserID: 10001, FriendID: 10002, Status: 1}
	b.friendships[[2]uint64{10002, 10001}] = model.Friendship{UserID: 10002, FriendID: 10001, Status: 1}
	dk := "10001:10002"
	b.convs[90001] = model.Conversation{ID: 90001, Type: model.ConversationTypeSingle, DirectKey: &dk}
	b.convMembers[[2]uint64{90001, 10001}] = model.ConversationMember{ID: 1, ConversationID: 90001, UserID: 10001, IsVisible: true, JoinedAt: now}
	b.convMembers[[2]uint64{90001, 10002}] = model.ConversationMember{ID: 2, ConversationID: 90001, UserID: 10002, IsVisible: true, JoinedAt: now}

	svc := NewMessageService(b, b, b, idGen, nil, 2*time.Minute, nil)
	svc.nowFn = func() time.Time { return now }
	pub := &recordedStreamPublisher{}
	svc.SetStreamPublisher(pub)

	// Seed seq=1 already persisted for (10001, "cid-convseq-same"), but simulate uk_msg_conv_seq conflict on CreateMessageTx.
	existingID, _ := idGen.NextID()
	b.mu.Lock()
	b.messages[existingID] = model.Message{
		ID:             existingID,
		ConversationID: 90001,
		Seq:            1,
		SenderID:       10001,
		ClientMsgID:    "cid-convseq-same",
		MsgType:        model.MessageTypeText,
		Content:        "same payload",
		Extra:          model.NullableJSON("{}"),
		Status:         model.MessageStatusNormal,
		CreatedAt:      now,
	}
	b.byConvSeq["90001:1"] = existingID
	b.mu.Unlock()
	// Force AllocateNextSeq to tentatively return seq=1 so uk_msg_conv_seq triggers!
	b.createHookFn = func(msg *model.Message) error {
		msg.Seq = 1
		return &mysqldriver.MySQLError{
			Number:  1062,
			Message: "Duplicate entry '90001-1' for key 'messages.uk_msg_conv_seq'",
		}
	}

	replayed, repErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10001,
		ClientMsgID:    "cid-convseq-same",
		MsgType:        model.MessageTypeText,
		Content:        "same payload",
	}, nil)
	if repErr != nil || replayed.IsNew || replayed.Message.MsgID != existingID || replayed.Message.Seq != 1 {
		t.Fatalf("expected idempotent recovery on uk_msg_conv_seq same sender/client, got res=%+v err=%v", replayed, repErr)
	}

	// Different sender/client occupying seq=1 must NOT leak existing message; returns retryable 14007.
	_, foreignErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: 90001,
		SenderID:       10002,
		ClientMsgID:    "cid-convseq-other",
		MsgType:        model.MessageTypeText,
		Content:        "other sender",
	}, nil)
	if foreignErr == nil || foreignErr.Code != errcode.ErrMessageInProgress.Code {
		t.Fatalf("expected 14007 when uk_msg_conv_seq owned by another sender/client, got %v", foreignErr)
	}

	// Verify SyncMessages strict ascending order, no duplicates, and upper bound <= conversation last_seq.
	b.createHookFn = nil
	b.mu.Lock()
	b.byClientKey["10001:cid-convseq-same"] = existingID
	b.memChatStore.mu.Lock()
	c := b.convs[90001]
	c.LastSeq = 1
	c.LastMsgID = existingID
	b.convs[90001] = c
	b.memChatStore.mu.Unlock()
	b.mu.Unlock()

	for i := 2; i <= 5; i++ {
		if _, err := svc.SendMessage(ctx, SendMessageInput{
			ConversationID: 90001,
			SenderID:       10001,
			ClientMsgID:    fmt.Sprintf("sync-cid-%d", i),
			MsgType:        model.MessageTypeText,
			Content:        fmt.Sprintf("msg-%d", i),
		}, nil); err != nil {
			t.Fatalf("send sync msg %d failed: %v", i, err)
		}
	}

	page1, err1 := svc.SyncMessages(ctx, 10002, 90001, 0, 3)
	if err1 != nil || !page1.HasMore || len(page1.Items) != 3 || page1.FromSeq != "1" || page1.ToSeq != "3" {
		t.Fatalf("unexpected sync page1: %+v err=%v", page1, err1)
	}
	page2, err2 := svc.SyncMessages(ctx, 10002, 90001, 3, 3)
	if err2 != nil || page2.HasMore || len(page2.Items) != 2 || page2.FromSeq != "4" || page2.ToSeq != "5" {
		t.Fatalf("unexpected sync page2: %+v err=%v", page2, err2)
	}
	seenSeq := make(map[uint64]bool)
	var prev uint64
	for _, it := range append(page1.Items, page2.Items...) {
		if it.Seq <= prev || seenSeq[it.Seq] || it.Seq > 5 {
			t.Fatalf("sync invariant violated at seq=%d prev=%d", it.Seq, prev)
		}
		seenSeq[it.Seq] = true
		prev = it.Seq
	}
}

type failingStreamPublisher struct {
	fail bool
	inner StreamPublisher
}

func (f *failingStreamPublisher) PublishMessage(ctx context.Context, msg StreamMessage) error {
	if f.fail {
		return errors.New("simulated stream outage")
	}
	return f.inner.PublishMessage(ctx, msg)
}

func (f *failingStreamPublisher) PublishEvent(ctx context.Context, evt StreamEvent) error {
	if f.fail {
		return errors.New("simulated stream outage")
	}
	return f.inner.PublishEvent(ctx, evt)
}

type svcStreamAdapter struct {
	pub *repository.RedisStreamPublisher
}

func (a *svcStreamAdapter) PublishMessage(ctx context.Context, msg StreamMessage) error {
	return a.pub.PublishMessage(ctx, repository.StreamMessageInput{
		Kind:           msg.Kind,
		MessageID:      msg.MessageID,
		ConversationID: msg.ConversationID,
		Seq:            msg.Seq,
		SenderID:       msg.SenderID,
		PayloadRef:     msg.PayloadRef,
		RecipientIDs:   msg.RecipientIDs,
	})
}

func (a *svcStreamAdapter) PublishEvent(ctx context.Context, evt StreamEvent) error {
	return a.pub.PublishEvent(ctx, repository.StreamEventInput{
		Kind:           evt.Kind,
		Event:          evt.Event,
		ConversationID: evt.ConversationID,
		ActorID:        evt.ActorID,
		TargetUserID:   evt.TargetUserID,
		TargetUserIDs:  evt.TargetUserIDs,
		MessageID:      evt.MessageID,
		Seq:            evt.Seq,
		Payload:        evt.Payload,
	})
}

func TestLiveMessageServiceStage7ReliabilityIntegration(t *testing.T) {
	mysqlPass := firstNonEmpty(os.Getenv("MYSQL_PASSWORD"), os.Getenv("IM_MYSQL_PASSWORD"))
	redisPass := firstNonEmpty(os.Getenv("REDIS_PASSWORD"), os.Getenv("IM_REDIS_PASSWORD"))
	if mysqlPass == "" && redisPass == "" {
		t.Skip("skipping live MessageService Stage 7 integration test: env credentials not set")
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
	db, err := repository.NewMySQL(config.MySQLConfig{
		Host:            host,
		Port:            port,
		User:            user,
		Password:        mysqlPass,
		Database:        dbName,
		Charset:         "utf8mb4",
		MaxOpenConns:    20,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
		DialTimeout:     3 * time.Second,
	}, log)
	if err != nil {
		t.Fatalf("NewMySQL failed: %v", err)
	}
	defer func() { _ = repository.CloseMySQL(db) }()

	rdb, err := repository.NewRedis(config.RedisConfig{
		Addr:         redisAddr,
		Password:     redisPass,
		DB:           testServiceRedisDB(),
		PoolSize:     20,
		MinIdleConns: 2,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRedis failed: %v", err)
	}
	defer func() { _ = repository.CloseRedis(rdb) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	suffix := time.Now().UnixNano()
	u1 := model.User{Username: fmt.Sprintf("s7_live_a_%d", suffix), PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82", Nickname: "S7Alice", Role: model.UserRoleUser, Status: model.UserStatusActive}
	u2 := model.User{Username: fmt.Sprintf("s7_live_b_%d", suffix), PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82", Nickname: "S7Bob", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.WithContext(ctx).Create(&u1).Error; err != nil {
		t.Fatalf("create u1: %v", err)
	}
	if err := db.WithContext(ctx).Create(&u2).Error; err != nil {
		t.Fatalf("create u2: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	dk := repository.DirectKey(u1.ID, u2.ID)
	conv := model.Conversation{Type: model.ConversationTypeSingle, DirectKey: &dk, CreatedAt: now, UpdatedAt: now}
	if err := db.WithContext(ctx).Create(&conv).Error; err != nil {
		t.Fatalf("create conv: %v", err)
	}
	if err := db.WithContext(ctx).Create(&[]model.Friendship{
		{UserID: u1.ID, FriendID: u2.ID, Status: model.FriendshipStatusActive, CreatedAt: now, UpdatedAt: now},
		{UserID: u2.ID, FriendID: u1.ID, Status: model.FriendshipStatusActive, CreatedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create friendships: %v", err)
	}
	if err := db.WithContext(ctx).Create(&[]model.ConversationMember{
		{ConversationID: conv.ID, UserID: u1.ID, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: conv.ID, UserID: u2.ID, IsVisible: true, JoinedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create conv members: %v", err)
	}

	testMsgsStream := fmt.Sprintf("im:stream:test:svc:messages:%d", suffix)
	testEvtsStream := fmt.Sprintf("im:stream:test:svc:events:%d", suffix)
	var dedupeKeys []string
	var streamMsgIDs []string
	var streamEvtIDs []string
	defer func() {
		cleanupCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = db.WithContext(cleanupCtx).Where("conversation_id = ?", conv.ID).Delete(&model.MessageReceipt{}).Error
		_ = db.WithContext(cleanupCtx).Where("conversation_id = ?", conv.ID).Delete(&model.Message{}).Error
		_ = db.WithContext(cleanupCtx).Where("conversation_id = ?", conv.ID).Delete(&model.ConversationMember{}).Error
		_ = db.WithContext(cleanupCtx).Where("id = ?", conv.ID).Delete(&model.Conversation{}).Error
		_ = db.WithContext(cleanupCtx).Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", u1.ID, u2.ID, u2.ID, u1.ID).Delete(&model.Friendship{}).Error
		_ = db.WithContext(cleanupCtx).Where("id IN ?", []uint64{u1.ID, u2.ID}).Delete(&model.User{}).Error
		_ = rdb.Del(cleanupCtx, fmt.Sprintf("conv:seq:%d", conv.ID), fmt.Sprintf("conv:lock:%d", conv.ID)).Err()
		if len(dedupeKeys) > 0 {
			_ = rdb.Del(cleanupCtx, dedupeKeys...).Err()
		}
		if len(streamMsgIDs) > 0 {
			_ = rdb.XDel(cleanupCtx, testMsgsStream, streamMsgIDs...).Err()
		}
		if len(streamEvtIDs) > 0 {
			_ = rdb.XDel(cleanupCtx, testEvtsStream, streamEvtIDs...).Err()
		}
		_ = rdb.Del(cleanupCtx, testMsgsStream, testEvtsStream).Err()
	}()

	convRepo := repository.NewConversationRepository(db)
	msgRepo := repository.NewMessageRepository(db, rdb)
	seqAlloc := repository.NewRedisSeqAllocator(rdb, 5*time.Second)
	idGen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	broadcaster := &testBroadcaster{}
	livePub := &svcStreamAdapter{pub: repository.NewRedisStreamPublisher(rdb, repository.WithStreamKeys(testMsgsStream, testEvtsStream))}
	switchPub := &failingStreamPublisher{inner: livePub}

	svc := NewMessageService(convRepo, msgRepo, seqAlloc, idGen, broadcaster, 2*time.Minute, log)
	svc.SetStreamPublisher(switchPub)

	// 1. Real MySQL + Redis 10x concurrent send with same client_msg_id -> 1 row, identical msg_id & seq=1, zero direct push.
	concCID := fmt.Sprintf("s7-live-conc-%d", suffix)
	dedupeKeys = append(dedupeKeys, repository.DedupeKey(u1.ID, concCID), repository.DedupeLockKey(u1.ID, concCID))
	var wg sync.WaitGroup
	resCh := make(chan *SendMessageResult, 10)
	errCh := make(chan *errcode.Error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, sErr := svc.SendMessage(ctx, SendMessageInput{
				ConversationID: conv.ID,
				SenderID:       u1.ID,
				ClientMsgID:    concCID,
				MsgType:        model.MessageTypeText,
				Content:        "live concurrent 10x",
			}, nil)
			if sErr != nil {
				errCh <- sErr
				return
			}
			resCh <- r
		}()
	}
	wg.Wait()
	close(resCh)
	close(errCh)
	for e := range errCh {
		t.Fatalf("unexpected live concurrent error: %v", e)
	}
	var firstMsgID uint64
	var newHits int
	for r := range resCh {
		if firstMsgID == 0 {
			firstMsgID = r.Message.MsgID
		}
		if r.Message.MsgID != firstMsgID || r.Message.Seq != 1 {
			t.Fatalf("live concurrent mismatch: got (%d,%d) want (%d,1)", r.Message.MsgID, r.Message.Seq, firstMsgID)
		}
		if r.IsNew {
			newHits++
		}
	}
	if newHits != 1 {
		t.Fatalf("expected exactly 1 IsNew=true across 10 concurrent live sends, got %d", newHits)
	}
	var rowCount int64
	if err := db.WithContext(ctx).Model(&model.Message{}).Where("sender_id = ? AND client_msg_id = ?", u1.ID, concCID).Count(&rowCount).Error; err != nil || rowCount != 1 {
		t.Fatalf("expected 1 MySQL row for 10x concurrent send, got %d (err=%v)", rowCount, err)
	}
	if len(broadcaster.events) != 0 {
		t.Fatalf("expected zero direct hub pushes in live production mode, got %d", len(broadcaster.events))
	}

	// Verify im:dedupe format & TTL 24h in live Redis.
	dedupeRaw, err := rdb.Get(ctx, repository.DedupeKey(u1.ID, concCID)).Result()
	wantVal := fmt.Sprintf("%d|%d|1|normal|sent", firstMsgID, conv.ID)
	if err != nil || dedupeRaw != wantVal {
		t.Fatalf("unexpected live im:dedupe value: got=%q want=%q err=%v", dedupeRaw, wantVal, err)
	}
	ttl, err := rdb.TTL(ctx, repository.DedupeKey(u1.ID, concCID)).Result()
	if err != nil || ttl <= 23*time.Hour || ttl > 24*time.Hour {
		t.Fatalf("expected ~24h TTL on im:dedupe, got %v (err=%v)", ttl, err)
	}

	// 2. Simulate receipt -> delivered in MySQL; replay returns delivery_status=delivered.
	if err := db.WithContext(ctx).Model(&model.MessageReceipt{}).
		Where("message_id = ? AND user_id = ?", firstMsgID, u2.ID).
		Update("status", model.ReceiptStatusDelivered).Error; err != nil {
		t.Fatalf("update receipt to delivered: %v", err)
	}
	repDeliv, repDelivErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: conv.ID,
		SenderID:       u1.ID,
		ClientMsgID:    concCID,
		MsgType:        model.MessageTypeText,
		Content:        "live concurrent 10x",
	}, nil)
	if repDelivErr != nil || repDeliv.Message.DeliveryStatus == nil || *repDeliv.Message.DeliveryStatus != model.ReceiptStatusDelivered {
		t.Fatalf("expected replay delivery_status=delivered, got %+v err=%v", repDeliv, repDelivErr)
	}

	// 3. Simulate 14008 on live DB -> message persisted, retry succeeds idempotently without duplicate row.
	cid14008 := fmt.Sprintf("s7-live-14008-%d", suffix)
	dedupeKeys = append(dedupeKeys, repository.DedupeKey(u1.ID, cid14008), repository.DedupeLockKey(u1.ID, cid14008))
	switchPub.fail = true
	if _, err := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: conv.ID, SenderID: u1.ID, ClientMsgID: cid14008, MsgType: model.MessageTypeText, Content: "live 14008",
	}, nil); err == nil || err.Code != errcode.ErrStreamUnavailable.Code {
		t.Fatalf("expected 14008 on live stream failure, got %v", err)
	}
	switchPub.fail = false
	r14008, r14008Err := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: conv.ID, SenderID: u1.ID, ClientMsgID: cid14008, MsgType: model.MessageTypeText, Content: "live 14008",
	}, nil)
	if r14008Err != nil || r14008.IsNew || r14008.Message.Seq != 2 {
		t.Fatalf("expected idempotent recovery at seq=2 after live 14008, got %+v err=%v", r14008, r14008Err)
	}

	// 4. Simulate dangling conv:seq=99 in live Redis -> next send calibrates from MySQL MAX(seq)=2 and assigns seq=3.
	if err := rdb.Set(ctx, fmt.Sprintf("conv:seq:%d", conv.ID), "99", 0).Err(); err != nil {
		t.Fatalf("set dangling conv:seq: %v", err)
	}
	cidCal := fmt.Sprintf("s7-live-cal-%d", suffix)
	dedupeKeys = append(dedupeKeys, repository.DedupeKey(u1.ID, cidCal), repository.DedupeLockKey(u1.ID, cidCal))
	calRes, calErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: conv.ID, SenderID: u1.ID, ClientMsgID: cidCal, MsgType: model.MessageTypeText, Content: "calibrated seq 3",
	}, nil)
	if calErr != nil || calRes.Message.Seq != 3 {
		t.Fatalf("expected calibrated seq=3 after dangling increment, got %+v err=%v", calRes, calErr)
	}

	// 5. Recall message -> im:stream:messages + im:stream:events(kind=recall), and replay reflects status=recalled.
	recRes, recErr := svc.RecallMessage(ctx, u1.ID, conv.ID, firstMsgID, nil)
	if recErr != nil || recRes.RecallMessage.Seq != 4 {
		t.Fatalf("expected live recall seq=4, got %+v err=%v", recRes, recErr)
	}
	recReplay, recReplayErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: conv.ID, SenderID: u1.ID, ClientMsgID: concCID, MsgType: model.MessageTypeText, Content: "live concurrent 10x",
	}, nil)
	if recReplayErr != nil || recReplay.Message.Status != model.MessageStatusRecalled {
		t.Fatalf("expected recalled status on live replay, got %+v err=%v", recReplay, recReplayErr)
	}

	// 6. SyncMessages verifies strict ascending seq 1..4 without duplicates.
	syncOut, syncErr := svc.SyncMessages(ctx, u2.ID, conv.ID, 0, 10)
	if syncErr != nil || len(syncOut.Items) != 4 || syncOut.FromSeq != "1" || syncOut.ToSeq != "4" {
		t.Fatalf("unexpected live sync output: %+v err=%v", syncOut, syncErr)
	}

	// 7. Verify Redis Streams entries were XADDed and collect IDs for cleanup.
	convStr := strconv.FormatUint(conv.ID, 10)
	if entries, xErr := rdb.XRevRangeN(ctx, testMsgsStream, "+", "-", 20).Result(); xErr != nil {
		t.Fatalf("XRevRangeN messages: %v", xErr)
	} else {
		var msgStreamCount int
		for _, e := range entries {
			if e.Values["conversation_id"] == convStr {
				streamMsgIDs = append(streamMsgIDs, e.ID)
				msgStreamCount++
			}
		}
		if msgStreamCount < 4 {
			t.Fatalf("expected at least 4 entries in %s for conv %d, got %d", testMsgsStream, conv.ID, msgStreamCount)
		}
	}
	if evts, xErr := rdb.XRevRangeN(ctx, testEvtsStream, "+", "-", 20).Result(); xErr != nil {
		t.Fatalf("XRevRangeN events: %v", xErr)
	} else {
		var recallEvtFound bool
		for _, e := range evts {
			if e.Values["conversation_id"] == convStr && e.Values["kind"] == "recall" {
				streamEvtIDs = append(streamEvtIDs, e.ID)
				recallEvtFound = true
			}
		}
		if !recallEvtFound {
			t.Fatalf("expected recall event in %s for conv %d", testEvtsStream, conv.ID)
		}
	}
}

func testServiceRedisDB() int {
	for _, key := range []string{"IM_REDIS_DB", "REDIS_DB"} {
		if v := os.Getenv(key); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
				return parsed
			}
		}
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
