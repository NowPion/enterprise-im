package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

var (
	// ErrRedisClientNil indicates a required Redis client dependency was not configured.
	ErrRedisClientNil = errors.New("redis client is nil")
	// ErrConvLockTimeout indicates acquiring conv:lock:{conversation_id} timed out.
	ErrConvLockTimeout = errors.New("conversation sequence lock acquisition timed out")
)

const (
	defaultConvLockTTL = 5 * time.Second
	maxAcquireWait     = 3 * time.Second
)

var (
	releaseConvLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

	rollbackConvSeqScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
	return 0
end
if ARGV[2] == "DEL" then
	return redis.call("DEL", KEYS[2])
end
return redis.call("SET", KEYS[2], ARGV[3])
`)

	allocateConvSeqScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
	return {0, "LOCK_LOST"}
end
local prev = redis.call("GET", KEYS[2])
local hadPrev = 1
if not prev then
	hadPrev = 0
	prev = ""
end
local maxSeq = tonumber(ARGV[2])
local prevNum = tonumber(prev)
if hadPrev == 0 or prevNum == nil or prevNum ~= maxSeq then
	redis.call("SET", KEYS[2], ARGV[2])
end
local nextSeq = redis.call("INCR", KEYS[2])
return {1, tostring(nextSeq), tostring(hadPrev), tostring(prev)}
`)
)

// DirectKey formats the canonical single-conversation unique key min(u1,u2):max(u1,u2).
func DirectKey(u1, u2 uint64) string {
	if u1 > u2 {
		u1, u2 = u2, u1
	}
	return fmt.Sprintf("%d:%d", u1, u2)
}

// SeqReservation holds an allocated conversation sequence number and a rollback compensator.
type SeqReservation struct {
	Seq      uint64
	Rollback func(ctx context.Context) error
}

// SeqAllocator coordinates distributed conversation sequence locking and allocation.
type SeqAllocator interface {
	AcquireConvLock(ctx context.Context, convID uint64) (string, error)
	AllocateNextSeq(ctx context.Context, tx *gorm.DB, convID uint64, lockToken string) (*SeqReservation, error)
	ReleaseConvLock(ctx context.Context, convID uint64, lockToken string) error
}

// RedisSeqAllocator implements SeqAllocator per docs/architecture.md using
// conv:lock:{conversation_id} and conv:seq:{conversation_id} calibrated with MySQL MAX(seq).
type RedisSeqAllocator struct {
	rdb     *redis.Client
	lockTTL time.Duration
}

// NewRedisSeqAllocator constructs a RedisSeqAllocator.
func NewRedisSeqAllocator(rdb *redis.Client, lockTTL time.Duration) *RedisSeqAllocator {
	if lockTTL <= 0 {
		lockTTL = defaultConvLockTTL
	}
	return &RedisSeqAllocator{
		rdb:     rdb,
		lockTTL: lockTTL,
	}
}

// AcquireConvLock acquires SET conv:lock:{convID} <token> NX PX <ttl>.
func (a *RedisSeqAllocator) AcquireConvLock(ctx context.Context, convID uint64) (string, error) {
	if a == nil || a.rdb == nil {
		return "", ErrRedisClientNil
	}
	if convID == 0 {
		return "", errors.New("conversation id must be positive")
	}

	token, err := newLockToken()
	if err != nil {
		return "", fmt.Errorf("generate lock token: %w", err)
	}
	lockKey := convLockKey(convID)

	waitLimit := a.lockTTL
	if waitLimit > maxAcquireWait {
		waitLimit = maxAcquireWait
	}
	deadline := time.Now().Add(waitLimit)

	for {
		acquired, setErr := a.rdb.SetNX(ctx, lockKey, token, a.lockTTL).Result()
		if setErr != nil {
			return "", fmt.Errorf("acquire conv lock: %w", setErr)
		}
		if acquired {
			return token, nil
		}

		if time.Now().After(deadline) {
			return "", ErrConvLockTimeout
		}

		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

// AllocateNextSeq calibrates conv:seq:{convID} against MySQL MAX(seq) inside the lock and executes INCR.
func (a *RedisSeqAllocator) AllocateNextSeq(ctx context.Context, tx *gorm.DB, convID uint64, lockToken string) (*SeqReservation, error) {
	if a == nil || a.rdb == nil {
		return nil, ErrRedisClientNil
	}
	if tx == nil {
		return nil, errors.New("tx must not be nil")
	}
	if convID == 0 || lockToken == "" {
		return nil, errors.New("invalid conversation id or lock token")
	}

	var maxSeq uint64
	if err := tx.WithContext(ctx).
		Model(&model.Message{}).
		Where("conversation_id = ?", convID).
		Select("COALESCE(MAX(seq), 0)").
		Scan(&maxSeq).Error; err != nil {
		return nil, fmt.Errorf("query max message seq: %w", err)
	}

	lockKey := convLockKey(convID)
	seqKey := convSeqKey(convID)

	// Atomically verify lock ownership, calibrate conv:seq against MySQL MAX(seq), and INCR.
	rawRes, err := allocateConvSeqScript.Run(ctx, a.rdb, []string{lockKey, seqKey}, lockToken, strconv.FormatUint(maxSeq, 10)).Result()
	if err != nil {
		return nil, fmt.Errorf("allocate conv seq: %w", err)
	}
	items, ok := rawRes.([]any)
	if !ok || len(items) < 2 {
		return nil, fmt.Errorf("unexpected allocate conv seq response: %T", rawRes)
	}
	statusInt, err := toInt64(items[0])
	if err != nil {
		return nil, fmt.Errorf("parse allocate seq status: %w", err)
	}
	if statusInt == 0 {
		return nil, ErrConvLockTimeout
	}
	if len(items) < 4 {
		return nil, errors.New("incomplete allocate seq response")
	}

	nextSeqStr := fmt.Sprint(items[1])
	nextSeq, err := strconv.ParseUint(nextSeqStr, 10, 64)
	if err != nil || nextSeq == 0 {
		return nil, fmt.Errorf("invalid incremented seq %q: %w", nextSeqStr, err)
	}
	hadPrev := fmt.Sprint(items[2]) == "1"
	prevVal := fmt.Sprint(items[3])

	return &SeqReservation{
		Seq: nextSeq,
		Rollback: func(rbCtx context.Context) error {
			mode := "SET"
			val := prevVal
			if !hadPrev {
				mode = "DEL"
				val = ""
			}
			return rollbackConvSeqScript.Run(rbCtx, a.rdb, []string{lockKey, seqKey}, lockToken, mode, val).Err()
		},
	}, nil
}

func toInt64(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case string:
		return strconv.ParseInt(t, 10, 64)
	default:
		return strconv.ParseInt(fmt.Sprint(v), 10, 64)
	}
}

// ReleaseConvLock releases conv:lock:{convID} if it is still owned by lockToken.
func (a *RedisSeqAllocator) ReleaseConvLock(ctx context.Context, convID uint64, lockToken string) error {
	if a == nil || a.rdb == nil || convID == 0 || lockToken == "" {
		return nil
	}
	return releaseConvLockScript.Run(ctx, a.rdb, []string{convLockKey(convID)}, lockToken).Err()
}

// PresenceChecker queries live online state for batches of users.
type PresenceChecker interface {
	BatchIsOnline(ctx context.Context, userIDs []uint64) (map[uint64]bool, error)
}

// RedisPresenceChecker reads `im:presence:user:{user_id}` keys via Redis MGET.
type RedisPresenceChecker struct {
	rdb *redis.Client
}

// NewRedisPresenceChecker constructs a RedisPresenceChecker.
func NewRedisPresenceChecker(rdb *redis.Client) *RedisPresenceChecker {
	return &RedisPresenceChecker{rdb: rdb}
}

// BatchIsOnline returns a map of userID -> true for users with active connection count > 0.
func (p *RedisPresenceChecker) BatchIsOnline(ctx context.Context, userIDs []uint64) (map[uint64]bool, error) {
	result := make(map[uint64]bool, len(userIDs))
	if p == nil || p.rdb == nil || len(userIDs) == 0 {
		return result, nil
	}

	uniqueIDs := dedupePositiveUint64s(userIDs)
	if len(uniqueIDs) == 0 {
		return result, nil
	}

	keys := make([]string, len(uniqueIDs))
	for i, id := range uniqueIDs {
		keys[i] = fmt.Sprintf("im:presence:user:%d", id)
	}

	vals, err := p.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("mget user presence: %w", err)
	}

	for i, raw := range vals {
		if raw == nil {
			continue
		}
		var count int64
		switch v := raw.(type) {
		case string:
			parsed, parseErr := strconv.ParseInt(v, 10, 64)
			if parseErr == nil {
				count = parsed
			}
		case []byte:
			parsed, parseErr := strconv.ParseInt(string(v), 10, 64)
			if parseErr == nil {
				count = parsed
			}
		case int64:
			count = v
		}
		if count > 0 {
			result[uniqueIDs[i]] = true
		}
	}
	return result, nil
}

// FriendRequestListFilter describes query filters and cursor pagination bounds.
type FriendRequestListFilter struct {
	UserID    uint64
	Direction string
	Status    string
	CursorID  uint64
	Limit     int
}

// FriendRequestRepository encapsulates MySQL persistence operations for friend requests
// and single-chat setup on acceptance.
type FriendRequestRepository interface {
	WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error
	LockUsersAscending(ctx context.Context, tx *gorm.DB, userA, userB uint64) (map[uint64]model.User, error)
	HasActiveFriendship(ctx context.Context, tx *gorm.DB, userA, userB uint64) (bool, error)
	HasPendingFriendRequest(ctx context.Context, tx *gorm.DB, userA, userB uint64) (bool, error)
	CreateFriendRequest(ctx context.Context, tx *gorm.DB, req *model.FriendRequest) error
	GetFriendRequestByID(ctx context.Context, id uint64) (*model.FriendRequest, error)
	LockFriendRequestByID(ctx context.Context, tx *gorm.DB, id uint64) (*model.FriendRequest, error)
	UpdateFriendRequestStatus(ctx context.Context, tx *gorm.DB, id uint64, fromStatus, toStatus string, handledAt time.Time) (bool, error)
	ListFriendRequests(ctx context.Context, filter FriendRequestListFilter) ([]model.FriendRequest, bool, error)
	BatchGetUsersByIDs(ctx context.Context, ids []uint64) (map[uint64]model.User, error)
	FindSingleConversationIDByDirectKey(ctx context.Context, directKey string) (uint64, error)
	LockSingleConversationByDirectKey(ctx context.Context, tx *gorm.DB, directKey string) (*model.Conversation, error)
	CreateSingleConversation(ctx context.Context, tx *gorm.DB, directKey string) (*model.Conversation, error)
	EnsureConversationMembersVisible(ctx context.Context, tx *gorm.DB, convID uint64, userIDs []uint64, baselineSeq uint64, now time.Time) error
	CreateSystemMessage(ctx context.Context, tx *gorm.DB, msg *model.Message) error
	UpdateConversationLastMessage(ctx context.Context, tx *gorm.DB, convID uint64, msg *model.Message) error
	IncrementMembersUnreadCount(ctx context.Context, tx *gorm.DB, convID uint64, userIDs []uint64, now time.Time) error
	CreateMessageReceipts(ctx context.Context, tx *gorm.DB, receipts []model.MessageReceipt) error
}

// GormFriendRequestRepository is the GORM/MySQL implementation of FriendRequestRepository.
type GormFriendRequestRepository struct {
	db *gorm.DB
}

// NewGormFriendRequestRepository constructs a GormFriendRequestRepository.
func NewGormFriendRequestRepository(db *gorm.DB) *GormFriendRequestRepository {
	return &GormFriendRequestRepository{db: db}
}

// WithTx executes fn inside a GORM database transaction.
func (r *GormFriendRequestRepository) WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return WithTx(ctx, r.db, fn)
}

// LockUsersAscending locks userA and userB rows in `users` using FOR UPDATE ordered by id ASC.
func (r *GormFriendRequestRepository) LockUsersAscending(ctx context.Context, tx *gorm.DB, userA, userB uint64) (map[uint64]model.User, error) {
	ids := dedupePositiveUint64s([]uint64{userA, userB})
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make(map[uint64]model.User, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	var users []model.User
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", ids).
		Order("id ASC").
		Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

// HasActiveFriendship reports whether either directional friendship record exists with status = 1.
func (r *GormFriendRequestRepository) HasActiveFriendship(ctx context.Context, tx *gorm.DB, userA, userB uint64) (bool, error) {
	var count int64
	err := tx.WithContext(ctx).
		Model(&model.Friendship{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("((user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)) AND status = ?",
			userA, userB, userB, userA, model.FriendshipStatusActive).
		Limit(1).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// HasPendingFriendRequest reports whether a pending friend request exists in either direction.
func (r *GormFriendRequestRepository) HasPendingFriendRequest(ctx context.Context, tx *gorm.DB, userA, userB uint64) (bool, error) {
	var count int64
	err := tx.WithContext(ctx).
		Model(&model.FriendRequest{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("((from_user_id = ? AND to_user_id = ?) OR (from_user_id = ? AND to_user_id = ?)) AND status = ?",
			userA, userB, userB, userA, model.FriendRequestStatusPending).
		Limit(1).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// CreateFriendRequest inserts a new friend_requests row.
func (r *GormFriendRequestRepository) CreateFriendRequest(ctx context.Context, tx *gorm.DB, req *model.FriendRequest) error {
	return tx.WithContext(ctx).Create(req).Error
}

// GetFriendRequestByID fetches a friend request without locking.
func (r *GormFriendRequestRepository) GetFriendRequestByID(ctx context.Context, id uint64) (*model.FriendRequest, error) {
	var req model.FriendRequest
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&req).Error
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// LockFriendRequestByID fetches and locks a friend_requests row FOR UPDATE.
func (r *GormFriendRequestRepository) LockFriendRequestByID(ctx context.Context, tx *gorm.DB, id uint64) (*model.FriendRequest, error) {
	var req model.FriendRequest
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", id).
		First(&req).Error
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// UpdateFriendRequestStatus conditionally transitions status from fromStatus to toStatus.
func (r *GormFriendRequestRepository) UpdateFriendRequestStatus(ctx context.Context, tx *gorm.DB, id uint64, fromStatus, toStatus string, handledAt time.Time) (bool, error) {
	res := tx.WithContext(ctx).
		Model(&model.FriendRequest{}).
		Where("id = ? AND status = ?", id, fromStatus).
		Updates(map[string]any{
			"status":     toStatus,
			"handled_at": handledAt,
			"updated_at": handledAt,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ListFriendRequests lists received or sent friend requests ordered by id DESC with cursor pagination.
func (r *GormFriendRequestRepository) ListFriendRequests(ctx context.Context, filter FriendRequestListFilter) ([]model.FriendRequest, bool, error) {
	q := r.db.WithContext(ctx).Model(&model.FriendRequest{})
	switch filter.Direction {
	case "received":
		q = q.Where("to_user_id = ?", filter.UserID)
	case "sent":
		q = q.Where("from_user_id = ?", filter.UserID)
	default:
		return nil, false, fmt.Errorf("unsupported direction %q", filter.Direction)
	}
	if filter.Status != "" {
		q = q.Where("status = ?", filter.Status)
	}
	if filter.CursorID > 0 {
		q = q.Where("id < ?", filter.CursorID)
	}

	var rows []model.FriendRequest
	if err := q.Order("id DESC").Limit(filter.Limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(rows) > filter.Limit {
		hasMore = true
		rows = rows[:filter.Limit]
	}
	return rows, hasMore, nil
}

// BatchGetUsersByIDs loads a batch of users using a single IN query (anti-N+1).
func (r *GormFriendRequestRepository) BatchGetUsersByIDs(ctx context.Context, ids []uint64) (map[uint64]model.User, error) {
	uniqueIDs := dedupePositiveUint64s(ids)
	out := make(map[uint64]model.User, len(uniqueIDs))
	if len(uniqueIDs) == 0 {
		return out, nil
	}
	var users []model.User
	if err := r.db.WithContext(ctx).Where("id IN ?", uniqueIDs).Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

// FindSingleConversationIDByDirectKey looks up an existing single conversation ID without locking.
func (r *GormFriendRequestRepository) FindSingleConversationIDByDirectKey(ctx context.Context, directKey string) (uint64, error) {
	var conv model.Conversation
	err := r.db.WithContext(ctx).
		Select("id").
		Where("direct_key = ?", directKey).
		Take(&conv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return conv.ID, nil
}

// LockSingleConversationByDirectKey loads and locks an existing single conversation row FOR UPDATE.
func (r *GormFriendRequestRepository) LockSingleConversationByDirectKey(ctx context.Context, tx *gorm.DB, directKey string) (*model.Conversation, error) {
	var conv model.Conversation
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("direct_key = ?", directKey).
		Take(&conv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &conv, nil
}

// CreateSingleConversation creates a unique single conversation keyed by directKey.
func (r *GormFriendRequestRepository) CreateSingleConversation(ctx context.Context, tx *gorm.DB, directKey string) (*model.Conversation, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	key := directKey
	conv := &model.Conversation{
		Type:      model.ConversationTypeSingle,
		DirectKey: &key,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := tx.WithContext(ctx).Create(conv).Error; err != nil {
		return nil, err
	}
	return conv, nil
}

// EnsureConversationMembersVisible creates or restores conversation_members rows with is_visible = true.
func (r *GormFriendRequestRepository) EnsureConversationMembersVisible(ctx context.Context, tx *gorm.DB, convID uint64, userIDs []uint64, baselineSeq uint64, now time.Time) error {
	uniqueIDs := dedupePositiveUint64s(userIDs)
	if len(uniqueIDs) == 0 {
		return nil
	}
	sort.Slice(uniqueIDs, func(i, j int) bool { return uniqueIDs[i] < uniqueIDs[j] })

	members := make([]model.ConversationMember, 0, len(uniqueIDs))
	for _, uid := range uniqueIDs {
		members = append(members, model.ConversationMember{
			ConversationID:   convID,
			UserID:           uid,
			LastDeliveredSeq: baselineSeq,
			LastReadSeq:      baselineSeq,
			UnreadCount:      0,
			IsVisible:        true,
			JoinedAt:         now,
			UpdatedAt:        now,
		})
	}

	return tx.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "conversation_id"},
				{Name: "user_id"},
			},
			DoUpdates: clause.Assignments(map[string]any{
				"is_visible": true,
				"updated_at": now,
			}),
		}).
		Create(&members).Error
}

// CreateSystemMessage inserts a Snowflake-ID system message into `messages`.
func (r *GormFriendRequestRepository) CreateSystemMessage(ctx context.Context, tx *gorm.DB, msg *model.Message) error {
	return tx.WithContext(ctx).Create(msg).Error
}

// UpdateConversationLastMessage updates the conversation summary fields after inserting a message.
func (r *GormFriendRequestRepository) UpdateConversationLastMessage(ctx context.Context, tx *gorm.DB, convID uint64, msg *model.Message) error {
	preview := truncateRunes(msg.Content, 255)
	ts := msg.CreatedAt.UTC().Truncate(time.Millisecond)
	return tx.WithContext(ctx).
		Model(&model.Conversation{}).
		Where("id = ?", convID).
		Updates(map[string]any{
			"last_msg_id":      msg.ID,
			"last_seq":         msg.Seq,
			"last_msg_preview": preview,
			"last_msg_at":      &ts,
			"updated_at":       ts,
		}).Error
}

// IncrementMembersUnreadCount increments unread_count by 1 for the specified conversation members.
func (r *GormFriendRequestRepository) IncrementMembersUnreadCount(ctx context.Context, tx *gorm.DB, convID uint64, userIDs []uint64, now time.Time) error {
	uniqueIDs := dedupePositiveUint64s(userIDs)
	if len(uniqueIDs) == 0 {
		return nil
	}
	return tx.WithContext(ctx).
		Model(&model.ConversationMember{}).
		Where("conversation_id = ? AND user_id IN ?", convID, uniqueIDs).
		Updates(map[string]any{
			"unread_count": gorm.Expr("unread_count + 1"),
			"updated_at":   now,
		}).Error
}

// CreateMessageReceipts persists initial message delivery receipt rows.
func (r *GormFriendRequestRepository) CreateMessageReceipts(ctx context.Context, tx *gorm.DB, receipts []model.MessageReceipt) error {
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

func convLockKey(convID uint64) string {
	return fmt.Sprintf("conv:lock:%d", convID)
}

func convSeqKey(convID uint64) string {
	return fmt.Sprintf("conv:seq:%d", convID)
}

func newLockToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func dedupePositiveUint64s(in []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(in))
	out := make([]uint64, 0, len(in))
	for _, v := range in {
		if v == 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return strings.Clone(s)
	}
	return string(runes[:maxRunes])
}
