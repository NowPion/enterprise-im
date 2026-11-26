package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/repository"
)

var (
	offlineConvSeqScript = redis.NewScript(`
local cur = tonumber(redis.call('HGET', KEYS[1], 'last_seq') or '0')
local nxt = tonumber(ARGV[1])
if nxt >= cur then
	redis.call('HSET', KEYS[1], 'last_seq', ARGV[1], 'last_msg_id', ARGV[2], 'updated_at', ARGV[3])
end
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 1
`)

	offlineUserSeqScript = redis.NewScript(`
local cur = tonumber(redis.call('HGET', KEYS[1], ARGV[1]) or '0')
local nxt = tonumber(ARGV[2])
if nxt > cur then
	redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
end
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)
)

func (cm *ConsumerManager) dispatchEntry(ctx context.Context, stream, group string, msg redis.XMessage) error {
	if cm.customHandlers != nil {
		if fn, ok := cm.customHandlers[group]; ok && fn != nil {
			return fn(ctx, stream, group, msg)
		}
	}
	switch group {
	case GroupMsgPersist:
		return cm.handleMsgPersist(ctx, msg)
	case GroupMsgDeliver:
		return cm.handleMsgDeliver(ctx, msg)
	case GroupEventDeliver:
		return cm.handleEventDeliver(ctx, msg)
	case GroupEventAudit:
		return cm.handleEventAudit(ctx, msg)
	default:
		return fmt.Errorf("unsupported consumer group %q", group)
	}
}

func (cm *ConsumerManager) handleMsgPersist(ctx context.Context, msg redis.XMessage) error {
	kind := stringField(msg.Values, "kind")
	if kind != "" && kind != KindMessage {
		return fmt.Errorf("%w: unexpected kind %q in %s", ErrInvalidStreamPayload, kind, StreamMessages)
	}
	msgID, ok1 := parseUint64Field(msg.Values, "message_id")
	convID, ok2 := parseUint64Field(msg.Values, "conversation_id")
	seq, ok3 := parseUint64Field(msg.Values, "seq")
	senderID, _ := parseUint64Field(msg.Values, "sender_id")
	if !ok1 || !ok2 || !ok3 || msgID == 0 || convID == 0 || seq == 0 {
		return fmt.Errorf("%w: missing message_id, conversation_id, or seq", ErrInvalidStreamPayload)
	}

	doneKeys := []string{
		doneKey(GroupMsgPersist, "msg:"+strconv.FormatUint(msgID, 10)),
		doneKey(GroupMsgPersist, "sid:"+msg.ID),
	}
	alreadyDone, err := cm.isAnyDone(ctx, doneKeys...)
	if err != nil {
		return err
	}
	if alreadyDone {
		return nil
	}

	now := cm.nowFn().UTC()
	var committed *CommittedMessage
	if cm.msgStore != nil {
		loaded, loadErr := cm.msgStore.GetCommittedMessage(ctx, msgID)
		if loadErr != nil {
			return loadErr
		}
		if loaded.ConversationID != convID || loaded.Seq != seq {
			return fmt.Errorf("%w: stream metadata (%d/%d) mismatches committed message (%d/%d)",
				ErrInvalidStreamPayload, convID, seq, loaded.ConversationID, loaded.Seq)
		}
		committed = loaded

		meta, recipients, recErr := cm.msgStore.ListConversationActiveRecipients(ctx, convID)
		if recErr != nil {
			return recErr
		}

		// Post-commit compensation 1: ensure dedupe cache entry exists if Redis write was lost after commit.
		if gormStore, ok := cm.msgStore.(*GormStore); ok && committed.SenderID > 0 {
			_ = gormStore.EnsureDedupeRecord(ctx, committed.SenderID, committed.ClientMsgID, committed.MsgID, convID, seq, committed.Status)
		}

		// Post-commit compensation 2: single-chat compensatory receipt or large-group delayed receipt init.
		nonSenders := excludeUser(recipients, committed.SenderID)
		if len(nonSenders) > 0 {
			switch meta.Type {
			case model.ConversationTypeSingle:
				peerID, hasPeer := repository.ParseDirectKeyPeer(meta.DirectKey, committed.SenderID)
				if hasPeer {
					if err := cm.msgStore.EnsureSingleReceipt(ctx, convID, msgID, seq, peerID, now); err != nil {
						return err
					}
				} else if committed.SenderID == 0 {
					if err := cm.msgStore.InitDelayedGroupReceipts(ctx, convID, msgID, seq, nonSenders, now); err != nil {
						return err
					}
				}
			case model.ConversationTypeGroup:
				if committed.SenderID > 0 {
					if err := cm.msgStore.InitDelayedGroupReceipts(ctx, convID, msgID, seq, nonSenders, now); err != nil {
						return err
					}
				}
			}
		}

		// Post-commit compensation 3: maintain Redis offline compensation index (without modifying MySQL unread_count).
		if err := cm.updateOfflineIndex(ctx, convID, msgID, seq, nonSenders, now); err != nil {
			return err
		}
	} else {
		// Redis-only mode (e.g. unit test without MySQL): still maintain conversation offline index.
		if err := cm.updateOfflineIndex(ctx, convID, msgID, seq, nil, now); err != nil {
			return err
		}
		_ = senderID
	}

	if cm.persistHook != nil {
		if err := cm.persistHook(ctx, msg, committed); err != nil {
			return err
		}
	}

	return cm.markDone(ctx, doneKeys...)
}

func (cm *ConsumerManager) handleMsgDeliver(ctx context.Context, msg redis.XMessage) error {
	kind := stringField(msg.Values, "kind")
	if kind != "" && kind != KindMessage {
		return fmt.Errorf("%w: unexpected kind %q in %s", ErrInvalidStreamPayload, kind, StreamMessages)
	}
	msgID, ok1 := parseUint64Field(msg.Values, "message_id")
	convID, ok2 := parseUint64Field(msg.Values, "conversation_id")
	seq, ok3 := parseUint64Field(msg.Values, "seq")
	if !ok1 || !ok2 || !ok3 || msgID == 0 || convID == 0 || seq == 0 {
		return fmt.Errorf("%w: missing message_id, conversation_id, or seq", ErrInvalidStreamPayload)
	}

	doneKeys := []string{
		doneKey(GroupMsgDeliver, "msg:"+strconv.FormatUint(msgID, 10)),
		doneKey(GroupMsgDeliver, "sid:"+msg.ID),
	}
	alreadyDone, err := cm.isAnyDone(ctx, doneKeys...)
	if err != nil {
		return err
	}
	if alreadyDone {
		return nil
	}

	var (
		committed  *CommittedMessage
		recipients []uint64
	)
	if cm.msgResolver != nil {
		var resErr error
		committed, recipients, resErr = cm.msgResolver(ctx, msg)
		if resErr != nil {
			return resErr
		}
	} else if cm.msgStore != nil {
		loaded, loadErr := cm.msgStore.GetCommittedMessage(ctx, msgID)
		if loadErr != nil {
			return loadErr
		}
		_, members, memErr := cm.msgStore.ListConversationActiveRecipients(ctx, convID)
		if memErr != nil {
			return memErr
		}
		committed = loaded
		recipients = members
	} else {
		return errors.New("im-msg-deliver requires message store or resolver")
	}

	if cm.hub != nil && committed != nil && len(recipients) > 0 {
		pushPayload := MessagePushPayload{
			PushType:       "message",
			Message:        *committed,
			DeliveryStatus: model.ReceiptStatusSent,
		}
		if err := cm.hub.DeliverToUsers(ctx, recipients, "msg.push", pushPayload); err != nil {
			return err
		}
	}

	return cm.markDone(ctx, doneKeys...)
}

func (cm *ConsumerManager) handleEventDeliver(ctx context.Context, msg redis.XMessage) error {
	kind := stringField(msg.Values, "kind")
	if !IsValidEventKind(kind) {
		return fmt.Errorf("%w: unsupported event kind %q", ErrInvalidStreamPayload, kind)
	}
	dedupeToken := stringField(msg.Values, "event_id")
	if dedupeToken == "" {
		dedupeToken = msg.ID
	}
	doneKeys := []string{
		doneKey(GroupEventDeliver, "evt:"+dedupeToken),
		doneKey(GroupEventDeliver, "sid:"+msg.ID),
	}
	alreadyDone, err := cm.isAnyDone(ctx, doneKeys...)
	if err != nil {
		return err
	}
	if alreadyDone {
		return nil
	}

	convID, _ := parseUint64Field(msg.Values, "conversation_id")
	actorID, _ := parseUint64Field(msg.Values, "actor_id")
	explicitTargetID, _ := parseUint64Field(msg.Values, "target_user_id")
	msgID, _ := parseUint64Field(msg.Values, "message_id")
	seq, _ := parseUint64Field(msg.Values, "seq")
	lastDeliveredSeq, _ := parseUint64Field(msg.Values, "last_delivered_seq")
	lastReadSeq, _ := parseUint64Field(msg.Values, "last_read_seq")
	eventName := stringField(msg.Values, "event")
	occurredAt := stringField(msg.Values, "occurred_at")
	if occurredAt == "" {
		occurredAt = cm.nowFn().UTC().Format(utcMillisLayout)
	}

	targets := parseTargetUserIDs(msg.Values)
	if len(targets) == 0 && convID > 0 && cm.msgStore != nil {
		if (kind == KindDelivered || kind == KindRead) && msgID > 0 {
			if committed, loadErr := cm.msgStore.GetCommittedMessage(ctx, msgID); loadErr == nil && committed.SenderID > 0 && committed.SenderID != actorID {
				targets = []uint64{committed.SenderID}
			}
		}
		if len(targets) == 0 {
			if _, members, memErr := cm.msgStore.ListConversationActiveRecipients(ctx, convID); memErr == nil {
				if kind == KindDelivered || kind == KindRead {
					targets = excludeUser(members, actorID)
				} else {
					targets = members
				}
			}
		}
	}

	frameType, frameData, buildErr := buildEventWSFrame(kind, eventName, dedupeToken, convID, actorID, explicitTargetID, msgID, seq, lastDeliveredSeq, lastReadSeq, occurredAt, stringField(msg.Values, "payload"))
	if buildErr != nil {
		return buildErr
	}

	if cm.hub != nil && len(targets) > 0 && frameType != "" {
		if err := cm.hub.DeliverToUsers(ctx, targets, frameType, frameData); err != nil {
			return err
		}
	}

	return cm.markDone(ctx, doneKeys...)
}

func (cm *ConsumerManager) handleEventAudit(ctx context.Context, msg redis.XMessage) error {
	kind := stringField(msg.Values, "kind")
	if !IsValidEventKind(kind) {
		return fmt.Errorf("%w: unsupported event kind %q", ErrInvalidStreamPayload, kind)
	}
	dedupeToken := stringField(msg.Values, "event_id")
	if dedupeToken == "" {
		dedupeToken = msg.ID
	}
	doneKeys := []string{
		doneKey(GroupEventAudit, "evt:"+dedupeToken),
		doneKey(GroupEventAudit, "sid:"+msg.ID),
	}
	alreadyDone, err := cm.isAnyDone(ctx, doneKeys...)
	if err != nil {
		return err
	}
	if alreadyDone {
		return nil
	}

	auditEntry, shouldPersistAudit, parseErr := buildAuditLogEntry(dedupeToken, kind, msg.Values, cm.nowFn().UTC())
	if parseErr != nil {
		return parseErr
	}
	if shouldPersistAudit && cm.auditStore != nil {
		if err := cm.auditStore.AppendAuditLog(ctx, auditEntry); err != nil {
			return err
		}
	}

	now := cm.nowFn().UTC()
	minuteKey := "im:mq:metrics:events:minute:" + now.Format("200601021504")
	pipe := cm.rdb.Pipeline()
	pipe.HIncrBy(ctx, "im:mq:metrics:events:total", kind, 1)
	pipe.HIncrBy(ctx, minuteKey, kind, 1)
	pipe.Expire(ctx, minuteKey, time.Hour)
	if shouldPersistAudit {
		pipe.HIncrBy(ctx, "im:mq:metrics:audit:total", auditEntry.Scope, 1)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("update event audit metrics: %w", err)
	}

	return cm.markDone(ctx, doneKeys...)
}

func buildEventWSFrame(
	kind, eventName, dedupeToken string,
	convID, actorID, explicitTargetID, msgID, seq, lastDeliveredSeq, lastReadSeq uint64,
	occurredAt, rawPayload string,
) (string, any, error) {
	switch kind {
	case KindDelivered, KindRead:
		var msgIDPtr, seqPtr *string
		if msgID > 0 {
			v := strconv.FormatUint(msgID, 10)
			msgIDPtr = &v
		}
		if seq > 0 {
			v := strconv.FormatUint(seq, 10)
			seqPtr = &v
		}
		return "msg.push", ReceiptPushPayload{
			PushType:         "receipt",
			ConversationID:   strconv.FormatUint(convID, 10),
			UserID:           strconv.FormatUint(actorID, 10),
			DeliveryStatus:   kind,
			MsgID:            msgIDPtr,
			Seq:              seqPtr,
			LastDeliveredSeq: strconv.FormatUint(lastDeliveredSeq, 10),
			LastReadSeq:      strconv.FormatUint(lastReadSeq, 10),
			UpdatedAt:        occurredAt,
		}, nil

	case KindPresence:
		payloadObj := decodePayloadMap(rawPayload)
		if _, ok := payloadObj["user_id"]; !ok && actorID > 0 {
			payloadObj["user_id"] = strconv.FormatUint(actorID, 10)
		}
		return "presence.update", payloadObj, nil

	case KindRecall, KindFriend, KindGroup, KindKick:
		resolvedEvent := strings.TrimSpace(eventName)
		if resolvedEvent == "" {
			switch kind {
			case KindRecall:
				resolvedEvent = "message.recalled"
			case KindFriend:
				resolvedEvent = "friend.requested"
			case KindGroup:
				resolvedEvent = "group.updated"
			case KindKick:
				resolvedEvent = "account.kicked"
			}
		}
		noticeID := dedupeToken
		if !strings.HasPrefix(noticeID, "n_") {
			noticeID = "n_" + noticeID
		}
		var convPtr, actorPtr, targetPtr *string
		if convID > 0 {
			v := strconv.FormatUint(convID, 10)
			convPtr = &v
		}
		if actorID > 0 {
			v := strconv.FormatUint(actorID, 10)
			actorPtr = &v
		}
		if explicitTargetID > 0 {
			v := strconv.FormatUint(explicitTargetID, 10)
			targetPtr = &v
		}
		payloadObj := decodePayloadMap(rawPayload)
		if kind == KindRecall {
			if _, ok := payloadObj["recalled_msg_id"]; !ok && msgID > 0 {
				payloadObj["recalled_msg_id"] = strconv.FormatUint(msgID, 10)
			}
			if _, ok := payloadObj["recall_seq"]; !ok && seq > 0 {
				payloadObj["recall_seq"] = strconv.FormatUint(seq, 10)
			}
			if _, ok := payloadObj["recalled_at"]; !ok {
				payloadObj["recalled_at"] = occurredAt
			}
		}
		return "system.notice", SystemNoticePayload{
			Event:          resolvedEvent,
			NoticeID:       noticeID,
			ConversationID: convPtr,
			ActorID:        actorPtr,
			TargetUserID:   targetPtr,
			Payload:        payloadObj,
		}, nil

	default:
		return "", nil, fmt.Errorf("%w: unsupported event kind %q", ErrInvalidStreamPayload, kind)
	}
}

func buildAuditLogEntry(eventID, kind string, values map[string]any, fallbackNow time.Time) (*model.AuditLog, bool, error) {
	rawAudit := stringField(values, "audit")
	convID, _ := parseUint64Field(values, "conversation_id")
	actorID, _ := parseUint64Field(values, "actor_id")
	targetUserID, _ := parseUint64Field(values, "target_user_id")
	msgID, _ := parseUint64Field(values, "message_id")
	eventName := stringField(values, "event")
	occurredAt := fallbackNow
	if tsStr := stringField(values, "occurred_at"); tsStr != "" {
		if parsed, err := time.Parse(utcMillisLayout, tsStr); err == nil {
			occurredAt = parsed.UTC()
		} else if parsed, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
			occurredAt = parsed.UTC()
		}
	}

	var rec AuditRecord
	if rawAudit != "" {
		if err := json.Unmarshal([]byte(rawAudit), &rec); err != nil {
			return nil, false, fmt.Errorf("%w: invalid audit JSON: %v", ErrInvalidStreamPayload, err)
		}
	} else {
		// Delivered, read, and presence events without explicit audit metadata are aggregated as metrics only.
		if kind != KindRecall && kind != KindKick && kind != KindGroup && kind != KindFriend {
			return nil, false, nil
		}
	}

	scope := normalizeAuditScope(rec.Scope, kind)
	targetType := normalizeAuditTargetType(rec.TargetType, kind)
	action := strings.TrimSpace(rec.Action)
	if action == "" {
		action = eventName
	}
	if action == "" {
		action = "event." + kind
	}

	targetID := rec.TargetID
	if targetID == 0 {
		switch targetType {
		case model.AuditTargetMessage:
			targetID = msgID
		case model.AuditTargetUser:
			targetID = targetUserID
		case model.AuditTargetConversation:
			targetID = convID
		}
	}

	var convPtr *uint64
	if rec.ConversationID != nil && *rec.ConversationID > 0 {
		v := *rec.ConversationID
		convPtr = &v
	} else if convID > 0 {
		v := convID
		convPtr = &v
	}

	var groupPtr *uint64
	if rec.GroupID != nil && *rec.GroupID > 0 {
		v := *rec.GroupID
		groupPtr = &v
	}

	var detail model.NullableJSON
	if len(rec.Detail) > 0 && json.Valid(rec.Detail) {
		detail = model.NullableJSON(rec.Detail)
	} else if rawPayload := stringField(values, "payload"); rawPayload != "" && json.Valid([]byte(rawPayload)) {
		detail = model.NullableJSON(rawPayload)
	}

	if len(eventID) > 64 {
		eventID = eventID[:64]
	}

	return &model.AuditLog{
		EventID:        eventID,
		Scope:          scope,
		Action:         truncateStr(action, 64),
		ActorID:        actorID,
		ActorRole:      truncateStr(rec.ActorRole, 32),
		TargetType:     targetType,
		TargetID:       targetID,
		GroupID:        groupPtr,
		ConversationID: convPtr,
		Reason:         truncateStr(rec.Reason, 512),
		Detail:         detail,
		RequestID:      truncateStr(rec.RequestID, 64),
		IPAddress:      truncateStr(rec.IPAddress, 64),
		UserAgent:      truncateStr(rec.UserAgent, 512),
		CreatedAt:      occurredAt.Truncate(time.Millisecond),
	}, true, nil
}

func normalizeAuditScope(raw, kind string) string {
	switch strings.TrimSpace(raw) {
	case model.AuditScopeAdmin, model.AuditScopeGroup, model.AuditScopeSystem:
		return strings.TrimSpace(raw)
	}
	if kind == KindGroup {
		return model.AuditScopeGroup
	}
	return model.AuditScopeSystem
}

func normalizeAuditTargetType(raw, kind string) string {
	switch strings.TrimSpace(raw) {
	case model.AuditTargetUser,
		model.AuditTargetSession,
		model.AuditTargetGroup,
		model.AuditTargetConversation,
		model.AuditTargetMessage,
		model.AuditTargetAsset,
		model.AuditTargetSystem:
		return strings.TrimSpace(raw)
	}
	switch kind {
	case KindRecall:
		return model.AuditTargetMessage
	case KindGroup:
		return model.AuditTargetGroup
	case KindKick, KindFriend:
		return model.AuditTargetUser
	default:
		return model.AuditTargetSystem
	}
}

func (cm *ConsumerManager) updateOfflineIndex(
	ctx context.Context,
	convID, msgID, seq uint64,
	recipients []uint64,
	now time.Time,
) error {
	if cm.rdb == nil {
		return nil
	}
	ttlMs := strconv.FormatInt(cm.cfg.StreamRetention.Milliseconds(), 10)
	convKey := "im:mq:offline:conv:" + strconv.FormatUint(convID, 10)
	if err := offlineConvSeqScript.Run(
		ctx,
		cm.rdb,
		[]string{convKey},
		strconv.FormatUint(seq, 10),
		strconv.FormatUint(msgID, 10),
		strconv.FormatInt(now.UnixMilli(), 10),
		ttlMs,
	).Err(); err != nil {
		return fmt.Errorf("update offline conv index: %w", err)
	}

	if len(recipients) == 0 {
		return nil
	}

	pipe := cm.rdb.Pipeline()
	checks := make([]*redis.StringCmd, len(recipients))
	for i, uid := range recipients {
		checks[i] = pipe.Get(ctx, "im:presence:user:"+strconv.FormatUint(uid, 10))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		cm.log.WarnContext(ctx, "mq_offline_presence_check_failed", slog.String("error", sanitizeErrorSummary(err)))
		return nil
	}

	convStr := strconv.FormatUint(convID, 10)
	seqStr := strconv.FormatUint(seq, 10)
	for i, uid := range recipients {
		val, err := checks[i].Result()
		if err == nil && val != "" && val != "0" {
			// User has active connection leases; no offline hint needed.
			continue
		}
		userKey := "im:mq:offline:user:" + strconv.FormatUint(uid, 10)
		if err := offlineUserSeqScript.Run(ctx, cm.rdb, []string{userKey}, convStr, seqStr, ttlMs).Err(); err != nil {
			return fmt.Errorf("update offline user index: %w", err)
		}
	}
	return nil
}

func (cm *ConsumerManager) isAnyDone(ctx context.Context, keys ...string) (bool, error) {
	if cm.rdb == nil || len(keys) == 0 {
		return false, nil
	}
	n, err := cm.rdb.Exists(ctx, keys...).Result()
	if err != nil {
		return false, fmt.Errorf("check idempotency keys: %w", err)
	}
	return n > 0, nil
}

func (cm *ConsumerManager) markDone(ctx context.Context, keys ...string) error {
	if cm.rdb == nil || len(keys) == 0 {
		return nil
	}
	pipe := cm.rdb.Pipeline()
	for _, k := range keys {
		pipe.Set(ctx, k, "1", cm.cfg.IdempotencyTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("mark idempotency keys: %w", err)
	}
	return nil
}

func doneKey(group, token string) string {
	return "im:mq:done:" + group + ":" + token
}

func excludeUser(ids []uint64, exclude uint64) []uint64 {
	if len(ids) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && id != exclude {
			out = append(out, id)
		}
	}
	return dedupePositiveUint64(out)
}

func decodePayloadMap(raw string) map[string]any {
	out := make(map[string]any)
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return out
	}
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func truncateStr(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}
