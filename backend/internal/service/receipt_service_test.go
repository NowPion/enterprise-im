package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
)

type memReceiptBackend struct {
	*memMessageBackend
}

func newMemReceiptBackend() *memReceiptBackend {
	return &memReceiptBackend{memMessageBackend: newMemMessageBackend()}
}

func (b *memReceiptBackend) LockConversationByIDTx(_ context.Context, _ *gorm.DB, convID uint64) (*model.Conversation, error) {
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()
	c, ok := b.convs[convID]
	if !ok {
		return nil, repository.ErrConversationNotFound
	}
	cp := c
	return &cp, nil
}

func (b *memReceiptBackend) HasGroupMemberTx(_ context.Context, _ *gorm.DB, groupID, userID uint64) (bool, error) {
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()
	_, ok := b.groupMembers[[2]uint64{groupID, userID}]
	return ok, nil
}

func (b *memReceiptBackend) LockConversationMemberTx(_ context.Context, _ *gorm.DB, convID, userID uint64) (*model.ConversationMember, error) {
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()
	cm, ok := b.convMembers[[2]uint64{convID, userID}]
	if !ok {
		return nil, repository.ErrNotConversationMember
	}
	cp := cm
	return &cp, nil
}

func (b *memReceiptBackend) GetMessageInConversation(_ context.Context, convID, msgID uint64) (*model.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.getMessageInConversationLocked(convID, msgID)
}

func (b *memReceiptBackend) GetMessageInConversationTx(_ context.Context, _ *gorm.DB, convID, msgID uint64) (*model.Message, error) {
	return b.getMessageInConversationLocked(convID, msgID)
}

func (b *memReceiptBackend) getMessageInConversationLocked(convID, msgID uint64) (*model.Message, error) {
	m, ok := b.messages[msgID]
	if !ok || m.ConversationID != convID {
		return nil, repository.ErrMessageNotFound
	}
	cp := m
	return &cp, nil
}

func (b *memReceiptBackend) LockMessageReceiptTx(_ context.Context, _ *gorm.DB, msgID, userID uint64) (*model.MessageReceipt, error) {
	k := fmt.Sprintf("%d:%d", msgID, userID)
	rec, ok := b.receipts[k]
	if !ok {
		return nil, nil
	}
	cp := rec
	return &cp, nil
}

func (b *memReceiptBackend) MarkSingleReceiptDeliveredTx(_ context.Context, _ *gorm.DB, receiptID uint64, now time.Time) (bool, error) {
	ts := now.UTC().Truncate(time.Millisecond)
	for k, rec := range b.receipts {
		if rec.ID == receiptID && rec.Status == model.ReceiptStatusSent {
			rec.Status = model.ReceiptStatusDelivered
			if rec.DeliveredAt == nil {
				rec.DeliveredAt = &ts
			}
			rec.UpdatedAt = ts
			b.receipts[k] = rec
			return true, nil
		}
	}
	return false, nil
}

func (b *memReceiptBackend) MarkReceiptsDeliveredUpToSeqTx(_ context.Context, _ *gorm.DB, convID, userID, targetSeq uint64, now time.Time) (int64, error) {
	ts := now.UTC().Truncate(time.Millisecond)
	var count int64
	for k, rec := range b.receipts {
		if rec.ConversationID == convID && rec.UserID == userID && rec.Seq <= targetSeq && rec.Status == model.ReceiptStatusSent {
			rec.Status = model.ReceiptStatusDelivered
			if rec.DeliveredAt == nil {
				rec.DeliveredAt = &ts
			}
			rec.UpdatedAt = ts
			b.receipts[k] = rec
			count++
		}
	}
	return count, nil
}

func (b *memReceiptBackend) MarkReceiptsReadUpToSeqTx(_ context.Context, _ *gorm.DB, convID, userID, targetSeq uint64, now time.Time) (int64, error) {
	ts := now.UTC().Truncate(time.Millisecond)
	var count int64
	for k, rec := range b.receipts {
		if rec.ConversationID == convID && rec.UserID == userID && rec.Seq <= targetSeq &&
			(rec.Status == model.ReceiptStatusSent || rec.Status == model.ReceiptStatusDelivered) {
			rec.Status = model.ReceiptStatusRead
			if rec.DeliveredAt == nil {
				rec.DeliveredAt = &ts
			}
			if rec.ReadAt == nil {
				rec.ReadAt = &ts
			}
			rec.UpdatedAt = ts
			b.receipts[k] = rec
			count++
		}
	}
	return count, nil
}

func (b *memReceiptBackend) UpdateMemberDeliveredWatermarkTx(_ context.Context, _ *gorm.DB, memberID, lastDeliveredSeq uint64, now time.Time) error {
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()
	ts := now.UTC().Truncate(time.Millisecond)
	for k, cm := range b.convMembers {
		if cm.ID == memberID {
			cm.LastDeliveredSeq = lastDeliveredSeq
			cm.UpdatedAt = ts
			b.convMembers[k] = cm
			return nil
		}
	}
	return repository.ErrNotConversationMember
}

func (b *memReceiptBackend) UpdateMemberReadWatermarkTx(_ context.Context, _ *gorm.DB, memberID, lastReadSeq, lastDeliveredSeq uint64, now time.Time) error {
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()
	ts := now.UTC().Truncate(time.Millisecond)
	for k, cm := range b.convMembers {
		if cm.ID == memberID {
			cm.LastReadSeq = lastReadSeq
			cm.LastDeliveredSeq = lastDeliveredSeq
			cm.UnreadCount = 0
			cm.UpdatedAt = ts
			b.convMembers[k] = cm
			return nil
		}
	}
	return repository.ErrNotConversationMember
}

func (b *memReceiptBackend) ListDistinctSendersInSeqRangeTx(_ context.Context, _ *gorm.DB, convID, fromSeqExclusive, toSeqInclusive, excludeUserID uint64) ([]uint64, error) {
	seen := make(map[uint64]struct{})
	var out []uint64
	for _, m := range b.messages {
		if m.ConversationID == convID && m.Seq > fromSeqExclusive && m.Seq <= toSeqInclusive && m.SenderID > 0 && m.SenderID != excludeUserID {
			if _, ok := seen[m.SenderID]; !ok {
				seen[m.SenderID] = struct{}{}
				out = append(out, m.SenderID)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (b *memReceiptBackend) QueryMessageReceiptDetails(
	_ context.Context,
	conv *model.Conversation,
	msg *model.Message,
	statusFilter string,
	cursorID uint64,
	limit int,
) (repository.ReceiptSummaryCounts, []repository.ReceiptMemberRow, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.memChatStore.mu.Lock()
	defer b.memChatStore.mu.Unlock()

	var summary repository.ReceiptSummaryCounts
	var allRows []repository.ReceiptMemberRow

	for _, cm := range b.convMembers {
		if cm.ConversationID != conv.ID {
			continue
		}
		if msg.SenderID > 0 && cm.UserID == msg.SenderID {
			continue
		}

		var rolePtr *string
		var deliveryStatus string
		var deliveredAt, readAt *time.Time

		if conv.Type == model.ConversationTypeGroup {
			if conv.GroupID == nil {
				continue
			}
			gm, ok := b.groupMembers[[2]uint64{*conv.GroupID, cm.UserID}]
			if !ok {
				continue
			}
			r := gm.Role
			rolePtr = &r
			deliveryStatus = deriveWatermarkStatus(cm.LastDeliveredSeq, cm.LastReadSeq, msg.Seq)
		} else {
			if rec, ok := b.receipts[fmt.Sprintf("%d:%d", msg.ID, cm.UserID)]; ok {
				deliveryStatus = rec.Status
				deliveredAt = rec.DeliveredAt
				readAt = rec.ReadAt
			} else {
				deliveryStatus = deriveWatermarkStatus(cm.LastDeliveredSeq, cm.LastReadSeq, msg.Seq)
			}
		}

		summary.TotalMembers++
		if deliveryStatus == model.ReceiptStatusDelivered || deliveryStatus == model.ReceiptStatusRead {
			summary.DeliveredCount++
		}
		if deliveryStatus == model.ReceiptStatusRead {
			summary.ReadCount++
		} else {
			summary.UnreadCount++
		}

		matchesFilter := true
		switch statusFilter {
		case repository.ReceiptFilterRead:
			matchesFilter = deliveryStatus == model.ReceiptStatusRead
		case repository.ReceiptFilterUnread:
			matchesFilter = deliveryStatus != model.ReceiptStatusRead
		case repository.ReceiptFilterDelivered:
			matchesFilter = deliveryStatus == model.ReceiptStatusDelivered || deliveryStatus == model.ReceiptStatusRead
		case repository.ReceiptFilterPending:
			matchesFilter = deliveryStatus == model.ReceiptStatusSent
		}
		if !matchesFilter {
			continue
		}
		if cursorID > 0 && cm.ID >= cursorID {
			continue
		}

		allRows = append(allRows, repository.ReceiptMemberRow{
			CMID:             cm.ID,
			UserID:           cm.UserID,
			Role:             rolePtr,
			DeliveryStatus:   deliveryStatus,
			LastDeliveredSeq: cm.LastDeliveredSeq,
			LastReadSeq:      cm.LastReadSeq,
			DeliveredAt:      deliveredAt,
			ReadAt:           readAt,
		})
	}

	sort.Slice(allRows, func(i, j int) bool { return allRows[i].CMID > allRows[j].CMID })
	if len(allRows) > limit+1 {
		allRows = allRows[:limit+1]
	}
	return summary, allRows, nil
}

type spyReceiptPublisher struct {
	mu     sync.Mutex
	events []ReceiptEvent
	err    error
}

func (p *spyReceiptPublisher) PublishEvent(_ context.Context, event ReceiptEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, event)
	return nil
}

func (p *spyReceiptPublisher) snapshot() []ReceiptEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ReceiptEvent(nil), p.events...)
}

func seedReceiptTestData(b *memReceiptBackend) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for i, uid := range []uint64{10001, 10002, 10003, 10004} {
		b.users[uid] = model.User{
			ID:        uid,
			Username:  fmt.Sprintf("user_%d", uid),
			Nickname:  fmt.Sprintf("User %d", i+1),
			Role:      model.UserRoleUser,
			Status:    model.UserStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}
	b.online[10002] = true

	// Single conversation 90001 between 10001 & 10002 with 3 messages (seq 1..3).
	dk := "10001:10002"
	b.convs[90001] = model.Conversation{ID: 90001, Type: model.ConversationTypeSingle, DirectKey: &dk, LastMsgID: 1003, LastSeq: 3}
	b.convMembers[[2]uint64{90001, 10001}] = model.ConversationMember{ID: 1, ConversationID: 90001, UserID: 10001, IsVisible: true, JoinedAt: now, UpdatedAt: now}
	b.convMembers[[2]uint64{90001, 10002}] = model.ConversationMember{ID: 2, ConversationID: 90001, UserID: 10002, UnreadCount: 3, IsVisible: true, JoinedAt: now, UpdatedAt: now}

	for seq := uint64(1); seq <= 3; seq++ {
		mid := 1000 + seq
		b.messages[mid] = model.Message{
			ID:             mid,
			ConversationID: 90001,
			Seq:            seq,
			SenderID:       10001,
			ClientMsgID:    fmt.Sprintf("c-single-%d", seq),
			MsgType:        model.MessageTypeText,
			Content:        fmt.Sprintf("msg %d", seq),
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		b.receipts[fmt.Sprintf("%d:%d", mid, 10002)] = model.MessageReceipt{
			ID:             seq,
			MessageID:      mid,
			ConversationID: 90001,
			Seq:            seq,
			UserID:         10002,
			Status:         model.ReceiptStatusSent,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
	}

	// Group conversation 90002 (group 501) with owner=10001, admin=10002, member=10003, removed=10004.
	gid := uint64(501)
	b.groups[gid] = model.Group{ID: gid, Name: "Test Group", OwnerID: 10001, Status: model.GroupStatusActive, MaxMembers: 500}
	b.groupMembers[[2]uint64{gid, 10001}] = model.GroupMember{ID: 1, GroupID: gid, UserID: 10001, Role: model.GroupRoleOwner, JoinedAt: now}
	b.groupMembers[[2]uint64{gid, 10002}] = model.GroupMember{ID: 2, GroupID: gid, UserID: 10002, Role: model.GroupRoleAdmin, JoinedAt: now}
	b.groupMembers[[2]uint64{gid, 10003}] = model.GroupMember{ID: 3, GroupID: gid, UserID: 10003, Role: model.GroupRoleMember, JoinedAt: now}
	b.convs[90002] = model.Conversation{ID: 90002, Type: model.ConversationTypeGroup, GroupID: &gid, LastMsgID: 2002, LastSeq: 2}
	b.convMembers[[2]uint64{90002, 10001}] = model.ConversationMember{ID: 10, ConversationID: 90002, UserID: 10001, LastDeliveredSeq: 1, LastReadSeq: 1, IsVisible: true, JoinedAt: now, UpdatedAt: now}
	b.convMembers[[2]uint64{90002, 10002}] = model.ConversationMember{ID: 11, ConversationID: 90002, UserID: 10002, UnreadCount: 2, IsVisible: true, JoinedAt: now, UpdatedAt: now}
	b.convMembers[[2]uint64{90002, 10003}] = model.ConversationMember{ID: 12, ConversationID: 90002, UserID: 10003, UnreadCount: 2, IsVisible: true, JoinedAt: now, UpdatedAt: now}
	// 10004 was removed: conversation_members retained hidden, NO group_members row.
	b.convMembers[[2]uint64{90002, 10004}] = model.ConversationMember{ID: 13, ConversationID: 90002, UserID: 10004, IsVisible: false, JoinedAt: now, UpdatedAt: now}

	b.messages[2001] = model.Message{ID: 2001, ConversationID: 90002, Seq: 1, SenderID: 0, ClientMsgID: "sys-grp-1", MsgType: model.MessageTypeSystem, Content: "created", Status: model.MessageStatusNormal, CreatedAt: now}
	b.messages[2002] = model.Message{ID: 2002, ConversationID: 90002, Seq: 2, SenderID: 10001, ClientMsgID: "c-grp-2", MsgType: model.MessageTypeText, Content: "hello group", Status: model.MessageStatusNormal, CreatedAt: now}
}

func TestReceiptServiceSingleChatStateMachineAndIdempotency(t *testing.T) {
	b := newMemReceiptBackend()
	seedReceiptTestData(b)
	pub := &spyReceiptPublisher{}
	svc := NewReceiptService(b, b, pub, nil)

	curTime := time.Date(2026, 11, 26, 12, 5, 0, 0, time.UTC)
	svc.nowFn = func() time.Time { return curTime }

	ctx := context.Background()

	// 1. Out-of-order delivered_ack on seq=3 advances last_delivered_seq to 3, marks msg 1003 delivered.
	ack3, bizErr := svc.AckDeliveredWS(ctx, 10002, 90001, 1003, 3)
	if bizErr != nil || !ack3.Changed || !ack3.EventPublished || ack3.DeliveryStatus != model.ReceiptStatusDelivered || ack3.LastDeliveredSeq != 3 {
		t.Fatalf("ack3 failed: res=%+v err=%v", ack3, bizErr)
	}
	if b.receipts["1003:10002"].Status != model.ReceiptStatusDelivered {
		t.Fatalf("expected receipt 1003 delivered, got %s", b.receipts["1003:10002"].Status)
	}

	// 2. Late delivered_ack on seq=2 keeps last_delivered_seq=3 (no backward move!) and still marks msg 1002 delivered.
	curTime = curTime.Add(time.Second)
	ack2, bizErr := svc.AckDeliveredWS(ctx, 10002, 90001, 1002, 2)
	if bizErr != nil || !ack2.Changed || !ack2.EventPublished || ack2.LastDeliveredSeq != 3 || ack2.DeliveryStatus != model.ReceiptStatusDelivered {
		t.Fatalf("ack2 failed: res=%+v err=%v", ack2, bizErr)
	}

	// 3. Duplicate delivered_ack on seq=2 is idempotent and does NOT emit duplicate event.
	beforeEvents := len(pub.snapshot())
	dupAck, bizErr := svc.AckDeliveredWS(ctx, 10002, 90001, 1002, 2)
	if bizErr != nil || dupAck.Changed || dupAck.LastDeliveredSeq != 3 {
		t.Fatalf("dupAck unexpected: res=%+v err=%v", dupAck, bizErr)
	}
	if len(pub.snapshot()) != beforeEvents {
		t.Fatalf("duplicate delivered_ack must not emit duplicate event")
	}

	// 4. MarkRead up to seq=2 sets last_read_seq=2, keeps last_delivered_seq=3, clears unread_count=0,
	//    marks msg 1001 & 1002 read (preserving delivered_at on 1002!), leaves 1003 delivered.
	curTime = curTime.Add(2 * time.Second)
	readRes, bizErr := svc.MarkRead(ctx, 10002, 90001, 2)
	if bizErr != nil || !readRes.Changed || !readRes.EventPublished || readRes.LastReadSeq != 2 || readRes.LastDeliveredSeq != 3 || readRes.UnreadCount != 0 {
		t.Fatalf("markRead failed: res=%+v err=%v", readRes, bizErr)
	}
	if b.receipts["1001:10002"].Status != model.ReceiptStatusRead || b.receipts["1002:10002"].Status != model.ReceiptStatusRead || b.receipts["1003:10002"].Status != model.ReceiptStatusDelivered {
		t.Fatalf("unexpected receipt statuses after read(2): 1001=%s 1002=%s 1003=%s",
			b.receipts["1001:10002"].Status, b.receipts["1002:10002"].Status, b.receipts["1003:10002"].Status)
	}
	if b.receipts["1002:10002"].DeliveredAt == nil || !b.receipts["1002:10002"].DeliveredAt.Equal(time.Date(2026, 11, 26, 12, 5, 1, 0, time.UTC)) {
		t.Fatalf("expected original delivered_at preserved on 1002, got %v", b.receipts["1002:10002"].DeliveredAt)
	}

	// 5. Late delivered_ack for already-read msg 1002 NEVER downgrades read -> delivered.
	lateAck, bizErr := svc.AckDeliveredWS(ctx, 10002, 90001, 1002, 2)
	if bizErr != nil || lateAck.Changed || lateAck.DeliveryStatus != model.ReceiptStatusRead || b.receipts["1002:10002"].Status != model.ReceiptStatusRead {
		t.Fatalf("late delivered_ack downgraded or failed: res=%+v err=%v", lateAck, bizErr)
	}

	// 6. Smaller/duplicate read_ack does NOT roll back and keeps current unread_count if new unread arrived.
	cm := b.convMembers[[2]uint64{90001, 10002}]
	cm.UnreadCount = 1
	b.convMembers[[2]uint64{90001, 10002}] = cm
	beforeEvents = len(pub.snapshot())
	smallerRead, bizErr := svc.AckReadWS(ctx, 10002, 90001, 1)
	if bizErr != nil || smallerRead.Changed || smallerRead.LastReadSeq != 2 || smallerRead.UnreadCount != 1 {
		t.Fatalf("smallerRead unexpected: res=%+v err=%v", smallerRead, bizErr)
	}
	if len(pub.snapshot()) != beforeEvents {
		t.Fatalf("smaller read_ack must not emit duplicate event")
	}

	// 7. Watermark > conv.LastSeq returns 13006 and leaves DB untouched.
	if _, err := svc.MarkRead(ctx, 10002, 90001, 99); err == nil || err.Code != errcode.ErrWatermarkExceedsSeq.Code {
		t.Fatalf("expected 13006 for read > last_seq, got %v", err)
	}
	if _, err := svc.MarkDelivered(ctx, 10002, 90001, 99); err == nil || err.Code != errcode.ErrWatermarkExceedsSeq.Code {
		t.Fatalf("expected 13006 for delivered > last_seq, got %v", err)
	}
	if _, err := svc.AckDeliveredWS(ctx, 10002, 90001, 1003, 99); err == nil || err.Code != errcode.ErrWatermarkExceedsSeq.Code {
		t.Fatalf("expected 13006 for ws delivered > last_seq, got %v", err)
	}

	// 8. Seq mismatch with message (<= last_seq) returns 18004; wrong msg/conv returns 14002.
	if _, err := svc.AckDeliveredWS(ctx, 10002, 90001, 1003, 2); err == nil || err.Code != errcode.ErrWSStateConflict.Code {
		t.Fatalf("expected 18004 for seq mismatch, got %v", err)
	}
	if _, err := svc.AckDeliveredWS(ctx, 10002, 90001, 999999, 1); err == nil || err.Code != errcode.ErrMessageNotFound.Code {
		t.Fatalf("expected 14002 for missing message, got %v", err)
	}

	// 9. Verify single-chat receipt detail query.
	detail, bizErr := svc.ListMessageReceipts(ctx, 10001, 90001, 1002, "", "", 20)
	if bizErr != nil || detail.TotalMembers != 1 || detail.DeliveredCount != 1 || detail.ReadCount != 1 || detail.UnreadCount != 0 || len(detail.Items) != 1 {
		t.Fatalf("unexpected single receipt detail: res=%+v err=%v", detail, bizErr)
	}
	if detail.Items[0].Role != nil || detail.Items[0].DeliveryStatus != model.ReceiptStatusRead || detail.Items[0].DeliveredAt == nil || detail.Items[0].ReadAt == nil || !detail.Items[0].User.Online {
		t.Fatalf("unexpected single receipt item: %+v", detail.Items[0])
	}
}

func TestReceiptServiceGroupPaginationDisbandedAndUnfriendedPermissions(t *testing.T) {
	b := newMemReceiptBackend()
	seedReceiptTestData(b)
	pub := &spyReceiptPublisher{}
	svc := NewReceiptService(b, b, pub, nil)
	ctx := context.Background()

	// 1. Removed user 10004 must be denied with 13002 across all receipt operations.
	if _, err := svc.AckDeliveredWS(ctx, 10004, 90002, 2002, 2); err == nil || err.Code != errcode.ErrNotConversationMember.Code {
		t.Fatalf("expected 13002 for removed member delivered_ack, got %v", err)
	}
	if _, err := svc.MarkDelivered(ctx, 10004, 90002, 2); err == nil || err.Code != errcode.ErrNotConversationMember.Code {
		t.Fatalf("expected 13002 for removed member POST delivered, got %v", err)
	}
	if _, err := svc.MarkRead(ctx, 10004, 90002, 2); err == nil || err.Code != errcode.ErrNotConversationMember.Code {
		t.Fatalf("expected 13002 for removed member POST read, got %v", err)
	}
	if _, err := svc.ListMessageReceipts(ctx, 10004, 90002, 2002, "", "", 20); err == nil || err.Code != errcode.ErrNotConversationMember.Code {
		t.Fatalf("expected 13002 for removed member GET receipts, got %v", err)
	}

	// 2. Group member 10002 marks delivered up to 2; 10003 marks read up to 2.
	if res, err := svc.MarkDelivered(ctx, 10002, 90002, 2); err != nil || res.LastDeliveredSeq != 2 || res.UnreadCount != 2 {
		t.Fatalf("group MarkDelivered failed: res=%+v err=%v", res, err)
	}
	if res, err := svc.MarkRead(ctx, 10003, 90002, 2); err != nil || res.LastReadSeq != 2 || res.LastDeliveredSeq != 2 || res.UnreadCount != 0 {
		t.Fatalf("group MarkRead failed: res=%+v err=%v", res, err)
	}

	// 3. Disband group -> remaining members (10001, 10002, 10003) MUST still be allowed to read & query receipts!
	g := b.groups[501]
	g.Status = model.GroupStatusDisbanded
	b.groups[501] = g

	if res, err := svc.MarkRead(ctx, 10002, 90002, 1); err != nil || res.LastReadSeq != 1 || res.UnreadCount != 0 {
		t.Fatalf("disbanded group member MarkRead must succeed: res=%+v err=%v", res, err)
	}
	// Pagination limit=1 across 2 recipients (10002 unread/delivered, 10003 read).
	p1, err := svc.ListMessageReceipts(ctx, 10001, 90002, 2002, "", "", 1)
	if err != nil || p1.TotalMembers != 2 || p1.DeliveredCount != 2 || p1.ReadCount != 1 || p1.UnreadCount != 1 || !p1.HasMore || p1.NextCursor == nil || len(p1.Items) != 1 {
		t.Fatalf("group receipts page 1 unexpected: res=%+v err=%v", p1, err)
	}
	p2, err := svc.ListMessageReceipts(ctx, 10001, 90002, 2002, "", *p1.NextCursor, 1)
	if err != nil || p2.HasMore || p2.NextCursor != nil || len(p2.Items) != 1 {
		t.Fatalf("group receipts page 2 unexpected: res=%+v err=%v", p2, err)
	}
	if p1.Items[0].User.ID == p2.Items[0].User.ID {
		t.Fatalf("pagination returned duplicate user %d", p1.Items[0].User.ID)
	}

	// Status filters: read, unread, delivered, pending.
	onlyRead, err := svc.ListMessageReceipts(ctx, 10001, 90002, 2002, "read", "", 20)
	if err != nil || len(onlyRead.Items) != 1 || onlyRead.Items[0].User.ID != 10003 || onlyRead.Items[0].DeliveryStatus != model.ReceiptStatusRead {
		t.Fatalf("status=read mismatch: res=%+v err=%v", onlyRead, err)
	}
	onlyUnread, err := svc.ListMessageReceipts(ctx, 10001, 90002, 2002, "unread", "", 20)
	if err != nil || len(onlyUnread.Items) != 1 || onlyUnread.Items[0].User.ID != 10002 || onlyUnread.Items[0].DeliveryStatus != model.ReceiptStatusDelivered {
		t.Fatalf("status=unread mismatch: res=%+v err=%v", onlyUnread, err)
	}
	onlyPending, err := svc.ListMessageReceipts(ctx, 10001, 90002, 2002, "pending", "", 20)
	if err != nil || len(onlyPending.Items) != 0 {
		t.Fatalf("status=pending mismatch: res=%+v err=%v", onlyPending, err)
	}

	// Cursor scope mismatch (status or msg_id changed) must fail with 10001.
	if _, err := svc.ListMessageReceipts(ctx, 10001, 90002, 2002, "read", *p1.NextCursor, 1); err == nil || err.Code != errcode.ErrInvalidValue.Code {
		t.Fatalf("expected 10001 on tampered cursor status, got %v", err)
	}

	// 4. Unfriended hidden single-chat member (is_visible=false) can still sync delivered & read.
	cmSingle := b.convMembers[[2]uint64{90001, 10002}]
	cmSingle.IsVisible = false
	b.convMembers[[2]uint64{90001, 10002}] = cmSingle
	if res, err := svc.MarkDelivered(ctx, 10002, 90001, 3); err != nil || res.LastDeliveredSeq != 3 {
		t.Fatalf("hidden single-chat MarkDelivered must succeed: res=%+v err=%v", res, err)
	}
	if res, err := svc.MarkRead(ctx, 10002, 90001, 3); err != nil || res.LastReadSeq != 3 || res.UnreadCount != 0 {
		t.Fatalf("hidden single-chat MarkRead must succeed: res=%+v err=%v", res, err)
	}
}

func TestReceiptServicePublisherUnwiredAndFailureDoNotRollbackDB(t *testing.T) {
	b := newMemReceiptBackend()
	seedReceiptTestData(b)

	var logBuf bytes.Buffer
	log, err := logger.New(config.LogConfig{Level: "warn", Format: "json"}, &logBuf)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. Nil (unwired) publisher: logs warning, records ErrReceiptPublisherUnwired, keeps committed DB state.
	unwiredSvc := NewReceiptService(b, b, nil, log)
	delRes, bizErr := unwiredSvc.AckDeliveredWS(ctx, 10002, 90001, 1001, 1)
	if bizErr != nil || !delRes.Changed || delRes.EventPublished || !errors.Is(delRes.EventPublishErr, ErrReceiptPublisherUnwired) {
		t.Fatalf("expected unwired publisher observable failure without rollback, got res=%+v err=%v", delRes, bizErr)
	}
	if b.convMembers[[2]uint64{90001, 10002}].LastDeliveredSeq != 1 || b.receipts["1001:10002"].Status != model.ReceiptStatusDelivered {
		t.Fatalf("expected committed DB state preserved despite unwired publisher")
	}
	if !strings.Contains(logBuf.String(), "receipt_event_publisher_unwired") {
		t.Fatalf("expected observable warning log for unwired publisher, got %s", logBuf.String())
	}

	// 2. Failing publisher: logs error, records publish error, keeps committed DB state!
	logBuf.Reset()
	failingPub := &spyReceiptPublisher{err: errors.New("redis stream unavailable")}
	failingSvc := NewReceiptService(b, b, failingPub, log)
	readRes, bizErr := failingSvc.MarkRead(ctx, 10002, 90001, 3)
	if bizErr != nil || !readRes.Changed || readRes.EventPublished || readRes.EventPublishErr == nil {
		t.Fatalf("expected observable publish failure without rollback, got res=%+v err=%v", readRes, bizErr)
	}
	if b.convMembers[[2]uint64{90001, 10002}].LastReadSeq != 3 || b.convMembers[[2]uint64{90001, 10002}].UnreadCount != 0 || b.receipts["1003:10002"].Status != model.ReceiptStatusRead {
		t.Fatalf("expected committed read watermark & receipts preserved when stream publish fails")
	}
	if !strings.Contains(logBuf.String(), "receipt_event_publish_failed") {
		t.Fatalf("expected observable error log for publish failure, got %s", logBuf.String())
	}

	// 3. Verify ReceiptEvent JSON serialization for delivered (non-null msg_id/seq) and read (null msg_id/seq).
	mid := uint64(188900000000001)
	seq := uint64(42)
	delJSON, _ := json.Marshal(ReceiptEvent{
		Kind:             ReceiptEventKindDelivered,
		ConversationID:   90001,
		UserID:           10002,
		TargetUserIDs:    []uint64{10001},
		MsgID:            &mid,
		Seq:              &seq,
		LastDeliveredSeq: 42,
		LastReadSeq:      10,
		UpdatedAt:        "2026-11-26T04:00:00.000Z",
	})
	if !strings.Contains(string(delJSON), `"msg_id":"188900000000001"`) || !strings.Contains(string(delJSON), `"seq":"42"`) {
		t.Fatalf("unexpected delivered event JSON: %s", delJSON)
	}
	readJSON, _ := json.Marshal(ReceiptEvent{
		Kind:             ReceiptEventKindRead,
		ConversationID:   90001,
		UserID:           10002,
		TargetUserIDs:    []uint64{10001},
		MsgID:            nil,
		Seq:              nil,
		LastDeliveredSeq: 42,
		LastReadSeq:      42,
		UpdatedAt:        "2026-11-26T04:00:05.000Z",
	})
	if !strings.Contains(string(readJSON), `"msg_id":null`) || !strings.Contains(string(readJSON), `"seq":null`) {
		t.Fatalf("unexpected read event JSON: %s", readJSON)
	}
}

func TestReceiptServiceLiveMySQLFlow(t *testing.T) {
	mysqlPass := os.Getenv("MYSQL_PASSWORD")
	if mysqlPass == "" {
		t.Skip("skipping live MySQL ReceiptService test: MYSQL_PASSWORD not set")
	}

	host := os.Getenv("MYSQL_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := 3306
	if p := os.Getenv("MYSQL_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	user := os.Getenv("MYSQL_USER")
	if user == "" {
		user = "root"
	}
	dbName := os.Getenv("MYSQL_DATABASE")
	if dbName == "" {
		dbName = "enterprise_im"
	}

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
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		DialTimeout:     3 * time.Second,
	}, log)
	if err != nil {
		t.Fatalf("NewMySQL failed: %v", err)
	}
	defer func() { _ = repository.CloseMySQL(db) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ts := time.Now().UTC().UnixNano()
	u1 := model.User{Username: fmt.Sprintf("rcpt_svc_u1_%d", ts), PasswordHash: "x", Nickname: "Alice", Role: model.UserRoleUser, Status: model.UserStatusActive}
	u2 := model.User{Username: fmt.Sprintf("rcpt_svc_u2_%d", ts), PasswordHash: "x", Nickname: "Bob", Role: model.UserRoleUser, Status: model.UserStatusActive}
	u3 := model.User{Username: fmt.Sprintf("rcpt_svc_u3_%d", ts), PasswordHash: "x", Nickname: "Charlie", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.WithContext(ctx).Create(&[]model.User{u1, u2, u3}).Error; err != nil {
		t.Fatalf("create users: %v", err)
	}
	var users []model.User
	if err := db.WithContext(ctx).Where("username IN ?", []string{u1.Username, u2.Username, u3.Username}).Order("id ASC").Find(&users).Error; err != nil {
		t.Fatalf("reload users: %v", err)
	}
	u1, u2, u3 = users[0], users[1], users[2]

	dk := repository.DirectKey(u1.ID, u2.ID)
	singleConv := model.Conversation{Type: model.ConversationTypeSingle, DirectKey: &dk, LastSeq: 2}
	if err := db.WithContext(ctx).Create(&singleConv).Error; err != nil {
		t.Fatalf("create single conv: %v", err)
	}
	grp := model.Group{Name: fmt.Sprintf("rcpt-svc-grp-%d", ts), OwnerID: u1.ID, Status: model.GroupStatusActive, MaxMembers: 500}
	if err := db.WithContext(ctx).Create(&grp).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}
	groupConv := model.Conversation{Type: model.ConversationTypeGroup, GroupID: &grp.ID, LastSeq: 3}
	if err := db.WithContext(ctx).Create(&groupConv).Error; err != nil {
		t.Fatalf("create group conv: %v", err)
	}

	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = db.WithContext(cctx).Where("conversation_id IN ?", []uint64{singleConv.ID, groupConv.ID}).Delete(&model.MessageReceipt{}).Error
		_ = db.WithContext(cctx).Where("conversation_id IN ?", []uint64{singleConv.ID, groupConv.ID}).Delete(&model.Message{}).Error
		_ = db.WithContext(cctx).Where("conversation_id IN ?", []uint64{singleConv.ID, groupConv.ID}).Delete(&model.ConversationMember{}).Error
		_ = db.WithContext(cctx).Where("group_id = ?", grp.ID).Delete(&model.GroupMember{}).Error
		_ = db.WithContext(cctx).Where("id IN ?", []uint64{singleConv.ID, groupConv.ID}).Delete(&model.Conversation{}).Error
		_ = db.WithContext(cctx).Where("id = ?", grp.ID).Delete(&model.Group{}).Error
		_ = db.WithContext(cctx).Where("id IN ?", []uint64{u1.ID, u2.ID, u3.ID}).Delete(&model.User{}).Error
	})

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := db.WithContext(ctx).Create(&[]model.ConversationMember{
		{ConversationID: singleConv.ID, UserID: u1.ID, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: singleConv.ID, UserID: u2.ID, UnreadCount: 2, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: groupConv.ID, UserID: u1.ID, LastDeliveredSeq: 1, LastReadSeq: 1, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: groupConv.ID, UserID: u2.ID, UnreadCount: 3, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: groupConv.ID, UserID: u3.ID, UnreadCount: 3, IsVisible: true, JoinedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create conv members: %v", err)
	}
	if err := db.WithContext(ctx).Create(&[]model.GroupMember{
		{GroupID: grp.ID, UserID: u1.ID, Role: model.GroupRoleOwner, JoinedAt: now, UpdatedAt: now},
		{GroupID: grp.ID, UserID: u2.ID, Role: model.GroupRoleAdmin, JoinedAt: now, UpdatedAt: now},
		{GroupID: grp.ID, UserID: u3.ID, Role: model.GroupRoleMember, JoinedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create group members: %v", err)
	}

	sMsg1ID, sMsg2ID := uint64(ts), uint64(ts+1)
	gMsg1ID, gMsg2ID, gMsg3ID := uint64(ts+2), uint64(ts+3), uint64(ts+4)
	if err := db.WithContext(ctx).Create(&[]model.Message{
		{ID: sMsg1ID, ConversationID: singleConv.ID, Seq: 1, SenderID: u1.ID, ClientMsgID: fmt.Sprintf("ls-1-%d", ts), MsgType: model.MessageTypeText, Content: "s1", Status: model.MessageStatusNormal, CreatedAt: now},
		{ID: sMsg2ID, ConversationID: singleConv.ID, Seq: 2, SenderID: u1.ID, ClientMsgID: fmt.Sprintf("ls-2-%d", ts), MsgType: model.MessageTypeText, Content: "s2", Status: model.MessageStatusNormal, CreatedAt: now},
		{ID: gMsg1ID, ConversationID: groupConv.ID, Seq: 1, SenderID: 0, ClientMsgID: fmt.Sprintf("lg-1-%d", ts), MsgType: model.MessageTypeSystem, Content: "sys", Status: model.MessageStatusNormal, CreatedAt: now},
		{ID: gMsg2ID, ConversationID: groupConv.ID, Seq: 2, SenderID: u1.ID, ClientMsgID: fmt.Sprintf("lg-2-%d", ts), MsgType: model.MessageTypeText, Content: "g2", Status: model.MessageStatusNormal, CreatedAt: now},
		{ID: gMsg3ID, ConversationID: groupConv.ID, Seq: 3, SenderID: u1.ID, ClientMsgID: fmt.Sprintf("lg-3-%d", ts), MsgType: model.MessageTypeText, Content: "g3", Status: model.MessageStatusNormal, CreatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create messages: %v", err)
	}
	if err := db.WithContext(ctx).Create(&[]model.MessageReceipt{
		{MessageID: sMsg1ID, ConversationID: singleConv.ID, Seq: 1, UserID: u2.ID, Status: model.ReceiptStatusSent, CreatedAt: now, UpdatedAt: now},
		{MessageID: sMsg2ID, ConversationID: singleConv.ID, Seq: 2, UserID: u2.ID, Status: model.ReceiptStatusSent, CreatedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create receipts: %v", err)
	}

	repo := repository.NewReceiptRepository(db)
	pub := &spyReceiptPublisher{}
	svc := NewReceiptService(repo, nil, pub, log)

	// Single chat: AckDeliveredWS(sMsg1) -> MarkRead(2) -> ListMessageReceipts filters.
	if res, bizErr := svc.AckDeliveredWS(ctx, u2.ID, singleConv.ID, sMsg1ID, 1); bizErr != nil || !res.Changed || res.LastDeliveredSeq != 1 {
		t.Fatalf("live AckDeliveredWS failed: res=%+v err=%v", res, bizErr)
	}
	if res, bizErr := svc.MarkRead(ctx, u2.ID, singleConv.ID, 2); bizErr != nil || !res.Changed || res.UnreadCount != 0 || res.LastReadSeq != 2 {
		t.Fatalf("live single MarkRead failed: res=%+v err=%v", res, bizErr)
	}
	for _, st := range []string{"", "read", "delivered"} {
		list, bizErr := svc.ListMessageReceipts(ctx, u1.ID, singleConv.ID, sMsg1ID, st, "", 10)
		if bizErr != nil || list.TotalMembers != 1 || list.ReadCount != 1 || len(list.Items) != 1 {
			t.Fatalf("live single ListMessageReceipts(%q) unexpected: res=%+v err=%v", st, list, bizErr)
		}
	}

	// Group chat: u2 MarkDelivered(3), u3 stays pending; test all 4 filters & pagination, then disband group and verify u3 can MarkRead!
	if res, bizErr := svc.MarkDelivered(ctx, u2.ID, groupConv.ID, 3); bizErr != nil || res.LastDeliveredSeq != 3 {
		t.Fatalf("live group MarkDelivered failed: res=%+v err=%v", res, bizErr)
	}
	page1, bizErr := svc.ListMessageReceipts(ctx, u1.ID, groupConv.ID, gMsg3ID, "", "", 1)
	if bizErr != nil || page1.TotalMembers != 2 || page1.DeliveredCount != 1 || page1.ReadCount != 0 || page1.UnreadCount != 2 || !page1.HasMore || page1.NextCursor == nil {
		t.Fatalf("live group page1 unexpected: res=%+v err=%v", page1, bizErr)
	}
	page2, bizErr := svc.ListMessageReceipts(ctx, u1.ID, groupConv.ID, gMsg3ID, "", *page1.NextCursor, 1)
	if bizErr != nil || page2.HasMore || page2.NextCursor != nil || len(page2.Items) != 1 {
		t.Fatalf("live group page2 unexpected: res=%+v err=%v", page2, bizErr)
	}
	pendingList, bizErr := svc.ListMessageReceipts(ctx, u1.ID, groupConv.ID, gMsg3ID, "pending", "", 10)
	if bizErr != nil || len(pendingList.Items) != 1 || pendingList.Items[0].User.ID != u3.ID {
		t.Fatalf("live group pending filter unexpected: res=%+v err=%v", pendingList, bizErr)
	}
	unreadList, bizErr := svc.ListMessageReceipts(ctx, u1.ID, groupConv.ID, gMsg3ID, "unread", "", 10)
	if bizErr != nil || len(unreadList.Items) != 2 {
		t.Fatalf("live group unread filter unexpected: res=%+v err=%v", unreadList, bizErr)
	}

	// Disband group in MySQL -> u3 can still MarkRead(3) even when publisher fails (no rollback!).
	if err := db.WithContext(ctx).Model(&model.Group{}).Where("id = ?", grp.ID).Update("status", model.GroupStatusDisbanded).Error; err != nil {
		t.Fatalf("disband group: %v", err)
	}
	svc.SetEventPublisher(&spyReceiptPublisher{err: errors.New("stream down")})
	disbandRead, bizErr := svc.MarkRead(ctx, u3.ID, groupConv.ID, 3)
	if bizErr != nil || !disbandRead.Changed || disbandRead.LastReadSeq != 3 || disbandRead.UnreadCount != 0 || disbandRead.EventPublished || disbandRead.EventPublishErr == nil {
		t.Fatalf("disbanded group MarkRead with failing publisher unexpected: res=%+v err=%v", disbandRead, bizErr)
	}
	var cm3 model.ConversationMember
	if err := db.WithContext(ctx).Where("conversation_id = ? AND user_id = ?", groupConv.ID, u3.ID).Take(&cm3).Error; err != nil {
		t.Fatalf("reload u3 conv member: %v", err)
	}
	if cm3.LastReadSeq != 3 || cm3.LastDeliveredSeq != 3 || cm3.UnreadCount != 0 {
		t.Fatalf("expected committed MySQL watermarks preserved on u3 despite publisher error, got %+v", cm3)
	}
}
