package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

const (
	DefaultMessagePageLimit = 20
	DefaultMessageSyncLimit = 100
	MaxMessagePageLimit     = 100

	maxClientMsgIDBytes   = 64
	maxMessageContentByte = 65535

	defaultDedupeTTL      = 24 * time.Hour
	defaultDedupeLockTTL  = 10 * time.Second
	defaultDedupeWaitTime = 500 * time.Millisecond
	defaultRecallWindow   = 2 * time.Minute
)

// SnowflakeIDGenerator generates unique Snowflake message IDs.
// *snowflake.Generator satisfies this interface directly.
type SnowflakeIDGenerator interface {
	NextID() (uint64, error)
}

// RealtimeChatBroadcaster delivers real-time chat frames (msg.push / system.notice)
// to online members connected to the WebSocket Hub.
// Note: In Stage 7+ production flow, online msg.push is handled asynchronously by the
// `im-msg-deliver` Redis Streams consumer group; direct Hub broadcast is disabled by default.
type RealtimeChatBroadcaster interface {
	BroadcastToUsers(ctx context.Context, userIDs []uint64, frameType string, data any)
}

// StreamMessage represents a committed message record published to `im:stream:messages`.
type StreamMessage struct {
	Kind           string
	MessageID      uint64
	ConversationID uint64
	Seq            uint64
	SenderID       uint64
	PayloadRef     string
	RecipientIDs   []uint64
}

// StreamEvent represents a domain event published to `im:stream:events`.
type StreamEvent struct {
	Kind           string
	Event          string
	ConversationID uint64
	ActorID        uint64
	TargetUserID   uint64
	TargetUserIDs  []uint64
	MessageID      uint64
	Seq            uint64
	Payload        map[string]any
}

// StreamPublisher publishes committed messages and events to Redis Streams after DB commit.
// Online msg.push delivery is handled by `im-msg-deliver` (not by the sending goroutine).
type StreamPublisher interface {
	PublishMessage(ctx context.Context, msg StreamMessage) error
	PublishEvent(ctx context.Context, evt StreamEvent) error
}

// MessageConvSeqFinder is an optional extension on MessageStore to reload an existing message
// by (conversation_id, seq) when a uk_msg_conv_seq unique constraint conflict occurs.
// *repository.MessageRepository satisfies this interface directly.
type MessageConvSeqFinder interface {
	FindMessageByConvSeq(ctx context.Context, convID, seq uint64) (*model.Message, error)
}

// MessageConversationStore defines conversation & membership operations needed by MessageService.
// *repository.ConversationRepository satisfies this interface directly.
type MessageConversationStore interface {
	GetConversationByID(ctx context.Context, convID uint64) (*model.Conversation, error)
	GetConversationMember(ctx context.Context, convID, userID uint64) (*model.ConversationMember, error)
	GetGroupByID(ctx context.Context, groupID uint64) (*model.Group, uint32, error)
	GetGroupMember(ctx context.Context, groupID, userID uint64) (*model.GroupMember, error)
	BatchGetFriendshipsForUser(ctx context.Context, userID uint64, peerIDs []uint64) (map[uint64]string, map[uint64]bool, error)
	ListActiveMemberUserIDs(ctx context.Context, conv *model.Conversation) ([]uint64, error)
	BatchGetUsersByIDs(ctx context.Context, userIDs []uint64) (map[uint64]model.User, error)
}

// MessageStore defines message, asset, receipt, and Redis dedupe persistence operations.
// *repository.MessageRepository satisfies this interface directly.
type MessageStore interface {
	WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error
	GetDedupeRecord(ctx context.Context, senderID uint64, clientMsgID string) (*repository.DedupeRecord, bool, error)
	SetDedupeRecordNX(ctx context.Context, senderID uint64, clientMsgID string, rec repository.DedupeRecord, ttl time.Duration) error
	DeleteDedupeRecord(ctx context.Context, senderID uint64, clientMsgID string) error
	AcquireDedupeLock(ctx context.Context, senderID uint64, clientMsgID string, lockTTL, waitTimeout time.Duration) (string, *repository.DedupeRecord, error)
	ReleaseDedupeLock(ctx context.Context, senderID uint64, clientMsgID, token string) error
	FindMessageByClientMsgID(ctx context.Context, senderID uint64, clientMsgID string) (*model.Message, error)
	FindMessageByID(ctx context.Context, msgID uint64) (*model.Message, error)
	LockConversationByID(ctx context.Context, tx *gorm.DB, convID uint64) (*model.Conversation, error)
	LockMessageInConversation(ctx context.Context, tx *gorm.DB, convID, msgID uint64) (*model.Message, error)
	GetAssetByAssetID(ctx context.Context, assetID string) (*model.Asset, error)
	LockAssetByAssetID(ctx context.Context, tx *gorm.DB, assetID string) (*model.Asset, error)
	BindAssetToMessageTx(ctx context.Context, tx *gorm.DB, asset *model.Asset, msgID, convID uint64, now time.Time) error
	CreateMessageTx(ctx context.Context, tx *gorm.DB, msg *model.Message) error
	UpdateConversationOnMessageTx(ctx context.Context, tx *gorm.DB, convID uint64, msg *model.Message, preview string, now time.Time) error
	UpdateMembersOnNewMessageTx(ctx context.Context, tx *gorm.DB, convID, senderID uint64, recipientIDs []uint64, now time.Time) error
	CreateMessageReceiptsTx(ctx context.Context, tx *gorm.DB, receipts []model.MessageReceipt) error
	MarkMessageRecalledTx(ctx context.Context, tx *gorm.DB, msgID uint64, recalledAt time.Time) (bool, error)
	AdjustMembersOnRecallTx(ctx context.Context, tx *gorm.DB, convID, senderID uint64, targetSeq uint64, recipientIDs []uint64, now time.Time) error
	ListMessagesBefore(ctx context.Context, convID uint64, beforeSeq *uint64, limit int) ([]model.Message, error)
	ListMessagesAfter(ctx context.Context, convID uint64, afterSeq uint64, limit int) ([]model.Message, error)
	BatchGetSingleDeliveryStatuses(ctx context.Context, convID, peerUserID uint64, msgIDs []uint64) (map[uint64]string, error)
}

// MessageDTO matches the frozen `Message` object in docs/api.md.
type MessageDTO struct {
	MsgID          uint64          `json:"msg_id,string"`
	ClientMsgID    string          `json:"client_msg_id"`
	ConversationID uint64          `json:"conversation_id,string"`
	Seq            uint64          `json:"seq,string"`
	SenderID       uint64          `json:"sender_id,string"`
	MsgType        string          `json:"msg_type"`
	Content        string          `json:"content"`
	Extra          json.RawMessage `json:"extra"`
	Status         string          `json:"status"`
	CreatedAt      string          `json:"created_at"`
	RecalledAt     *string         `json:"recalled_at"`
	DeliveryStatus *string         `json:"delivery_status,omitempty"`
}

// MessagePageDTO matches GET /api/v1/conversations/{conversation_id}/messages response data.
type MessagePageDTO struct {
	Items      []MessageDTO `json:"items"`
	NextCursor *string      `json:"next_cursor"`
	HasMore    bool         `json:"has_more"`
}

// MessageSyncDTO matches GET /api/v1/conversations/{conversation_id}/sync response data.
type MessageSyncDTO struct {
	Items   []MessageDTO `json:"items"`
	FromSeq string       `json:"from_seq"`
	ToSeq   string       `json:"to_seq"`
	HasMore bool         `json:"has_more"`
}

// SendMessageInput holds parameters for sending a message via HTTP or WS.
type SendMessageInput struct {
	ConversationID uint64
	SenderID       uint64
	ClientMsgID    string
	MsgType        string
	Content        string
	Extra          json.RawMessage
}

// SendMessageResult wraps the persisted or replayed message plus real-time push recipients.
type SendMessageResult struct {
	Message      MessageDTO
	IsNew        bool
	RecipientIDs []uint64
}

// MessagePushPayload matches WS `msg.push` (`push_type="message"`) in docs/api.md.
type MessagePushPayload struct {
	PushType       string     `json:"push_type"`
	Message        MessageDTO `json:"message"`
	DeliveryStatus string     `json:"delivery_status"`
}

// MsgAckPayload matches WS `msg.ack` in docs/api.md.
type MsgAckPayload struct {
	ClientMsgID    string `json:"client_msg_id"`
	MsgID          string `json:"msg_id"`
	ConversationID string `json:"conversation_id"`
	Seq            string `json:"seq"`
	Status         string `json:"status"`
	DeliveryStatus string `json:"delivery_status"`
}

// RecallMessageResult matches HTTP POST .../recall response data and WS recall push payloads.
type RecallMessageResult struct {
	RecalledMessage MessageDTO `json:"recalled_message"`
	RecallMessage   MessageDTO `json:"recall_message"`
	RecipientIDs    []uint64   `json:"-"`
}

// MsgRecallAckPayload matches WS `msg.recall` response in docs/api.md.
type MsgRecallAckPayload struct {
	ConversationID string `json:"conversation_id"`
	RecalledMsgID  string `json:"recalled_msg_id"`
	RecallMsgID    string `json:"recall_msg_id"`
	Seq            string `json:"seq"`
	RecalledAt     string `json:"recalled_at"`
}

// SystemNoticePayload matches WS `system.notice` in docs/api.md.
type SystemNoticePayload struct {
	Event          string  `json:"event"`
	NoticeID       string  `json:"notice_id"`
	ConversationID *string `json:"conversation_id"`
	ActorID        *string `json:"actor_id"`
	TargetUserID   *string `json:"target_user_id"`
	Payload        any     `json:"payload"`
}

// MessageService implements message history, incremental sync, idempotent send, recall, and system messages.
type MessageService struct {
	convStore          MessageConversationStore
	msgStore           MessageStore
	seqAllocator       repository.SeqAllocator
	idGen              SnowflakeIDGenerator
	broadcaster        RealtimeChatBroadcaster
	streamPublisher    StreamPublisher
	directPushFallback bool
	recallWindow       time.Duration
	nowFn              func() time.Time
	log                *slog.Logger
}

// NewMessageService constructs a MessageService.
// By default directPushFallback is false so online msg.push is delivered via im-msg-deliver
// after StreamPublisher XADD, rather than directly by the sending goroutine.
func NewMessageService(
	convStore MessageConversationStore,
	msgStore MessageStore,
	seqAllocator repository.SeqAllocator,
	idGen SnowflakeIDGenerator,
	broadcaster RealtimeChatBroadcaster,
	recallWindow time.Duration,
	log *slog.Logger,
) *MessageService {
	if recallWindow <= 0 {
		recallWindow = defaultRecallWindow
	}
	if log == nil {
		log = slog.Default()
	}
	return &MessageService{
		convStore:          convStore,
		msgStore:           msgStore,
		seqAllocator:       seqAllocator,
		idGen:              idGen,
		broadcaster:        broadcaster,
		directPushFallback: false,
		recallWindow:       recallWindow,
		nowFn:              func() time.Time { return time.Now().UTC() },
		log:                log,
	}
}

// SetBroadcaster attaches or replaces the real-time Hub broadcaster after construction.
func (s *MessageService) SetBroadcaster(broadcaster RealtimeChatBroadcaster) {
	if s != nil {
		s.broadcaster = broadcaster
	}
}

// SetStreamPublisher configures the post-commit Redis Streams producer.
// When set, online msg.push is handled solely by im-msg-deliver and direct Hub push is never invoked.
func (s *MessageService) SetStreamPublisher(pub StreamPublisher) {
	if s != nil {
		s.streamPublisher = pub
	}
}

// SetDirectPushFallback toggles the explicit transition/test fallback that directly broadcasts
// via RealtimeChatBroadcaster when no StreamPublisher is attached. Defaults to false.
func (s *MessageService) SetDirectPushFallback(enabled bool) {
	if s != nil {
		s.directPushFallback = enabled
	}
}

// ListMessages queries cursor-paginated message history by before_seq or after_seq (never OFFSET).
// When before_seq (or default latest page) is requested, queries seq DESC then returns items in ascending seq order.
func (s *MessageService) ListMessages(
	ctx context.Context,
	userID uint64,
	convID uint64,
	beforeSeq *uint64,
	afterSeq *uint64,
	limit int,
) (MessagePageDTO, *errcode.Error) {
	empty := MessagePageDTO{
		Items:      []MessageDTO{},
		NextCursor: nil,
		HasMore:    false,
	}
	if userID == 0 {
		return empty, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 {
		return empty, errcode.ErrInvalidValue.WithMessage("invalid conversation_id")
	}
	if beforeSeq != nil && afterSeq != nil {
		return empty, errcode.ErrInvalidValue.WithMessage("before_seq and after_seq cannot be used together")
	}
	if limit == 0 {
		limit = DefaultMessagePageLimit
	}
	if limit < 1 || limit > MaxMessagePageLimit {
		return empty, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}
	if s == nil || s.convStore == nil || s.msgStore == nil {
		return empty, errcode.ErrMySQLUnavailable
	}

	conv, _, bizErr := s.verifyMembership(ctx, userID, convID)
	if bizErr != nil {
		return empty, bizErr
	}

	var (
		rows    []model.Message
		err     error
		isAfter = afterSeq != nil
	)
	if isAfter {
		rows, err = s.msgStore.ListMessagesAfter(ctx, convID, *afterSeq, limit)
	} else {
		rows, err = s.msgStore.ListMessagesBefore(ctx, convID, beforeSeq, limit)
	}
	if err != nil {
		return empty, s.mapStoreError(ctx, userID, "list_messages", err)
	}
	if len(rows) == 0 {
		return empty, nil
	}

	hasMore := false
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	if !isAfter {
		reverseMessages(rows)
	}

	items, bizErr := s.assembleMessageDTOs(ctx, userID, conv, rows)
	if bizErr != nil {
		return empty, bizErr
	}

	var nextCursor *string
	if hasMore && len(items) > 0 {
		if isAfter {
			nc := strconv.FormatUint(items[len(items)-1].Seq, 10)
			nextCursor = &nc
		} else {
			nc := strconv.FormatUint(items[0].Seq, 10)
			nextCursor = &nc
		}
	}

	return MessagePageDTO{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// SyncMessages incrementally pulls messages with seq > afterSeq ordered ascending.
func (s *MessageService) SyncMessages(
	ctx context.Context,
	userID uint64,
	convID uint64,
	afterSeq uint64,
	limit int,
) (MessageSyncDTO, *errcode.Error) {
	afterStr := strconv.FormatUint(afterSeq, 10)
	empty := MessageSyncDTO{
		Items:   []MessageDTO{},
		FromSeq: afterStr,
		ToSeq:   afterStr,
		HasMore: false,
	}
	if userID == 0 {
		return empty, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 {
		return empty, errcode.ErrInvalidValue.WithMessage("invalid conversation_id")
	}
	if limit == 0 {
		limit = DefaultMessageSyncLimit
	}
	if limit < 1 || limit > MaxMessagePageLimit {
		return empty, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}
	if s == nil || s.convStore == nil || s.msgStore == nil {
		return empty, errcode.ErrMySQLUnavailable
	}

	conv, _, bizErr := s.verifyMembership(ctx, userID, convID)
	if bizErr != nil {
		return empty, bizErr
	}

	rows, err := s.msgStore.ListMessagesAfter(ctx, convID, afterSeq, limit)
	if err != nil {
		return empty, s.mapStoreError(ctx, userID, "sync_messages", err)
	}
	if len(rows) == 0 {
		return empty, nil
	}

	hasMore := false
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	items, bizErr := s.assembleMessageDTOs(ctx, userID, conv, rows)
	if bizErr != nil {
		return empty, bizErr
	}

	return MessageSyncDTO{
		Items:   items,
		FromSeq: strconv.FormatUint(items[0].Seq, 10),
		ToSeq:   strconv.FormatUint(items[len(items)-1].Seq, 10),
		HasMore: hasMore,
	}, nil
}

// SendMessage validates, deduplicates, persists, and pushes a new message (text/image/file/voice).
// When beforePush is non-nil (used by WS msg.send), beforePush runs right after commit and before Hub push
// so the sending connection receives msg.ack prior to msg.push.
func (s *MessageService) SendMessage(
	ctx context.Context,
	input SendMessageInput,
	beforePush func(res *SendMessageResult),
) (*SendMessageResult, *errcode.Error) {
	if input.SenderID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if input.ConversationID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid conversation_id")
	}
	if s == nil || s.convStore == nil || s.msgStore == nil || s.idGen == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if s.seqAllocator == nil {
		return nil, errcode.ErrRedisUnavailable
	}

	parsed, bizErr := validateSendPayload(input)
	if bizErr != nil {
		return nil, bizErr
	}

	conv, member, bizErr := s.verifyMembership(ctx, input.SenderID, input.ConversationID)
	if bizErr != nil {
		return nil, bizErr
	}
	if bizErr := s.verifyConversationWritable(ctx, conv, member, input.SenderID); bizErr != nil {
		return nil, bizErr
	}

	// Fast-path dedupe check in Redis before acquiring distributed locks.
	dedupeRec, found, err := s.msgStore.GetDedupeRecord(ctx, input.SenderID, parsed.ClientMsgID)
	if err != nil {
		return nil, s.mapRedisError(ctx, input.SenderID, "dedupe_get", err)
	}
	if found {
		return s.resolveDedupeReplay(ctx, conv, input.SenderID, parsed, dedupeRec, beforePush)
	}

	// Check MySQL fallback before taking conv:lock (handles expired Redis TTL >24h or flush).
	// If streamPublisher is active and existing is still normal without an im:dedupe cache entry
	// (e.g. retry after 14008 stream failure), fall through into im:dedupe:lock so concurrent retries serialize.
	if existing, findErr := s.msgStore.FindMessageByClientMsgID(ctx, input.SenderID, parsed.ClientMsgID); findErr != nil {
		return nil, s.mapStoreError(ctx, input.SenderID, "dedupe_mysql_precheck", findErr)
	} else if existing != nil && (s.streamPublisher == nil || existing.Status == model.MessageStatusRecalled) {
		return s.handleExistingMessageReplay(ctx, conv, input.SenderID, parsed, existing, beforePush, false)
	}

	// Acquire per-(sender, client_msg_id) short concurrency lock.
	dedupeLockToken, hitRec, lockErr := s.msgStore.AcquireDedupeLock(
		ctx,
		input.SenderID,
		parsed.ClientMsgID,
		defaultDedupeLockTTL,
		defaultDedupeWaitTime,
	)
	if lockErr != nil {
		if errors.Is(lockErr, repository.ErrDedupeInProgress) {
			return nil, errcode.ErrMessageInProgress
		}
		return nil, s.mapRedisError(ctx, input.SenderID, "acquire_dedupe_lock", lockErr)
	}
	if hitRec != nil {
		return s.resolveDedupeReplay(ctx, conv, input.SenderID, parsed, hitRec, beforePush)
	}
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if relErr := s.msgStore.ReleaseDedupeLock(relCtx, input.SenderID, parsed.ClientMsgID, dedupeLockToken); relErr != nil {
			s.log.WarnContext(ctx, "release_dedupe_lock_failed", slog.Uint64("sender_id", input.SenderID), slog.Any("error", relErr))
		}
	}()

	// Re-check MySQL inside dedupe lock in case a concurrent waiter just finished committing.
	if existing, findErr := s.msgStore.FindMessageByClientMsgID(ctx, input.SenderID, parsed.ClientMsgID); findErr != nil {
		return nil, s.mapStoreError(ctx, input.SenderID, "dedupe_mysql_recheck", findErr)
	} else if existing != nil {
		needPublish := s.streamPublisher != nil && existing.Status == model.MessageStatusNormal
		return s.handleExistingMessageReplay(ctx, conv, input.SenderID, parsed, existing, beforePush, needPublish)
	}

	// Validate asset reference before taking conversation lock.
	if parsed.AssetID != "" {
		asset, assetErr := s.msgStore.GetAssetByAssetID(ctx, parsed.AssetID)
		if assetErr != nil {
			if errors.Is(assetErr, repository.ErrAssetNotFound) {
				return nil, errcode.ErrInvalidMessagePayload.WithMessage("referenced asset not found")
			}
			return nil, s.mapStoreError(ctx, input.SenderID, "precheck_asset", assetErr)
		}
		if bizErr := validateAssetForMessage(asset, input.SenderID, parsed.MsgType, input.ConversationID); bizErr != nil {
			return nil, bizErr
		}
	}

	res, conflictSeq, bizErr := s.commitNewMessage(ctx, input.SenderID, input.ConversationID, parsed)
	if bizErr != nil {
		// Handle rare race on uk_msg_sender_client as idempotent replay.
		if bizErr.Code == errcode.ErrClientMsgIDConflict.Code {
			if existing, findErr := s.msgStore.FindMessageByClientMsgID(ctx, input.SenderID, parsed.ClientMsgID); findErr == nil && existing != nil {
				needPublish := s.streamPublisher != nil && existing.Status == model.MessageStatusNormal
				return s.handleExistingMessageReplay(ctx, conv, input.SenderID, parsed, existing, beforePush, needPublish)
			}
		}
		// Handle uk_msg_conv_seq conflict by reloading from MySQL and applying idempotency rules.
		if conflictSeq > 0 {
			if finder, ok := s.msgStore.(MessageConvSeqFinder); ok {
				if existing, findErr := finder.FindMessageByConvSeq(ctx, input.ConversationID, conflictSeq); findErr == nil && existing != nil {
					if existing.SenderID == input.SenderID && existing.ClientMsgID == parsed.ClientMsgID {
						needPublish := s.streamPublisher != nil && existing.Status == model.MessageStatusNormal
						return s.handleExistingMessageReplay(ctx, conv, input.SenderID, parsed, existing, beforePush, needPublish)
					}
				}
			}
			if existing, findErr := s.msgStore.FindMessageByClientMsgID(ctx, input.SenderID, parsed.ClientMsgID); findErr == nil && existing != nil {
				needPublish := s.streamPublisher != nil && existing.Status == model.MessageStatusNormal
				return s.handleExistingMessageReplay(ctx, conv, input.SenderID, parsed, existing, beforePush, needPublish)
			}
			return nil, errcode.ErrMessageInProgress
		}
		return nil, bizErr
	}

	// Acknowledge persistence to sender (WS msg.ack) prior to XADD im:stream:messages.
	if beforePush != nil {
		beforePush(res)
	}
	if pubErr := s.deliverOrPublishNewMessage(ctx, res); pubErr != nil {
		return nil, pubErr
	}

	// Best-effort populate im:dedupe cache after MySQL commit and Stream publish.
	_ = s.msgStore.SetDedupeRecordNX(ctx, input.SenderID, parsed.ClientMsgID, repository.DedupeRecord{
		MsgID:          res.Message.MsgID,
		ConversationID: res.Message.ConversationID,
		Seq:            res.Message.Seq,
		Status:         res.Message.Status,
		DeliveryStatus: model.ReceiptStatusSent,
	}, defaultDedupeTTL)

	return res, nil
}

// PersistSystemMessage persists an ordered server-generated system message (sender_id=0)
// and publishes it to im:stream:messages (or falls back to direct Hub broadcast only when configured).
func (s *MessageService) PersistSystemMessage(
	ctx context.Context,
	convID uint64,
	clientMsgID string,
	content string,
	extra json.RawMessage,
) (*SendMessageResult, *errcode.Error) {
	if convID == 0 || strings.TrimSpace(content) == "" {
		return nil, errcode.ErrInvalidParams
	}
	if s == nil || s.convStore == nil || s.msgStore == nil || s.idGen == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if s.seqAllocator == nil {
		return nil, errcode.ErrRedisUnavailable
	}
	if strings.TrimSpace(clientMsgID) == "" {
		clientMsgID = "sys:" + uuid.NewString()
	}
	if len(clientMsgID) > maxClientMsgIDBytes {
		return nil, errcode.ErrInvalidValue
	}

	extraNorm := model.NullableJSON("{}")
	if trimmed := bytes.TrimSpace(extra); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		var obj map[string]any
		if json.Unmarshal(trimmed, &obj) != nil {
			return nil, errcode.ErrInvalidMessagePayload
		}
		extraNorm = model.NullableJSON(trimmed)
	}

	parsed := validatedSendPayload{
		ClientMsgID: clientMsgID,
		MsgType:     model.MessageTypeSystem,
		Content:     content,
		ExtraJSON:   extraNorm,
	}

	res, _, bizErr := s.commitNewMessage(ctx, 0, convID, parsed)
	if bizErr != nil {
		return nil, bizErr
	}
	if pubErr := s.deliverOrPublishNewMessage(ctx, res); pubErr != nil {
		return nil, pubErr
	}
	return res, nil
}

// RecallMessage marks a sent message as recalled within the configured recall window,
// appends a new seq recall record, and pushes msg.push + system.notice(message.recalled) to online members.
func (s *MessageService) RecallMessage(
	ctx context.Context,
	userID uint64,
	convID uint64,
	msgID uint64,
	beforePush func(res *RecallMessageResult),
) (*RecallMessageResult, *errcode.Error) {
	if userID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 || msgID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid conversation_id or msg_id")
	}
	if s == nil || s.convStore == nil || s.msgStore == nil || s.idGen == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if s.seqAllocator == nil {
		return nil, errcode.ErrRedisUnavailable
	}

	conv, member, bizErr := s.verifyMembership(ctx, userID, convID)
	if bizErr != nil {
		return nil, bizErr
	}
	if bizErr := s.verifyConversationWritable(ctx, conv, member, userID); bizErr != nil {
		return nil, bizErr
	}

	// Pre-read target message to reject unauthorized/invalid recalls before taking conv:lock.
	preMsg, err := s.msgStore.FindMessageByID(ctx, msgID)
	if err != nil {
		return nil, s.mapStoreError(ctx, userID, "recall_preread_message", err)
	}
	if preMsg.ConversationID != convID {
		return nil, errcode.ErrMessageNotFound
	}
	if preMsg.SenderID == 0 || preMsg.SenderID != userID {
		return nil, errcode.ErrMessageAccessDenied
	}
	if preMsg.MsgType == model.MessageTypeRecall || preMsg.MsgType == model.MessageTypeSystem {
		return nil, errcode.ErrMessageAccessDenied
	}
	if preMsg.Status == model.MessageStatusRecalled {
		return nil, errcode.ErrMessageAlreadyRecalled
	}
	if s.nowFn().Sub(preMsg.CreatedAt.UTC()) > s.recallWindow {
		return nil, errcode.ErrRecallWindowExpired
	}

	recallContent := "撤回了一条消息"
	if usersMap, uErr := s.convStore.BatchGetUsersByIDs(ctx, []uint64{userID}); uErr == nil {
		if u, exists := usersMap[userID]; exists && strings.TrimSpace(u.Nickname) != "" {
			recallContent = u.Nickname + " 撤回了一条消息"
		}
	}

	lockToken, lockErr := s.seqAllocator.AcquireConvLock(ctx, convID)
	if lockErr != nil {
		return nil, s.mapSeqError(ctx, userID, "recall_acquire_conv_lock", lockErr)
	}
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if relErr := s.seqAllocator.ReleaseConvLock(relCtx, convID, lockToken); relErr != nil {
			s.log.WarnContext(ctx, "recall_release_conv_lock_failed", slog.Uint64("conversation_id", convID), slog.Any("error", relErr))
		}
	}()

	var (
		seqRes          *repository.SeqReservation
		recalledTarget  model.Message
		recallMsg       model.Message
		recipientIDs    []uint64
		targetClientMsg string
	)

	txErr := s.msgStore.WithTx(ctx, func(tx *gorm.DB) error {
		lockedConv, cErr := s.msgStore.LockConversationByID(ctx, tx, convID)
		if cErr != nil {
			return s.mapStoreError(ctx, userID, "recall_lock_conv", cErr)
		}

		lockedMsg, mErr := s.msgStore.LockMessageInConversation(ctx, tx, convID, msgID)
		if mErr != nil {
			return s.mapStoreError(ctx, userID, "recall_lock_msg", mErr)
		}
		if lockedMsg.SenderID == 0 || lockedMsg.SenderID != userID {
			return newServiceError(errcode.ErrMessageAccessDenied)
		}
		if lockedMsg.MsgType == model.MessageTypeRecall || lockedMsg.MsgType == model.MessageTypeSystem {
			return newServiceError(errcode.ErrMessageAccessDenied)
		}
		if lockedMsg.Status == model.MessageStatusRecalled {
			return newServiceError(errcode.ErrMessageAlreadyRecalled)
		}

		now := s.nowFn().UTC().Truncate(time.Millisecond)
		if now.Sub(lockedMsg.CreatedAt.UTC()) > s.recallWindow {
			return newServiceError(errcode.ErrRecallWindowExpired)
		}

		if bizErr := s.verifyConversationWritable(ctx, lockedConv, member, userID); bizErr != nil {
			return newServiceError(bizErr)
		}

		ok, markErr := s.msgStore.MarkMessageRecalledTx(ctx, tx, lockedMsg.ID, now)
		if markErr != nil {
			return s.mapStoreError(ctx, userID, "recall_mark_msg", markErr)
		}
		if !ok {
			return newServiceError(errcode.ErrMessageAlreadyRecalled)
		}

		lockedMsg.Status = model.MessageStatusRecalled
		lockedMsg.RecalledAt = &now
		recalledTarget = *lockedMsg
		targetClientMsg = lockedMsg.ClientMsgID

		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, convID, lockToken)
		if seqErr != nil {
			return newServiceError(s.mapSeqError(ctx, userID, "recall_allocate_seq", seqErr))
		}
		seqRes = allocated

		recallID, genErr := s.idGen.NextID()
		if genErr != nil {
			s.log.ErrorContext(ctx, "recall_snowflake_failed", slog.Any("error", genErr))
			return newServiceError(errcode.ErrInternalConsistency)
		}

		extraBytes, _ := json.Marshal(map[string]any{
			"recalled_msg_id": strconv.FormatUint(lockedMsg.ID, 10),
			"recalled_seq":    strconv.FormatUint(lockedMsg.Seq, 10),
			"recalled_at":     FormatUTCMillis(now),
		})
		recallMsg = model.Message{
			ID:             recallID,
			ConversationID: convID,
			Seq:            seqRes.Seq,
			SenderID:       userID,
			ClientMsgID:    fmt.Sprintf("recall:%d", lockedMsg.ID),
			MsgType:        model.MessageTypeRecall,
			Content:        recallContent,
			Extra:          model.NullableJSON(extraBytes),
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if cMsgErr := s.msgStore.CreateMessageTx(ctx, tx, &recallMsg); cMsgErr != nil {
			return s.mapStoreError(ctx, userID, "recall_create_record", cMsgErr)
		}

		if updErr := s.msgStore.UpdateConversationOnMessageTx(ctx, tx, convID, &recallMsg, recallContent, now); updErr != nil {
			return s.mapStoreError(ctx, userID, "recall_update_conv", updErr)
		}

		activeIDs, memErr := s.convStore.ListActiveMemberUserIDs(ctx, lockedConv)
		if memErr != nil {
			return s.mapStoreError(ctx, userID, "recall_list_members", memErr)
		}
		recipientIDs = activeIDs

		if adjErr := s.msgStore.AdjustMembersOnRecallTx(ctx, tx, convID, userID, lockedMsg.Seq, recipientIDs, now); adjErr != nil {
			return s.mapStoreError(ctx, userID, "recall_adjust_unread", adjErr)
		}

		if lockedConv.Type == model.ConversationTypeSingle {
			if peerID, ok := repository.ParseDirectKeyPeer(lockedConv.DirectKey, userID); ok {
				receipts := []model.MessageReceipt{{
					MessageID:      recallMsg.ID,
					ConversationID: convID,
					Seq:            recallMsg.Seq,
					UserID:         peerID,
					Status:         model.ReceiptStatusSent,
					CreatedAt:      now,
					UpdatedAt:      now,
				}}
				if recErr := s.msgStore.CreateMessageReceiptsTx(ctx, tx, receipts); recErr != nil {
					return s.mapStoreError(ctx, userID, "recall_create_receipt", recErr)
				}
			}
		}

		return nil
	})
	if txErr != nil {
		if seqRes != nil && seqRes.Rollback != nil {
			rbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if rbErr := seqRes.Rollback(rbCtx); rbErr != nil {
				s.log.WarnContext(ctx, "recall_seq_rollback_failed", slog.Uint64("conversation_id", convID), slog.Any("error", rbErr))
			}
			cancel()
		}
		return nil, s.unwrapServiceError(ctx, userID, "recall_message", txErr)
	}

	// Evict stale im:dedupe cache entry for the recalled message so retries reflect status="recalled".
	if targetClientMsg != "" {
		_ = s.msgStore.DeleteDedupeRecord(ctx, userID, targetClientMsg)
	}

	recalledDTO := toMessageDTO(recalledTarget, nil)
	recallDTO := toMessageDTO(recallMsg, nil)
	if conv.Type == model.ConversationTypeSingle {
		if ds, ok := s.lookupSingleDeliveryStatus(ctx, conv, userID, recalledTarget.ID); ok {
			recalledDTO.DeliveryStatus = &ds
		}
		sent := model.ReceiptStatusSent
		recallDTO.DeliveryStatus = &sent
	}

	res := &RecallMessageResult{
		RecalledMessage: recalledDTO,
		RecallMessage:   recallDTO,
		RecipientIDs:    recipientIDs,
	}

	if beforePush != nil {
		beforePush(res)
	}
	if pubErr := s.deliverOrPublishRecall(ctx, res); pubErr != nil {
		return nil, pubErr
	}
	return res, nil
}

func (s *MessageService) commitNewMessage(
	ctx context.Context,
	senderID uint64,
	convID uint64,
	parsed validatedSendPayload,
) (*SendMessageResult, uint64, *errcode.Error) {
	lockToken, lockErr := s.seqAllocator.AcquireConvLock(ctx, convID)
	if lockErr != nil {
		return nil, 0, s.mapSeqError(ctx, senderID, "send_acquire_conv_lock", lockErr)
	}
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if relErr := s.seqAllocator.ReleaseConvLock(relCtx, convID, lockToken); relErr != nil {
			s.log.WarnContext(ctx, "send_release_conv_lock_failed", slog.Uint64("conversation_id", convID), slog.Any("error", relErr))
		}
	}()

	var (
		seqRes       *repository.SeqReservation
		persisted    model.Message
		recipientIDs []uint64
		dupConflict  bool
		seqConflict  bool
		conflictSeq  uint64
	)

	txErr := s.msgStore.WithTx(ctx, func(tx *gorm.DB) error {
		lockedConv, cErr := s.msgStore.LockConversationByID(ctx, tx, convID)
		if cErr != nil {
			return s.mapStoreError(ctx, senderID, "send_lock_conv", cErr)
		}

		if senderID > 0 {
			member, mErr := s.convStore.GetConversationMember(ctx, convID, senderID)
			if mErr != nil {
				return s.mapStoreError(ctx, senderID, "send_recheck_member", mErr)
			}
			if bizErr := s.verifyConversationWritable(ctx, lockedConv, member, senderID); bizErr != nil {
				return newServiceError(bizErr)
			}
		}

		now := s.nowFn().UTC().Truncate(time.Millisecond)
		extraJSON := parsed.ExtraJSON
		var lockedAsset *model.Asset
		var preview string

		if parsed.AssetID != "" {
			asset, aErr := s.msgStore.LockAssetByAssetID(ctx, tx, parsed.AssetID)
			if aErr != nil {
				if errors.Is(aErr, repository.ErrAssetNotFound) {
					return newServiceError(errcode.ErrInvalidMessagePayload.WithMessage("referenced asset not found"))
				}
				return s.mapStoreError(ctx, senderID, "send_lock_asset", aErr)
			}
			if bizErr := validateAssetForMessage(asset, senderID, parsed.MsgType, convID); bizErr != nil {
				return newServiceError(bizErr)
			}
			lockedAsset = asset
			extraJSON = buildCanonicalAssetExtra(asset)
			preview = buildMessagePreview(parsed.MsgType, parsed.Content, asset.Filename)
		} else {
			preview = buildMessagePreview(parsed.MsgType, parsed.Content, "")
		}

		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, convID, lockToken)
		if seqErr != nil {
			return newServiceError(s.mapSeqError(ctx, senderID, "send_allocate_seq", seqErr))
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			s.log.ErrorContext(ctx, "send_snowflake_failed", slog.Any("error", genErr))
			return newServiceError(errcode.ErrInternalConsistency)
		}

		persisted = model.Message{
			ID:             msgID,
			ConversationID: convID,
			Seq:            seqRes.Seq,
			SenderID:       senderID,
			ClientMsgID:    parsed.ClientMsgID,
			MsgType:        parsed.MsgType,
			Content:        parsed.Content,
			Extra:          extraJSON,
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if createErr := s.msgStore.CreateMessageTx(ctx, tx, &persisted); createErr != nil {
			if repository.IsDuplicateClientMsgError(createErr) {
				dupConflict = true
				return newServiceError(errcode.ErrClientMsgIDConflict)
			}
			if repository.IsDuplicateConvSeqError(createErr) {
				seqConflict = true
				conflictSeq = persisted.Seq
				return newServiceError(errcode.ErrInternalConsistency)
			}
			return s.mapStoreError(ctx, senderID, "send_create_message", createErr)
		}

		if lockedAsset != nil {
			if bindErr := s.msgStore.BindAssetToMessageTx(ctx, tx, lockedAsset, persisted.ID, convID, now); bindErr != nil {
				return s.mapStoreError(ctx, senderID, "send_bind_asset", bindErr)
			}
		}

		if updErr := s.msgStore.UpdateConversationOnMessageTx(ctx, tx, convID, &persisted, preview, now); updErr != nil {
			return s.mapStoreError(ctx, senderID, "send_update_conv", updErr)
		}

		activeIDs, listErr := s.convStore.ListActiveMemberUserIDs(ctx, lockedConv)
		if listErr != nil {
			return s.mapStoreError(ctx, senderID, "send_list_members", listErr)
		}
		recipientIDs = activeIDs

		if memErr := s.msgStore.UpdateMembersOnNewMessageTx(ctx, tx, convID, senderID, recipientIDs, now); memErr != nil {
			return s.mapStoreError(ctx, senderID, "send_update_members", memErr)
		}

		if lockedConv.Type == model.ConversationTypeSingle {
			var receipts []model.MessageReceipt
			for _, uid := range recipientIDs {
				if uid == senderID {
					continue
				}
				receipts = append(receipts, model.MessageReceipt{
					MessageID:      persisted.ID,
					ConversationID: convID,
					Seq:            persisted.Seq,
					UserID:         uid,
					Status:         model.ReceiptStatusSent,
					CreatedAt:      now,
					UpdatedAt:      now,
				})
			}
			if recErr := s.msgStore.CreateMessageReceiptsTx(ctx, tx, receipts); recErr != nil {
				return s.mapStoreError(ctx, senderID, "send_create_receipts", recErr)
			}
		}

		return nil
	})
	if txErr != nil {
		if seqRes != nil && seqRes.Rollback != nil {
			rbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if rbErr := seqRes.Rollback(rbCtx); rbErr != nil {
				s.log.WarnContext(ctx, "send_seq_rollback_failed", slog.Uint64("conversation_id", convID), slog.Any("error", rbErr))
			}
			cancel()
		}
		if dupConflict {
			return nil, 0, errcode.ErrClientMsgIDConflict
		}
		if seqConflict {
			return nil, conflictSeq, errcode.ErrInternalConsistency
		}
		return nil, 0, s.unwrapServiceError(ctx, senderID, "send_message", txErr)
	}

	sent := model.ReceiptStatusSent
	return &SendMessageResult{
		Message:      toMessageDTO(persisted, &sent),
		IsNew:        true,
		RecipientIDs: recipientIDs,
	}, 0, nil
}

func (s *MessageService) resolveDedupeReplay(
	ctx context.Context,
	conv *model.Conversation,
	senderID uint64,
	parsed validatedSendPayload,
	rec *repository.DedupeRecord,
	beforePush func(res *SendMessageResult),
) (*SendMessageResult, *errcode.Error) {
	if rec.ConversationID != conv.ID {
		return nil, errcode.ErrClientMsgIDConflict
	}
	existing, err := s.msgStore.FindMessageByID(ctx, rec.MsgID)
	if err != nil {
		if errors.Is(err, repository.ErrMessageNotFound) {
			existing, err = s.msgStore.FindMessageByClientMsgID(ctx, senderID, parsed.ClientMsgID)
			if err != nil {
				return nil, s.mapStoreError(ctx, senderID, "dedupe_reload_by_client", err)
			}
			if existing == nil {
				return nil, errcode.ErrInternalConsistency
			}
		} else {
			return nil, s.mapStoreError(ctx, senderID, "dedupe_reload_by_id", err)
		}
	}
	return s.handleExistingMessageReplay(ctx, conv, senderID, parsed, existing, beforePush, false)
}

func (s *MessageService) handleExistingMessageReplay(
	ctx context.Context,
	conv *model.Conversation,
	senderID uint64,
	parsed validatedSendPayload,
	existing *model.Message,
	beforePush func(res *SendMessageResult),
	publishIfNeeded bool,
) (*SendMessageResult, *errcode.Error) {
	if !matchesExistingPayload(existing, conv.ID, parsed) {
		return nil, errcode.ErrClientMsgIDConflict
	}

	deliveryStatus := model.ReceiptStatusSent
	if conv.Type == model.ConversationTypeSingle {
		if ds, ok := s.lookupSingleDeliveryStatus(ctx, conv, senderID, existing.ID); ok {
			deliveryStatus = ds
		}
	}

	res := &SendMessageResult{
		Message: toMessageDTO(*existing, &deliveryStatus),
		IsNew:   false,
	}
	if beforePush != nil {
		beforePush(res)
	}
	if publishIfNeeded && s.streamPublisher != nil && existing.Status == model.MessageStatusNormal {
		if pubErr := s.publishNewMessageToStream(ctx, res); pubErr != nil {
			return nil, pubErr
		}
	}

	_ = s.msgStore.SetDedupeRecordNX(ctx, senderID, parsed.ClientMsgID, repository.DedupeRecord{
		MsgID:          existing.ID,
		ConversationID: existing.ConversationID,
		Seq:            existing.Seq,
		Status:         existing.Status,
		DeliveryStatus: deliveryStatus,
	}, defaultDedupeTTL)

	return res, nil
}

func (s *MessageService) verifyMembership(ctx context.Context, userID, convID uint64) (*model.Conversation, *model.ConversationMember, *errcode.Error) {
	conv, err := s.convStore.GetConversationByID(ctx, convID)
	if err != nil {
		return nil, nil, s.mapStoreError(ctx, userID, "verify_conv_exists", err)
	}
	member, err := s.convStore.GetConversationMember(ctx, convID, userID)
	if err != nil {
		return nil, nil, s.mapStoreError(ctx, userID, "verify_conv_member", err)
	}
	if conv.Type == model.ConversationTypeGroup {
		if conv.GroupID == nil || *conv.GroupID == 0 {
			return nil, nil, errcode.ErrGroupNotFound
		}
		if _, gmErr := s.convStore.GetGroupMember(ctx, *conv.GroupID, userID); gmErr != nil {
			return nil, nil, s.mapStoreError(ctx, userID, "verify_group_member", gmErr)
		}
	}
	return conv, member, nil
}

func (s *MessageService) verifyConversationWritable(ctx context.Context, conv *model.Conversation, member *model.ConversationMember, userID uint64) *errcode.Error {
	if conv == nil || member == nil {
		return errcode.ErrInternalConsistency
	}
	switch conv.Type {
	case model.ConversationTypeSingle:
		peerID, ok := repository.ParseDirectKeyPeer(conv.DirectKey, userID)
		if !ok {
			return errcode.ErrNotConversationMember
		}
		_, activeBoth, err := s.convStore.BatchGetFriendshipsForUser(ctx, userID, []uint64{peerID})
		if err != nil {
			return s.mapStoreError(ctx, userID, "verify_single_friendship", err)
		}
		if !activeBoth[peerID] {
			if !member.IsVisible {
				return errcode.ErrConversationReadOnly
			}
			return errcode.ErrNotFriends
		}
		return nil
	case model.ConversationTypeGroup:
		if conv.GroupID == nil || *conv.GroupID == 0 {
			return errcode.ErrGroupNotFound
		}
		group, _, err := s.convStore.GetGroupByID(ctx, *conv.GroupID)
		if err != nil {
			return s.mapStoreError(ctx, userID, "verify_group_status", err)
		}
		if group.Status != model.GroupStatusActive {
			return errcode.ErrConversationReadOnly
		}
		return nil
	default:
		return errcode.ErrInternalConsistency
	}
}

func (s *MessageService) assembleMessageDTOs(
	ctx context.Context,
	viewerID uint64,
	conv *model.Conversation,
	rows []model.Message,
) ([]MessageDTO, *errcode.Error) {
	var deliveryMap map[uint64]string
	if conv.Type == model.ConversationTypeSingle {
		if peerID, ok := repository.ParseDirectKeyPeer(conv.DirectKey, viewerID); ok {
			senderMsgIDs := make([]uint64, 0, len(rows))
			for _, m := range rows {
				if m.SenderID == viewerID {
					senderMsgIDs = append(senderMsgIDs, m.ID)
				}
			}
			if len(senderMsgIDs) > 0 {
				var err error
				deliveryMap, err = s.msgStore.BatchGetSingleDeliveryStatuses(ctx, conv.ID, peerID, senderMsgIDs)
				if err != nil {
					return nil, s.mapStoreError(ctx, viewerID, "batch_get_delivery_statuses", err)
				}
			}
		}
	}

	items := make([]MessageDTO, 0, len(rows))
	for _, m := range rows {
		var dsPtr *string
		if conv.Type == model.ConversationTypeSingle && m.SenderID == viewerID {
			ds := model.ReceiptStatusSent
			if val, ok := deliveryMap[m.ID]; ok && val != "" {
				ds = val
			}
			dsPtr = &ds
		}
		items = append(items, toMessageDTO(m, dsPtr))
	}
	return items, nil
}

func (s *MessageService) lookupSingleDeliveryStatus(ctx context.Context, conv *model.Conversation, senderID, msgID uint64) (string, bool) {
	peerID, ok := repository.ParseDirectKeyPeer(conv.DirectKey, senderID)
	if !ok {
		return model.ReceiptStatusSent, false
	}
	m, err := s.msgStore.BatchGetSingleDeliveryStatuses(ctx, conv.ID, peerID, []uint64{msgID})
	if err != nil {
		return model.ReceiptStatusSent, false
	}
	if status, exists := m[msgID]; exists && status != "" {
		return status, true
	}
	return model.ReceiptStatusSent, true
}

// deliverOrPublishNewMessage publishes newly committed messages to im:stream:messages.
// 在线 msg.push 由 im-msg-deliver 负责；生产路径默认仅走 Publish，绝不与 hub 直推双推。
// 仅当未配置 StreamPublisher 且明确启用 directPushFallback 时，才回退到旧直推开关。
func (s *MessageService) deliverOrPublishNewMessage(ctx context.Context, res *SendMessageResult) *errcode.Error {
	if res == nil {
		return nil
	}
	if s.streamPublisher != nil {
		return s.publishNewMessageToStream(ctx, res)
	}
	if s.directPushFallback {
		s.pushNewMessage(ctx, res)
	}
	return nil
}

func (s *MessageService) publishNewMessageToStream(ctx context.Context, res *SendMessageResult) *errcode.Error {
	if s.streamPublisher == nil || res == nil {
		return nil
	}
	if err := s.streamPublisher.PublishMessage(ctx, StreamMessage{
		Kind:           "message",
		MessageID:      res.Message.MsgID,
		ConversationID: res.Message.ConversationID,
		Seq:            res.Message.Seq,
		SenderID:       res.Message.SenderID,
		PayloadRef:     fmt.Sprintf("mysql:messages:%d", res.Message.MsgID),
		RecipientIDs:   res.RecipientIDs,
	}); err != nil {
		// Stream 生产失败不回滚已提交 MySQL 事务，返回 14008 供客户端以原 client_msg_id 安全重试。
		s.log.ErrorContext(ctx, "stream_publish_message_failed",
			slog.Uint64("msg_id", res.Message.MsgID),
			slog.Uint64("conversation_id", res.Message.ConversationID),
			slog.Uint64("seq", res.Message.Seq),
			slog.Any("error", err),
		)
		return errcode.ErrStreamUnavailable
	}
	return nil
}

// deliverOrPublishRecall publishes committed recall record to im:stream:messages and
// message.recalled event to im:stream:events(kind=recall).
// 在线 msg.push 与 system.notice 由 im-msg-deliver / im-event-deliver 负责，生产路径不再直推 hub。
func (s *MessageService) deliverOrPublishRecall(ctx context.Context, res *RecallMessageResult) *errcode.Error {
	if res == nil {
		return nil
	}
	if s.streamPublisher != nil {
		if err := s.streamPublisher.PublishMessage(ctx, StreamMessage{
			Kind:           "message",
			MessageID:      res.RecallMessage.MsgID,
			ConversationID: res.RecallMessage.ConversationID,
			Seq:            res.RecallMessage.Seq,
			SenderID:       res.RecallMessage.SenderID,
			PayloadRef:     fmt.Sprintf("mysql:messages:%d", res.RecallMessage.MsgID),
			RecipientIDs:   res.RecipientIDs,
		}); err != nil {
			s.log.ErrorContext(ctx, "stream_publish_recall_message_failed",
				slog.Uint64("recall_msg_id", res.RecallMessage.MsgID),
				slog.Uint64("conversation_id", res.RecallMessage.ConversationID),
				slog.Any("error", err),
			)
			return errcode.ErrStreamUnavailable
		}

		recalledAtStr := ""
		if res.RecalledMessage.RecalledAt != nil {
			recalledAtStr = *res.RecalledMessage.RecalledAt
		}
		if err := s.streamPublisher.PublishEvent(ctx, StreamEvent{
			Kind:           "recall",
			Event:          "message.recalled",
			ConversationID: res.RecallMessage.ConversationID,
			ActorID:        res.RecallMessage.SenderID,
			TargetUserIDs:  res.RecipientIDs,
			MessageID:      res.RecalledMessage.MsgID,
			Seq:            res.RecallMessage.Seq,
			Payload: map[string]any{
				"recalled_msg_id": strconv.FormatUint(res.RecalledMessage.MsgID, 10),
				"recall_msg_id":   strconv.FormatUint(res.RecallMessage.MsgID, 10),
				"recall_seq":      strconv.FormatUint(res.RecallMessage.Seq, 10),
				"recalled_at":     recalledAtStr,
			},
		}); err != nil {
			s.log.ErrorContext(ctx, "stream_publish_recall_event_failed",
				slog.Uint64("recalled_msg_id", res.RecalledMessage.MsgID),
				slog.Uint64("recall_msg_id", res.RecallMessage.MsgID),
				slog.Uint64("conversation_id", res.RecallMessage.ConversationID),
				slog.Any("error", err),
			)
			return errcode.ErrStreamUnavailable
		}
		return nil
	}
	if s.directPushFallback {
		s.pushRecallNotifications(ctx, res)
	}
	return nil
}

func (s *MessageService) pushNewMessage(ctx context.Context, res *SendMessageResult) {
	if s.broadcaster == nil || res == nil || len(res.RecipientIDs) == 0 {
		return
	}
	pushMsg := res.Message
	pushMsg.DeliveryStatus = nil
	s.broadcaster.BroadcastToUsers(ctx, res.RecipientIDs, "msg.push", MessagePushPayload{
		PushType:       "message",
		Message:        pushMsg,
		DeliveryStatus: model.ReceiptStatusSent,
	})
}

func (s *MessageService) pushRecallNotifications(ctx context.Context, res *RecallMessageResult) {
	if s.broadcaster == nil || res == nil || len(res.RecipientIDs) == 0 {
		return
	}
	pushMsg := res.RecallMessage
	pushMsg.DeliveryStatus = nil
	s.broadcaster.BroadcastToUsers(ctx, res.RecipientIDs, "msg.push", MessagePushPayload{
		PushType:       "message",
		Message:        pushMsg,
		DeliveryStatus: model.ReceiptStatusSent,
	})

	convIDStr := strconv.FormatUint(res.RecallMessage.ConversationID, 10)
	actorIDStr := strconv.FormatUint(res.RecallMessage.SenderID, 10)
	recalledAtStr := ""
	if res.RecalledMessage.RecalledAt != nil {
		recalledAtStr = *res.RecalledMessage.RecalledAt
	}
	s.broadcaster.BroadcastToUsers(ctx, res.RecipientIDs, "system.notice", SystemNoticePayload{
		Event:          "message.recalled",
		NoticeID:       "n_" + uuid.NewString(),
		ConversationID: &convIDStr,
		ActorID:        &actorIDStr,
		TargetUserID:   nil,
		Payload: map[string]any{
			"recalled_msg_id": strconv.FormatUint(res.RecalledMessage.MsgID, 10),
			"recall_msg_id":   strconv.FormatUint(res.RecallMessage.MsgID, 10),
			"recall_seq":      strconv.FormatUint(res.RecallMessage.Seq, 10),
			"recalled_at":     recalledAtStr,
		},
	})
}

func (s *MessageService) mapSeqError(ctx context.Context, userID uint64, op string, err error) *errcode.Error {
	if errors.Is(err, repository.ErrConvLockTimeout) {
		s.log.WarnContext(ctx, op+"_timeout", slog.Uint64("user_id", userID), slog.Any("error", err))
		return errcode.ErrMessageInProgress
	}
	s.log.ErrorContext(ctx, op+"_failed", slog.Uint64("user_id", userID), slog.Any("error", err))
	return errcode.ErrRedisUnavailable
}

func (s *MessageService) mapRedisError(ctx context.Context, userID uint64, op string, err error) *errcode.Error {
	s.log.ErrorContext(ctx, op+"_failed", slog.Uint64("user_id", userID), slog.Any("error", err))
	return errcode.ErrRedisUnavailable
}

func (s *MessageService) mapStoreError(ctx context.Context, userID uint64, op string, err error) *errcode.Error {
	var svcErr *ServiceError
	if errors.As(err, &svcErr) && svcErr.BizErr != nil {
		return svcErr.BizErr
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
	s.log.ErrorContext(ctx, op+"_failed", slog.Uint64("user_id", userID), slog.Any("error", err))
	if isInfrastructureUnavailableErr(err) {
		return errcode.ErrMySQLUnavailable
	}
	return errcode.ErrInternalConsistency
}

func (s *MessageService) unwrapServiceError(ctx context.Context, userID uint64, op string, err error) *errcode.Error {
	var svcErr *ServiceError
	if errors.As(err, &svcErr) && svcErr.BizErr != nil {
		return svcErr.BizErr
	}
	return s.mapStoreError(ctx, userID, op, err)
}

type validatedSendPayload struct {
	ClientMsgID string
	MsgType     string
	Content     string
	AssetID     string
	ExtraJSON   model.NullableJSON
}

func validateSendPayload(input SendMessageInput) (validatedSendPayload, *errcode.Error) {
	clientMsgID := strings.TrimSpace(input.ClientMsgID)
	if clientMsgID == "" {
		return validatedSendPayload{}, errcode.ErrInvalidParams.WithMessage("client_msg_id is required")
	}
	if len(clientMsgID) > maxClientMsgIDBytes || !utf8.ValidString(clientMsgID) ||
		strings.IndexFunc(clientMsgID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return validatedSendPayload{}, errcode.ErrInvalidMessagePayload.WithMessage("invalid client_msg_id")
	}

	if !utf8.ValidString(input.Content) || strings.ContainsRune(input.Content, 0) || len(input.Content) > maxMessageContentByte {
		return validatedSendPayload{}, errcode.ErrInvalidMessagePayload.WithMessage("invalid message content")
	}

	extraMap, err := parseExtraObject(input.Extra)
	if err != nil {
		return validatedSendPayload{}, errcode.ErrInvalidMessagePayload.WithMessage("extra must be a JSON object")
	}

	switch input.MsgType {
	case model.MessageTypeText:
		if strings.TrimSpace(input.Content) == "" {
			return validatedSendPayload{}, errcode.ErrInvalidMessagePayload.WithMessage("text message content must not be empty")
		}
		if len(extraMap) > 0 {
			return validatedSendPayload{}, errcode.ErrInvalidMessagePayload.WithMessage("text message does not accept extra attachments")
		}
		return validatedSendPayload{
			ClientMsgID: clientMsgID,
			MsgType:     model.MessageTypeText,
			Content:     input.Content,
			ExtraJSON:   model.NullableJSON("{}"),
		}, nil

	case model.MessageTypeImage, model.MessageTypeFile, model.MessageTypeVoice:
		rawAssetID, exists := extraMap["asset_id"]
		if !exists {
			return validatedSendPayload{}, errcode.ErrInvalidMessagePayload.WithMessage("media message requires extra.asset_id")
		}
		assetID, ok := rawAssetID.(string)
		assetID = strings.TrimSpace(assetID)
		if !ok || assetID == "" || len(assetID) > 36 {
			return validatedSendPayload{}, errcode.ErrInvalidMessagePayload.WithMessage("invalid extra.asset_id")
		}
		return validatedSendPayload{
			ClientMsgID: clientMsgID,
			MsgType:     input.MsgType,
			Content:     input.Content,
			AssetID:     assetID,
			ExtraJSON:   model.NullableJSON("{}"),
		}, nil

	default:
		return validatedSendPayload{}, errcode.ErrInvalidMessagePayload.WithMessage("unsupported msg_type")
	}
}

func parseExtraObject(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, nil
	}
	if trimmed[0] != '{' {
		return nil, errors.New("extra is not a JSON object")
	}
	var m map[string]any
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func validateAssetForMessage(asset *model.Asset, senderID uint64, msgType string, convID uint64) *errcode.Error {
	if asset == nil || asset.Status != model.AssetStatusActive {
		return errcode.ErrInvalidMessagePayload.WithMessage("referenced asset not found or deleted")
	}
	if asset.Purpose != msgType {
		return errcode.ErrInvalidMessagePayload.WithMessage("asset purpose does not match msg_type")
	}
	if asset.OwnerID != senderID {
		return errcode.ErrInvalidMessagePayload.WithMessage("asset does not belong to sender")
	}
	switch asset.RefType {
	case model.AssetRefNone:
		return nil
	case model.AssetRefMessage:
		if asset.RefConversationID == nil || *asset.RefConversationID != convID {
			return errcode.ErrInvalidMessagePayload.WithMessage("asset is already bound to another conversation")
		}
		return nil
	default:
		return errcode.ErrInvalidMessagePayload.WithMessage("asset reference type incompatible with message")
	}
}

func buildCanonicalAssetExtra(asset *model.Asset) model.NullableJSON {
	obj := map[string]any{
		"asset_id":   asset.AssetID,
		"purpose":    asset.Purpose,
		"url":        fmt.Sprintf("/api/v1/uploads/%s/download", asset.AssetID),
		"filename":   asset.Filename,
		"mime":       asset.Mime,
		"size_bytes": asset.SizeBytes,
	}
	if asset.Width != nil {
		obj["width"] = *asset.Width
	}
	if asset.Height != nil {
		obj["height"] = *asset.Height
	}
	if asset.ThumbnailKey != nil && *asset.ThumbnailKey != "" {
		obj["thumbnail_url"] = fmt.Sprintf("/api/v1/uploads/%s/download?variant=thumbnail", asset.AssetID)
	}
	if asset.DurationMs != nil {
		obj["duration_ms"] = *asset.DurationMs
	}
	raw, _ := json.Marshal(obj)
	return model.NullableJSON(raw)
}

func buildMessagePreview(msgType, content, filename string) string {
	if trimmed := strings.TrimSpace(content); trimmed != "" {
		return trimmed
	}
	switch msgType {
	case model.MessageTypeImage:
		return "[图片]"
	case model.MessageTypeFile:
		if strings.TrimSpace(filename) != "" {
			return "[文件] " + strings.TrimSpace(filename)
		}
		return "[文件]"
	case model.MessageTypeVoice:
		return "[语音]"
	default:
		return content
	}
}

func matchesExistingPayload(existing *model.Message, convID uint64, parsed validatedSendPayload) bool {
	if existing == nil ||
		existing.ConversationID != convID ||
		existing.MsgType != parsed.MsgType ||
		existing.Content != parsed.Content {
		return false
	}
	if parsed.MsgType == model.MessageTypeText {
		return true
	}
	var existingExtra map[string]any
	if len(existing.Extra) > 0 {
		if err := json.Unmarshal(existing.Extra, &existingExtra); err != nil {
			return false
		}
	}
	existingAssetID, _ := existingExtra["asset_id"].(string)
	return strings.TrimSpace(existingAssetID) == parsed.AssetID
}

func toMessageDTO(m model.Message, deliveryStatus *string) MessageDTO {
	extra := json.RawMessage("{}")
	if trimmed := bytes.TrimSpace(m.Extra); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		extra = json.RawMessage(trimmed)
	}
	return MessageDTO{
		MsgID:          m.ID,
		ClientMsgID:    m.ClientMsgID,
		ConversationID: m.ConversationID,
		Seq:            m.Seq,
		SenderID:       m.SenderID,
		MsgType:        m.MsgType,
		Content:        m.Content,
		Extra:          extra,
		Status:         m.Status,
		CreatedAt:      FormatUTCMillis(m.CreatedAt),
		RecalledAt:     FormatUTCMillisPtr(m.RecalledAt),
		DeliveryStatus: deliveryStatus,
	}
}

func reverseMessages(rows []model.Message) {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
}
