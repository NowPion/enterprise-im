package mq

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

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/config"
)

// ProducerOption customizes StreamProducer behavior (primarily for testing).
type ProducerOption func(*StreamProducer)

// WithProducerClock overrides the producer's wall clock (used in tests).
func WithProducerClock(nowFn func() time.Time) ProducerOption {
	return func(p *StreamProducer) {
		if nowFn != nil {
			p.nowFn = nowFn
		}
	}
}

// WithExactTrim switches stream trimming from `~` approximate trimming to exact trimming.
func WithExactTrim(exact bool) ProducerOption {
	return func(p *StreamProducer) {
		p.exactTrim = exact
	}
}

// WithProducerStreams overrides the target Redis Stream keys for messages and events (primarily for isolated tests).
func WithProducerStreams(messagesStream, eventsStream string) ProducerOption {
	return func(p *StreamProducer) {
		if trimmed := strings.TrimSpace(messagesStream); trimmed != "" {
			p.messagesStream = trimmed
		}
		if trimmed := strings.TrimSpace(eventsStream); trimmed != "" {
			p.eventsStream = trimmed
		}
	}
}

// StreamProducer publishes committed chat messages and domain events to Redis Streams
// and enforces retention/length trimming guarded by active Consumer Group progress.
type StreamProducer struct {
	rdb            *redis.Client
	cfg            config.MQConfig
	log            *slog.Logger
	nowFn          func() time.Time
	exactTrim      bool
	messagesStream string
	eventsStream   string
}

var _ Producer = (*StreamProducer)(nil)

// NewProducer creates a StreamProducer backed by rdb.
func NewProducer(rdb *redis.Client, cfg config.MQConfig, log *slog.Logger, opts ...ProducerOption) *StreamProducer {
	if log == nil {
		log = slog.Default()
	}
	p := &StreamProducer{
		rdb:            rdb,
		cfg:            NormalizeMQConfig(cfg),
		log:            log,
		nowFn:          func() time.Time { return time.Now().UTC() },
		messagesStream: StreamMessages,
		eventsStream:   StreamEvents,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	return p
}

// PublishMessage appends a committed message reference to `im:stream:messages`.
// Callers must NOT ignore returned errors and may map ErrStreamPublishFailed to code 14008.
func (p *StreamProducer) PublishMessage(ctx context.Context, msg MessageRecord) (string, error) {
	if msg.MessageID == 0 || msg.ConversationID == 0 || msg.Seq == 0 {
		return "", fmt.Errorf("%w: message_id, conversation_id, and seq must be positive", ErrInvalidStreamPayload)
	}
	if p == nil || p.rdb == nil {
		return "", fmt.Errorf("%w: redis client unavailable", ErrStreamPublishFailed)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	payloadRef := strings.TrimSpace(msg.PayloadRef)
	if payloadRef == "" {
		payloadRef = fmt.Sprintf("messages:%d", msg.MessageID)
	}

	values := map[string]any{
		"kind":            KindMessage,
		"message_id":      strconv.FormatUint(msg.MessageID, 10),
		"conversation_id": strconv.FormatUint(msg.ConversationID, 10),
		"seq":             strconv.FormatUint(msg.Seq, 10),
		"sender_id":       strconv.FormatUint(msg.SenderID, 10),
		"payload_ref":     payloadRef,
	}

	entryID, err := p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.messagesStream,
		ID:     "*",
		Values: values,
	}).Result()
	if err != nil {
		p.log.WarnContext(ctx, "mq_publish_message_failed",
			slog.Uint64("message_id", msg.MessageID),
			slog.Uint64("conversation_id", msg.ConversationID),
			slog.Uint64("seq", msg.Seq),
			slog.String("error", sanitizeErrorSummary(err)),
		)
		return "", fmt.Errorf("%w: xadd %s: %s", ErrStreamPublishFailed, p.messagesStream, sanitizeErrorSummary(err))
	}
	return entryID, nil
}

// PublishEvent appends a domain or receipt event to `im:stream:events`.
func (p *StreamProducer) PublishEvent(ctx context.Context, evt EventRecord) (string, error) {
	kind := strings.TrimSpace(evt.Kind)
	if !IsValidEventKind(kind) {
		return "", fmt.Errorf("%w: unsupported event kind %q", ErrInvalidStreamPayload, evt.Kind)
	}

	payloadStr := "{}"
	if trimmed := bytes.TrimSpace(evt.Payload); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if !json.Valid(trimmed) {
			return "", fmt.Errorf("%w: event payload is not valid JSON", ErrInvalidStreamPayload)
		}
		payloadStr = string(trimmed)
	}

	var auditJSON string
	if evt.Audit != nil {
		auditRaw, err := json.Marshal(evt.Audit)
		if err != nil {
			return "", fmt.Errorf("%w: marshal audit payload: %v", ErrInvalidStreamPayload, err)
		}
		auditJSON = string(auditRaw)
	}

	if p == nil || p.rdb == nil {
		return "", fmt.Errorf("%w: redis client unavailable", ErrStreamPublishFailed)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	eventID := strings.TrimSpace(evt.EventID)
	if eventID == "" {
		eventID = uuid.NewString()
	}
	occurredAt := evt.OccurredAt.UTC()
	if occurredAt.IsZero() {
		occurredAt = p.nowFn().UTC()
	}

	targets := make([]uint64, 0, len(evt.TargetUserIDs)+1)
	targets = append(targets, evt.TargetUserIDs...)
	if evt.TargetUserID > 0 {
		targets = append(targets, evt.TargetUserID)
	}

	values := map[string]any{
		"kind":               kind,
		"event_id":           eventID,
		"event":              strings.TrimSpace(evt.Event),
		"conversation_id":    strconv.FormatUint(evt.ConversationID, 10),
		"actor_id":           strconv.FormatUint(evt.ActorID, 10),
		"target_user_id":     strconv.FormatUint(evt.TargetUserID, 10),
		"target_user_ids":    formatUint64CSV(targets),
		"message_id":         strconv.FormatUint(evt.MessageID, 10),
		"seq":                strconv.FormatUint(evt.Seq, 10),
		"last_delivered_seq": strconv.FormatUint(evt.LastDeliveredSeq, 10),
		"last_read_seq":      strconv.FormatUint(evt.LastReadSeq, 10),
		"occurred_at":        occurredAt.Truncate(time.Millisecond).Format(utcMillisLayout),
		"payload":            payloadStr,
	}

	if auditJSON != "" {
		values["audit"] = auditJSON
	}

	entryID, err := p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.eventsStream,
		ID:     "*",
		Values: values,
	}).Result()
	if err != nil {
		p.log.WarnContext(ctx, "mq_publish_event_failed",
			slog.String("kind", kind),
			slog.String("event_id", eventID),
			slog.Uint64("conversation_id", evt.ConversationID),
			slog.String("error", sanitizeErrorSummary(err)),
		)
		return "", fmt.Errorf("%w: xadd %s: %s", ErrStreamPublishFailed, p.eventsStream, sanitizeErrorSummary(err))
	}
	return entryID, nil
}

// TrimStreams enforces 7-day retention and length caps across the configured messages and events streams,
// strictly trimming only entries already covered (delivered and ACKed) by all required consumer groups.
func (p *StreamProducer) TrimStreams(ctx context.Context) (TrimReport, error) {
	if p == nil || p.rdb == nil {
		return TrimReport{}, fmt.Errorf("%w: redis client unavailable", ErrStreamPublishFailed)
	}
	var report TrimReport
	msgTrimmed, msgErr := p.TrimStream(ctx, p.messagesStream, DefaultMessagesGroups())
	report.MessagesTrimmed = msgTrimmed
	evtTrimmed, evtErr := p.TrimStream(ctx, p.eventsStream, DefaultEventsGroups())
	report.EventsTrimmed = evtTrimmed
	return report, errors.Join(msgErr, evtErr)
}

// TrimStream trims entries older than StreamRetention or exceeding StreamMaxLen on stream,
// bounded strictly by the minimum safe cursor across requiredGroups so unconsumed or pending entries
// are never evicted.
func (p *StreamProducer) TrimStream(ctx context.Context, stream string, requiredGroups []string) (int64, error) {
	if p == nil || p.rdb == nil {
		return 0, fmt.Errorf("%w: redis client unavailable", ErrStreamPublishFailed)
	}
	if len(requiredGroups) == 0 {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	groups, err := p.rdb.XInfoGroups(ctx, stream).Result()
	if err != nil {
		if isNoSuchKeyErr(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("xinfo groups %s: %w", stream, err)
	}
	groupByName := make(map[string]redis.XInfoGroup, len(groups))
	for _, g := range groups {
		groupByName[g.Name] = g
	}

	safeBound := ""
	for _, reqGroup := range requiredGroups {
		g, ok := groupByName[reqGroup]
		if !ok {
			// A required business consumer group has not registered yet; never trim.
			return 0, nil
		}
		if g.LastDeliveredID == "" || g.LastDeliveredID == "0-0" {
			return 0, nil
		}

		groupSafe := nextStreamID(g.LastDeliveredID)
		pending, pErr := p.rdb.XPending(ctx, stream, reqGroup).Result()
		if pErr != nil {
			return 0, fmt.Errorf("xpending %s %s: %w", stream, reqGroup, pErr)
		}
		if pending.Count > 0 && pending.Lower != "" {
			groupSafe = minStreamID(groupSafe, pending.Lower)
		}

		if safeBound == "" {
			safeBound = groupSafe
		} else {
			safeBound = minStreamID(safeBound, groupSafe)
		}
	}

	if safeBound == "" || compareStreamID(safeBound, "0-0") <= 0 {
		return 0, nil
	}

	var totalTrimmed int64

	// 1. Age-based retention trim (7d default).
	if p.cfg.StreamRetention > 0 {
		cutoffMs := p.nowFn().UTC().Add(-p.cfg.StreamRetention).UnixMilli()
		if cutoffMs > 0 {
			ageCutoffID := fmt.Sprintf("%d-0", cutoffMs)
			trimTarget := minStreamID(ageCutoffID, safeBound)
			if compareStreamID(trimTarget, "0-0") > 0 {
				n, trimErr := p.xtrimMinID(ctx, stream, trimTarget)
				if trimErr != nil {
					return totalTrimmed, trimErr
				}
				totalTrimmed += n
			}
		}
	}

	// 2. Length-cap retention trim bounded by safeBound.
	if p.cfg.StreamMaxLen > 0 {
		xlen, lenErr := p.rdb.XLen(ctx, stream).Result()
		if lenErr != nil {
			return totalTrimmed, fmt.Errorf("xlen %s: %w", stream, lenErr)
		}
		if xlen > p.cfg.StreamMaxLen {
			excess := xlen - p.cfg.StreamMaxLen
			fetchCount := excess + 1
			if fetchCount > 10000 {
				fetchCount = 10000
			}
			oldest, rangeErr := p.rdb.XRangeN(ctx, stream, "-", "+", fetchCount).Result()
			if rangeErr != nil {
				return totalTrimmed, fmt.Errorf("xrange %s for maxlen trim: %w", stream, rangeErr)
			}
			if len(oldest) > 0 {
				boundaryIdx := int(excess)
				if boundaryIdx >= len(oldest) {
					boundaryIdx = len(oldest) - 1
				}
				lenCutoffID := oldest[boundaryIdx].ID
				trimTarget := minStreamID(lenCutoffID, safeBound)
				if compareStreamID(trimTarget, oldest[0].ID) > 0 {
					n, trimErr := p.xtrimMinID(ctx, stream, trimTarget)
					if trimErr != nil {
						return totalTrimmed, trimErr
					}
					totalTrimmed += n
				}
			}
		}
	}

	return totalTrimmed, nil
}

func (p *StreamProducer) xtrimMinID(ctx context.Context, stream, minID string) (int64, error) {
	if p.exactTrim {
		n, err := p.rdb.XTrimMinID(ctx, stream, minID).Result()
		if err != nil {
			return 0, fmt.Errorf("xtrim minid %s: %w", stream, err)
		}
		return n, nil
	}
	n, err := p.rdb.XTrimMinIDApprox(ctx, stream, minID, 0).Result()
	if err != nil {
		return 0, fmt.Errorf("xtrim minid ~ %s: %w", stream, err)
	}
	return n, nil
}

func isNoSuchKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such key")
}
