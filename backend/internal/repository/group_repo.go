package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

// UserGroupListFilter defines filters and cursor bounds for listing groups a user belongs to.
type UserGroupListFilter struct {
	UserID   uint64
	Status   string
	CursorID uint64
	Limit    int
}

// UserGroupSummaryRow represents a group row enriched with the caller's role, conversation_id, and member_count.
type UserGroupSummaryRow struct {
	Group          model.Group
	Role           string
	ConversationID uint64
	MemberCount    int
}

// GroupMemberListFilter defines filters and cursor bounds for listing group members.
type GroupMemberListFilter struct {
	GroupID  uint64
	Role     string
	CursorID uint64
	Limit    int
}

// GroupAuditListFilter defines filters and cursor bounds for listing group audit logs.
type GroupAuditListFilter struct {
	GroupID  uint64
	From     *time.Time
	To       *time.Time
	CursorID uint64
	Limit    int
}

// GroupRepository encapsulates MySQL persistence operations for group management,
// synchronized group conversation/member updates, asset references, and append-only audit logs.
type GroupRepository interface {
	WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error
	LockUsersAscending(ctx context.Context, tx *gorm.DB, ids []uint64) (map[uint64]model.User, error)
	BatchGetUsersByIDs(ctx context.Context, ids []uint64) (map[uint64]model.User, error)

	GetGroupAndConversation(ctx context.Context, groupID uint64) (*model.Group, *model.Conversation, error)
	LockGroupByID(ctx context.Context, tx *gorm.DB, groupID uint64) (*model.Group, error)
	CreateGroup(ctx context.Context, tx *gorm.DB, group *model.Group) error
	UpdateGroupFields(ctx context.Context, tx *gorm.DB, groupID uint64, updates map[string]any) error

	LockGroupConversationByGroupID(ctx context.Context, tx *gorm.DB, groupID uint64) (*model.Conversation, error)
	CreateGroupConversation(ctx context.Context, tx *gorm.DB, groupID uint64, now time.Time) (*model.Conversation, error)

	LockGroupMembers(ctx context.Context, tx *gorm.DB, groupID uint64) ([]model.GroupMember, error)
	GetGroupMember(ctx context.Context, groupID, userID uint64) (*model.GroupMember, error)
	LockGroupMember(ctx context.Context, tx *gorm.DB, groupID, userID uint64) (*model.GroupMember, error)
	CreateGroupMembers(ctx context.Context, tx *gorm.DB, members []model.GroupMember) error
	UpdateGroupMemberRole(ctx context.Context, tx *gorm.DB, groupID, userID uint64, role string, now time.Time) error
	UpdateGroupMemberNickname(ctx context.Context, tx *gorm.DB, groupID, userID uint64, nickname string, now time.Time) error
	DeleteGroupMember(ctx context.Context, tx *gorm.DB, groupID, userID uint64) error
	CountGroupMembers(ctx context.Context, groupID uint64) (int, error)
	ListUserGroups(ctx context.Context, filter UserGroupListFilter) ([]UserGroupSummaryRow, bool, error)
	ListGroupMembers(ctx context.Context, filter GroupMemberListFilter) ([]model.GroupMember, error)

	GetConversationMember(ctx context.Context, convID, userID uint64) (*model.ConversationMember, error)
	CreateConversationMembers(ctx context.Context, tx *gorm.DB, members []model.ConversationMember) error
	UpsertConversationMembersOnJoin(ctx context.Context, tx *gorm.DB, convID uint64, userIDs []uint64, baselineSeq uint64, unreadCount uint32, now time.Time) ([]model.ConversationMember, error)
	HideConversationMember(ctx context.Context, tx *gorm.DB, convID, userID uint64, now time.Time) error
	IncrementActiveGroupMembersUnread(ctx context.Context, tx *gorm.DB, convID uint64, userIDs []uint64, now time.Time) error

	CreateSystemMessage(ctx context.Context, tx *gorm.DB, msg *model.Message) error
	UpdateConversationLastMessage(ctx context.Context, tx *gorm.DB, convID uint64, msg *model.Message) error

	LockAssetByAssetID(ctx context.Context, tx *gorm.DB, assetID string) (*model.Asset, error)
	BindGroupAvatarAsset(ctx context.Context, tx *gorm.DB, assetID string, groupID uint64, now time.Time) error
	UnbindGroupAvatarAssets(ctx context.Context, tx *gorm.DB, groupID uint64, now time.Time) error

	AppendAuditLog(ctx context.Context, tx *gorm.DB, entry *model.AuditLog) error
	ListGroupAuditLogs(ctx context.Context, filter GroupAuditListFilter) ([]model.AuditLog, bool, error)
}

// GormGroupRepository is the GORM/MySQL implementation of GroupRepository.
type GormGroupRepository struct {
	db *gorm.DB
}

// NewGormGroupRepository constructs a GormGroupRepository.
func NewGormGroupRepository(db *gorm.DB) *GormGroupRepository {
	return &GormGroupRepository{db: db}
}

// WithTx executes fn inside a GORM database transaction.
func (r *GormGroupRepository) WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	if r == nil || r.db == nil {
		return ErrRepositoryUnavailable
	}
	return WithTx(ctx, r.db, fn)
}

// LockUsersAscending locks user rows in `users` using FOR UPDATE ordered by id ASC.
func (r *GormGroupRepository) LockUsersAscending(ctx context.Context, tx *gorm.DB, ids []uint64) (map[uint64]model.User, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(ids)
	sort.Slice(uniqueIDs, func(i, j int) bool { return uniqueIDs[i] < uniqueIDs[j] })
	out := make(map[uint64]model.User, len(uniqueIDs))
	if len(uniqueIDs) == 0 {
		return out, nil
	}

	var users []model.User
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", uniqueIDs).
		Order("id ASC").
		Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

// BatchGetUsersByIDs loads a batch of users using a single IN query without sensitive credentials.
func (r *GormGroupRepository) BatchGetUsersByIDs(ctx context.Context, ids []uint64) (map[uint64]model.User, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(ids)
	out := make(map[uint64]model.User, len(uniqueIDs))
	if len(uniqueIDs) == 0 {
		return out, nil
	}
	var users []model.User
	if err := r.db.WithContext(ctx).
		Select("id", "username", "nickname", "avatar_url", "signature", "status", "last_online_at").
		Where("id IN ?", uniqueIDs).
		Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

// GetGroupAndConversation fetches the group and its linked group conversation without locking.
func (r *GormGroupRepository) GetGroupAndConversation(ctx context.Context, groupID uint64) (*model.Group, *model.Conversation, error) {
	if r == nil || r.db == nil {
		return nil, nil, ErrRepositoryUnavailable
	}
	if groupID == 0 {
		return nil, nil, gorm.ErrRecordNotFound
	}

	var group model.Group
	if err := r.db.WithContext(ctx).Where("id = ?", groupID).Take(&group).Error; err != nil {
		return nil, nil, err
	}

	var conv model.Conversation
	err := r.db.WithContext(ctx).
		Where("type = ? AND group_id = ?", model.ConversationTypeGroup, groupID).
		Take(&conv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &group, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &group, &conv, nil
}

// LockGroupByID loads and locks a `groups` row FOR UPDATE.
func (r *GormGroupRepository) LockGroupByID(ctx context.Context, tx *gorm.DB, groupID uint64) (*model.Group, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var group model.Group
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", groupID).
		Take(&group).Error; err != nil {
		return nil, err
	}
	return &group, nil
}

// CreateGroup inserts a new `groups` row.
func (r *GormGroupRepository) CreateGroup(ctx context.Context, tx *gorm.DB, group *model.Group) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).Create(group).Error
}

// UpdateGroupFields updates specified columns on `groups`.
func (r *GormGroupRepository) UpdateGroupFields(ctx context.Context, tx *gorm.DB, groupID uint64, updates map[string]any) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	if len(updates) == 0 {
		return nil
	}
	return tx.WithContext(ctx).
		Model(&model.Group{}).
		Where("id = ?", groupID).
		Updates(updates).Error
}

// LockGroupConversationByGroupID loads and locks the unique group conversation row FOR UPDATE.
func (r *GormGroupRepository) LockGroupConversationByGroupID(ctx context.Context, tx *gorm.DB, groupID uint64) (*model.Conversation, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var conv model.Conversation
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("type = ? AND group_id = ?", model.ConversationTypeGroup, groupID).
		Take(&conv).Error; err != nil {
		return nil, err
	}
	return &conv, nil
}

// CreateGroupConversation creates the unique group conversation for groupID.
func (r *GormGroupRepository) CreateGroupConversation(ctx context.Context, tx *gorm.DB, groupID uint64, now time.Time) (*model.Conversation, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	gid := groupID
	ts := now.UTC().Truncate(time.Millisecond)
	conv := &model.Conversation{
		Type:      model.ConversationTypeGroup,
		GroupID:   &gid,
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	if err := tx.WithContext(ctx).Create(conv).Error; err != nil {
		return nil, err
	}
	return conv, nil
}

// LockGroupMembers loads and locks all current `group_members` rows for groupID in ascending ID order.
func (r *GormGroupRepository) LockGroupMembers(ctx context.Context, tx *gorm.DB, groupID uint64) ([]model.GroupMember, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var rows []model.GroupMember
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("group_id = ?", groupID).
		Order("id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetGroupMember fetches a single group member row without locking.
func (r *GormGroupRepository) GetGroupMember(ctx context.Context, groupID, userID uint64) (*model.GroupMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	var member model.GroupMember
	if err := r.db.WithContext(ctx).
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Take(&member).Error; err != nil {
		return nil, err
	}
	return &member, nil
}

// LockGroupMember loads and locks a single group member row FOR UPDATE inside tx.
func (r *GormGroupRepository) LockGroupMember(ctx context.Context, tx *gorm.DB, groupID, userID uint64) (*model.GroupMember, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var member model.GroupMember
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Take(&member).Error; err != nil {
		return nil, err
	}
	return &member, nil
}

// CreateGroupMembers batch-inserts `group_members` rows.
func (r *GormGroupRepository) CreateGroupMembers(ctx context.Context, tx *gorm.DB, members []model.GroupMember) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	if len(members) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Create(&members).Error
}

// UpdateGroupMemberRole updates a member's role in `group_members`.
func (r *GormGroupRepository) UpdateGroupMemberRole(ctx context.Context, tx *gorm.DB, groupID, userID uint64, role string, now time.Time) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).
		Model(&model.GroupMember{}).
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Updates(map[string]any{
			"role":       role,
			"updated_at": now,
		}).Error
}

// UpdateGroupMemberNickname updates a member's `nickname_in_group`.
func (r *GormGroupRepository) UpdateGroupMemberNickname(ctx context.Context, tx *gorm.DB, groupID, userID uint64, nickname string, now time.Time) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).
		Model(&model.GroupMember{}).
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Updates(map[string]any{
			"nickname_in_group": nickname,
			"updated_at":        now,
		}).Error
}

// DeleteGroupMember deletes the active `group_members` record on leave or removal so rejoining creates a new row.
func (r *GormGroupRepository) DeleteGroupMember(ctx context.Context, tx *gorm.DB, groupID, userID uint64) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Delete(&model.GroupMember{}).Error
}

// CountGroupMembers returns the current member count for groupID.
func (r *GormGroupRepository) CountGroupMembers(ctx context.Context, groupID uint64) (int, error) {
	if r == nil || r.db == nil {
		return 0, ErrRepositoryUnavailable
	}
	var cnt int64
	if err := r.db.WithContext(ctx).
		Model(&model.GroupMember{}).
		Where("group_id = ?", groupID).
		Count(&cnt).Error; err != nil {
		return 0, err
	}
	return int(cnt), nil
}

// ListUserGroups returns cursor-paginated groups the user belongs to, ordered by groups.id DESC,
// and batch-assembles conversation_id and member_count without N+1 queries.
func (r *GormGroupRepository) ListUserGroups(ctx context.Context, filter UserGroupListFilter) ([]UserGroupSummaryRow, bool, error) {
	if r == nil || r.db == nil {
		return nil, false, ErrRepositoryUnavailable
	}
	if filter.UserID == 0 || filter.Limit <= 0 {
		return []UserGroupSummaryRow{}, false, nil
	}

	type joinedRow struct {
		ID           uint64    `gorm:"column:id"`
		Name         string    `gorm:"column:name"`
		AvatarURL    string    `gorm:"column:avatar_url"`
		OwnerID      uint64    `gorm:"column:owner_id"`
		Announcement *string   `gorm:"column:announcement"`
		MaxMembers   uint32    `gorm:"column:max_members"`
		Status       string    `gorm:"column:status"`
		CreatedAt    time.Time `gorm:"column:created_at"`
		UpdatedAt    time.Time `gorm:"column:updated_at"`
		MyRole       string    `gorm:"column:my_role"`
	}

	var rows []joinedRow
	if err := r.buildListUserGroupsQuery(ctx, filter).Find(&rows).Error; err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(rows) > filter.Limit {
		hasMore = true
		rows = rows[:filter.Limit]
	}
	if len(rows) == 0 {
		return []UserGroupSummaryRow{}, false, nil
	}

	groupIDs := make([]uint64, len(rows))
	for i, row := range rows {
		groupIDs[i] = row.ID
	}

	var convs []model.Conversation
	if err := r.db.WithContext(ctx).
		Select("id", "group_id").
		Where("type = ? AND group_id IN ?", model.ConversationTypeGroup, groupIDs).
		Find(&convs).Error; err != nil {
		return nil, false, err
	}
	convByGroup := make(map[uint64]uint64, len(convs))
	for _, c := range convs {
		if c.GroupID != nil {
			convByGroup[*c.GroupID] = c.ID
		}
	}

	type countRow struct {
		GroupID uint64 `gorm:"column:group_id"`
		Total   int64  `gorm:"column:total"`
	}
	var counts []countRow
	if err := r.db.WithContext(ctx).
		Model(&model.GroupMember{}).
		Select("group_id, COUNT(*) AS total").
		Where("group_id IN ?", groupIDs).
		Group("group_id").
		Find(&counts).Error; err != nil {
		return nil, false, err
	}
	countByGroup := make(map[uint64]int, len(counts))
	for _, c := range counts {
		countByGroup[c.GroupID] = int(c.Total)
	}

	out := make([]UserGroupSummaryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, UserGroupSummaryRow{
			Group: model.Group{
				ID:           row.ID,
				Name:         row.Name,
				AvatarURL:    row.AvatarURL,
				OwnerID:      row.OwnerID,
				Announcement: row.Announcement,
				MaxMembers:   row.MaxMembers,
				Status:       row.Status,
				CreatedAt:    row.CreatedAt,
				UpdatedAt:    row.UpdatedAt,
			},
			Role:           row.MyRole,
			ConversationID: convByGroup[row.ID],
			MemberCount:    countByGroup[row.ID],
		})
	}
	return out, hasMore, nil
}

func (r *GormGroupRepository) buildListUserGroupsQuery(ctx context.Context, filter UserGroupListFilter) *gorm.DB {
	q := r.db.WithContext(ctx).
		Table("`groups` AS g").
		Select("g.id, g.name, g.avatar_url, g.owner_id, g.announcement, g.max_members, g.status, g.created_at, g.updated_at, gm.role AS my_role").
		Joins("INNER JOIN `group_members` AS gm ON gm.group_id = g.id AND gm.user_id = ?", filter.UserID)

	if filter.Status != "" {
		q = q.Where("g.status = ?", filter.Status)
	}
	if filter.CursorID > 0 {
		q = q.Where("g.id < ?", filter.CursorID)
	}
	return q.Order("g.id DESC").Limit(filter.Limit + 1)
}

// ListGroupMembers returns group_members rows ordered by id DESC up to filter.Limit.
func (r *GormGroupRepository) ListGroupMembers(ctx context.Context, filter GroupMemberListFilter) ([]model.GroupMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if filter.GroupID == 0 || filter.Limit <= 0 {
		return []model.GroupMember{}, nil
	}

	q := r.db.WithContext(ctx).
		Model(&model.GroupMember{}).
		Where("group_id = ?", filter.GroupID)
	if filter.Role != "" {
		q = q.Where("role = ?", filter.Role)
	}
	if filter.CursorID > 0 {
		q = q.Where("id < ?", filter.CursorID)
	}

	var rows []model.GroupMember
	if err := q.Order("id DESC").Limit(filter.Limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetConversationMember fetches the user's conversation_members row without locking.
func (r *GormGroupRepository) GetConversationMember(ctx context.Context, convID, userID uint64) (*model.ConversationMember, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	var cm model.ConversationMember
	err := r.db.WithContext(ctx).
		Where("conversation_id = ? AND user_id = ?", convID, userID).
		Take(&cm).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cm, nil
}

// CreateConversationMembers batch-inserts initial `conversation_members` rows.
func (r *GormGroupRepository) CreateConversationMembers(ctx context.Context, tx *gorm.DB, members []model.ConversationMember) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	if len(members) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Create(&members).Error
}

// UpsertConversationMembersOnJoin inserts or updates `conversation_members` for newly added or rejoined members,
// resetting their watermark baseline to baselineSeq, joined_at to now, unread_count to unreadCount, and is_visible to true.
func (r *GormGroupRepository) UpsertConversationMembersOnJoin(
	ctx context.Context,
	tx *gorm.DB,
	convID uint64,
	userIDs []uint64,
	baselineSeq uint64,
	unreadCount uint32,
	now time.Time,
) ([]model.ConversationMember, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(userIDs)
	if len(uniqueIDs) == 0 {
		return []model.ConversationMember{}, nil
	}
	sort.Slice(uniqueIDs, func(i, j int) bool { return uniqueIDs[i] < uniqueIDs[j] })

	rows := make([]model.ConversationMember, 0, len(uniqueIDs))
	for _, uid := range uniqueIDs {
		rows = append(rows, model.ConversationMember{
			ConversationID:   convID,
			UserID:           uid,
			LastDeliveredSeq: baselineSeq,
			LastReadSeq:      baselineSeq,
			UnreadCount:      unreadCount,
			IsVisible:        true,
			JoinedAt:         now,
			UpdatedAt:        now,
		})
	}

	if err := tx.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "conversation_id"}, {Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"last_delivered_seq": baselineSeq,
				"last_read_seq":      baselineSeq,
				"unread_count":       unreadCount,
				"is_visible":         true,
				"joined_at":          now,
				"updated_at":         now,
			}),
		}).
		Create(&rows).Error; err != nil {
		return nil, err
	}

	var persisted []model.ConversationMember
	if err := tx.WithContext(ctx).
		Where("conversation_id = ? AND user_id IN ?", convID, uniqueIDs).
		Order("user_id ASC").
		Find(&persisted).Error; err != nil {
		return nil, err
	}
	return persisted, nil
}

// HideConversationMember hides the conversation for a removed/departed user and clears unread_count.
func (r *GormGroupRepository) HideConversationMember(ctx context.Context, tx *gorm.DB, convID, userID uint64, now time.Time) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).
		Model(&model.ConversationMember{}).
		Where("conversation_id = ? AND user_id = ?", convID, userID).
		Updates(map[string]any{
			"is_visible":   false,
			"unread_count": 0,
			"updated_at":   now,
		}).Error
}

// IncrementActiveGroupMembersUnread increments unread_count by 1 for active members in userIDs.
func (r *GormGroupRepository) IncrementActiveGroupMembersUnread(ctx context.Context, tx *gorm.DB, convID uint64, userIDs []uint64, now time.Time) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	uniqueIDs := dedupePositiveUint64(userIDs)
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

// CreateSystemMessage inserts a Snowflake-ID system message into `messages`.
func (r *GormGroupRepository) CreateSystemMessage(ctx context.Context, tx *gorm.DB, msg *model.Message) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).Create(msg).Error
}

// UpdateConversationLastMessage updates the conversation summary fields after inserting a message.
func (r *GormGroupRepository) UpdateConversationLastMessage(ctx context.Context, tx *gorm.DB, convID uint64, msg *model.Message) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
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

// LockAssetByAssetID locks and loads an uploaded asset row by its external asset_id UUID.
func (r *GormGroupRepository) LockAssetByAssetID(ctx context.Context, tx *gorm.DB, assetID string) (*model.Asset, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var asset model.Asset
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("asset_id = ?", assetID).
		Take(&asset).Error; err != nil {
		return nil, err
	}
	return &asset, nil
}

// BindGroupAvatarAsset binds assetID as the avatar for groupID and releases any prior bound avatar for groupID.
func (r *GormGroupRepository) BindGroupAvatarAsset(ctx context.Context, tx *gorm.DB, assetID string, groupID uint64, now time.Time) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	if err := tx.WithContext(ctx).
		Model(&model.Asset{}).
		Where("ref_type = ? AND ref_id = ? AND asset_id <> ?", model.AssetRefGroupAvatar, groupID, assetID).
		Updates(map[string]any{
			"ref_type":   model.AssetRefNone,
			"ref_id":     nil,
			"ref_count":  0,
			"updated_at": now,
		}).Error; err != nil {
		return fmt.Errorf("unbind previous group avatar assets: %w", err)
	}

	return tx.WithContext(ctx).
		Model(&model.Asset{}).
		Where("asset_id = ?", assetID).
		Updates(map[string]any{
			"ref_type":   model.AssetRefGroupAvatar,
			"ref_id":     groupID,
			"ref_count":  1,
			"bound_at":   gorm.Expr("COALESCE(bound_at, ?)", now),
			"updated_at": now,
		}).Error
}

// UnbindGroupAvatarAssets releases any group_avatar asset bound to groupID when avatar is cleared.
func (r *GormGroupRepository) UnbindGroupAvatarAssets(ctx context.Context, tx *gorm.DB, groupID uint64, now time.Time) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).
		Model(&model.Asset{}).
		Where("ref_type = ? AND ref_id = ?", model.AssetRefGroupAvatar, groupID).
		Updates(map[string]any{
			"ref_type":   model.AssetRefNone,
			"ref_id":     nil,
			"ref_count":  0,
			"updated_at": now,
		}).Error
}

// AppendAuditLog appends an immutable audit log entry (INSERT only).
func (r *GormGroupRepository) AppendAuditLog(ctx context.Context, tx *gorm.DB, entry *model.AuditLog) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).Create(entry).Error
}

// ListGroupAuditLogs lists immutable audit log rows for groupID ordered by id DESC with optional time filters.
func (r *GormGroupRepository) ListGroupAuditLogs(ctx context.Context, filter GroupAuditListFilter) ([]model.AuditLog, bool, error) {
	if r == nil || r.db == nil {
		return nil, false, ErrRepositoryUnavailable
	}
	if filter.GroupID == 0 || filter.Limit <= 0 {
		return []model.AuditLog{}, false, nil
	}

	q := r.db.WithContext(ctx).
		Model(&model.AuditLog{}).
		Where("group_id = ?", filter.GroupID)
	if filter.From != nil {
		q = q.Where("created_at >= ?", filter.From.UTC())
	}
	if filter.To != nil {
		q = q.Where("created_at <= ?", filter.To.UTC())
	}
	if filter.CursorID > 0 {
		q = q.Where("id < ?", filter.CursorID)
	}

	var rows []model.AuditLog
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
