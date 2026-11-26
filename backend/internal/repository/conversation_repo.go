package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

var (
	// ErrConversationNotFound indicates the requested conversation does not exist.
	ErrConversationNotFound = errors.New("conversation not found")
	// ErrNotConversationMember indicates the user is not an active member of the conversation.
	ErrNotConversationMember = errors.New("not a conversation member")
	// ErrGroupNotFound indicates the backing group record for a group conversation does not exist.
	ErrGroupNotFound = errors.New("group not found")
)

// ConversationListItemRow holds joined conversation_members + conversations fields.
type ConversationListItemRow struct {
	CMID             uint64     `gorm:"column:cm_id"`
	ConversationID   uint64     `gorm:"column:conversation_id"`
	UserID           uint64     `gorm:"column:user_id"`
	LastDeliveredSeq uint64     `gorm:"column:last_delivered_seq"`
	LastReadSeq      uint64     `gorm:"column:last_read_seq"`
	UnreadCount      uint32     `gorm:"column:unread_count"`
	IsPinned         bool       `gorm:"column:is_pinned"`
	IsMuted          bool       `gorm:"column:is_muted"`
	IsVisible        bool       `gorm:"column:is_visible"`
	JoinedAt         time.Time  `gorm:"column:joined_at"`
	MemberUpdatedAt  time.Time  `gorm:"column:member_updated_at"`
	Type             string     `gorm:"column:type"`
	DirectKey        *string    `gorm:"column:direct_key"`
	GroupID          *uint64    `gorm:"column:group_id"`
	LastMsgID        uint64     `gorm:"column:last_msg_id"`
	LastSeq          uint64     `gorm:"column:last_seq"`
	LastMsgPreview   string     `gorm:"column:last_msg_preview"`
	LastMsgAt        *time.Time `gorm:"column:last_msg_at"`
}

// ConversationListCursor defines the keyset pagination cursor for conversation listing.
type ConversationListCursor struct {
	IsPinned       bool
	UpdatedAt      time.Time
	ConversationID uint64
}

// ConversationRepository provides GORM persistence queries for conversations and membership.
type ConversationRepository struct {
	db *gorm.DB
}

// NewConversationRepository constructs a ConversationRepository.
func NewConversationRepository(db *gorm.DB) *ConversationRepository {
	return &ConversationRepository{db: db}
}

// ParseDirectKeyPeer parses a single conversation direct_key ("min:max") and returns the peer's user ID.
func ParseDirectKeyPeer(directKey *string, selfUserID uint64) (uint64, bool) {
	if directKey == nil || selfUserID == 0 {
		return 0, false
	}
	left, right, ok := strings.Cut(strings.TrimSpace(*directKey), ":")
	if !ok {
		return 0, false
	}
	u1, err1 := strconv.ParseUint(left, 10, 64)
	u2, err2 := strconv.ParseUint(right, 10, 64)
	if err1 != nil || err2 != nil || u1 == 0 || u2 == 0 || u1 == u2 {
		return 0, false
	}
	switch selfUserID {
	case u1:
		return u2, true
	case u2:
		return u1, true
	default:
		return 0, false
	}
}

// ListUserConversations returns userID's conversations ordered by is_pinned DESC, updated_at DESC, conversation_id DESC.
// Fetches limit+1 rows so callers can determine has_more without a COUNT query.
func (r *ConversationRepository) ListUserConversations(
	ctx context.Context,
	userID uint64,
	includeHidden bool,
	cursor *ConversationListCursor,
	limit int,
) ([]ConversationListItemRow, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if userID == 0 || limit <= 0 {
		return []ConversationListItemRow{}, nil
	}

	q := r.db.WithContext(ctx).
		Table("conversation_members AS cm").
		Select([]string{
			"cm.id AS cm_id",
			"cm.conversation_id AS conversation_id",
			"cm.user_id AS user_id",
			"cm.last_delivered_seq AS last_delivered_seq",
			"cm.last_read_seq AS last_read_seq",
			"cm.unread_count AS unread_count",
			"cm.is_pinned AS is_pinned",
			"cm.is_muted AS is_muted",
			"cm.is_visible AS is_visible",
			"cm.joined_at AS joined_at",
			"cm.updated_at AS member_updated_at",
			"c.type AS type",
			"c.direct_key AS direct_key",
			"c.group_id AS group_id",
			"c.last_msg_id AS last_msg_id",
			"c.last_seq AS last_seq",
			"c.last_msg_preview AS last_msg_preview",
			"c.last_msg_at AS last_msg_at",
		}).
		Joins("JOIN conversations AS c ON c.id = cm.conversation_id").
		Where("cm.user_id = ?", userID).
		Where("(c.type = ? OR EXISTS (SELECT 1 FROM group_members AS gm WHERE gm.group_id = c.group_id AND gm.user_id = cm.user_id))", model.ConversationTypeSingle)

	if !includeHidden {
		q = q.Where("cm.is_visible = ?", true)
	}
	if cursor != nil && cursor.ConversationID > 0 {
		ts := cursor.UpdatedAt.UTC().Truncate(time.Millisecond)
		q = q.Where(
			"(cm.is_pinned < ? OR (cm.is_pinned = ? AND cm.updated_at < ?) OR (cm.is_pinned = ? AND cm.updated_at = ? AND cm.conversation_id < ?))",
			cursor.IsPinned,
			cursor.IsPinned, ts,
			cursor.IsPinned, ts, cursor.ConversationID,
		)
	}

	var rows []ConversationListItemRow
	if err := q.Order("cm.is_pinned DESC, cm.updated_at DESC, cm.conversation_id DESC").
		Limit(limit + 1).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("list user conversations: %w", err)
	}
	return rows, nil
}

// GetConversationByID fetches a conversation by primary key.
func (r *ConversationRepository) GetConversationByID(ctx context.Context, convID uint64) (*model.Conversation, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if convID == 0 {
		return nil, ErrConversationNotFound
	}
	var conv model.Conversation
	err := r.db.WithContext(ctx).Where("id = ?", convID).Take(&conv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation by id: %w", err)
	}
	return &conv, nil
}

// GetConversationMember fetches a user's conversation_members row for convID.
func (r *ConversationRepository) GetConversationMember(ctx context.Context, convID, userID uint64) (*model.ConversationMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if convID == 0 || userID == 0 {
		return nil, ErrNotConversationMember
	}
	var member model.ConversationMember
	err := r.db.WithContext(ctx).
		Where("conversation_id = ? AND user_id = ?", convID, userID).
		Take(&member).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotConversationMember
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation member: %w", err)
	}
	return &member, nil
}

// ListConversationMembers fetches cursor-paginated members of a conversation ordered by conversation_members.id DESC.
// For group conversations, only users present in group_members are returned.
func (r *ConversationRepository) ListConversationMembers(
	ctx context.Context,
	convID uint64,
	convType string,
	groupID *uint64,
	cursorID uint64,
	limit int,
) ([]model.ConversationMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if convID == 0 || limit <= 0 {
		return []model.ConversationMember{}, nil
	}

	q := r.db.WithContext(ctx).
		Model(&model.ConversationMember{}).
		Where("conversation_id = ?", convID)

	if convType == model.ConversationTypeGroup && groupID != nil && *groupID > 0 {
		q = q.Where("EXISTS (SELECT 1 FROM group_members AS gm WHERE gm.group_id = ? AND gm.user_id = conversation_members.user_id)", *groupID)
	}
	if cursorID > 0 {
		q = q.Where("id < ?", cursorID)
	}

	var rows []model.ConversationMember
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list conversation members: %w", err)
	}
	return rows, nil
}

// UpdateMemberSettings updates is_pinned, is_muted, and/or is_visible for userID in convID inside a transaction.
func (r *ConversationRepository) UpdateMemberSettings(
	ctx context.Context,
	convID uint64,
	userID uint64,
	isPinned *bool,
	isMuted *bool,
	isVisible *bool,
	now time.Time,
) (*model.ConversationMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if convID == 0 || userID == 0 {
		return nil, ErrNotConversationMember
	}

	var updated model.ConversationMember
	err := WithTx(ctx, r.db, func(tx *gorm.DB) error {
		var conv model.Conversation
		if err := tx.Where("id = ?", convID).Take(&conv).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrConversationNotFound
			}
			return fmt.Errorf("load conversation for settings: %w", err)
		}

		if conv.Type == model.ConversationTypeGroup {
			if conv.GroupID == nil || *conv.GroupID == 0 {
				return ErrGroupNotFound
			}
			var gmCount int64
			if err := tx.Model(&model.GroupMember{}).
				Where("group_id = ? AND user_id = ?", *conv.GroupID, userID).
				Count(&gmCount).Error; err != nil {
				return fmt.Errorf("check group membership for settings: %w", err)
			}
			if gmCount == 0 {
				return ErrNotConversationMember
			}
		}

		var member model.ConversationMember
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("conversation_id = ? AND user_id = ?", convID, userID).
			Take(&member).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotConversationMember
			}
			return fmt.Errorf("lock conversation member for settings: %w", err)
		}

		ts := now.UTC().Truncate(time.Millisecond)
		updates := map[string]any{
			"updated_at": ts,
		}
		if isPinned != nil {
			updates["is_pinned"] = *isPinned
		}
		if isMuted != nil {
			updates["is_muted"] = *isMuted
		}
		if isVisible != nil {
			updates["is_visible"] = *isVisible
		}

		if err := tx.Model(&model.ConversationMember{}).
			Where("id = ?", member.ID).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update conversation member settings: %w", err)
		}

		if err := tx.Where("id = ?", member.ID).Take(&updated).Error; err != nil {
			return fmt.Errorf("reload conversation member settings: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// BatchGetUsersByIDs fetches public User fields in one IN query.
func (r *ConversationRepository) BatchGetUsersByIDs(ctx context.Context, userIDs []uint64) (map[uint64]model.User, error) {
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

// BatchGetFriendshipsForUser loads caller remarks and bidirectional active friendship status for peerIDs in one query.
func (r *ConversationRepository) BatchGetFriendshipsForUser(
	ctx context.Context,
	userID uint64,
	peerIDs []uint64,
) (remarks map[uint64]string, activeBoth map[uint64]bool, err error) {
	if r == nil || r.db == nil {
		return nil, nil, ErrRepositoryUnavailable
	}
	remarks = make(map[uint64]string)
	activeBoth = make(map[uint64]bool)

	uniqueIDs := dedupePositiveUint64(peerIDs)
	if userID == 0 || len(uniqueIDs) == 0 {
		return remarks, activeBoth, nil
	}

	var rows []model.Friendship
	if err := r.db.WithContext(ctx).
		Where("status = ? AND ((user_id = ? AND friend_id IN ?) OR (friend_id = ? AND user_id IN ?))",
			model.FriendshipStatusActive, userID, uniqueIDs, userID, uniqueIDs).
		Find(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("batch get friendships for user: %w", err)
	}

	forward := make(map[uint64]bool, len(uniqueIDs))
	reverse := make(map[uint64]bool, len(uniqueIDs))
	for _, row := range rows {
		if row.UserID == userID {
			forward[row.FriendID] = true
			remarks[row.FriendID] = row.Remark
		} else if row.FriendID == userID {
			reverse[row.UserID] = true
		}
	}
	for _, pid := range uniqueIDs {
		activeBoth[pid] = forward[pid] && reverse[pid]
	}
	return remarks, activeBoth, nil
}

// BatchGetGroupsByIDs fetches groups and active member counts in two aggregate IN queries (zero N+1).
func (r *ConversationRepository) BatchGetGroupsByIDs(
	ctx context.Context,
	groupIDs []uint64,
) (map[uint64]model.Group, map[uint64]uint32, error) {
	if r == nil || r.db == nil {
		return nil, nil, ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(groupIDs)
	groupsOut := make(map[uint64]model.Group, len(uniqueIDs))
	countsOut := make(map[uint64]uint32, len(uniqueIDs))
	if len(uniqueIDs) == 0 {
		return groupsOut, countsOut, nil
	}

	var groups []model.Group
	if err := r.db.WithContext(ctx).
		Where("id IN ?", uniqueIDs).
		Find(&groups).Error; err != nil {
		return nil, nil, fmt.Errorf("batch get groups: %w", err)
	}
	for _, g := range groups {
		groupsOut[g.ID] = g
	}

	type countRow struct {
		GroupID uint64 `gorm:"column:group_id"`
		Cnt     uint32 `gorm:"column:cnt"`
	}
	var countRows []countRow
	if err := r.db.WithContext(ctx).
		Model(&model.GroupMember{}).
		Select("group_id, COUNT(*) AS cnt").
		Where("group_id IN ?", uniqueIDs).
		Group("group_id").
		Scan(&countRows).Error; err != nil {
		return nil, nil, fmt.Errorf("batch count group members: %w", err)
	}
	for _, c := range countRows {
		countsOut[c.GroupID] = c.Cnt
	}

	return groupsOut, countsOut, nil
}

// GetGroupByID fetches a group and its member count.
func (r *ConversationRepository) GetGroupByID(ctx context.Context, groupID uint64) (*model.Group, uint32, error) {
	if r == nil || r.db == nil {
		return nil, 0, ErrRepositoryUnavailable
	}
	if groupID == 0 {
		return nil, 0, ErrGroupNotFound
	}
	var group model.Group
	err := r.db.WithContext(ctx).Where("id = ?", groupID).Take(&group).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, 0, ErrGroupNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("get group by id: %w", err)
	}
	var cnt int64
	if err := r.db.WithContext(ctx).
		Model(&model.GroupMember{}).
		Where("group_id = ?", groupID).
		Count(&cnt).Error; err != nil {
		return nil, 0, fmt.Errorf("count group members: %w", err)
	}
	return &group, uint32(cnt), nil
}

// GetGroupMember fetches a group member record for (groupID, userID).
func (r *ConversationRepository) GetGroupMember(ctx context.Context, groupID, userID uint64) (*model.GroupMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if groupID == 0 || userID == 0 {
		return nil, ErrNotConversationMember
	}
	var gm model.GroupMember
	err := r.db.WithContext(ctx).
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Take(&gm).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotConversationMember
	}
	if err != nil {
		return nil, fmt.Errorf("get group member: %w", err)
	}
	return &gm, nil
}

// BatchGetGroupMembers fetches group_members rows for userIDs within groupID in one IN query.
func (r *ConversationRepository) BatchGetGroupMembers(ctx context.Context, groupID uint64, userIDs []uint64) (map[uint64]model.GroupMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(userIDs)
	out := make(map[uint64]model.GroupMember, len(uniqueIDs))
	if groupID == 0 || len(uniqueIDs) == 0 {
		return out, nil
	}
	var rows []model.GroupMember
	if err := r.db.WithContext(ctx).
		Where("group_id = ? AND user_id IN ?", groupID, uniqueIDs).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("batch get group members: %w", err)
	}
	for _, gm := range rows {
		out[gm.UserID] = gm
	}
	return out, nil
}

// ListActiveMemberUserIDs returns the active user IDs participating in conv.
// For single conversations, returns conversation_members user IDs; for group conversations, returns current group_members user IDs.
func (r *ConversationRepository) ListActiveMemberUserIDs(ctx context.Context, conv *model.Conversation) ([]uint64, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	return ListActiveMemberUserIDsTx(ctx, r.db, conv)
}

// ListActiveMemberUserIDsTx returns active participant user IDs for conv using tx/db.
func ListActiveMemberUserIDsTx(ctx context.Context, tx *gorm.DB, conv *model.Conversation) ([]uint64, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	if conv == nil || conv.ID == 0 {
		return nil, ErrConversationNotFound
	}
	var ids []uint64
	if conv.Type == model.ConversationTypeGroup && conv.GroupID != nil && *conv.GroupID > 0 {
		if err := tx.WithContext(ctx).
			Model(&model.GroupMember{}).
			Where("group_id = ?", *conv.GroupID).
			Pluck("user_id", &ids).Error; err != nil {
			return nil, fmt.Errorf("list active group member ids: %w", err)
		}
		return dedupePositiveUint64(ids), nil
	}
	if err := tx.WithContext(ctx).
		Model(&model.ConversationMember{}).
		Where("conversation_id = ?", conv.ID).
		Pluck("user_id", &ids).Error; err != nil {
		return nil, fmt.Errorf("list single conversation member ids: %w", err)
	}
	return dedupePositiveUint64(ids), nil
}
