package mq

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
)

const (
	// Frozen Redis Stream names from docs/architecture.md.
	StreamMessages     = "im:stream:messages"
	StreamEvents       = "im:stream:events"
	StreamDeadMessages = "im:stream:dead:messages"
	StreamDeadEvents   = "im:stream:dead:events"

	// Frozen Consumer Group names from docs/architecture.md.
	GroupMsgPersist   = "im-msg-persist"
	GroupMsgDeliver   = "im-msg-deliver"
	GroupEventDeliver = "im-event-deliver"
	GroupEventAudit   = "im-event-audit"

	// Stream entry kind constants.
	KindMessage   = "message"
	KindDelivered = "delivered"
	KindRead      = "read"
	KindPresence  = "presence"
	KindFriend    = "friend"
	KindGroup     = "group"
	KindRecall    = "recall"
	KindKick      = "kick"

	utcMillisLayout = "2006-01-02T15:04:05.000Z"

	defaultStreamMaxLen        int64 = 100000
	defaultStreamRetention           = 7 * 24 * time.Hour
	defaultTrimInterval              = 5 * time.Minute
	defaultReadCount           int64 = 100
	defaultBlockTimeout              = 2 * time.Second
	defaultReclaimInterval           = 30 * time.Second
	defaultReclaimMinIdle            = 30 * time.Second
	defaultMaxRetries                = 5
	defaultMaxPendingAge             = 15 * time.Minute
	defaultDeadStreamMaxLen    int64 = 50000
	defaultPendingReclaimTTL         = 24 * time.Hour
	defaultIdempotencyTTL            = 24 * time.Hour
	defaultWorkerCount               = 1
	defaultLargeGroupThreshold       = 100
)

var (
	// ErrStreamPublishFailed indicates Redis Streams XADD failed (callers may map to 14008).
	ErrStreamPublishFailed = errors.New("redis stream publish failed")
	// ErrInvalidStreamPayload indicates missing or invalid message/event fields before XADD.
	ErrInvalidStreamPayload = errors.New("invalid redis stream payload")
	// ErrConsumerAlreadyStarted indicates Start was called more than once without Drain.
	ErrConsumerAlreadyStarted = errors.New("mq consumer manager already started")
)

// IsStreamUnavailable reports whether err represents a Redis Stream publish failure.
func IsStreamUnavailable(err error) bool {
	return errors.Is(err, ErrStreamPublishFailed)
}

// AsStreamError converts a stream publish failure into frozen errcode 14008 (ErrStreamUnavailable).
func AsStreamError(err error) *errcode.Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrStreamPublishFailed) {
		return errcode.ErrStreamUnavailable
	}
	if errors.Is(err, ErrInvalidStreamPayload) {
		return errcode.ErrInvalidMessagePayload
	}
	return errcode.ErrStreamUnavailable
}

// DefaultMessagesGroups returns the frozen Consumer Groups on im:stream:messages.
func DefaultMessagesGroups() []string {
	return []string{GroupMsgPersist, GroupMsgDeliver}
}

// DefaultEventsGroups returns the frozen Consumer Groups on im:stream:events.
func DefaultEventsGroups() []string {
	return []string{GroupEventDeliver, GroupEventAudit}
}

// PendingReclaimKey returns the observability Hash key `im:stream:pending:reclaim:{messages|events}:{group}`.
func PendingReclaimKey(stream, group string) string {
	return "im:stream:pending:reclaim:" + streamTag(stream) + ":" + group
}

// DeadLetterStream returns `im:stream:dead:{messages|events}` for a source stream.
func DeadLetterStream(stream string) string {
	if stream == StreamEvents {
		return StreamDeadEvents
	}
	return StreamDeadMessages
}

func streamTag(stream string) string {
	if stream == StreamEvents {
		return "events"
	}
	return "messages"
}

// IsValidEventKind reports whether kind is one of the frozen im:stream:events kinds.
func IsValidEventKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case KindDelivered, KindRead, KindPresence, KindFriend, KindGroup, KindRecall, KindKick:
		return true
	default:
		return false
	}
}

// NormalizeMQConfig applies spec-compliant defaults to zero-valued fields in cfg.
func NormalizeMQConfig(cfg config.MQConfig) config.MQConfig {
	if cfg.StreamMaxLen <= 0 {
		cfg.StreamMaxLen = defaultStreamMaxLen
	}
	if cfg.StreamRetention <= 0 {
		cfg.StreamRetention = defaultStreamRetention
	}
	if cfg.TrimInterval <= 0 {
		cfg.TrimInterval = defaultTrimInterval
	}
	if cfg.ReadCount <= 0 {
		cfg.ReadCount = defaultReadCount
	}
	if cfg.BlockTimeout <= 0 {
		cfg.BlockTimeout = defaultBlockTimeout
	}
	if cfg.ReclaimInterval <= 0 {
		cfg.ReclaimInterval = defaultReclaimInterval
	}
	if cfg.ReclaimMinIdle <= 0 {
		cfg.ReclaimMinIdle = defaultReclaimMinIdle
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.MaxPendingAge <= 0 {
		cfg.MaxPendingAge = defaultMaxPendingAge
	}
	if cfg.DeadStreamMaxLen <= 0 {
		cfg.DeadStreamMaxLen = defaultDeadStreamMaxLen
	}
	if cfg.PendingReclaimTTL <= 0 {
		cfg.PendingReclaimTTL = defaultPendingReclaimTTL
	}
	if cfg.IdempotencyTTL <= 0 {
		cfg.IdempotencyTTL = defaultIdempotencyTTL
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = defaultWorkerCount
	}
	if cfg.LargeGroupReceiptThreshold <= 0 {
		cfg.LargeGroupReceiptThreshold = defaultLargeGroupThreshold
	}
	return cfg
}

// MessageRecord carries the frozen fields for XADD im:stream:messages.
type MessageRecord struct {
	MessageID      uint64
	ConversationID uint64
	Seq            uint64
	SenderID       uint64
	PayloadRef     string
}

// EventRecord carries fields for XADD im:stream:events.
type EventRecord struct {
	Kind             string
	EventID          string
	Event            string
	ConversationID   uint64
	ActorID          uint64
	TargetUserID     uint64
	TargetUserIDs    []uint64
	MessageID        uint64
	Seq              uint64
	LastDeliveredSeq uint64
	LastReadSeq      uint64
	OccurredAt       time.Time
	Payload          json.RawMessage
	Audit            *AuditRecord
}

// AuditRecord carries optional structured audit metadata appended by im-event-audit.
type AuditRecord struct {
	Scope          string          `json:"scope"`
	Action         string          `json:"action"`
	ActorRole      string          `json:"actor_role,omitempty"`
	TargetType     string          `json:"target_type"`
	TargetID       uint64          `json:"target_id,string,omitempty"`
	GroupID        *uint64         `json:"group_id,string,omitempty"`
	ConversationID *uint64         `json:"conversation_id,string,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Detail         json.RawMessage `json:"detail,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	IPAddress      string          `json:"ip_address,omitempty"`
	UserAgent      string          `json:"user_agent,omitempty"`
}

// Producer publishes committed messages and domain events into Redis Streams.
type Producer interface {
	PublishMessage(ctx context.Context, msg MessageRecord) (string, error)
	PublishEvent(ctx context.Context, evt EventRecord) (string, error)
}

// HubDeliverer places committed frames into local WebSocket bounded send queues.
// *ws.Hub satisfies this interface directly via Go structural typing.
type HubDeliverer interface {
	DeliverToUsers(ctx context.Context, targetUserIDs []uint64, frameType string, data any) error
}

// CommittedMessage matches the frozen `Message` JSON object in docs/api.md.
type CommittedMessage struct {
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
}

// ConversationMeta holds minimal conversation routing metadata for consumers.
type ConversationMeta struct {
	ID        uint64
	Type      string
	DirectKey *string
	GroupID   *uint64
}

// MessagePushPayload matches WS `msg.push` (`push_type="message"`) in docs/api.md.
type MessagePushPayload struct {
	PushType       string           `json:"push_type"`
	Message        CommittedMessage `json:"message"`
	DeliveryStatus string           `json:"delivery_status"`
}

// ReceiptPushPayload matches WS `msg.push` (`push_type="receipt"`) in docs/api.md.
type ReceiptPushPayload struct {
	PushType         string  `json:"push_type"`
	ConversationID   string  `json:"conversation_id"`
	UserID           string  `json:"user_id"`
	DeliveryStatus   string  `json:"delivery_status"`
	MsgID            *string `json:"msg_id"`
	Seq              *string `json:"seq"`
	LastDeliveredSeq string  `json:"last_delivered_seq"`
	LastReadSeq      string  `json:"last_read_seq"`
	UpdatedAt        string  `json:"updated_at"`
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

// MessageStore loads committed records and performs post-commit compensation writes.
type MessageStore interface {
	GetCommittedMessage(ctx context.Context, msgID uint64) (*CommittedMessage, error)
	ListConversationActiveRecipients(ctx context.Context, convID uint64) (*ConversationMeta, []uint64, error)
	InitDelayedGroupReceipts(ctx context.Context, convID, msgID, seq uint64, recipientIDs []uint64, now time.Time) error
	EnsureSingleReceipt(ctx context.Context, convID, msgID, seq, peerID uint64, now time.Time) error
}

// AuditStore appends immutable audit_logs entries idempotently on event_id.
type AuditStore interface {
	AppendAuditLog(ctx context.Context, entry *model.AuditLog) error
}

// TrimReport summarizes trimmed entry counts across streams.
type TrimReport struct {
	MessagesTrimmed int64
	EventsTrimmed   int64
}
