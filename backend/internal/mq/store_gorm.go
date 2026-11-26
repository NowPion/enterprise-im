package mq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/repository"
)

const receiptInitBatchSize = 100

var ErrMessageNotFound = errors.New("committed message not found")

// GormStore implements MessageStore and AuditStore backed by MySQL (GORM) and Redis.
type GormStore struct {
	db  *gorm.DB
	rdb *redis.Client
}

var (
	_ MessageStore = (*GormStore)(nil)
	_ AuditStore   = (*GormStore)(nil)
)

// NewGormStore constructs a GormStore. Either db or rdb may be nil when not used.
func NewGormStore(db *gorm.DB, rdb *redis.Client) *GormStore {
	return &GormStore{db: db, rdb: rdb}
}

// GetCommittedMessage loads a persisted message from MySQL by Snowflake ID.
func (s *GormStore) GetCommittedMessage(ctx context.Context, msgID uint64) (*CommittedMessage, error) {
	if s == nil || s.db == nil {
		return nil, repository.ErrRepositoryUnavailable
	}
	if msgID == 0 {
		return nil, ErrMessageNotFound
	}
	var msg model.Message
	err := s.db.WithContext(ctx).Where("id = ?", msgID).Take(&msg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrMessageNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load committed message %d: %w", msgID, err)
	}

	extra := json.RawMessage("{}")
	if trimmed := bytes.TrimSpace(msg.Extra); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		extra = json.RawMessage(trimmed)
	}
	var recalledAt *string
	if msg.RecalledAt != nil {
		v := msg.RecalledAt.UTC().Format(utcMillisLayout)
		recalledAt = &v
	}

	return &CommittedMessage{
		MsgID:          msg.ID,
		ClientMsgID:    msg.ClientMsgID,
		ConversationID: msg.ConversationID,
		Seq:            msg.Seq,
		SenderID:       msg.SenderID,
		MsgType:        msg.MsgType,
		Content:        msg.Content,
		Extra:          extra,
		Status:         msg.Status,
		CreatedAt:      msg.CreatedAt.UTC().Format(utcMillisLayout),
		RecalledAt:     recalledAt,
	}, nil
}

// ListConversationActiveRecipients loads conversation metadata and currently active member user IDs.
func (s *GormStore) ListConversationActiveRecipients(ctx context.Context, convID uint64) (*ConversationMeta, []uint64, error) {
	if s == nil || s.db == nil {
		return nil, nil, repository.ErrRepositoryUnavailable
	}
	if convID == 0 {
		return nil, nil, repository.ErrConversationNotFound
	}
	var conv model.Conversation
	err := s.db.WithContext(ctx).Where("id = ?", convID).Take(&conv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, repository.ErrConversationNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load conversation %d: %w", convID, err)
	}

	recipientIDs, err := repository.ListActiveMemberUserIDsTx(ctx, s.db, &conv)
	if err != nil {
		return nil, nil, err
	}
	return &ConversationMeta{
		ID:        conv.ID,
		Type:      conv.Type,
		DirectKey: conv.DirectKey,
		GroupID:   conv.GroupID,
	}, recipientIDs, nil
}

// InitDelayedGroupReceipts idempotently inserts initial 'sent' message_receipts rows in batches
// without touching any receipts already promoted to 'delivered' or 'read'.
func (s *GormStore) InitDelayedGroupReceipts(
	ctx context.Context,
	convID, msgID, seq uint64,
	recipientIDs []uint64,
	now time.Time,
) error {
	if s == nil || s.db == nil {
		return repository.ErrRepositoryUnavailable
	}
	unique := dedupePositiveUint64(recipientIDs)
	if convID == 0 || msgID == 0 || seq == 0 || len(unique) == 0 {
		return nil
	}
	ts := now.UTC().Truncate(time.Millisecond)
	rows := make([]model.MessageReceipt, 0, len(unique))
	for _, uid := range unique {
		rows = append(rows, model.MessageReceipt{
			MessageID:      msgID,
			ConversationID: convID,
			Seq:            seq,
			UserID:         uid,
			Status:         model.ReceiptStatusSent,
			CreatedAt:      ts,
			UpdatedAt:      ts,
		})
	}
	for start := 0; start < len(rows); start += receiptInitBatchSize {
		end := start + receiptInitBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		if err := s.db.WithContext(ctx).
			Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "message_id"}, {Name: "user_id"}},
				DoNothing: true,
			}).
			Create(&chunk).Error; err != nil {
			return fmt.Errorf("init delayed group receipts: %w", err)
		}
	}
	return nil
}

// EnsureSingleReceipt idempotently ensures a single-chat peer receipt row exists.
func (s *GormStore) EnsureSingleReceipt(
	ctx context.Context,
	convID, msgID, seq, peerID uint64,
	now time.Time,
) error {
	if peerID == 0 {
		return nil
	}
	return s.InitDelayedGroupReceipts(ctx, convID, msgID, seq, []uint64{peerID}, now)
}

// EnsureDedupeRecord compensates missing Redis im:dedupe cache entries after MySQL commit.
func (s *GormStore) EnsureDedupeRecord(
	ctx context.Context,
	senderID uint64,
	clientMsgID string,
	msgID uint64,
	convID uint64,
	seq uint64,
	status string,
) error {
	if s == nil || s.rdb == nil || senderID == 0 || strings.TrimSpace(clientMsgID) == "" {
		return nil
	}
	if status != model.MessageStatusNormal && status != model.MessageStatusRecalled {
		status = model.MessageStatusNormal
	}
	val := repository.FormatDedupeValue(repository.DedupeRecord{
		MsgID:          msgID,
		ConversationID: convID,
		Seq:            seq,
		Status:         status,
		DeliveryStatus: model.ReceiptStatusSent,
	})
	return s.rdb.SetNX(ctx, repository.DedupeKey(senderID, clientMsgID), val, defaultIdempotencyTTL).Err()
}

// AppendAuditLog inserts an immutable audit row idempotently on uk_audit_event_id.
func (s *GormStore) AppendAuditLog(ctx context.Context, entry *model.AuditLog) error {
	if s == nil || s.db == nil {
		return repository.ErrRepositoryUnavailable
	}
	if entry == nil || strings.TrimSpace(entry.EventID) == "" {
		return fmt.Errorf("%w: audit log event_id is required", ErrInvalidStreamPayload)
	}
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "event_id"}},
			DoNothing: true,
		}).
		Create(entry).Error
}
