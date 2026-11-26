package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

const (
	redisPresenceUserKeyPrefix = "im:presence:user:"
	redisPresenceConnKeyPrefix = "im:presence:conn:"
	redisRouteUserKeyPrefix    = "im:route:user:"
)

var (
	// ErrInvalidUserPair indicates user IDs are zero or identical when forming a friendship/direct conversation pair.
	ErrInvalidUserPair = errors.New("invalid user pair: user IDs must be positive and distinct")
	// ErrFriendshipRecordNotFound indicates no friendship row exists for the requested user pair.
	ErrFriendshipRecordNotFound = errors.New("friendship record not found")
	// ErrFriendshipInactive indicates a friendship row exists but is deactivated (status != 1).
	ErrFriendshipInactive = errors.New("friendship is inactive")
	// ErrRepositoryUnavailable indicates MySQL or Redis dependency handle is nil.
	ErrRepositoryUnavailable = errors.New("repository dependency handle is nil")
)

// DeleteFriendshipOutcome captures the persisted result of deactivating a bidirectional friendship.
type DeleteFriendshipOutcome struct {
	FriendID       uint64
	ConversationID *uint64
	IsVisible      bool
}

// FriendshipRepository provides GORM MySQL and Redis persistence operations for friendships
// and single-chat conversation visibility/lookup.
type FriendshipRepository struct {
	db  *gorm.DB
	rdb *redis.Client
}

// NewFriendshipRepository constructs a FriendshipRepository with MySQL and Redis handles.
func NewFriendshipRepository(db *gorm.DB, rdb *redis.Client) *FriendshipRepository {
	return &FriendshipRepository{
		db:  db,
		rdb: rdb,
	}
}

// OrderedUserPair normalizes two positive distinct user IDs into ascending (minID, maxID) order.
func OrderedUserPair(userA, userB uint64) (uint64, uint64, error) {
	if userA == 0 || userB == 0 || userA == userB {
		return 0, 0, ErrInvalidUserPair
	}
	if userA < userB {
		return userA, userB, nil
	}
	return userB, userA, nil
}

// FormatDirectKey returns the canonical single-chat direct_key ("min(u1,u2):max(u1,u2)").
func FormatDirectKey(userA, userB uint64) (string, error) {
	minID, maxID, err := OrderedUserPair(userA, userB)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", minID, maxID), nil
}

// ListActiveFriendships returns active friendships for userID ordered by id DESC with cursor pagination.
// It queries up to limit+1 rows so callers can compute has_more without a separate COUNT query.
func (r *FriendshipRepository) ListActiveFriendships(
	ctx context.Context,
	userID uint64,
	cursorID uint64,
	limit int,
) ([]model.Friendship, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if userID == 0 || limit <= 0 {
		return []model.Friendship{}, nil
	}

	q := r.db.WithContext(ctx).
		Model(&model.Friendship{}).
		Where("user_id = ? AND status = ?", userID, model.FriendshipStatusActive)
	if cursorID > 0 {
		q = q.Where("id < ?", cursorID)
	}

	var rows []model.Friendship
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list active friendships: %w", err)
	}
	return rows, nil
}

// BatchGetUsersByIDs fetches non-sensitive user fields for the given IDs in a single IN query.
func (r *FriendshipRepository) BatchGetUsersByIDs(ctx context.Context, userIDs []uint64) (map[uint64]model.User, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(userIDs)
	if len(uniqueIDs) == 0 {
		return map[uint64]model.User{}, nil
	}

	var users []model.User
	if err := r.db.WithContext(ctx).
		Select("id", "username", "nickname", "avatar_url", "signature", "last_online_at").
		Where("id IN ?", uniqueIDs).
		Find(&users).Error; err != nil {
		return nil, fmt.Errorf("batch get users: %w", err)
	}

	out := make(map[uint64]model.User, len(users))
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

// BatchGetSingleConversationsByDirectKeys fetches single conversations matching directKeys in one IN query,
// keyed by direct_key.
func (r *FriendshipRepository) BatchGetSingleConversationsByDirectKeys(
	ctx context.Context,
	directKeys []string,
) (map[string]model.Conversation, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	uniqueKeys := dedupeNonEmptyStrings(directKeys)
	if len(uniqueKeys) == 0 {
		return map[string]model.Conversation{}, nil
	}

	var convs []model.Conversation
	if err := r.db.WithContext(ctx).
		Select("id", "type", "direct_key").
		Where("type = ? AND direct_key IN ?", model.ConversationTypeSingle, uniqueKeys).
		Find(&convs).Error; err != nil {
		return nil, fmt.Errorf("batch get single conversations: %w", err)
	}

	out := make(map[string]model.Conversation, len(convs))
	for _, c := range convs {
		if c.DirectKey != nil && *c.DirectKey != "" {
			out[*c.DirectKey] = c
		}
	}
	return out, nil
}

// BatchGetActiveFriendships fetches active friendship rows for userID against friendIDs in one IN query,
// keyed by friend_id. Useful for conversation/contact assembly without N+1 queries.
func (r *FriendshipRepository) BatchGetActiveFriendships(
	ctx context.Context,
	userID uint64,
	friendIDs []uint64,
) (map[uint64]model.Friendship, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(friendIDs)
	if userID == 0 || len(uniqueIDs) == 0 {
		return map[uint64]model.Friendship{}, nil
	}

	var rows []model.Friendship
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND status = ? AND friend_id IN ?", userID, model.FriendshipStatusActive, uniqueIDs).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("batch get active friendships: %w", err)
	}

	out := make(map[uint64]model.Friendship, len(rows))
	for _, row := range rows {
		out[row.FriendID] = row
	}
	return out, nil
}

// GetOnlineStatusBatch resolves merged multi-device online status for userIDs via Redis Pipeline.
// Following docs/architecture.md:
//   - When im:route:user:{uid} contains route members ("node_id:conn_id"), at least one member must
//     have a live im:presence:conn:{conn_id} lease to count as online (filters stale route entries).
//   - When im:route:user:{uid} is empty, im:presence:user:{uid} > 0 indicates online.
//   - Total Redis round-trips are O(1): 1 pipeline (or 2 if active route sets exist).
func (r *FriendshipRepository) GetOnlineStatusBatch(ctx context.Context, userIDs []uint64) (map[uint64]bool, error) {
	if r == nil || r.rdb == nil {
		return nil, ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(userIDs)
	out := make(map[uint64]bool, len(uniqueIDs))
	if len(uniqueIDs) == 0 {
		return out, nil
	}

	pipe := r.rdb.Pipeline()
	countCmds := make(map[uint64]*redis.StringCmd, len(uniqueIDs))
	routeCmds := make(map[uint64]*redis.StringSliceCmd, len(uniqueIDs))
	for _, uid := range uniqueIDs {
		uidStr := strconv.FormatUint(uid, 10)
		countCmds[uid] = pipe.Get(ctx, redisPresenceUserKeyPrefix+uidStr)
		routeCmds[uid] = pipe.SMembers(ctx, redisRouteUserKeyPrefix+uidStr)
	}

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("redis presence pipeline exec: %w", err)
	}

	userConnIDs := make(map[uint64][]string)
	allConnIDs := make([]string, 0)
	for _, uid := range uniqueIDs {
		members, routeErr := routeCmds[uid].Result()
		if routeErr != nil && !errors.Is(routeErr, redis.Nil) {
			return nil, fmt.Errorf("redis route smembers uid %d: %w", uid, routeErr)
		}

		if len(members) > 0 {
			connIDs := make([]string, 0, len(members))
			for _, member := range members {
				if connID := extractConnIDFromRouteMember(member); connID != "" {
					connIDs = append(connIDs, connID)
					allConnIDs = append(allConnIDs, connID)
				}
			}
			userConnIDs[uid] = connIDs
			out[uid] = false
			continue
		}

		rawCount, countErr := countCmds[uid].Result()
		if errors.Is(countErr, redis.Nil) {
			out[uid] = false
			continue
		}
		if countErr != nil {
			return nil, fmt.Errorf("redis presence get uid %d: %w", uid, countErr)
		}
		n, convErr := strconv.ParseInt(strings.TrimSpace(rawCount), 10, 64)
		out[uid] = convErr == nil && n > 0
	}

	uniqueConnIDs := dedupeNonEmptyStrings(allConnIDs)
	if len(uniqueConnIDs) == 0 {
		return out, nil
	}

	connPipe := r.rdb.Pipeline()
	connExistsCmds := make(map[string]*redis.IntCmd, len(uniqueConnIDs))
	for _, connID := range uniqueConnIDs {
		connExistsCmds[connID] = connPipe.Exists(ctx, redisPresenceConnKeyPrefix+connID)
	}
	if _, err := connPipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("redis conn lease pipeline exec: %w", err)
	}

	for uid, connIDs := range userConnIDs {
		for _, connID := range connIDs {
			exists, err := connExistsCmds[connID].Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				return nil, fmt.Errorf("redis conn exists %q: %w", connID, err)
			}
			if exists > 0 {
				out[uid] = true
				break
			}
		}
	}

	return out, nil
}

// GetFriendship queries the directional friendship row for (userID, friendID).
func (r *FriendshipRepository) GetFriendship(ctx context.Context, userID, friendID uint64) (*model.Friendship, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	return GetFriendshipTx(ctx, r.db, userID, friendID)
}

// GetFriendshipTx queries the directional friendship row for (userID, friendID) using tx/db.
func GetFriendshipTx(ctx context.Context, tx *gorm.DB, userID, friendID uint64) (*model.Friendship, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	if userID == 0 || friendID == 0 {
		return nil, ErrFriendshipRecordNotFound
	}
	var row model.Friendship
	err := tx.WithContext(ctx).
		Where("user_id = ? AND friend_id = ?", userID, friendID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrFriendshipRecordNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get friendship: %w", err)
	}
	return &row, nil
}

// AreActiveFriends reports whether an active bidirectional friendship (status=1 on both sides) exists.
func (r *FriendshipRepository) AreActiveFriends(ctx context.Context, userA, userB uint64) (bool, error) {
	if r == nil || r.db == nil {
		return false, ErrRepositoryUnavailable
	}
	return AreActiveFriendsTx(ctx, r.db, userA, userB)
}

// AreActiveFriendsTx reports whether an active bidirectional friendship (status=1 on both sides) exists inside tx.
func AreActiveFriendsTx(ctx context.Context, tx *gorm.DB, userA, userB uint64) (bool, error) {
	if tx == nil {
		return false, ErrRepositoryUnavailable
	}
	minID, maxID, err := OrderedUserPair(userA, userB)
	if err != nil {
		return false, nil
	}
	var count int64
	if err := tx.WithContext(ctx).
		Model(&model.Friendship{}).
		Where("((user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)) AND status = ?",
			minID, maxID, maxID, minID, model.FriendshipStatusActive).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("check active bidirectional friendship: %w", err)
	}
	return count == 2, nil
}

// UpdateRemark updates ONLY the caller's (userID -> friendID) remark when the friendship is active.
func (r *FriendshipRepository) UpdateRemark(ctx context.Context, userID, friendID uint64, remark string) error {
	if r == nil || r.db == nil {
		return ErrRepositoryUnavailable
	}
	if _, _, err := OrderedUserPair(userID, friendID); err != nil {
		return ErrFriendshipRecordNotFound
	}

	return WithTx(ctx, r.db, func(tx *gorm.DB) error {
		var row model.Friendship
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND friend_id = ?", userID, friendID).
			First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrFriendshipRecordNotFound
		}
		if err != nil {
			return fmt.Errorf("lock friendship for remark update: %w", err)
		}
		if row.Status != model.FriendshipStatusActive {
			return ErrFriendshipInactive
		}

		now := time.Now().UTC()
		if err := tx.Model(&model.Friendship{}).
			Where("id = ?", row.ID).
			Updates(map[string]any{
				"remark":     remark,
				"updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("update friendship remark: %w", err)
		}
		return nil
	})
}

// DeactivateBidirectionalFriendship deactivates both directions of the friendship (status = 0)
// and hides the single conversation for both members (is_visible = false) inside one transaction.
// Messages and conversation_members records are preserved so existing members can still read history by ID.
func (r *FriendshipRepository) DeactivateBidirectionalFriendship(
	ctx context.Context,
	userID uint64,
	friendID uint64,
) (DeleteFriendshipOutcome, error) {
	if r == nil || r.db == nil {
		return DeleteFriendshipOutcome{}, ErrRepositoryUnavailable
	}
	minID, maxID, err := OrderedUserPair(userID, friendID)
	if err != nil {
		return DeleteFriendshipOutcome{}, ErrFriendshipRecordNotFound
	}
	directKey := fmt.Sprintf("%d:%d", minID, maxID)

	var outcome DeleteFriendshipOutcome
	outcome.FriendID = friendID
	outcome.IsVisible = false

	err = WithTx(ctx, r.db, func(tx *gorm.DB) error {
		// Lock both users in ascending ID order to align with AcceptFriendRequest lock ordering.
		var lockedUsers []uint64
		if err := tx.Model(&model.User{}).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id IN ?", []uint64{minID, maxID}).
			Order("id ASC").
			Pluck("id", &lockedUsers).Error; err != nil {
			return fmt.Errorf("lock users for friendship delete: %w", err)
		}

		var pairRows []model.Friendship
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)",
				minID, maxID, maxID, minID).
			Order("user_id ASC, friend_id ASC").
			Find(&pairRows).Error; err != nil {
			return fmt.Errorf("lock friendships for delete: %w", err)
		}

		var callerRow *model.Friendship
		for i := range pairRows {
			if pairRows[i].UserID == userID && pairRows[i].FriendID == friendID {
				callerRow = &pairRows[i]
				break
			}
		}
		if callerRow == nil {
			return ErrFriendshipRecordNotFound
		}
		if callerRow.Status != model.FriendshipStatusActive {
			return ErrFriendshipInactive
		}

		now := time.Now().UTC()
		if err := tx.Model(&model.Friendship{}).
			Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)",
				minID, maxID, maxID, minID).
			Updates(map[string]any{
				"status":     model.FriendshipStatusDeleted,
				"updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("deactivate bidirectional friendships: %w", err)
		}

		conv, convErr := FindSingleConversationByDirectKeyForUpdateTx(ctx, tx, directKey)
		if convErr != nil {
			if errors.Is(convErr, gorm.ErrRecordNotFound) {
				return nil
			}
			return convErr
		}

		convID := conv.ID
		outcome.ConversationID = &convID

		if err := tx.Model(&model.ConversationMember{}).
			Where("conversation_id = ? AND user_id IN ?", conv.ID, []uint64{minID, maxID}).
			Updates(map[string]any{
				"is_visible": false,
				"updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("hide single conversation members: %w", err)
		}

		return nil
	})
	if err != nil {
		return DeleteFriendshipOutcome{}, err
	}
	return outcome, nil
}

// FindSingleConversationByDirectKey looks up a single conversation by direct_key ("min:max").
func (r *FriendshipRepository) FindSingleConversationByDirectKey(
	ctx context.Context,
	directKey string,
) (*model.Conversation, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	return FindSingleConversationByDirectKeyTx(ctx, r.db, directKey)
}

// FindSingleConversationByDirectKeyTx looks up a single conversation by direct_key inside tx/db.
func FindSingleConversationByDirectKeyTx(
	ctx context.Context,
	tx *gorm.DB,
	directKey string,
) (*model.Conversation, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var conv model.Conversation
	err := tx.WithContext(ctx).
		Where("type = ? AND direct_key = ?", model.ConversationTypeSingle, directKey).
		First(&conv).Error
	if err != nil {
		return nil, err
	}
	return &conv, nil
}

// FindSingleConversationByDirectKeyForUpdateTx locks and fetches a single conversation by direct_key inside tx.
func FindSingleConversationByDirectKeyForUpdateTx(
	ctx context.Context,
	tx *gorm.DB,
	directKey string,
) (*model.Conversation, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var conv model.Conversation
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("type = ? AND direct_key = ?", model.ConversationTypeSingle, directKey).
		First(&conv).Error
	if err != nil {
		return nil, err
	}
	return &conv, nil
}

// UpsertBidirectionalFriendshipTx inserts or reactivates (status=1) both directional friendship rows
// for (userA, userB) inside tx, operating in ascending (minID, maxID) order to avoid deadlocks.
// Existing remarks are preserved when restoring previously deleted friendships.
func UpsertBidirectionalFriendshipTx(ctx context.Context, tx *gorm.DB, userA, userB uint64) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	minID, maxID, err := OrderedUserPair(userA, userB)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	rows := []model.Friendship{
		{
			UserID:    minID,
			FriendID:  maxID,
			Remark:    "",
			Status:    model.FriendshipStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			UserID:    maxID,
			FriendID:  minID,
			Remark:    "",
			Status:    model.FriendshipStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}

	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "friend_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"status":     model.FriendshipStatusActive,
			"updated_at": now,
		}),
	}).Create(&rows).Error; err != nil {
		return fmt.Errorf("upsert bidirectional friendship: %w", err)
	}
	return nil
}

// EnsureSingleConversationTx finds or creates the unique single conversation for (userA, userB)
// using direct_key = "min:max", and creates or restores (is_visible=true) both conversation_members rows.
func EnsureSingleConversationTx(ctx context.Context, tx *gorm.DB, userA, userB uint64) (*model.Conversation, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	minID, maxID, err := OrderedUserPair(userA, userB)
	if err != nil {
		return nil, err
	}
	directKey := fmt.Sprintf("%d:%d", minID, maxID)

	conv, err := FindSingleConversationByDirectKeyForUpdateTx(ctx, tx, directKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		now := time.Now().UTC()
		newConv := model.Conversation{
			Type:      model.ConversationTypeSingle,
			DirectKey: &directKey,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if createErr := tx.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "direct_key"}},
			DoNothing: true,
		}).Create(&newConv).Error; createErr != nil {
			return nil, fmt.Errorf("create single conversation: %w", createErr)
		}
		conv, err = FindSingleConversationByDirectKeyForUpdateTx(ctx, tx, directKey)
	}
	if err != nil {
		return nil, fmt.Errorf("load single conversation: %w", err)
	}

	now := time.Now().UTC()
	members := []model.ConversationMember{
		{
			ConversationID: conv.ID,
			UserID:         minID,
			IsVisible:      true,
			JoinedAt:       now,
			UpdatedAt:      now,
		},
		{
			ConversationID: conv.ID,
			UserID:         maxID,
			IsVisible:      true,
			JoinedAt:       now,
			UpdatedAt:      now,
		},
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "conversation_id"}, {Name: "user_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"is_visible": true,
			"updated_at": now,
		}),
	}).Create(&members).Error; err != nil {
		return nil, fmt.Errorf("upsert single conversation members: %w", err)
	}

	return conv, nil
}

// EnsureFriendshipAndConversationTx establishes or restores bidirectional friendships
// and the unique visible single conversation + conversation_members inside tx.
func EnsureFriendshipAndConversationTx(
	ctx context.Context,
	tx *gorm.DB,
	userA uint64,
	userB uint64,
) (*model.Conversation, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	minID, maxID, err := OrderedUserPair(userA, userB)
	if err != nil {
		return nil, err
	}

	// Lock both user rows in ascending ID order when present to keep consistent lock ordering.
	var lockedUsers []uint64
	if err := tx.WithContext(ctx).
		Model(&model.User{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", []uint64{minID, maxID}).
		Order("id ASC").
		Pluck("id", &lockedUsers).Error; err != nil {
		return nil, fmt.Errorf("lock users for friendship restore: %w", err)
	}

	if err := UpsertBidirectionalFriendshipTx(ctx, tx, minID, maxID); err != nil {
		return nil, err
	}
	return EnsureSingleConversationTx(ctx, tx, minID, maxID)
}

func extractConnIDFromRouteMember(member string) string {
	trimmed := strings.TrimSpace(member)
	if trimmed == "" {
		return ""
	}
	if idx := strings.IndexByte(trimmed, ':'); idx >= 0 {
		return strings.TrimSpace(trimmed[idx+1:])
	}
	return trimmed
}

func dedupePositiveUint64(ids []uint64) []uint64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func dedupeNonEmptyStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}
