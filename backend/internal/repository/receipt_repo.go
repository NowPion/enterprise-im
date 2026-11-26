package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

const (
	// ReceiptFilterRead filters recipients who have read the message.
	ReceiptFilterRead = "read"
	// ReceiptFilterUnread filters recipients who have not yet read the message.
	ReceiptFilterUnread = "unread"
	// ReceiptFilterDelivered filters recipients to whom the message has been delivered (including read).
	ReceiptFilterDelivered = "delivered"
	// ReceiptFilterPending filters recipients to whom the message has not yet been delivered.
	ReceiptFilterPending = "pending"
)

// ReceiptSummaryCounts holds aggregate delivery/read counts for a message.
type ReceiptSummaryCounts struct {
	TotalMembers   uint32 `gorm:"column:total_members"`
	DeliveredCount uint32 `gorm:"column:delivered_count"`
	ReadCount      uint32 `gorm:"column:read_count"`
	UnreadCount    uint32 `gorm:"column:unread_count"`
}

// ReceiptMemberRow represents a single recipient row joined with watermark/receipt state.
type ReceiptMemberRow struct {
	CMID             uint64     `gorm:"column:cm_id"`
	UserID           uint64     `gorm:"column:user_id"`
	Role             *string    `gorm:"column:role"`
	DeliveryStatus   string     `gorm:"column:delivery_status"`
	LastDeliveredSeq uint64     `gorm:"column:last_delivered_seq"`
	LastReadSeq      uint64     `gorm:"column:last_read_seq"`
	DeliveredAt      *time.Time `gorm:"column:delivered_at"`
	ReadAt           *time.Time `gorm:"column:read_at"`
}

// ReceiptRepository provides GORM persistence for Stage 7 delivered/read receipts and watermarks.
type ReceiptRepository struct {
	db       *gorm.DB
	convRepo *ConversationRepository
}

// NewReceiptRepository constructs a ReceiptRepository.
func NewReceiptRepository(db *gorm.DB) *ReceiptRepository {
	return &ReceiptRepository{
		db:       db,
		convRepo: NewConversationRepository(db),
	}
}

// WithTx executes fn inside a GORM MySQL transaction.
func (r *ReceiptRepository) WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	if r == nil || r.db == nil {
		return ErrRepositoryUnavailable
	}
	return WithTx(ctx, r.db, fn)
}

// GetConversationByID fetches a conversation without locking.
func (r *ReceiptRepository) GetConversationByID(ctx context.Context, convID uint64) (*model.Conversation, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	return r.convRepo.GetConversationByID(ctx, convID)
}

// GetConversationMember fetches a conversation_members row without locking.
func (r *ReceiptRepository) GetConversationMember(ctx context.Context, convID, userID uint64) (*model.ConversationMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	return r.convRepo.GetConversationMember(ctx, convID, userID)
}

// GetGroupMember fetches a group_members row without locking.
func (r *ReceiptRepository) GetGroupMember(ctx context.Context, groupID, userID uint64) (*model.GroupMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	return r.convRepo.GetGroupMember(ctx, groupID, userID)
}

// BatchGetUsersByIDs fetches public User records in one IN query.
func (r *ReceiptRepository) BatchGetUsersByIDs(ctx context.Context, userIDs []uint64) (map[uint64]model.User, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	return r.convRepo.BatchGetUsersByIDs(ctx, userIDs)
}

// GetMessageInConversation fetches a message scoped to convID.
func (r *ReceiptRepository) GetMessageInConversation(ctx context.Context, convID, msgID uint64) (*model.Message, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	return getMessageInConversation(ctx, r.db, convID, msgID)
}

// LockConversationByIDTx locks the conversation row FOR UPDATE inside tx to serialize with sends/recalls/member changes.
func (r *ReceiptRepository) LockConversationByIDTx(ctx context.Context, tx *gorm.DB, convID uint64) (*model.Conversation, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	if convID == 0 {
		return nil, ErrConversationNotFound
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
		return nil, fmt.Errorf("lock conversation for receipt: %w", err)
	}
	return &conv, nil
}

// HasGroupMemberTx verifies active/disbanded membership in group_members inside tx.
func (r *ReceiptRepository) HasGroupMemberTx(ctx context.Context, tx *gorm.DB, groupID, userID uint64) (bool, error) {
	if tx == nil {
		return false, ErrRepositoryUnavailable
	}
	if groupID == 0 || userID == 0 {
		return false, nil
	}
	var count int64
	if err := tx.WithContext(ctx).
		Model(&model.GroupMember{}).
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("check group member for receipt: %w", err)
	}
	return count > 0, nil
}

// LockConversationMemberTx locks the user's conversation_members row FOR UPDATE inside tx.
func (r *ReceiptRepository) LockConversationMemberTx(ctx context.Context, tx *gorm.DB, convID, userID uint64) (*model.ConversationMember, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	if convID == 0 || userID == 0 {
		return nil, ErrNotConversationMember
	}
	var cm model.ConversationMember
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("conversation_id = ? AND user_id = ?", convID, userID).
		Take(&cm).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotConversationMember
	}
	if err != nil {
		return nil, fmt.Errorf("lock conversation member for receipt: %w", err)
	}
	return &cm, nil
}

// GetMessageInConversationTx loads a message scoped to convID inside tx.
func (r *ReceiptRepository) GetMessageInConversationTx(ctx context.Context, tx *gorm.DB, convID, msgID uint64) (*model.Message, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	return getMessageInConversation(ctx, tx, convID, msgID)
}

func getMessageInConversation(ctx context.Context, db *gorm.DB, convID, msgID uint64) (*model.Message, error) {
	if convID == 0 || msgID == 0 {
		return nil, ErrMessageNotFound
	}
	var msg model.Message
	err := db.WithContext(ctx).
		Where("id = ? AND conversation_id = ?", msgID, convID).
		Take(&msg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrMessageNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get message in conversation: %w", err)
	}
	return &msg, nil
}

// LockMessageReceiptTx locks a user's single-chat message_receipts row FOR UPDATE if present.
// Returns (nil, nil) when no receipt row exists for (msgID, userID).
func (r *ReceiptRepository) LockMessageReceiptTx(ctx context.Context, tx *gorm.DB, msgID, userID uint64) (*model.MessageReceipt, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	if msgID == 0 || userID == 0 {
		return nil, nil
	}
	var rec model.MessageReceipt
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("message_id = ? AND user_id = ?", msgID, userID).
		Take(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock message receipt: %w", err)
	}
	return &rec, nil
}

// MarkSingleReceiptDeliveredTx transitions one message_receipts row from sent -> delivered.
// It never downgrades delivered or read rows.
func (r *ReceiptRepository) MarkSingleReceiptDeliveredTx(ctx context.Context, tx *gorm.DB, receiptID uint64, now time.Time) (bool, error) {
	if tx == nil {
		return false, ErrRepositoryUnavailable
	}
	ts := now.UTC().Truncate(time.Millisecond)
	res := tx.WithContext(ctx).
		Model(&model.MessageReceipt{}).
		Where("id = ? AND status = ?", receiptID, model.ReceiptStatusSent).
		Updates(map[string]any{
			"status":       model.ReceiptStatusDelivered,
			"delivered_at": gorm.Expr("COALESCE(delivered_at, ?)", ts),
			"updated_at":   ts,
		})
	if res.Error != nil {
		return false, fmt.Errorf("mark single receipt delivered: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// MarkReceiptsDeliveredUpToSeqTx batch-transitions sent receipts with seq <= targetSeq to delivered.
func (r *ReceiptRepository) MarkReceiptsDeliveredUpToSeqTx(
	ctx context.Context,
	tx *gorm.DB,
	convID uint64,
	userID uint64,
	targetSeq uint64,
	now time.Time,
) (int64, error) {
	if tx == nil {
		return 0, ErrRepositoryUnavailable
	}
	if targetSeq == 0 {
		return 0, nil
	}
	ts := now.UTC().Truncate(time.Millisecond)
	res := tx.WithContext(ctx).
		Model(&model.MessageReceipt{}).
		Where("conversation_id = ? AND user_id = ? AND seq <= ? AND status = ?", convID, userID, targetSeq, model.ReceiptStatusSent).
		Updates(map[string]any{
			"status":       model.ReceiptStatusDelivered,
			"delivered_at": gorm.Expr("COALESCE(delivered_at, ?)", ts),
			"updated_at":   ts,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("batch mark receipts delivered: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// MarkReceiptsReadUpToSeqTx batch-transitions sent/delivered receipts with seq <= targetSeq to read,
// preserving existing delivered_at when already populated.
func (r *ReceiptRepository) MarkReceiptsReadUpToSeqTx(
	ctx context.Context,
	tx *gorm.DB,
	convID uint64,
	userID uint64,
	targetSeq uint64,
	now time.Time,
) (int64, error) {
	if tx == nil {
		return 0, ErrRepositoryUnavailable
	}
	if targetSeq == 0 {
		return 0, nil
	}
	ts := now.UTC().Truncate(time.Millisecond)
	res := tx.WithContext(ctx).
		Model(&model.MessageReceipt{}).
		Where("conversation_id = ? AND user_id = ? AND seq <= ? AND status IN ?",
			convID, userID, targetSeq, []string{model.ReceiptStatusSent, model.ReceiptStatusDelivered}).
		Updates(map[string]any{
			"status":       model.ReceiptStatusRead,
			"delivered_at": gorm.Expr("COALESCE(delivered_at, ?)", ts),
			"read_at":      gorm.Expr("COALESCE(read_at, ?)", ts),
			"updated_at":   ts,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("batch mark receipts read: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// UpdateMemberDeliveredWatermarkTx updates conversation_members.last_delivered_seq monotonically.
func (r *ReceiptRepository) UpdateMemberDeliveredWatermarkTx(
	ctx context.Context,
	tx *gorm.DB,
	memberID uint64,
	lastDeliveredSeq uint64,
	now time.Time,
) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	ts := now.UTC().Truncate(time.Millisecond)
	if err := tx.WithContext(ctx).
		Model(&model.ConversationMember{}).
		Where("id = ?", memberID).
		Updates(map[string]any{
			"last_delivered_seq": lastDeliveredSeq,
			"updated_at":         ts,
		}).Error; err != nil {
		return fmt.Errorf("update member delivered watermark: %w", err)
	}
	return nil
}

// UpdateMemberReadWatermarkTx updates last_read_seq, last_delivered_seq, and clears unread_count=0.
func (r *ReceiptRepository) UpdateMemberReadWatermarkTx(
	ctx context.Context,
	tx *gorm.DB,
	memberID uint64,
	lastReadSeq uint64,
	lastDeliveredSeq uint64,
	now time.Time,
) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	ts := now.UTC().Truncate(time.Millisecond)
	if err := tx.WithContext(ctx).
		Model(&model.ConversationMember{}).
		Where("id = ?", memberID).
		Updates(map[string]any{
			"last_read_seq":      lastReadSeq,
			"last_delivered_seq": lastDeliveredSeq,
			"unread_count":       0,
			"updated_at":         ts,
		}).Error; err != nil {
		return fmt.Errorf("update member read watermark: %w", err)
	}
	return nil
}

// ListDistinctSendersInSeqRangeTx returns distinct non-zero sender IDs (excluding excludeUserID)
// for messages in (fromSeqExclusive, toSeqInclusive].
func (r *ReceiptRepository) ListDistinctSendersInSeqRangeTx(
	ctx context.Context,
	tx *gorm.DB,
	convID uint64,
	fromSeqExclusive uint64,
	toSeqInclusive uint64,
	excludeUserID uint64,
) ([]uint64, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	if convID == 0 || toSeqInclusive <= fromSeqExclusive {
		return []uint64{}, nil
	}
	var ids []uint64
	if err := tx.WithContext(ctx).
		Model(&model.Message{}).
		Distinct("sender_id").
		Where("conversation_id = ? AND seq > ? AND seq <= ? AND sender_id > 0 AND sender_id <> ?",
			convID, fromSeqExclusive, toSeqInclusive, excludeUserID).
		Pluck("sender_id", &ids).Error; err != nil {
		return nil, fmt.Errorf("list distinct senders in seq range: %w", err)
	}
	return dedupePositiveUint64(ids), nil
}

// QueryMessageReceiptDetails computes aggregate receipt counts and fetches cursor-paginated
// recipient rows (limit+1) for either a single or group conversation.
func (r *ReceiptRepository) QueryMessageReceiptDetails(
	ctx context.Context,
	conv *model.Conversation,
	msg *model.Message,
	statusFilter string,
	cursorID uint64,
	limit int,
) (ReceiptSummaryCounts, []ReceiptMemberRow, error) {
	if r == nil || r.db == nil {
		return ReceiptSummaryCounts{}, nil, ErrRepositoryUnavailable
	}
	if conv == nil || msg == nil || limit <= 0 {
		return ReceiptSummaryCounts{}, []ReceiptMemberRow{}, nil
	}

	if conv.Type == model.ConversationTypeGroup {
		if conv.GroupID == nil || *conv.GroupID == 0 {
			return ReceiptSummaryCounts{}, nil, ErrGroupNotFound
		}
		return r.queryGroupMessageReceipts(ctx, conv.ID, *conv.GroupID, msg, statusFilter, cursorID, limit)
	}
	return r.querySingleMessageReceipts(ctx, conv.ID, msg, statusFilter, cursorID, limit)
}

func (r *ReceiptRepository) querySingleMessageReceipts(
	ctx context.Context,
	convID uint64,
	msg *model.Message,
	statusFilter string,
	cursorID uint64,
	limit int,
) (ReceiptSummaryCounts, []ReceiptMemberRow, error) {
	effectiveExpr := "COALESCE(mr.status, CASE WHEN cm.last_read_seq >= ? THEN 'read' WHEN cm.last_delivered_seq >= ? THEN 'delivered' ELSE 'sent' END)"

	base := r.db.WithContext(ctx).
		Table("conversation_members AS cm").
		Joins("LEFT JOIN message_receipts AS mr ON mr.message_id = ? AND mr.user_id = cm.user_id", msg.ID).
		Where("cm.conversation_id = ?", convID)
	if msg.SenderID > 0 {
		base = base.Where("cm.user_id <> ?", msg.SenderID)
	}

	var summary ReceiptSummaryCounts
	if err := base.Session(&gorm.Session{}).
		Select(
			"COUNT(*) AS total_members, "+
				"COALESCE(SUM(CASE WHEN "+effectiveExpr+" IN ('delivered', 'read') THEN 1 ELSE 0 END), 0) AS delivered_count, "+
				"COALESCE(SUM(CASE WHEN "+effectiveExpr+" = 'read' THEN 1 ELSE 0 END), 0) AS read_count, "+
				"COALESCE(SUM(CASE WHEN "+effectiveExpr+" <> 'read' THEN 1 ELSE 0 END), 0) AS unread_count",
			msg.Seq, msg.Seq,
			msg.Seq, msg.Seq,
			msg.Seq, msg.Seq,
		).
		Scan(&summary).Error; err != nil {
		return ReceiptSummaryCounts{}, nil, fmt.Errorf("summarize single message receipts: %w", err)
	}

	listQ := base.Session(&gorm.Session{}).
		Select(
			"cm.id AS cm_id, "+
				"cm.user_id AS user_id, "+
				"NULL AS role, "+
				effectiveExpr+" AS delivery_status, "+
				"cm.last_delivered_seq AS last_delivered_seq, "+
				"cm.last_read_seq AS last_read_seq, "+
				"mr.delivered_at AS delivered_at, "+
				"mr.read_at AS read_at",
			msg.Seq, msg.Seq,
		)

	switch statusFilter {
	case ReceiptFilterRead:
		listQ = listQ.Where(effectiveExpr+" = ?", msg.Seq, msg.Seq, model.ReceiptStatusRead)
	case ReceiptFilterUnread:
		listQ = listQ.Where(effectiveExpr+" IN ?", msg.Seq, msg.Seq, []string{model.ReceiptStatusSent, model.ReceiptStatusDelivered})
	case ReceiptFilterDelivered:
		listQ = listQ.Where(effectiveExpr+" IN ?", msg.Seq, msg.Seq, []string{model.ReceiptStatusDelivered, model.ReceiptStatusRead})
	case ReceiptFilterPending:
		listQ = listQ.Where(effectiveExpr+" = ?", msg.Seq, msg.Seq, model.ReceiptStatusSent)
	}
	if cursorID > 0 {
		listQ = listQ.Where("cm.id < ?", cursorID)
	}

	var rows []ReceiptMemberRow
	if err := listQ.Order("cm.id DESC").Limit(limit + 1).Scan(&rows).Error; err != nil {
		return ReceiptSummaryCounts{}, nil, fmt.Errorf("list single message receipts: %w", err)
	}
	return summary, rows, nil
}

func (r *ReceiptRepository) queryGroupMessageReceipts(
	ctx context.Context,
	convID uint64,
	groupID uint64,
	msg *model.Message,
	statusFilter string,
	cursorID uint64,
	limit int,
) (ReceiptSummaryCounts, []ReceiptMemberRow, error) {
	base := r.db.WithContext(ctx).
		Table("conversation_members AS cm").
		Joins("INNER JOIN group_members AS gm ON gm.group_id = ? AND gm.user_id = cm.user_id", groupID).
		Where("cm.conversation_id = ?", convID)
	if msg.SenderID > 0 {
		base = base.Where("cm.user_id <> ?", msg.SenderID)
	}

	var summary ReceiptSummaryCounts
	if err := base.Session(&gorm.Session{}).
		Select(
			"COUNT(*) AS total_members, "+
				"COALESCE(SUM(CASE WHEN cm.last_delivered_seq >= ? OR cm.last_read_seq >= ? THEN 1 ELSE 0 END), 0) AS delivered_count, "+
				"COALESCE(SUM(CASE WHEN cm.last_read_seq >= ? THEN 1 ELSE 0 END), 0) AS read_count, "+
				"COALESCE(SUM(CASE WHEN cm.last_read_seq < ? THEN 1 ELSE 0 END), 0) AS unread_count",
			msg.Seq, msg.Seq, msg.Seq, msg.Seq,
		).
		Scan(&summary).Error; err != nil {
		return ReceiptSummaryCounts{}, nil, fmt.Errorf("summarize group message receipts: %w", err)
	}

	listQ := base.Session(&gorm.Session{}).
		Select(
			"cm.id AS cm_id, "+
				"cm.user_id AS user_id, "+
				"gm.role AS role, "+
				"CASE WHEN cm.last_read_seq >= ? THEN 'read' WHEN cm.last_delivered_seq >= ? THEN 'delivered' ELSE 'sent' END AS delivery_status, "+
				"cm.last_delivered_seq AS last_delivered_seq, "+
				"cm.last_read_seq AS last_read_seq, "+
				"NULL AS delivered_at, "+
				"NULL AS read_at",
			msg.Seq, msg.Seq,
		)

	switch statusFilter {
	case ReceiptFilterRead:
		listQ = listQ.Where("cm.last_read_seq >= ?", msg.Seq)
	case ReceiptFilterUnread:
		listQ = listQ.Where("cm.last_read_seq < ?", msg.Seq)
	case ReceiptFilterDelivered:
		listQ = listQ.Where("(cm.last_delivered_seq >= ? OR cm.last_read_seq >= ?)", msg.Seq, msg.Seq)
	case ReceiptFilterPending:
		listQ = listQ.Where("(cm.last_delivered_seq < ? AND cm.last_read_seq < ?)", msg.Seq, msg.Seq)
	}
	if cursorID > 0 {
		listQ = listQ.Where("cm.id < ?", cursorID)
	}

	var rows []ReceiptMemberRow
	if err := listQ.Order("cm.id DESC").Limit(limit + 1).Scan(&rows).Error; err != nil {
		return ReceiptSummaryCounts{}, nil, fmt.Errorf("list group message receipts: %w", err)
	}
	return summary, rows, nil
}
