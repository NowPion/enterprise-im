package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

const (
	defaultDedupeTTL      = 24 * time.Hour
	defaultDedupeLockTTL  = 10 * time.Second
	defaultDedupeWaitTime = 500 * time.Millisecond
)

var (
	// ErrMessageNotFound indicates the requested message does not exist in the conversation.
	ErrMessageNotFound = errors.New("message not found")
	// ErrDedupeInProgress indicates an identical (sender_id, client_msg_id) send is currently in flight.
	ErrDedupeInProgress = errors.New("message send in progress")

	releaseDedupeLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)
)

// DedupeRecord mirrors the frozen Redis dedupe value format:
// msg_id|conversation_id|seq|status|delivery_status
type DedupeRecord struct {
	MsgID          uint64
	ConversationID uint64
	Seq            uint64
	Status         string
	DeliveryStatus string
}

// DedupeKey returns `im:dedupe:{sender_id}:{client_msg_id}`.
func DedupeKey(senderID uint64, clientMsgID string) string {
	return fmt.Sprintf("im:dedupe:%d:%s", senderID, clientMsgID)
}

// DedupeLockKey returns `im:dedupe:lock:{sender_id}:{client_msg_id}`.
func DedupeLockKey(senderID uint64, clientMsgID string) string {
	return fmt.Sprintf("im:dedupe:lock:%d:%s", senderID, clientMsgID)
}

// FormatDedupeValue serializes a DedupeRecord into `msg_id|conversation_id|seq|status|delivery_status`.
func FormatDedupeValue(rec DedupeRecord) string {
	return fmt.Sprintf("%d|%d|%d|%s|%s", rec.MsgID, rec.ConversationID, rec.Seq, rec.Status, rec.DeliveryStatus)
}

// ParseDedupeValue parses `msg_id|conversation_id|seq|status|delivery_status`.
func ParseDedupeValue(raw string) (*DedupeRecord, bool) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) != 5 {
		return nil, false
	}
	msgID, err1 := strconv.ParseUint(parts[0], 10, 64)
	convID, err2 := strconv.ParseUint(parts[1], 10, 64)
	seq, err3 := strconv.ParseUint(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || msgID == 0 || convID == 0 || seq == 0 {
		return nil, false
	}
	status := parts[3]
	if status != model.MessageStatusNormal && status != model.MessageStatusRecalled {
		return nil, false
	}
	delivery := parts[4]
	switch delivery {
	case model.ReceiptStatusSent, model.ReceiptStatusDelivered, model.ReceiptStatusRead:
	default:
		return nil, false
	}
	return &DedupeRecord{
		MsgID:          msgID,
		ConversationID: convID,
		Seq:            seq,
		Status:         status,
		DeliveryStatus: delivery,
	}, true
}

// IsDuplicateClientMsgError reports whether err is a MySQL duplicate key error on uk_msg_sender_client.
func IsDuplicateClientMsgError(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysqldriver.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return strings.Contains(mysqlErr.Message, "uk_msg_sender_client") ||
			strings.Contains(mysqlErr.Message, "client_msg_id")
	}
	return false
}

// MessageRepository provides MySQL and Redis persistence operations for messages, receipts, and assets.
type MessageRepository struct {
	db  *gorm.DB
	rdb *redis.Client
}

// NewMessageRepository constructs a MessageRepository.
func NewMessageRepository(db *gorm.DB, rdb *redis.Client) *MessageRepository {
	return &MessageRepository{db: db, rdb: rdb}
}

// WithTx executes fn inside a GORM MySQL transaction.
func (r *MessageRepository) WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	if r == nil || r.db == nil {
		return ErrRepositoryUnavailable
	}
	return WithTx(ctx, r.db, fn)
}

// GetDedupeRecord looks up `im:dedupe:{sender_id}:{client_msg_id}` in Redis.
func (r *MessageRepository) GetDedupeRecord(ctx context.Context, senderID uint64, clientMsgID string) (*DedupeRecord, bool, error) {
	if r == nil || r.rdb == nil {
		return nil, false, ErrRedisClientNil
	}
	val, err := r.rdb.Get(ctx, DedupeKey(senderID, clientMsgID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get dedupe key: %w", err)
	}
	rec, ok := ParseDedupeValue(val)
	if !ok {
		return nil, false, nil
	}
	return rec, true, nil
}

// SetDedupeRecordNX stores `im:dedupe:{sender_id}:{client_msg_id}` using SET NX with 24h TTL.
func (r *MessageRepository) SetDedupeRecordNX(ctx context.Context, senderID uint64, clientMsgID string, rec DedupeRecord, ttl time.Duration) error {
	if r == nil || r.rdb == nil {
		return ErrRedisClientNil
	}
	if ttl <= 0 {
		ttl = defaultDedupeTTL
	}
	return r.rdb.SetNX(ctx, DedupeKey(senderID, clientMsgID), FormatDedupeValue(rec), ttl).Err()
}

// DeleteDedupeRecord removes `im:dedupe:{sender_id}:{client_msg_id}` when message status changes (e.g. recall).
func (r *MessageRepository) DeleteDedupeRecord(ctx context.Context, senderID uint64, clientMsgID string) error {
	if r == nil || r.rdb == nil || senderID == 0 || clientMsgID == "" {
		return nil
	}
	return r.rdb.Del(ctx, DedupeKey(senderID, clientMsgID)).Err()
}

// AcquireDedupeLock acquires `im:dedupe:lock:{sender_id}:{client_msg_id}` (PX 10s), polling briefly until
// either the lock is acquired or an existing DedupeRecord becomes visible.
func (r *MessageRepository) AcquireDedupeLock(
	ctx context.Context,
	senderID uint64,
	clientMsgID string,
	lockTTL time.Duration,
	waitTimeout time.Duration,
) (string, *DedupeRecord, error) {
	if r == nil || r.rdb == nil {
		return "", nil, ErrRedisClientNil
	}
	if lockTTL <= 0 {
		lockTTL = defaultDedupeLockTTL
	}
	if waitTimeout <= 0 {
		waitTimeout = defaultDedupeWaitTime
	}

	token, err := newLockToken()
	if err != nil {
		return "", nil, fmt.Errorf("generate dedupe lock token: %w", err)
	}

	lockKey := DedupeLockKey(senderID, clientMsgID)
	deadline := time.Now().Add(waitTimeout)

	for {
		if rec, found, getErr := r.GetDedupeRecord(ctx, senderID, clientMsgID); getErr != nil {
			return "", nil, getErr
		} else if found {
			return "", rec, nil
		}

		acquired, setErr := r.rdb.SetNX(ctx, lockKey, token, lockTTL).Result()
		if setErr != nil {
			return "", nil, fmt.Errorf("acquire dedupe lock: %w", setErr)
		}
		if acquired {
			return token, nil, nil
		}

		if time.Now().After(deadline) {
			return "", nil, ErrDedupeInProgress
		}

		timer := time.NewTimer(15 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// ReleaseDedupeLock releases `im:dedupe:lock:{sender_id}:{client_msg_id}` when owned by token.
func (r *MessageRepository) ReleaseDedupeLock(ctx context.Context, senderID uint64, clientMsgID, token string) error {
	if r == nil || r.rdb == nil || senderID == 0 || clientMsgID == "" || token == "" {
		return nil
	}
	return releaseDedupeLockScript.Run(ctx, r.rdb, []string{DedupeLockKey(senderID, clientMsgID)}, token).Err()
}

// FindMessageByClientMsgID looks up a message by the unique key (sender_id, client_msg_id).
func (r *MessageRepository) FindMessageByClientMsgID(ctx context.Context, senderID uint64, clientMsgID string) (*model.Message, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	var msg model.Message
	err := r.db.WithContext(ctx).
		Where("sender_id = ? AND client_msg_id = ?", senderID, clientMsgID).
		Take(&msg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find message by client_msg_id: %w", err)
	}
	return &msg, nil
}

// FindMessageByID looks up a message by its Snowflake ID.
func (r *MessageRepository) FindMessageByID(ctx context.Context, msgID uint64) (*model.Message, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if msgID == 0 {
		return nil, ErrMessageNotFound
	}
	var msg model.Message
	err := r.db.WithContext(ctx).Where("id = ?", msgID).Take(&msg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrMessageNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find message by id: %w", err)
	}
	return &msg, nil
}

// LockConversationByID locks a conversation row FOR UPDATE inside tx.
func (r *MessageRepository) LockConversationByID(ctx context.Context, tx *gorm.DB, convID uint64) (*model.Conversation, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var conv model.Conversation
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", convID).
		Take(&conv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock conversation: %w", err)
	}
	return &conv, nil
}

// LockMessageInConversation locks a message row FOR UPDATE scoped to convID.
func (r *MessageRepository) LockMessageInConversation(ctx context.Context, tx *gorm.DB, convID, msgID uint64) (*model.Message, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var msg model.Message
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND conversation_id = ?", msgID, convID).
		Take(&msg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrMessageNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock message in conversation: %w", err)
	}
	return &msg, nil
}

// GetAssetByAssetID fetches an asset record by external asset_id UUID without locking.
func (r *MessageRepository) GetAssetByAssetID(ctx context.Context, assetID string) (*model.Asset, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if strings.TrimSpace(assetID) == "" {
		return nil, ErrAssetNotFound
	}
	var asset model.Asset
	err := r.db.WithContext(ctx).Where("asset_id = ?", assetID).Take(&asset).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get asset by asset_id: %w", err)
	}
	return &asset, nil
}

// LockAssetByAssetID locks an asset record FOR UPDATE by external asset_id UUID inside tx.
func (r *MessageRepository) LockAssetByAssetID(ctx context.Context, tx *gorm.DB, assetID string) (*model.Asset, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	if strings.TrimSpace(assetID) == "" {
		return nil, ErrAssetNotFound
	}
	var asset model.Asset
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("asset_id = ?", assetID).
		Take(&asset).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock asset by asset_id: %w", err)
	}
	return &asset, nil
}

// BindAssetToMessageTx binds an active asset to a persisted message and increments ref_count inside tx.
func (r *MessageRepository) BindAssetToMessageTx(
	ctx context.Context,
	tx *gorm.DB,
	asset *model.Asset,
	msgID uint64,
	convID uint64,
	now time.Time,
) error {
	if tx == nil || asset == nil {
		return ErrRepositoryUnavailable
	}
	ts := now.UTC().Truncate(time.Millisecond)
	updates := map[string]any{
		"ref_count":  gorm.Expr("ref_count + 1"),
		"updated_at": ts,
	}
	if asset.RefType == model.AssetRefNone {
		updates["ref_type"] = model.AssetRefMessage
		updates["ref_id"] = msgID
		updates["ref_conversation_id"] = convID
		if asset.BoundAt == nil {
			updates["bound_at"] = ts
		}
	}
	return tx.WithContext(ctx).
		Model(&model.Asset{}).
		Where("id = ?", asset.ID).
		Updates(updates).Error
}

// CreateMessageTx inserts a Snowflake-ID message into `messages` inside tx.
func (r *MessageRepository) CreateMessageTx(ctx context.Context, tx *gorm.DB, msg *model.Message) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).Create(msg).Error
}

// UpdateConversationOnMessageTx updates conversation summary fields inside tx.
func (r *MessageRepository) UpdateConversationOnMessageTx(
	ctx context.Context,
	tx *gorm.DB,
	convID uint64,
	msg *model.Message,
	preview string,
	now time.Time,
) error {
	if tx == nil || msg == nil {
		return ErrRepositoryUnavailable
	}
	ts := now.UTC().Truncate(time.Millisecond)
	return tx.WithContext(ctx).
		Model(&model.Conversation{}).
		Where("id = ?", convID).
		Updates(map[string]any{
			"last_msg_id":      msg.ID,
			"last_seq":         msg.Seq,
			"last_msg_preview": truncateRunes(preview, 255),
			"last_msg_at":      &ts,
			"updated_at":       ts,
		}).Error
}

// UpdateMembersOnNewMessageTx restores visibility and updates timestamps for sender,
// and increments unread_count + restores visibility for other active recipients.
func (r *MessageRepository) UpdateMembersOnNewMessageTx(
	ctx context.Context,
	tx *gorm.DB,
	convID uint64,
	senderID uint64,
	recipientIDs []uint64,
	now time.Time,
) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	ts := now.UTC().Truncate(time.Millisecond)

	if senderID > 0 {
		if err := tx.WithContext(ctx).
			Model(&model.ConversationMember{}).
			Where("conversation_id = ? AND user_id = ?", convID, senderID).
			Updates(map[string]any{
				"is_visible": true,
				"updated_at": ts,
			}).Error; err != nil {
			return fmt.Errorf("update sender conversation member: %w", err)
		}
	}

	otherIDs := make([]uint64, 0, len(recipientIDs))
	for _, uid := range dedupePositiveUint64(recipientIDs) {
		if uid != senderID {
			otherIDs = append(otherIDs, uid)
		}
	}
	if len(otherIDs) == 0 {
		return nil
	}

	if err := tx.WithContext(ctx).
		Model(&model.ConversationMember{}).
		Where("conversation_id = ? AND user_id IN ?", convID, otherIDs).
		Updates(map[string]any{
			"unread_count": gorm.Expr("unread_count + 1"),
			"is_visible":   true,
			"updated_at":   ts,
		}).Error; err != nil {
		return fmt.Errorf("increment recipients unread count: %w", err)
	}
	return nil
}

// CreateMessageReceiptsTx inserts initial message_receipts rows idempotently inside tx.
func (r *MessageRepository) CreateMessageReceiptsTx(ctx context.Context, tx *gorm.DB, receipts []model.MessageReceipt) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	if len(receipts) == 0 {
		return nil
	}
	return tx.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "message_id"},
				{Name: "user_id"},
			},
			DoNothing: true,
		}).
		Create(&receipts).Error
}

// MarkMessageRecalledTx atomically transitions a normal message to recalled inside tx.
func (r *MessageRepository) MarkMessageRecalledTx(ctx context.Context, tx *gorm.DB, msgID uint64, recalledAt time.Time) (bool, error) {
	if tx == nil {
		return false, ErrRepositoryUnavailable
	}
	ts := recalledAt.UTC().Truncate(time.Millisecond)
	res := tx.WithContext(ctx).
		Model(&model.Message{}).
		Where("id = ? AND status = ?", msgID, model.MessageStatusNormal).
		Updates(map[string]any{
			"status":      model.MessageStatusRecalled,
			"recalled_at": &ts,
		})
	if res.Error != nil {
		return false, fmt.Errorf("mark message recalled: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// AdjustMembersOnRecallTx updates updated_at for active members and decrements unread_count by 1
// for non-sender members who had not yet read targetSeq.
func (r *MessageRepository) AdjustMembersOnRecallTx(
	ctx context.Context,
	tx *gorm.DB,
	convID uint64,
	senderID uint64,
	targetSeq uint64,
	recipientIDs []uint64,
	now time.Time,
) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	ts := now.UTC().Truncate(time.Millisecond)

	if senderID > 0 {
		if err := tx.WithContext(ctx).
			Model(&model.ConversationMember{}).
			Where("conversation_id = ? AND user_id = ?", convID, senderID).
			Updates(map[string]any{
				"updated_at": ts,
			}).Error; err != nil {
			return fmt.Errorf("update sender member on recall: %w", err)
		}
	}

	otherIDs := make([]uint64, 0, len(recipientIDs))
	for _, uid := range dedupePositiveUint64(recipientIDs) {
		if uid != senderID {
			otherIDs = append(otherIDs, uid)
		}
	}
	if len(otherIDs) == 0 {
		return nil
	}

	if err := tx.WithContext(ctx).
		Model(&model.ConversationMember{}).
		Where("conversation_id = ? AND user_id IN ?", convID, otherIDs).
		Updates(map[string]any{
			"unread_count": gorm.Expr("CASE WHEN last_read_seq < ? AND unread_count > 0 THEN unread_count - 1 ELSE unread_count END", targetSeq),
			"updated_at":   ts,
		}).Error; err != nil {
		return fmt.Errorf("adjust recipients unread count on recall: %w", err)
	}
	return nil
}

// ListMessagesBefore queries messages in convID with optional seq < beforeSeq ordered by seq DESC (limit+1).
func (r *MessageRepository) ListMessagesBefore(
	ctx context.Context,
	convID uint64,
	beforeSeq *uint64,
	limit int,
) ([]model.Message, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if convID == 0 || limit <= 0 {
		return []model.Message{}, nil
	}

	q := r.db.WithContext(ctx).
		Model(&model.Message{}).
		Where("conversation_id = ?", convID)
	if beforeSeq != nil {
		q = q.Where("seq < ?", *beforeSeq)
	}

	var rows []model.Message
	if err := q.Order("seq DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list messages before seq: %w", err)
	}
	return rows, nil
}

// ListMessagesAfter queries messages in convID with seq > afterSeq ordered by seq ASC (limit+1).
func (r *MessageRepository) ListMessagesAfter(
	ctx context.Context,
	convID uint64,
	afterSeq uint64,
	limit int,
) ([]model.Message, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if convID == 0 || limit <= 0 {
		return []model.Message{}, nil
	}

	var rows []model.Message
	if err := r.db.WithContext(ctx).
		Model(&model.Message{}).
		Where("conversation_id = ? AND seq > ?", convID, afterSeq).
		Order("seq ASC").
		Limit(limit + 1).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list messages after seq: %w", err)
	}
	return rows, nil
}

// BatchGetSingleDeliveryStatuses loads message receipt statuses for a peer in a single conversation
// across msgIDs using one IN query.
func (r *MessageRepository) BatchGetSingleDeliveryStatuses(
	ctx context.Context,
	convID uint64,
	peerUserID uint64,
	msgIDs []uint64,
) (map[uint64]string, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(msgIDs)
	out := make(map[uint64]string, len(uniqueIDs))
	if convID == 0 || peerUserID == 0 || len(uniqueIDs) == 0 {
		return out, nil
	}

	var rows []model.MessageReceipt
	if err := r.db.WithContext(ctx).
		Select("message_id", "status").
		Where("conversation_id = ? AND user_id = ? AND message_id IN ?", convID, peerUserID, uniqueIDs).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("batch get message receipts: %w", err)
	}
	for _, row := range rows {
		out[row.MessageID] = row.Status
	}
	return out, nil
}
