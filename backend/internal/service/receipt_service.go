package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

const (
	DefaultReceiptPageLimit = 20
	MaxReceiptPageLimit     = 100

	receiptListCursorScope = "msg_receipts"

	// ReceiptEventKindDelivered and ReceiptEventKindRead align with `im:stream:events` in docs/architecture.md.
	ReceiptEventKindDelivered = "delivered"
	ReceiptEventKindRead      = "read"
)

// ErrReceiptPublisherUnwired indicates receipt state committed to MySQL while the
// Redis Stream producer (`mq.Producer`) is not yet wired on this service instance.
var ErrReceiptPublisherUnwired = errors.New("receipt event publisher not configured")

// ReceiptEventPublisher publishes post-commit receipt events toward `im:stream:events`.
// It follows the mq.Producer style so Stage 7 integration can wire the real Stream producer
// without changing receipt service logic.
//
// Failure semantics:
//   - MySQL receipt/watermark mutations commit BEFORE calling PublishEvent.
//   - If publisher is nil or returns an error, the failure is logged and exposed via
//     result metadata (EventPublished=false, EventPublishErr!=nil), while committed DB state
//     is NEVER rolled back and Hub direct-push fallback is NOT performed (no double push).
type ReceiptEventPublisher interface {
	PublishEvent(ctx context.Context, event ReceiptEvent) error
}

// ReceiptEvent represents a receipt/watermark event published to `im:stream:events`.
// Single-message delivered_ack includes MsgID and Seq; batch delivered/read leave them nil.
type ReceiptEvent struct {
	Kind             string   `json:"kind"`
	ConversationID   uint64   `json:"conversation_id,string"`
	UserID           uint64   `json:"user_id,string"`
	TargetUserIDs    []uint64 `json:"target_user_ids"`
	MsgID            *uint64  `json:"msg_id,string"`
	Seq              *uint64  `json:"seq,string"`
	LastDeliveredSeq uint64   `json:"last_delivered_seq,string"`
	LastReadSeq      uint64   `json:"last_read_seq,string"`
	UpdatedAt        string   `json:"updated_at"`
}

// ReceiptStore defines persistence operations required by ReceiptService.
// *repository.ReceiptRepository satisfies this interface directly.
type ReceiptStore interface {
	WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error
	GetConversationByID(ctx context.Context, convID uint64) (*model.Conversation, error)
	GetConversationMember(ctx context.Context, convID, userID uint64) (*model.ConversationMember, error)
	GetGroupMember(ctx context.Context, groupID, userID uint64) (*model.GroupMember, error)
	BatchGetUsersByIDs(ctx context.Context, userIDs []uint64) (map[uint64]model.User, error)
	GetMessageInConversation(ctx context.Context, convID, msgID uint64) (*model.Message, error)
	LockConversationByIDTx(ctx context.Context, tx *gorm.DB, convID uint64) (*model.Conversation, error)
	HasGroupMemberTx(ctx context.Context, tx *gorm.DB, groupID, userID uint64) (bool, error)
	LockConversationMemberTx(ctx context.Context, tx *gorm.DB, convID, userID uint64) (*model.ConversationMember, error)
	GetMessageInConversationTx(ctx context.Context, tx *gorm.DB, convID, msgID uint64) (*model.Message, error)
	LockMessageReceiptTx(ctx context.Context, tx *gorm.DB, msgID, userID uint64) (*model.MessageReceipt, error)
	MarkSingleReceiptDeliveredTx(ctx context.Context, tx *gorm.DB, receiptID uint64, now time.Time) (bool, error)
	MarkReceiptsDeliveredUpToSeqTx(ctx context.Context, tx *gorm.DB, convID, userID, targetSeq uint64, now time.Time) (int64, error)
	MarkReceiptsReadUpToSeqTx(ctx context.Context, tx *gorm.DB, convID, userID, targetSeq uint64, now time.Time) (int64, error)
	UpdateMemberDeliveredWatermarkTx(ctx context.Context, tx *gorm.DB, memberID, lastDeliveredSeq uint64, now time.Time) error
	UpdateMemberReadWatermarkTx(ctx context.Context, tx *gorm.DB, memberID, lastReadSeq, lastDeliveredSeq uint64, now time.Time) error
	ListDistinctSendersInSeqRangeTx(ctx context.Context, tx *gorm.DB, convID, fromSeqExclusive, toSeqInclusive, excludeUserID uint64) ([]uint64, error)
	QueryMessageReceiptDetails(ctx context.Context, conv *model.Conversation, msg *model.Message, statusFilter string, cursorID uint64, limit int) (repository.ReceiptSummaryCounts, []repository.ReceiptMemberRow, error)
}

// AckDeliveredResult holds the outcome of WS msg.delivered_ack.
type AckDeliveredResult struct {
	ConversationID   uint64 `json:"conversation_id,string"`
	MsgID            uint64 `json:"msg_id,string"`
	Seq              uint64 `json:"seq,string"`
	DeliveryStatus   string `json:"delivery_status"`
	LastDeliveredSeq uint64 `json:"last_delivered_seq,string"`
	LastReadSeq      uint64 `json:"last_read_seq,string"`
	UpdatedAt        string `json:"updated_at"`
	Changed          bool   `json:"-"`
	EventPublished   bool   `json:"-"`
	EventPublishErr  error  `json:"-"`
}

// MarkDeliveredResult matches HTTP POST /api/v1/conversations/{conversation_id}/delivered response data.
type MarkDeliveredResult struct {
	ConversationID   uint64 `json:"conversation_id,string"`
	LastDeliveredSeq uint64 `json:"last_delivered_seq,string"`
	LastReadSeq      uint64 `json:"last_read_seq,string"`
	UnreadCount      uint32 `json:"unread_count"`
	DeliveredAt      string `json:"delivered_at"`
	Changed          bool   `json:"-"`
	EventPublished   bool   `json:"-"`
	EventPublishErr  error  `json:"-"`
}

// MarkReadResult matches HTTP POST /api/v1/conversations/{conversation_id}/read response data
// and WS msg.read_ack processing output.
type MarkReadResult struct {
	ConversationID   uint64 `json:"conversation_id,string"`
	LastReadSeq      uint64 `json:"last_read_seq,string"`
	LastDeliveredSeq uint64 `json:"last_delivered_seq,string"`
	UnreadCount      uint32 `json:"unread_count"`
	ReadAt           string `json:"read_at"`
	Changed          bool   `json:"-"`
	EventPublished   bool   `json:"-"`
	EventPublishErr  error  `json:"-"`
}

// MessageReceiptItemDTO matches each item in GET /api/v1/conversations/{conversation_id}/messages/{msg_id}/receipts.
type MessageReceiptItemDTO struct {
	User             ChatUserDTO `json:"user"`
	Role             *string     `json:"role"`
	DeliveryStatus   string      `json:"delivery_status"`
	LastDeliveredSeq uint64      `json:"last_delivered_seq,string"`
	LastReadSeq      uint64      `json:"last_read_seq,string"`
	DeliveredAt      *string     `json:"delivered_at"`
	ReadAt           *string     `json:"read_at"`
}

// MessageReceiptListDTO matches GET /api/v1/conversations/{conversation_id}/messages/{msg_id}/receipts response data.
type MessageReceiptListDTO struct {
	MsgID          uint64                  `json:"msg_id,string"`
	ConversationID uint64                  `json:"conversation_id,string"`
	Seq            uint64                  `json:"seq,string"`
	TotalMembers   uint32                  `json:"total_members"`
	DeliveredCount uint32                  `json:"delivered_count"`
	ReadCount      uint32                  `json:"read_count"`
	UnreadCount    uint32                  `json:"unread_count"`
	Items          []MessageReceiptItemDTO `json:"items"`
	NextCursor     *string                 `json:"next_cursor"`
	HasMore        bool                    `json:"has_more"`
}

type messageReceiptCursorPayload struct {
	Version        int    `json:"v"`
	Scope          string `json:"s"`
	UserID         uint64 `json:"u,string"`
	ConversationID uint64 `json:"c,string"`
	MessageID      uint64 `json:"m,string"`
	Status         string `json:"st"`
	LastID         uint64 `json:"id,string"`
}

// ReceiptService implements the Stage 7 delivered/read receipt state machine and receipt queries.
type ReceiptService struct {
	store     ReceiptStore
	presence  OnlineStatusProvider
	publisher ReceiptEventPublisher
	nowFn     func() time.Time
	log       *slog.Logger
}

// NewReceiptService constructs a ReceiptService.
func NewReceiptService(
	store ReceiptStore,
	presence OnlineStatusProvider,
	publisher ReceiptEventPublisher,
	log *slog.Logger,
) *ReceiptService {
	if log == nil {
		log = slog.Default()
	}
	return &ReceiptService{
		store:     store,
		presence:  presence,
		publisher: publisher,
		nowFn:     func() time.Time { return time.Now().UTC() },
		log:       log,
	}
}

// SetEventPublisher attaches or replaces the receipt event publisher after construction.
func (s *ReceiptService) SetEventPublisher(publisher ReceiptEventPublisher) {
	if s != nil {
		s.publisher = publisher
	}
}

// AckDeliveredWS processes a per-message WS `msg.delivered_ack` frame.
// It validates membership and message seq, transitions single-chat receipt sent->delivered
// without regressing read receipts or last_delivered_seq, and emits im:stream:events(kind=delivered).
func (s *ReceiptService) AckDeliveredWS(
	ctx context.Context,
	userID uint64,
	convID uint64,
	msgID uint64,
	seq uint64,
) (*AckDeliveredResult, *errcode.Error) {
	if userID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 || msgID == 0 || seq == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid conversation_id, msg_id, or seq")
	}
	if s == nil || s.store == nil {
		return nil, errcode.ErrMySQLUnavailable
	}

	var (
		res     AckDeliveredResult
		targets []uint64
		now     time.Time
	)

	txErr := s.store.WithTx(ctx, func(tx *gorm.DB) error {
		conv, member, err := s.lockMembershipTx(ctx, tx, userID, convID)
		if err != nil {
			return s.mapStoreError(ctx, userID, "ack_delivered_lock_member", err)
		}

		msg, err := s.store.GetMessageInConversationTx(ctx, tx, convID, msgID)
		if err != nil {
			return s.mapStoreError(ctx, userID, "ack_delivered_get_msg", err)
		}
		if seq > conv.LastSeq {
			return newServiceError(errcode.ErrWatermarkExceedsSeq)
		}
		if seq != msg.Seq {
			return newServiceError(errcode.ErrWSStateConflict.WithMessage("seq does not match message"))
		}

		now = s.nowFn().UTC().Truncate(time.Millisecond)
		prevDeliveredSeq := member.LastDeliveredSeq
		newDeliveredSeq := max(prevDeliveredSeq, msg.Seq)

		receiptTransitioned := false
		deliveryStatus := deriveWatermarkStatus(newDeliveredSeq, member.LastReadSeq, msg.Seq)

		if conv.Type == model.ConversationTypeSingle {
			rec, recErr := s.store.LockMessageReceiptTx(ctx, tx, msg.ID, userID)
			if recErr != nil {
				return s.mapStoreError(ctx, userID, "ack_delivered_lock_receipt", recErr)
			}
			if rec != nil {
				switch rec.Status {
				case model.ReceiptStatusSent:
					transitioned, updErr := s.store.MarkSingleReceiptDeliveredTx(ctx, tx, rec.ID, now)
					if updErr != nil {
						return s.mapStoreError(ctx, userID, "ack_delivered_update_receipt", updErr)
					}
					receiptTransitioned = transitioned
					deliveryStatus = model.ReceiptStatusDelivered
				case model.ReceiptStatusDelivered:
					deliveryStatus = model.ReceiptStatusDelivered
				case model.ReceiptStatusRead:
					deliveryStatus = model.ReceiptStatusRead
				}
			}
		}

		if newDeliveredSeq > prevDeliveredSeq {
			if updErr := s.store.UpdateMemberDeliveredWatermarkTx(ctx, tx, member.ID, newDeliveredSeq, now); updErr != nil {
				return s.mapStoreError(ctx, userID, "ack_delivered_update_watermark", updErr)
			}
		}

		changed := (newDeliveredSeq > prevDeliveredSeq) || receiptTransitioned
		effectiveUpdatedAt := member.UpdatedAt
		if changed {
			effectiveUpdatedAt = now
			targets = s.singleMessageTargetUsers(conv, msg, userID)
		}

		res = AckDeliveredResult{
			ConversationID:   convID,
			MsgID:            msg.ID,
			Seq:              msg.Seq,
			DeliveryStatus:   deliveryStatus,
			LastDeliveredSeq: newDeliveredSeq,
			LastReadSeq:      member.LastReadSeq,
			UpdatedAt:        FormatUTCMillis(effectiveUpdatedAt),
			Changed:          changed,
		}
		return nil
	})
	if txErr != nil {
		return nil, s.unwrapServiceError(ctx, userID, "ack_delivered_ws", txErr)
	}

	if res.Changed {
		msgIDCopy := res.MsgID
		seqCopy := res.Seq
		res.EventPublished, res.EventPublishErr = s.publishEvent(ctx, ReceiptEvent{
			Kind:             ReceiptEventKindDelivered,
			ConversationID:   convID,
			UserID:           userID,
			TargetUserIDs:    targets,
			MsgID:            &msgIDCopy,
			Seq:              &seqCopy,
			LastDeliveredSeq: res.LastDeliveredSeq,
			LastReadSeq:      res.LastReadSeq,
			UpdatedAt:        res.UpdatedAt,
		})
	}
	return &res, nil
}

// MarkDelivered handles HTTP POST /api/v1/conversations/{conversation_id}/delivered.
// It advances last_delivered_seq monotonically for offline sync compensation and batch-updates
// pending single-chat receipts up to targetSeq.
func (s *ReceiptService) MarkDelivered(
	ctx context.Context,
	userID uint64,
	convID uint64,
	targetSeq uint64,
) (*MarkDeliveredResult, *errcode.Error) {
	if userID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid conversation_id")
	}
	if s == nil || s.store == nil {
		return nil, errcode.ErrMySQLUnavailable
	}

	var (
		res     MarkDeliveredResult
		targets []uint64
		now     time.Time
	)

	txErr := s.store.WithTx(ctx, func(tx *gorm.DB) error {
		conv, member, err := s.lockMembershipTx(ctx, tx, userID, convID)
		if err != nil {
			return s.mapStoreError(ctx, userID, "mark_delivered_lock_member", err)
		}
		if targetSeq > conv.LastSeq {
			return newServiceError(errcode.ErrWatermarkExceedsSeq)
		}

		now = s.nowFn().UTC().Truncate(time.Millisecond)
		prevDeliveredSeq := member.LastDeliveredSeq
		newDeliveredSeq := max(prevDeliveredSeq, targetSeq)

		var receiptsUpdated int64
		if conv.Type == model.ConversationTypeSingle && targetSeq > 0 {
			updated, updErr := s.store.MarkReceiptsDeliveredUpToSeqTx(ctx, tx, convID, userID, targetSeq, now)
			if updErr != nil {
				return s.mapStoreError(ctx, userID, "mark_delivered_update_receipts", updErr)
			}
			receiptsUpdated = updated
		}

		if newDeliveredSeq > prevDeliveredSeq {
			if updErr := s.store.UpdateMemberDeliveredWatermarkTx(ctx, tx, member.ID, newDeliveredSeq, now); updErr != nil {
				return s.mapStoreError(ctx, userID, "mark_delivered_update_watermark", updErr)
			}
		}

		changed := (newDeliveredSeq > prevDeliveredSeq) || receiptsUpdated > 0
		effectiveUpdatedAt := member.UpdatedAt
		if changed {
			effectiveUpdatedAt = now
			t, tErr := s.rangeTargetUsersTx(ctx, tx, conv, prevDeliveredSeq, newDeliveredSeq, userID)
			if tErr != nil {
				return s.mapStoreError(ctx, userID, "mark_delivered_target_users", tErr)
			}
			targets = t
		}

		res = MarkDeliveredResult{
			ConversationID:   convID,
			LastDeliveredSeq: newDeliveredSeq,
			LastReadSeq:      member.LastReadSeq,
			UnreadCount:      member.UnreadCount,
			DeliveredAt:      FormatUTCMillis(effectiveUpdatedAt),
			Changed:          changed,
		}
		return nil
	})
	if txErr != nil {
		return nil, s.unwrapServiceError(ctx, userID, "mark_delivered", txErr)
	}

	if res.Changed {
		res.EventPublished, res.EventPublishErr = s.publishEvent(ctx, ReceiptEvent{
			Kind:             ReceiptEventKindDelivered,
			ConversationID:   convID,
			UserID:           userID,
			TargetUserIDs:    targets,
			MsgID:            nil,
			Seq:              nil,
			LastDeliveredSeq: res.LastDeliveredSeq,
			LastReadSeq:      res.LastReadSeq,
			UpdatedAt:        res.DeliveredAt,
		})
	}
	return &res, nil
}

// AckReadWS handles WS `msg.read_ack` using the exact same state machine as HTTP POST .../read.
func (s *ReceiptService) AckReadWS(
	ctx context.Context,
	userID uint64,
	convID uint64,
	lastReadSeq uint64,
) (*MarkReadResult, *errcode.Error) {
	return s.MarkRead(ctx, userID, convID, lastReadSeq)
}

// MarkRead handles HTTP POST /api/v1/conversations/{conversation_id}/read and WS `msg.read_ack`.
// It rejects targetSeq > conv.LastSeq with 13006 without modifying DB, keeps smaller/duplicate
// watermarks idempotent without regression, batch-marks receipts read, clears unread_count=0 when
// advancing, and publishes im:stream:events(kind=read).
func (s *ReceiptService) MarkRead(
	ctx context.Context,
	userID uint64,
	convID uint64,
	targetSeq uint64,
) (*MarkReadResult, *errcode.Error) {
	if userID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid conversation_id")
	}
	if s == nil || s.store == nil {
		return nil, errcode.ErrMySQLUnavailable
	}

	var (
		res     MarkReadResult
		targets []uint64
		now     time.Time
	)

	txErr := s.store.WithTx(ctx, func(tx *gorm.DB) error {
		conv, member, err := s.lockMembershipTx(ctx, tx, userID, convID)
		if err != nil {
			return s.mapStoreError(ctx, userID, "mark_read_lock_member", err)
		}
		if targetSeq > conv.LastSeq {
			return newServiceError(errcode.ErrWatermarkExceedsSeq)
		}

		now = s.nowFn().UTC().Truncate(time.Millisecond)
		prevReadSeq := member.LastReadSeq
		prevDeliveredSeq := member.LastDeliveredSeq
		newReadSeq := max(prevReadSeq, targetSeq)
		newDeliveredSeq := max(prevDeliveredSeq, newReadSeq)

		var receiptsUpdated int64
		if conv.Type == model.ConversationTypeSingle && targetSeq > 0 {
			updated, updErr := s.store.MarkReceiptsReadUpToSeqTx(ctx, tx, convID, userID, targetSeq, now)
			if updErr != nil {
				return s.mapStoreError(ctx, userID, "mark_read_update_receipts", updErr)
			}
			receiptsUpdated = updated
		}

		unreadCount := member.UnreadCount
		advancedWatermark := newReadSeq > prevReadSeq
		shouldClearUnread := advancedWatermark || (targetSeq == conv.LastSeq && member.UnreadCount > 0)
		if shouldClearUnread {
			if updErr := s.store.UpdateMemberReadWatermarkTx(ctx, tx, member.ID, newReadSeq, newDeliveredSeq, now); updErr != nil {
				return s.mapStoreError(ctx, userID, "mark_read_update_watermark", updErr)
			}
			unreadCount = 0
		}

		changed := shouldClearUnread || receiptsUpdated > 0
		effectiveReadAt := member.UpdatedAt
		if changed {
			effectiveReadAt = now
			t, tErr := s.rangeTargetUsersTx(ctx, tx, conv, prevReadSeq, newReadSeq, userID)
			if tErr != nil {
				return s.mapStoreError(ctx, userID, "mark_read_target_users", tErr)
			}
			targets = t
		}

		res = MarkReadResult{
			ConversationID:   convID,
			LastReadSeq:      newReadSeq,
			LastDeliveredSeq: newDeliveredSeq,
			UnreadCount:      unreadCount,
			ReadAt:           FormatUTCMillis(effectiveReadAt),
			Changed:          changed,
		}
		return nil
	})
	if txErr != nil {
		return nil, s.unwrapServiceError(ctx, userID, "mark_read", txErr)
	}

	if res.Changed {
		res.EventPublished, res.EventPublishErr = s.publishEvent(ctx, ReceiptEvent{
			Kind:             ReceiptEventKindRead,
			ConversationID:   convID,
			UserID:           userID,
			TargetUserIDs:    targets,
			MsgID:            nil,
			Seq:              nil,
			LastDeliveredSeq: res.LastDeliveredSeq,
			LastReadSeq:      res.LastReadSeq,
			UpdatedAt:        res.ReadAt,
		})
	}
	return &res, nil
}

// ListMessageReceipts returns aggregate receipt counts and cursor-paginated member details
// for GET /api/v1/conversations/{conversation_id}/messages/{msg_id}/receipts.
func (s *ReceiptService) ListMessageReceipts(
	ctx context.Context,
	userID uint64,
	convID uint64,
	msgID uint64,
	statusFilter string,
	cursor string,
	limit int,
) (*MessageReceiptListDTO, *errcode.Error) {
	if userID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 || msgID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid conversation_id or msg_id")
	}

	normalizedStatus := strings.TrimSpace(statusFilter)
	switch normalizedStatus {
	case "", repository.ReceiptFilterRead, repository.ReceiptFilterUnread, repository.ReceiptFilterDelivered, repository.ReceiptFilterPending:
	default:
		return nil, errcode.ErrInvalidValue.WithMessage("status must be read, unread, delivered, or pending")
	}

	if limit == 0 {
		limit = DefaultReceiptPageLimit
	}
	if limit < 1 || limit > MaxReceiptPageLimit {
		return nil, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}
	if s == nil || s.store == nil {
		return nil, errcode.ErrMySQLUnavailable
	}

	var cursorID uint64
	if trimmed := strings.TrimSpace(cursor); trimmed != "" {
		decoded, decErr := decodeMessageReceiptCursor(trimmed, userID, convID, msgID, normalizedStatus)
		if decErr != nil {
			return nil, decErr
		}
		cursorID = decoded
	}

	conv, err := s.store.GetConversationByID(ctx, convID)
	if err != nil {
		return nil, s.mapStoreError(ctx, userID, "list_receipts_get_conv", err)
	}
	if _, err := s.store.GetConversationMember(ctx, convID, userID); err != nil {
		return nil, s.mapStoreError(ctx, userID, "list_receipts_check_cm", err)
	}
	if conv.Type == model.ConversationTypeGroup {
		if conv.GroupID == nil || *conv.GroupID == 0 {
			return nil, errcode.ErrGroupNotFound
		}
		if _, err := s.store.GetGroupMember(ctx, *conv.GroupID, userID); err != nil {
			return nil, s.mapStoreError(ctx, userID, "list_receipts_check_gm", err)
		}
	}

	msg, err := s.store.GetMessageInConversation(ctx, convID, msgID)
	if err != nil {
		return nil, s.mapStoreError(ctx, userID, "list_receipts_get_msg", err)
	}

	summary, rows, err := s.store.QueryMessageReceiptDetails(ctx, conv, msg, normalizedStatus, cursorID, limit)
	if err != nil {
		return nil, s.mapStoreError(ctx, userID, "list_receipts_query", err)
	}

	hasMore := false
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	userIDs := make([]uint64, 0, len(rows))
	for _, row := range rows {
		if row.UserID > 0 {
			userIDs = append(userIDs, row.UserID)
		}
	}

	usersMap, err := s.store.BatchGetUsersByIDs(ctx, userIDs)
	if err != nil {
		return nil, s.mapStoreError(ctx, userID, "list_receipts_batch_users", err)
	}
	onlineMap := s.lookupOnlineBatch(ctx, userIDs)

	items := make([]MessageReceiptItemDTO, 0, len(rows))
	for _, row := range rows {
		u, exists := usersMap[row.UserID]
		if !exists {
			continue
		}
		items = append(items, MessageReceiptItemDTO{
			User:             toChatUserDTO(u, onlineMap[row.UserID]),
			Role:             row.Role,
			DeliveryStatus:   row.DeliveryStatus,
			LastDeliveredSeq: row.LastDeliveredSeq,
			LastReadSeq:      row.LastReadSeq,
			DeliveredAt:      FormatUTCMillisPtr(row.DeliveredAt),
			ReadAt:           FormatUTCMillisPtr(row.ReadAt),
		})
	}

	var nextCursor *string
	if hasMore && len(rows) > 0 {
		nc := encodeMessageReceiptCursor(userID, convID, msgID, normalizedStatus, rows[len(rows)-1].CMID)
		nextCursor = &nc
	}

	return &MessageReceiptListDTO{
		MsgID:          msg.ID,
		ConversationID: conv.ID,
		Seq:            msg.Seq,
		TotalMembers:   summary.TotalMembers,
		DeliveredCount: summary.DeliveredCount,
		ReadCount:      summary.ReadCount,
		UnreadCount:    summary.UnreadCount,
		Items:          items,
		NextCursor:     nextCursor,
		HasMore:        hasMore,
	}, nil
}

func (s *ReceiptService) lockMembershipTx(
	ctx context.Context,
	tx *gorm.DB,
	userID uint64,
	convID uint64,
) (*model.Conversation, *model.ConversationMember, error) {
	conv, err := s.store.LockConversationByIDTx(ctx, tx, convID)
	if err != nil {
		return nil, nil, err
	}
	if conv.Type == model.ConversationTypeGroup {
		if conv.GroupID == nil || *conv.GroupID == 0 {
			return nil, nil, repository.ErrGroupNotFound
		}
		ok, gmErr := s.store.HasGroupMemberTx(ctx, tx, *conv.GroupID, userID)
		if gmErr != nil {
			return nil, nil, gmErr
		}
		if !ok {
			return nil, nil, repository.ErrNotConversationMember
		}
	}
	member, err := s.store.LockConversationMemberTx(ctx, tx, convID, userID)
	if err != nil {
		return nil, nil, err
	}
	return conv, member, nil
}

func (s *ReceiptService) singleMessageTargetUsers(conv *model.Conversation, msg *model.Message, ackUserID uint64) []uint64 {
	if msg != nil && msg.SenderID > 0 && msg.SenderID != ackUserID {
		return []uint64{msg.SenderID}
	}
	if conv != nil && conv.Type == model.ConversationTypeSingle {
		if peerID, ok := repository.ParseDirectKeyPeer(conv.DirectKey, ackUserID); ok && peerID != ackUserID {
			return []uint64{peerID}
		}
	}
	return nil
}

func (s *ReceiptService) rangeTargetUsersTx(
	ctx context.Context,
	tx *gorm.DB,
	conv *model.Conversation,
	fromSeqExclusive uint64,
	toSeqInclusive uint64,
	ackUserID uint64,
) ([]uint64, error) {
	if conv == nil {
		return nil, nil
	}
	if conv.Type == model.ConversationTypeSingle {
		if peerID, ok := repository.ParseDirectKeyPeer(conv.DirectKey, ackUserID); ok && peerID != ackUserID {
			return []uint64{peerID}, nil
		}
		return nil, nil
	}
	return s.store.ListDistinctSendersInSeqRangeTx(ctx, tx, conv.ID, fromSeqExclusive, toSeqInclusive, ackUserID)
}

func (s *ReceiptService) publishEvent(ctx context.Context, event ReceiptEvent) (bool, error) {
	if s.publisher == nil {
		s.log.WarnContext(ctx, "receipt_event_publisher_unwired",
			slog.String("kind", event.Kind),
			slog.Uint64("conversation_id", event.ConversationID),
			slog.Uint64("user_id", event.UserID),
			slog.Uint64("last_delivered_seq", event.LastDeliveredSeq),
			slog.Uint64("last_read_seq", event.LastReadSeq),
		)
		return false, ErrReceiptPublisherUnwired
	}

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := s.publisher.PublishEvent(pubCtx, event); err != nil {
		s.log.ErrorContext(ctx, "receipt_event_publish_failed",
			slog.String("kind", event.Kind),
			slog.Uint64("conversation_id", event.ConversationID),
			slog.Uint64("user_id", event.UserID),
			slog.Uint64("last_delivered_seq", event.LastDeliveredSeq),
			slog.Uint64("last_read_seq", event.LastReadSeq),
			slog.Any("error", err),
		)
		return false, err
	}
	return true, nil
}

func (s *ReceiptService) lookupOnlineBatch(ctx context.Context, userIDs []uint64) map[uint64]bool {
	if s.presence == nil || len(userIDs) == 0 {
		return map[uint64]bool{}
	}
	onlineMap, err := s.presence.GetOnlineStatusBatch(ctx, userIDs)
	if err != nil {
		s.log.WarnContext(ctx, "receipt_presence_lookup_failed",
			slog.Int("user_count", len(userIDs)),
			slog.Any("error", err),
		)
		return map[uint64]bool{}
	}
	return onlineMap
}

func (s *ReceiptService) mapStoreError(ctx context.Context, userID uint64, op string, err error) *errcode.Error {
	var svcErr *ServiceError
	if errors.As(err, &svcErr) && svcErr.BizErr != nil {
		return svcErr.BizErr
	}
	var bizErr *errcode.Error
	if errors.As(err, &bizErr) && bizErr != nil {
		return bizErr
	}
	switch {
	case errors.Is(err, repository.ErrConversationNotFound):
		return errcode.ErrConversationNotFound
	case errors.Is(err, repository.ErrNotConversationMember):
		return errcode.ErrNotConversationMember
	case errors.Is(err, repository.ErrGroupNotFound):
		return errcode.ErrGroupNotFound
	case errors.Is(err, repository.ErrMessageNotFound):
		return errcode.ErrMessageNotFound
	}
	s.log.ErrorContext(ctx, op+"_failed",
		slog.Uint64("user_id", userID),
		slog.Any("error", err),
	)
	if isInfrastructureUnavailableErr(err) {
		return errcode.ErrMySQLUnavailable
	}
	return errcode.ErrInternalConsistency
}

func (s *ReceiptService) unwrapServiceError(ctx context.Context, userID uint64, op string, err error) *errcode.Error {
	var svcErr *ServiceError
	if errors.As(err, &svcErr) && svcErr.BizErr != nil {
		return svcErr.BizErr
	}
	var bizErr *errcode.Error
	if errors.As(err, &bizErr) && bizErr != nil {
		return bizErr
	}
	return s.mapStoreError(ctx, userID, op, err)
}

func deriveWatermarkStatus(lastDeliveredSeq, lastReadSeq, msgSeq uint64) string {
	if lastReadSeq >= msgSeq {
		return model.ReceiptStatusRead
	}
	if lastDeliveredSeq >= msgSeq {
		return model.ReceiptStatusDelivered
	}
	return model.ReceiptStatusSent
}

func encodeMessageReceiptCursor(userID, convID, msgID uint64, status string, lastCMID uint64) string {
	raw, _ := json.Marshal(messageReceiptCursorPayload{
		Version:        1,
		Scope:          receiptListCursorScope,
		UserID:         userID,
		ConversationID: convID,
		MessageID:      msgID,
		Status:         status,
		LastID:         lastCMID,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeMessageReceiptCursor(token string, expectedUserID, expectedConvID, expectedMsgID uint64, expectedStatus string) (uint64, *errcode.Error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	var payload messageReceiptCursorPayload
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return 0, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return 0, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	if payload.Version != 1 ||
		payload.Scope != receiptListCursorScope ||
		payload.UserID != expectedUserID ||
		payload.ConversationID != expectedConvID ||
		payload.MessageID != expectedMsgID ||
		payload.Status != expectedStatus ||
		payload.LastID == 0 {
		return 0, errcode.ErrInvalidValue.WithMessage("cursor does not match query scope")
	}
	return payload.LastID, nil
}
