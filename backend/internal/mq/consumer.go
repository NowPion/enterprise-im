package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
)

// HandlerFunc handles one Redis Stream entry for a consumer group (primarily for test hooks).
type HandlerFunc func(ctx context.Context, stream, group string, msg redis.XMessage) error

// MessageResolver resolves a committed message and target user IDs in tests without MySQL.
type MessageResolver func(ctx context.Context, msg redis.XMessage) (*CommittedMessage, []uint64, error)

// PersistHook runs an optional test/extension callback inside im-msg-persist after compensation writes.
type PersistHook func(ctx context.Context, msg redis.XMessage, committed *CommittedMessage) error

// ConsumerOption customizes ConsumerManager behavior.
type ConsumerOption func(*ConsumerManager)

// WithMessageStore overrides the default GormStore for message loading & compensation.
func WithMessageStore(store MessageStore) ConsumerOption {
	return func(cm *ConsumerManager) {
		cm.msgStore = store
	}
}

// WithAuditStore overrides the default GormStore for append-only audit persistence.
func WithAuditStore(store AuditStore) ConsumerOption {
	return func(cm *ConsumerManager) {
		cm.auditStore = store
	}
}

// WithConsumerClock overrides the wall clock (used in tests for age/DLQ timing).
func WithConsumerClock(nowFn func() time.Time) ConsumerOption {
	return func(cm *ConsumerManager) {
		if nowFn != nil {
			cm.nowFn = nowFn
		}
	}
}

// WithCustomGroupHandler overrides processing for a specific consumer group (useful in fault/DLQ/drain tests).
func WithCustomGroupHandler(group string, fn HandlerFunc) ConsumerOption {
	return func(cm *ConsumerManager) {
		if cm.customHandlers == nil {
			cm.customHandlers = make(map[string]HandlerFunc)
		}
		cm.customHandlers[group] = fn
	}
}

// WithMessageResolver overrides message + recipient resolution in im-msg-deliver (useful for Redis-only unit tests).
func WithMessageResolver(fn MessageResolver) ConsumerOption {
	return func(cm *ConsumerManager) {
		cm.msgResolver = fn
	}
}

// WithPersistHook attaches an extra hook invoked by im-msg-persist.
func WithPersistHook(fn PersistHook) ConsumerOption {
	return func(cm *ConsumerManager) {
		cm.persistHook = fn
	}
}

// WithGroups restricts which stream/group pairs the ConsumerManager runs (empty = all 4 frozen groups).
func WithGroups(targets ...StreamGroup) ConsumerOption {
	return func(cm *ConsumerManager) {
		if len(targets) > 0 {
			cm.targets = append([]StreamGroup(nil), targets...)
		}
	}
}

// StreamGroup pairs a Redis Stream name with a Consumer Group name.
type StreamGroup struct {
	Stream string
	Group  string
}

// DefaultStreamGroups returns all 4 frozen (stream, group) pairs.
func DefaultStreamGroups() []StreamGroup {
	return []StreamGroup{
		{Stream: StreamMessages, Group: GroupMsgPersist},
		{Stream: StreamMessages, Group: GroupMsgDeliver},
		{Stream: StreamEvents, Group: GroupEventDeliver},
		{Stream: StreamEvents, Group: GroupEventAudit},
	}
}

// ReclaimResult summarizes one XAUTOCLAIM sweep on a consumer group.
type ReclaimResult struct {
	Stream           string
	Group            string
	StartID          string
	NextID           string
	ClaimedCount     int
	AckedCount       int
	FailedCount      int
	DeadLetterCount  int
}

// ConsumerManager runs the 4 frozen Redis Streams consumer groups, startup PEL recovery,
// periodic XAUTOCLAIM, dead-letter routing, periodic retention trimming, and graceful draining.
type ConsumerManager struct {
	cfg            config.MQConfig
	nodeID         string
	rdb            *redis.Client
	hub            HubDeliverer
	msgStore       MessageStore
	auditStore     AuditStore
	producer       *StreamProducer
	log            *slog.Logger
	nowFn          func() time.Time
	targets        []StreamGroup
	customHandlers map[string]HandlerFunc
	msgResolver    MessageResolver
	persistHook    PersistHook

	mu             sync.Mutex
	started        bool
	draining       bool
	claimCancel    context.CancelFunc
	inFlightCancel context.CancelFunc
	wg             sync.WaitGroup

	cursorMu       sync.Mutex
	reclaimCursors map[string]string
}

// NewConsumerManager creates a ConsumerManager bound to nodeID, rdb, db, and hub.
func NewConsumerManager(
	cfg config.MQConfig,
	nodeID string,
	rdb *redis.Client,
	db *gorm.DB,
	hub HubDeliverer,
	log *slog.Logger,
	opts ...ConsumerOption,
) *ConsumerManager {
	normalizedCfg := NormalizeMQConfig(cfg)
	trimmedNode := strings.TrimSpace(nodeID)
	if trimmedNode == "" {
		trimmedNode = "node-1"
	}
	if log == nil {
		log = slog.Default()
	}

	var defaultStore *GormStore
	if db != nil || rdb != nil {
		defaultStore = NewGormStore(db, rdb)
	}

	cm := &ConsumerManager{
		cfg:            normalizedCfg,
		nodeID:         trimmedNode,
		rdb:            rdb,
		hub:            hub,
		log:            log,
		nowFn:          func() time.Time { return time.Now().UTC() },
		targets:        DefaultStreamGroups(),
		reclaimCursors: make(map[string]string),
	}
	if db != nil {
		cm.msgStore = defaultStore
		cm.auditStore = defaultStore
	}
	for _, opt := range opts {
		if opt != nil {
			opt(cm)
		}
	}
	cm.producer = NewProducer(rdb, normalizedCfg, log, WithProducerClock(cm.nowFn))
	return cm
}

// EnsureGroups creates all configured consumer groups idempotently (`XGROUP CREATE ... 0 MKSTREAM`).
func (cm *ConsumerManager) EnsureGroups(ctx context.Context) error {
	if cm == nil || cm.rdb == nil {
		return errors.New("redis client is required for mq consumers")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, tg := range cm.targets {
		err := cm.rdb.XGroupCreateMkStream(ctx, tg.Stream, tg.Group, "0").Err()
		if err != nil && !isBusyGroupErr(err) {
			return fmt.Errorf("xgroup create %s %s: %w", tg.Stream, tg.Group, err)
		}
	}
	return nil
}

// Start initializes consumer groups and starts consumer workers, XAUTOCLAIM reclaim loop,
// and retention trim loop.
func (cm *ConsumerManager) Start(ctx context.Context) error {
	if cm == nil || cm.rdb == nil {
		return errors.New("redis client is required for mq consumers")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := cm.EnsureGroups(ctx); err != nil {
		return err
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.started {
		return ErrConsumerAlreadyStarted
	}

	claimCtx, claimCancel := context.WithCancel(ctx)
	inFlightCtx, inFlightCancel := context.WithCancel(context.WithoutCancel(ctx))
	cm.claimCancel = claimCancel
	cm.inFlightCancel = inFlightCancel
	cm.started = true
	cm.draining = false

	for _, tg := range cm.targets {
		for idx := 1; idx <= cm.cfg.WorkerCount; idx++ {
			consumerName := fmt.Sprintf("%s-%s-%d", cm.nodeID, workerShortName(tg.Group), idx)
			stream, group := tg.Stream, tg.Group
			cm.wg.Add(1)
			go func() {
				defer cm.wg.Done()
				cm.runWorker(claimCtx, inFlightCtx, stream, group, consumerName)
			}()
		}
	}

	// Reclaim ticker (default 30s).
	cm.wg.Add(1)
	go func() {
		defer cm.wg.Done()
		cm.runReclaimLoop(claimCtx, inFlightCtx)
	}()

	// Safe retention trim ticker (default 5m).
	cm.wg.Add(1)
	go func() {
		defer cm.wg.Done()
		cm.runTrimLoop(claimCtx, inFlightCtx)
	}()

	cm.log.InfoContext(ctx, "mq_consumers_started",
		slog.String("node_id", cm.nodeID),
		slog.Int("group_count", len(cm.targets)),
		slog.Int("workers_per_group", cm.cfg.WorkerCount),
	)
	return nil
}

// Stop stops claiming new entries and drains active in-flight consumer work within ctx.
// It is an alias for Drain so callers and tests can pair Start/Stop or Start/Drain symmetrically.
func (cm *ConsumerManager) Stop(ctx context.Context) error {
	return cm.Drain(ctx)
}

// StartConsumers constructs, initializes, and starts a ConsumerManager sharing rdb, db, and hub,
// returning the manager and a stop/drain function suitable for tests and lifecycle wiring.
func StartConsumers(
	ctx context.Context,
	cfg config.MQConfig,
	nodeID string,
	rdb *redis.Client,
	db *gorm.DB,
	hub HubDeliverer,
	log *slog.Logger,
	opts ...ConsumerOption,
) (*ConsumerManager, func(context.Context) error, error) {
	cm := NewConsumerManager(cfg, nodeID, rdb, db, hub, log, opts...)
	if err := cm.Start(ctx); err != nil {
		return nil, nil, err
	}
	return cm, cm.Drain, nil
}

// Drain stops claiming new entries from Redis Streams and waits up to ctx for in-flight handlers
// to finish and XACK. Any entries still unacknowledged remain in the Redis PEL for future reclaim.
func (cm *ConsumerManager) Drain(ctx context.Context) error {
	if cm == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cm.mu.Lock()
	if !cm.started {
		cm.mu.Unlock()
		return nil
	}
	cm.draining = true
	claimCancel := cm.claimCancel
	inFlightCancel := cm.inFlightCancel
	cm.mu.Unlock()

	if claimCancel != nil {
		claimCancel()
	}

	done := make(chan struct{})
	go func() {
		cm.wg.Wait()
		close(done)
	}()

	var drainErr error
	select {
	case <-done:
		if inFlightCancel != nil {
			inFlightCancel()
		}
		cm.log.Info("mq_consumers_drained", slog.String("node_id", cm.nodeID))
	case <-ctx.Done():
		drainErr = ctx.Err()
		if inFlightCancel != nil {
			inFlightCancel()
		}
		// Briefly wait for workers to observe cancellation cleanly for race safety.
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
		cm.log.Warn("mq_consumers_drain_timeout",
			slog.String("node_id", cm.nodeID),
			slog.String("error", sanitizeErrorSummary(drainErr)),
		)
	}

	cm.mu.Lock()
	cm.started = false
	cm.draining = false
	cm.claimCancel = nil
	cm.inFlightCancel = nil
	cm.mu.Unlock()
	return drainErr
}

func (cm *ConsumerManager) runWorker(claimCtx, inFlightCtx context.Context, stream, group, consumer string) {
	// Recovery phase: process existing pending entries for this consumer before reading `>`.
	cm.recoverPending(claimCtx, inFlightCtx, stream, group, consumer)

	for claimCtx.Err() == nil {
		streams, err := cm.rdb.XReadGroup(claimCtx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: consumer,
			Streams:  []string{stream, ">"},
			Count:    cm.cfg.ReadCount,
			Block:    cm.cfg.BlockTimeout,
		}).Result()
		if err != nil {
			if claimCtx.Err() != nil {
				return
			}
			if errors.Is(err, redis.Nil) {
				continue
			}
			if isNoGroupErr(err) {
				if ensureErr := cm.EnsureGroups(claimCtx); ensureErr != nil && claimCtx.Err() == nil {
					cm.log.Warn("mq_ensure_groups_recover_failed",
						slog.String("stream", stream),
						slog.String("group", group),
						slog.String("error", sanitizeErrorSummary(ensureErr)),
					)
				} else {
					continue
				}
			}
			cm.log.Warn("mq_xreadgroup_failed",
				slog.String("stream", stream),
				slog.String("group", group),
				slog.String("consumer", consumer),
				slog.String("error", sanitizeErrorSummary(err)),
			)
			select {
			case <-claimCtx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}

		for _, s := range streams {
			for _, msg := range s.Messages {
				if inFlightCtx.Err() != nil {
					return
				}
				_, _, _ = cm.handleEntry(inFlightCtx, stream, group, consumer, msg, 1)
			}
		}
	}
}

func (cm *ConsumerManager) recoverPending(claimCtx, inFlightCtx context.Context, stream, group, consumer string) {
	cursor := "0-0"
	for claimCtx.Err() == nil {
		streams, err := cm.rdb.XReadGroup(claimCtx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: consumer,
			Streams:  []string{stream, cursor},
			Count:    cm.cfg.ReadCount,
		}).Result()
		if err != nil {
			if claimCtx.Err() == nil && !errors.Is(err, redis.Nil) {
				cm.log.Warn("mq_recover_pending_failed",
					slog.String("stream", stream),
					slog.String("group", group),
					slog.String("consumer", consumer),
					slog.String("error", sanitizeErrorSummary(err)),
				)
			}
			return
		}
		total := 0
		var lastID string
		for _, s := range streams {
			for _, msg := range s.Messages {
				total++
				lastID = msg.ID
				if inFlightCtx.Err() != nil {
					return
				}
				deliveryCount := cm.lookupDeliveryCount(inFlightCtx, stream, group, msg.ID, 1)
				_, _, _ = cm.handleEntry(inFlightCtx, stream, group, consumer, msg, deliveryCount)
			}
		}
		if total == 0 || lastID == "" {
			return
		}
		cursor = lastID
	}
}

func (cm *ConsumerManager) runReclaimLoop(claimCtx, inFlightCtx context.Context) {
	ticker := time.NewTicker(cm.cfg.ReclaimInterval)
	defer ticker.Stop()
	for {
		select {
		case <-claimCtx.Done():
			return
		case <-ticker.C:
			if claimCtx.Err() != nil {
				return
			}
			if _, err := cm.ReclaimOnce(inFlightCtx); err != nil && claimCtx.Err() == nil {
				cm.log.Warn("mq_reclaim_loop_error", slog.String("error", sanitizeErrorSummary(err)))
			}
		}
	}
}

func (cm *ConsumerManager) runTrimLoop(claimCtx, inFlightCtx context.Context) {
	ticker := time.NewTicker(cm.cfg.TrimInterval)
	defer ticker.Stop()
	for {
		select {
		case <-claimCtx.Done():
			return
		case <-ticker.C:
			if claimCtx.Err() != nil {
				return
			}
			if _, err := cm.producer.TrimStreams(inFlightCtx); err != nil && claimCtx.Err() == nil {
				cm.log.Warn("mq_trim_loop_error", slog.String("error", sanitizeErrorSummary(err)))
			}
		}
	}
}

// ReclaimOnce runs one XAUTOCLAIM sweep across all configured (stream, group) pairs.
func (cm *ConsumerManager) ReclaimOnce(ctx context.Context) ([]ReclaimResult, error) {
	if cm == nil || cm.rdb == nil {
		return nil, errors.New("redis client is required for mq reclaim")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	results := make([]ReclaimResult, 0, len(cm.targets))
	var errs []error
	for _, tg := range cm.targets {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		res, err := cm.ReclaimGroupOnce(ctx, tg.Stream, tg.Group)
		results = append(results, res)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return results, errors.Join(errs...)
}

// ReclaimGroupOnce executes `XAUTOCLAIM <stream> <group> reclaim-{node_id} <min_idle> <start> COUNT <count>`,
// updates `im:stream:pending:reclaim:{messages|events}:{group}` with daily TTL, and processes/DLQs claimed entries.
func (cm *ConsumerManager) ReclaimGroupOnce(ctx context.Context, stream, group string) (ReclaimResult, error) {
	reclaimer := "reclaim-" + cm.nodeID
	startCursor := cm.getReclaimCursor(stream, group)
	res := ReclaimResult{
		Stream:  stream,
		Group:   group,
		StartID: startCursor,
	}

	msgs, nextStart, err := cm.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   stream,
		Group:    group,
		MinIdle:  cm.cfg.ReclaimMinIdle,
		Start:    startCursor,
		Count:    cm.cfg.ReadCount,
		Consumer: reclaimer,
	}).Result()
	if err != nil {
		if isNoSuchKeyErr(err) || isNoGroupErr(err) {
			return res, nil
		}
		return res, fmt.Errorf("xautoclaim %s %s: %w", stream, group, err)
	}
	if nextStart == "" {
		nextStart = "0-0"
	}
	res.NextID = nextStart
	res.ClaimedCount = len(msgs)
	cm.setReclaimCursor(stream, group, nextStart)

	for _, msg := range msgs {
		if ctx.Err() != nil {
			break
		}
		deliveryCount := cm.lookupDeliveryCount(ctx, stream, group, msg.ID, 2)
		acked, dead, handleErr := cm.handleEntry(ctx, stream, group, reclaimer, msg, deliveryCount)
		if acked && !dead {
			res.AckedCount++
		}
		if handleErr != nil {
			res.FailedCount++
		}
		if dead {
			res.DeadLetterCount++
		}
	}

	hashKey := PendingReclaimKey(stream, group)
	nowStr := cm.nowFn().UTC().Format(utcMillisLayout)
	pipe := cm.rdb.Pipeline()
	pipe.HSet(ctx, hashKey, map[string]any{
		"start_id":          startCursor,
		"next_id":           nextStart,
		"claimed_count":     res.ClaimedCount,
		"acked_count":       res.AckedCount,
		"failed_count":      res.FailedCount,
		"dead_letter_count": res.DeadLetterCount,
		"reclaimed_at":      nowStr,
		"consumer":          reclaimer,
	})
	pipe.Expire(ctx, hashKey, cm.cfg.PendingReclaimTTL)
	if _, pipeErr := pipe.Exec(ctx); pipeErr != nil {
		return res, fmt.Errorf("update reclaim observability hash %s: %w", hashKey, pipeErr)
	}

	return res, nil
}

func (cm *ConsumerManager) handleEntry(
	ctx context.Context,
	stream, group, consumer string,
	msg redis.XMessage,
	pelDeliveryCount int,
) (acked bool, dead bool, err error) {
	retryKey := retryMetaKey(stream, group, msg.ID)
	priorFailures, priorErr := cm.loadRetryMeta(ctx, retryKey)
	age := cm.entryAge(msg.ID)

	// If already at retry limit or already failed and exceeded max pending age, send straight to DLQ.
	if priorFailures >= cm.cfg.MaxRetries ||
		pelDeliveryCount > cm.cfg.MaxRetries ||
		(priorFailures > 0 && age >= cm.cfg.MaxPendingAge) {
		summary := priorErr
		if summary == "" {
			summary = fmt.Sprintf("exceeded pending limits (failures=%d, pel_deliveries=%d, age=%s)",
				priorFailures, pelDeliveryCount, age.Round(time.Second))
		}
		effective := max(priorFailures, pelDeliveryCount)
		if dlqErr := cm.sendToDeadLetterAndAck(ctx, stream, group, consumer, msg, effective, summary, retryKey); dlqErr != nil {
			return false, false, dlqErr
		}
		return true, true, errors.New(summary)
	}

	dispatchErr := cm.dispatchEntry(ctx, stream, group, msg)
	if dispatchErr == nil {
		if ackErr := cm.rdb.XAck(ctx, stream, group, msg.ID).Err(); ackErr != nil {
			return false, false, fmt.Errorf("xack %s %s %s: %w", stream, group, msg.ID, ackErr)
		}
		_ = cm.rdb.Del(ctx, retryKey).Err()
		return true, false, nil
	}

	// Do NOT XACK on context cancellation/shutdown unless threshold reached; keep in PEL for next node.
	if ctx.Err() != nil && errors.Is(dispatchErr, context.Canceled) {
		return false, false, dispatchErr
	}

	summary := sanitizeErrorSummary(dispatchErr)
	failures := cm.recordFailure(ctx, retryKey, consumer, summary)
	effectiveFailures := max(failures, pelDeliveryCount)

	if effectiveFailures >= cm.cfg.MaxRetries || age >= cm.cfg.MaxPendingAge {
		if dlqErr := cm.sendToDeadLetterAndAck(ctx, stream, group, consumer, msg, effectiveFailures, summary, retryKey); dlqErr != nil {
			return false, false, errors.Join(dispatchErr, dlqErr)
		}
		return true, true, dispatchErr
	}

	return false, false, dispatchErr
}

func (cm *ConsumerManager) sendToDeadLetterAndAck(
	ctx context.Context,
	stream, group, lastConsumer string,
	msg redis.XMessage,
	failureCount int,
	errSummary string,
	retryKey string,
) error {
	deadStream := DeadLetterStream(stream)
	payloadJSON, _ := json.Marshal(msg.Values)
	values := map[string]any{
		"source_stream":      stream,
		"group":              group,
		"original_stream_id": msg.ID,
		"last_consumer":      lastConsumer,
		"failure_count":      strconv.Itoa(failureCount),
		"error_summary":      errSummary,
		"failed_at":          cm.nowFn().UTC().Format(utcMillisLayout),
		"payload_json":       string(payloadJSON),
	}
	for _, k := range []string{"kind", "message_id", "conversation_id", "seq", "event_id", "payload_ref"} {
		if v := stringField(msg.Values, k); v != "" {
			values[k] = v
		}
	}

	deadID, addErr := cm.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: deadStream,
		MaxLen: cm.cfg.DeadStreamMaxLen,
		Approx: true,
		ID:     "*",
		Values: values,
	}).Result()
	if addErr != nil {
		return fmt.Errorf("xadd dead letter %s: %w", deadStream, addErr)
	}

	if ackErr := cm.rdb.XAck(ctx, stream, group, msg.ID).Err(); ackErr != nil {
		return fmt.Errorf("xack after dead letter %s %s %s: %w", stream, group, msg.ID, ackErr)
	}
	_ = cm.rdb.Del(ctx, retryKey).Err()

	cm.log.Warn("mq_entry_dead_lettered",
		slog.String("source_stream", stream),
		slog.String("dead_stream", deadStream),
		slog.String("dead_id", deadID),
		slog.String("group", group),
		slog.String("original_stream_id", msg.ID),
		slog.String("last_consumer", lastConsumer),
		slog.Int("failure_count", failureCount),
	)
	return nil
}

func (cm *ConsumerManager) lookupDeliveryCount(ctx context.Context, stream, group, id string, fallback int) int {
	ext, err := cm.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: stream,
		Group:  group,
		Start:  id,
		End:    id,
		Count:  1,
	}).Result()
	if err != nil || len(ext) == 0 {
		return fallback
	}
	if ext[0].RetryCount > 0 {
		return int(ext[0].RetryCount)
	}
	return fallback
}

func (cm *ConsumerManager) entryAge(streamID string) time.Duration {
	created, ok := streamIDTime(streamID)
	if !ok {
		return 0
	}
	age := cm.nowFn().UTC().Sub(created)
	if age < 0 {
		return 0
	}
	return age
}

func (cm *ConsumerManager) loadRetryMeta(ctx context.Context, retryKey string) (int, string) {
	fields, err := cm.rdb.HGetAll(ctx, retryKey).Result()
	if err != nil || len(fields) == 0 {
		return 0, ""
	}
	failures, _ := strconv.Atoi(fields["failures"])
	return failures, fields["last_error"]
}

func (cm *ConsumerManager) recordFailure(ctx context.Context, retryKey, consumer, summary string) int {
	pipe := cm.rdb.Pipeline()
	incrCmd := pipe.HIncrBy(ctx, retryKey, "failures", 1)
	pipe.HSet(ctx, retryKey, map[string]any{
		"last_error":     summary,
		"last_consumer":  consumer,
		"last_failed_at": cm.nowFn().UTC().Format(utcMillisLayout),
	})
	pipe.Expire(ctx, retryKey, max(24*time.Hour, cm.cfg.PendingReclaimTTL))
	if _, err := pipe.Exec(ctx); err != nil {
		return 1
	}
	return int(incrCmd.Val())
}

func (cm *ConsumerManager) getReclaimCursor(stream, group string) string {
	cm.cursorMu.Lock()
	defer cm.cursorMu.Unlock()
	k := stream + "|" + group
	if cur := cm.reclaimCursors[k]; cur != "" {
		return cur
	}
	return "0-0"
}

func (cm *ConsumerManager) setReclaimCursor(stream, group, cursor string) {
	cm.cursorMu.Lock()
	defer cm.cursorMu.Unlock()
	if cursor == "" {
		cursor = "0-0"
	}
	cm.reclaimCursors[stream+"|"+group] = cursor
}

func retryMetaKey(stream, group, streamID string) string {
	return fmt.Sprintf("im:mq:retry:%s:%s:%s", streamTag(stream), group, streamID)
}

func workerShortName(group string) string {
	return strings.TrimPrefix(group, "im-")
}

func isBusyGroupErr(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "BUSYGROUP")
}

func isNoGroupErr(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "NOGROUP")
}
