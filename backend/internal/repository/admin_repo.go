package repository

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

// AdminUserListFilter defines filter and cursor parameters for admin user listing.
type AdminUserListFilter struct {
	Query    string
	Status   *int8
	Role     string
	CursorID uint64
	Limit    int
}

// AdminGroupListFilter defines filter and cursor parameters for admin group listing.
type AdminGroupListFilter struct {
	Query    string
	Status   string
	CursorID uint64
	Limit    int
}

// AdminGroupSummaryRow aggregates group metadata with conversation and owner details.
type AdminGroupSummaryRow struct {
	Group          model.Group
	ConversationID uint64
	LastSeq        uint64
	MemberCount    int
	Owner          *model.User
}

// AdminMessageAuditFilter defines query filters for admin message audit search.
type AdminMessageAuditFilter struct {
	ConversationID *uint64
	SenderID       *uint64
	Keyword        string
	From           *time.Time
	To             *time.Time
	CursorID       uint64
	Limit          int
}

// AdminStreamsSummary summarizes Redis Streams lengths, pending totals, and dead-letter counts.
type AdminStreamsSummary struct {
	MessagesLen     int64 `json:"messages_len"`
	EventsLen       int64 `json:"events_len"`
	PendingTotal    int64 `json:"pending_total"`
	DeadLetterCount int64 `json:"dead_letter_count"`
}

// AdminStreamConsumerRow represents one stream consumer group/consumer metric row.
type AdminStreamConsumerRow struct {
	Stream           string `json:"stream"`
	Group            string `json:"group"`
	Consumer         string `json:"consumer"`
	LastDeliveredID  string `json:"last_delivered_id"`
	PendingCount     int64  `json:"pending_count"`
	OldestPendingAge int64  `json:"oldest_pending_age"`
}

// AdminRepository defines MySQL and Redis operations required by AdminService.
type AdminRepository interface {
	WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error

	ListUsers(ctx context.Context, filter AdminUserListFilter) ([]model.User, bool, error)
	BatchCountActiveSessions(ctx context.Context, userIDs []uint64, now time.Time) (map[uint64]int, error)
	GetUserByID(ctx context.Context, userID uint64) (*model.User, error)
	LockUserByID(ctx context.Context, tx *gorm.DB, userID uint64) (*model.User, error)
	CountActiveAdminsForUpdate(ctx context.Context, tx *gorm.DB) (int64, error)
	UpdateUserStatus(ctx context.Context, tx *gorm.DB, userID uint64, status int8, now time.Time) error

	ListUserSessions(ctx context.Context, userID uint64, limit int) ([]model.UserSession, error)
	CountUserFriends(ctx context.Context, userID uint64) (int64, error)
	CountUserGroups(ctx context.Context, userID uint64) (int64, error)
	ListUserAuditLogs(ctx context.Context, userID uint64, limit int) ([]model.AuditLog, error)

	LockUnrevokedSessionsByUser(ctx context.Context, tx *gorm.DB, userID uint64) ([]model.UserSession, error)
	LockSessionByID(ctx context.Context, tx *gorm.DB, sessionID uint64) (*model.UserSession, error)
	RevokeSessionsByIDs(ctx context.Context, tx *gorm.DB, sessionIDs []uint64, revokedAt time.Time) (int64, error)

	ListGroups(ctx context.Context, filter AdminGroupListFilter) ([]AdminGroupSummaryRow, bool, error)
	GetGroupDetail(ctx context.Context, groupID uint64) (*AdminGroupSummaryRow, []model.GroupMember, map[uint64]model.User, []model.AuditLog, error)
	LockGroupAndConversation(ctx context.Context, tx *gorm.DB, groupID uint64) (*model.Group, *model.Conversation, []model.GroupMember, error)
	MarkGroupDisbanded(ctx context.Context, tx *gorm.DB, groupID uint64, now time.Time) error
	CreateSystemMessageAndUpdateConv(ctx context.Context, tx *gorm.DB, msg *model.Message, convID uint64, memberIDs []uint64, now time.Time) error

	AppendAuditLog(ctx context.Context, tx *gorm.DB, entry *model.AuditLog) error
	ListMessageAudit(ctx context.Context, filter AdminMessageAuditFilter) ([]model.Message, map[uint64]model.User, bool, error)

	CountMessagesSince(ctx context.Context, since time.Time) (int64, error)
	ProbeMySQL(ctx context.Context) (version string, latencyMs int64, err error)
	ProbeRedis(ctx context.Context) (version string, latencyMs int64, err error)

	GetUsersOnline(ctx context.Context, userIDs []uint64) (map[uint64]bool, error)
	CountLivePresence(ctx context.Context) (onlineUsers int64, wsConnections int64, err error)
	CountLiveConnectionsForSessions(ctx context.Context, userID uint64, sessionIDs []uint64, includeUserRoutes bool) (int64, []uint64, error)
	BlacklistAccessJTIs(ctx context.Context, jtis []string, reason string, ttl time.Duration) error
	CollectStreamsOverview(ctx context.Context) (AdminStreamsSummary, error)
	CollectStreamConsumers(ctx context.Context) ([]AdminStreamConsumerRow, error)
}

// GormAdminRepository implements AdminRepository backed by MySQL (GORM) and Redis.
type GormAdminRepository struct {
	db  *gorm.DB
	rdb *redis.Client
}

var _ AdminRepository = (*GormAdminRepository)(nil)

// NewGormAdminRepository constructs a GormAdminRepository.
func NewGormAdminRepository(db *gorm.DB, rdb *redis.Client) *GormAdminRepository {
	return &GormAdminRepository{db: db, rdb: rdb}
}

func (r *GormAdminRepository) WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	if r == nil || r.db == nil {
		return ErrRepositoryUnavailable
	}
	return WithTx(ctx, r.db, fn)
}

func (r *GormAdminRepository) ListUsers(ctx context.Context, filter AdminUserListFilter) ([]model.User, bool, error) {
	if r == nil || r.db == nil {
		return nil, false, ErrRepositoryUnavailable
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	q := r.db.WithContext(ctx).
		Model(&model.User{}).
		Select(selfUserSelectColumns)

	if trimmed := strings.TrimSpace(filter.Query); trimmed != "" {
		likePattern := "%" + EscapeLikeLiteral(trimmed) + "%"
		if parsedID, err := strconv.ParseUint(trimmed, 10, 64); err == nil && parsedID > 0 {
			q = q.Where("(id = ? OR username LIKE ? ESCAPE '!' OR nickname LIKE ? ESCAPE '!')", parsedID, likePattern, likePattern)
		} else {
			q = q.Where("(username LIKE ? ESCAPE '!' OR nickname LIKE ? ESCAPE '!')", likePattern, likePattern)
		}
	}
	if filter.Status != nil {
		q = q.Where("status = ?", *filter.Status)
	}
	if filter.Role != "" {
		q = q.Where("role = ?", filter.Role)
	}
	if filter.CursorID > 0 {
		q = q.Where("id < ?", filter.CursorID)
	}

	var rows []model.User
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	hasMore := false
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}
	return rows, hasMore, nil
}

func (r *GormAdminRepository) BatchCountActiveSessions(ctx context.Context, userIDs []uint64, now time.Time) (map[uint64]int, error) {
	out := make(map[uint64]int, len(userIDs))
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	unique := dedupePositiveUint64(userIDs)
	if len(unique) == 0 {
		return out, nil
	}

	var rows []struct {
		UserID uint64 `gorm:"column:user_id"`
		Cnt    int    `gorm:"column:cnt"`
	}
	err := r.db.WithContext(ctx).
		Model(&model.UserSession{}).
		Select("user_id, COUNT(*) AS cnt").
		Where("user_id IN ? AND revoked_at IS NULL AND expires_at > ?", unique, now.UTC()).
		Group("user_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.UserID] = row.Cnt
	}
	return out, nil
}

func (r *GormAdminRepository) GetUserByID(ctx context.Context, userID uint64) (*model.User, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	var user model.User
	if err := r.db.WithContext(ctx).Select(selfUserSelectColumns).Where("id = ?", userID).Take(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *GormAdminRepository) LockUserByID(ctx context.Context, tx *gorm.DB, userID uint64) (*model.User, error) {
	if tx == nil {
		return nil, ErrRepositoryUnavailable
	}
	var user model.User
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select(selfUserSelectColumns).
		Where("id = ?", userID).
		Take(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *GormAdminRepository) CountActiveAdminsForUpdate(ctx context.Context, tx *gorm.DB) (int64, error) {
	if tx == nil {
		return 0, ErrRepositoryUnavailable
	}
	var ids []uint64
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Model(&model.User{}).
		Where("role = ? AND status = ?", model.UserRoleAdmin, model.UserStatusActive).
		Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

func (r *GormAdminRepository) UpdateUserStatus(ctx context.Context, tx *gorm.DB, userID uint64, status int8, now time.Time) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).
		Model(&model.User{}).
		Where("id = ?", userID).
		Updates(map[string]any{
			"status":     status,
			"updated_at": now.UTC().Truncate(time.Millisecond),
		}).Error
}

func (r *GormAdminRepository) ListUserSessions(ctx context.Context, userID uint64, limit int) ([]model.UserSession, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if limit <= 0 {
		limit = 50
	}
	var sessions []model.UserSession
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("id DESC").
		Limit(limit).
		Find(&sessions).Error
	return sessions, err
}

func (r *GormAdminRepository) CountUserFriends(ctx context.Context, userID uint64) (int64, error) {
	if r == nil || r.db == nil {
		return 0, ErrRepositoryUnavailable
	}
	var cnt int64
	err := r.db.WithContext(ctx).
		Model(&model.Friendship{}).
		Where("user_id = ? AND status = ?", userID, model.FriendshipStatusActive).
		Count(&cnt).Error
	return cnt, err
}

func (r *GormAdminRepository) CountUserGroups(ctx context.Context, userID uint64) (int64, error) {
	if r == nil || r.db == nil {
		return 0, ErrRepositoryUnavailable
	}
	var cnt int64
	err := r.db.WithContext(ctx).
		Model(&model.GroupMember{}).
		Where("user_id = ?", userID).
		Count(&cnt).Error
	return cnt, err
}

func (r *GormAdminRepository) ListUserAuditLogs(ctx context.Context, userID uint64, limit int) ([]model.AuditLog, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryUnavailable
	}
	if limit <= 0 {
		limit = 20
	}
	var logs []model.AuditLog
	err := r.db.WithContext(ctx).
		Where("(target_type = ? AND target_id = ?) OR actor_id = ?", model.AuditTargetUser, userID, userID).
		Order("id DESC").
		Limit(limit).
		Find(&logs).Error
	return logs, err
}

func (r *GormAdminRepository) LockUnrevokedSessionsByUser(ctx context.Context, tx *gorm.DB, userID uint64) ([]model.UserSession, error) {
	return NewSessionRepository(r.db, r.rdb).ListUnrevokedByUserForUpdate(ctx, tx, userID)
}

func (r *GormAdminRepository) LockSessionByID(ctx context.Context, tx *gorm.DB, sessionID uint64) (*model.UserSession, error) {
	return NewSessionRepository(r.db, r.rdb).FindByIDForUpdate(ctx, tx, sessionID)
}

func (r *GormAdminRepository) RevokeSessionsByIDs(ctx context.Context, tx *gorm.DB, sessionIDs []uint64, revokedAt time.Time) (int64, error) {
	return NewSessionRepository(r.db, r.rdb).RevokeByIDs(ctx, tx, sessionIDs, revokedAt)
}

func (r *GormAdminRepository) ListGroups(ctx context.Context, filter AdminGroupListFilter) ([]AdminGroupSummaryRow, bool, error) {
	if r == nil || r.db == nil {
		return nil, false, ErrRepositoryUnavailable
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	q := r.db.WithContext(ctx).Model(&model.Group{})
	if trimmed := strings.TrimSpace(filter.Query); trimmed != "" {
		likePattern := "%" + EscapeLikeLiteral(trimmed) + "%"
		if parsedID, err := strconv.ParseUint(trimmed, 10, 64); err == nil && parsedID > 0 {
			q = q.Where("(id = ? OR name LIKE ? ESCAPE '!')", parsedID, likePattern)
		} else {
			q = q.Where("name LIKE ? ESCAPE '!'", likePattern)
		}
	}
	if filter.Status != "" {
		q = q.Where("status = ?", filter.Status)
	}
	if filter.CursorID > 0 {
		q = q.Where("id < ?", filter.CursorID)
	}

	var groups []model.Group
	if err := q.Order("id DESC").Limit(limit + 1).Find(&groups).Error; err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(groups) > limit {
		hasMore = true
		groups = groups[:limit]
	}
	if len(groups) == 0 {
		return []AdminGroupSummaryRow{}, false, nil
	}

	groupIDs := make([]uint64, 0, len(groups))
	ownerIDs := make([]uint64, 0, len(groups))
	for _, g := range groups {
		groupIDs = append(groupIDs, g.ID)
		ownerIDs = append(ownerIDs, g.OwnerID)
	}

	var convs []model.Conversation
	if err := r.db.WithContext(ctx).
		Select("id", "group_id", "last_seq").
		Where("group_id IN ?", groupIDs).
		Find(&convs).Error; err != nil {
		return nil, false, err
	}
	convByGroup := make(map[uint64]model.Conversation, len(convs))
	for _, c := range convs {
		if c.GroupID != nil {
			convByGroup[*c.GroupID] = c
		}
	}

	var countRows []struct {
		GroupID uint64 `gorm:"column:group_id"`
		Cnt     int    `gorm:"column:cnt"`
	}
	if err := r.db.WithContext(ctx).
		Model(&model.GroupMember{}).
		Select("group_id, COUNT(*) AS cnt").
		Where("group_id IN ?", groupIDs).
		Group("group_id").
		Scan(&countRows).Error; err != nil {
		return nil, false, err
	}
	memberCountByGroup := make(map[uint64]int, len(countRows))
	for _, row := range countRows {
		memberCountByGroup[row.GroupID] = row.Cnt
	}

	ownersByID, err := r.batchGetUsers(ctx, ownerIDs)
	if err != nil {
		return nil, false, err
	}

	rows := make([]AdminGroupSummaryRow, 0, len(groups))
	for _, g := range groups {
		conv := convByGroup[g.ID]
		var ownerPtr *model.User
		if u, ok := ownersByID[g.OwnerID]; ok {
			uCopy := u
			ownerPtr = &uCopy
		}
		rows = append(rows, AdminGroupSummaryRow{
			Group:          g,
			ConversationID: conv.ID,
			LastSeq:        conv.LastSeq,
			MemberCount:    memberCountByGroup[g.ID],
			Owner:          ownerPtr,
		})
	}
	return rows, hasMore, nil
}

func (r *GormAdminRepository) GetGroupDetail(ctx context.Context, groupID uint64) (*AdminGroupSummaryRow, []model.GroupMember, map[uint64]model.User, []model.AuditLog, error) {
	if r == nil || r.db == nil {
		return nil, nil, nil, nil, ErrRepositoryUnavailable
	}
	var group model.Group
	if err := r.db.WithContext(ctx).Where("id = ?", groupID).Take(&group).Error; err != nil {
		return nil, nil, nil, nil, err
	}

	var conv model.Conversation
	convErr := r.db.WithContext(ctx).Where("group_id = ?", groupID).Take(&conv).Error
	if convErr != nil && !errors.Is(convErr, gorm.ErrRecordNotFound) {
		return nil, nil, nil, nil, convErr
	}

	var members []model.GroupMember
	if err := r.db.WithContext(ctx).
		Where("group_id = ?", groupID).
		Order("FIELD(role, 'owner', 'admin', 'member'), id ASC").
		Find(&members).Error; err != nil {
		return nil, nil, nil, nil, err
	}

	userIDs := make([]uint64, 0, len(members)+1)
	userIDs = append(userIDs, group.OwnerID)
	for _, m := range members {
		userIDs = append(userIDs, m.UserID)
	}
	usersByID, err := r.batchGetUsers(ctx, userIDs)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	var auditLogs []model.AuditLog
	if err := r.db.WithContext(ctx).
		Where("group_id = ?", groupID).
		Order("id DESC").
		Limit(20).
		Find(&auditLogs).Error; err != nil {
		return nil, nil, nil, nil, err
	}

	var ownerPtr *model.User
	if owner, ok := usersByID[group.OwnerID]; ok {
		ownerCopy := owner
		ownerPtr = &ownerCopy
	}
	summary := &AdminGroupSummaryRow{
		Group:          group,
		ConversationID: conv.ID,
		LastSeq:        conv.LastSeq,
		MemberCount:    len(members),
		Owner:          ownerPtr,
	}
	return summary, members, usersByID, auditLogs, nil
}

func (r *GormAdminRepository) LockGroupAndConversation(ctx context.Context, tx *gorm.DB, groupID uint64) (*model.Group, *model.Conversation, []model.GroupMember, error) {
	if tx == nil {
		return nil, nil, nil, ErrRepositoryUnavailable
	}
	var group model.Group
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", groupID).
		Take(&group).Error; err != nil {
		return nil, nil, nil, err
	}

	var conv model.Conversation
	convErr := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("group_id = ?", groupID).
		Take(&conv).Error
	var convPtr *model.Conversation
	if convErr == nil {
		convPtr = &conv
	} else if !errors.Is(convErr, gorm.ErrRecordNotFound) {
		return nil, nil, nil, convErr
	}

	var members []model.GroupMember
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("group_id = ?", groupID).
		Find(&members).Error; err != nil {
		return nil, nil, nil, err
	}
	return &group, convPtr, members, nil
}

func (r *GormAdminRepository) MarkGroupDisbanded(ctx context.Context, tx *gorm.DB, groupID uint64, now time.Time) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).
		Model(&model.Group{}).
		Where("id = ?", groupID).
		Updates(map[string]any{
			"status":     model.GroupStatusDisbanded,
			"updated_at": now.UTC().Truncate(time.Millisecond),
		}).Error
}

func (r *GormAdminRepository) CreateSystemMessageAndUpdateConv(
	ctx context.Context,
	tx *gorm.DB,
	msg *model.Message,
	convID uint64,
	memberIDs []uint64,
	now time.Time,
) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	if err := tx.WithContext(ctx).Create(msg).Error; err != nil {
		return err
	}
	ts := msg.CreatedAt.UTC().Truncate(time.Millisecond)
	preview := truncateRunesRepo(msg.Content, 255)
	if err := tx.WithContext(ctx).
		Model(&model.Conversation{}).
		Where("id = ?", convID).
		Updates(map[string]any{
			"last_msg_id":      msg.ID,
			"last_seq":         msg.Seq,
			"last_msg_preview": preview,
			"last_msg_at":      &ts,
			"updated_at":       ts,
		}).Error; err != nil {
		return err
	}
	if len(memberIDs) > 0 {
		if err := tx.WithContext(ctx).
			Model(&model.ConversationMember{}).
			Where("conversation_id = ? AND user_id IN ?", convID, dedupePositiveUint64(memberIDs)).
			Updates(map[string]any{
				"unread_count": gorm.Expr("unread_count + 1"),
				"updated_at":   now.UTC().Truncate(time.Millisecond),
			}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (r *GormAdminRepository) AppendAuditLog(ctx context.Context, tx *gorm.DB, entry *model.AuditLog) error {
	if tx == nil {
		return ErrRepositoryUnavailable
	}
	return tx.WithContext(ctx).Create(entry).Error
}

func (r *GormAdminRepository) ListMessageAudit(ctx context.Context, filter AdminMessageAuditFilter) ([]model.Message, map[uint64]model.User, bool, error) {
	if r == nil || r.db == nil {
		return nil, nil, false, ErrRepositoryUnavailable
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	q := r.db.WithContext(ctx).Model(&model.Message{})
	if filter.ConversationID != nil && *filter.ConversationID > 0 {
		q = q.Where("conversation_id = ?", *filter.ConversationID)
	}
	if filter.SenderID != nil && *filter.SenderID > 0 {
		q = q.Where("sender_id = ?", *filter.SenderID)
	}
	if kw := strings.TrimSpace(filter.Keyword); kw != "" {
		q = q.Where("content LIKE ? ESCAPE '!'", "%"+EscapeLikeLiteral(kw)+"%")
	}
	if filter.From != nil {
		q = q.Where("created_at >= ?", filter.From.UTC())
	}
	if filter.To != nil {
		q = q.Where("created_at <= ?", filter.To.UTC())
	}
	if filter.CursorID > 0 {
		q = q.Where("id < ?", filter.CursorID)
	}

	var msgs []model.Message
	if err := q.Order("id DESC").Limit(limit + 1).Find(&msgs).Error; err != nil {
		return nil, nil, false, err
	}

	hasMore := false
	if len(msgs) > limit {
		hasMore = true
		msgs = msgs[:limit]
	}

	senderIDs := make([]uint64, 0, len(msgs))
	for _, m := range msgs {
		if m.SenderID > 0 {
			senderIDs = append(senderIDs, m.SenderID)
		}
	}
	senders, err := r.batchGetUsers(ctx, senderIDs)
	if err != nil {
		return nil, nil, false, err
	}
	return msgs, senders, hasMore, nil
}

func (r *GormAdminRepository) CountMessagesSince(ctx context.Context, since time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, ErrRepositoryUnavailable
	}
	var cnt int64
	err := r.db.WithContext(ctx).
		Model(&model.Message{}).
		Where("created_at >= ?", since.UTC()).
		Count(&cnt).Error
	return cnt, err
}

func (r *GormAdminRepository) ProbeMySQL(ctx context.Context) (string, int64, error) {
	if r == nil || r.db == nil {
		return "", 0, ErrRepositoryUnavailable
	}
	start := time.Now()
	if err := PingMySQL(ctx, r.db); err != nil {
		return "", time.Since(start).Milliseconds(), err
	}
	var version string
	if err := r.db.WithContext(ctx).Raw("SELECT VERSION()").Scan(&version).Error; err != nil {
		return "", time.Since(start).Milliseconds(), err
	}
	return strings.TrimSpace(version), time.Since(start).Milliseconds(), nil
}

func (r *GormAdminRepository) ProbeRedis(ctx context.Context) (string, int64, error) {
	if r == nil || r.rdb == nil {
		return "", 0, errors.New("redis client is nil")
	}
	start := time.Now()
	if err := PingRedis(ctx, r.rdb); err != nil {
		return "", time.Since(start).Milliseconds(), err
	}
	info, err := r.rdb.Info(ctx, "server").Result()
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", latency, err
	}
	return parseRedisVersion(info), latency, nil
}

func (r *GormAdminRepository) GetUsersOnline(ctx context.Context, userIDs []uint64) (map[uint64]bool, error) {
	if r == nil || r.rdb == nil {
		out := make(map[uint64]bool, len(userIDs))
		for _, id := range userIDs {
			out[id] = false
		}
		return out, nil
	}
	return NewUserPresenceRepository(r.rdb).GetUsersOnline(ctx, userIDs)
}

func (r *GormAdminRepository) CountLivePresence(ctx context.Context) (int64, int64, error) {
	if r == nil || r.rdb == nil {
		return 0, 0, errors.New("redis client is nil")
	}
	activeMap, err := r.rdb.HGetAll(ctx, "im:presence:active").Result()
	if err != nil {
		return 0, 0, err
	}
	if len(activeMap) == 0 {
		return 0, 0, nil
	}

	userIDs := make([]string, 0, len(activeMap))
	for uid := range activeMap {
		if uid != "" {
			userIDs = append(userIDs, uid)
		}
	}
	pipe := r.rdb.Pipeline()
	cmds := make(map[string]*redis.StringCmd, len(userIDs))
	for _, uid := range userIDs {
		cmds[uid] = pipe.Get(ctx, redisPresenceUserPrefix+uid)
	}
	_, execErr := pipe.Exec(ctx)
	if execErr != nil && !errors.Is(execErr, redis.Nil) {
		return 0, 0, execErr
	}

	var onlineUsers, wsConns int64
	for _, uid := range userIDs {
		val, cmdErr := cmds[uid].Result()
		if cmdErr != nil {
			continue
		}
		cnt, parseErr := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if parseErr == nil && cnt > 0 {
			onlineUsers++
			wsConns += cnt
		}
	}
	return onlineUsers, wsConns, nil
}

func (r *GormAdminRepository) CountLiveConnectionsForSessions(
	ctx context.Context,
	userID uint64,
	sessionIDs []uint64,
	includeUserRoutes bool,
) (int64, []uint64, error) {
	if r == nil || r.rdb == nil {
		return 0, sessionIDs, nil
	}
	pipe := r.rdb.Pipeline()
	sessCmds := make([]*redis.StringSliceCmd, 0, len(sessionIDs))
	for _, sid := range sessionIDs {
		sessCmds = append(sessCmds, pipe.SMembers(ctx, "im:route:session:"+strconv.FormatUint(sid, 10)))
	}
	var userRouteCmd *redis.StringSliceCmd
	if includeUserRoutes && userID > 0 {
		userRouteCmd = pipe.SMembers(ctx, "im:route:user:"+strconv.FormatUint(userID, 10))
	}
	_, execErr := pipe.Exec(ctx)
	if execErr != nil && !errors.Is(execErr, redis.Nil) {
		return 0, sessionIDs, execErr
	}

	connIDs := make([]string, 0)
	seenConn := make(map[string]struct{})
	addRoute := func(route string) {
		if _, connID, ok := SplitRoute(route); ok {
			if _, exists := seenConn[connID]; !exists {
				seenConn[connID] = struct{}{}
				connIDs = append(connIDs, connID)
			}
		}
	}
	for _, cmd := range sessCmds {
		routes, _ := cmd.Result()
		for _, route := range routes {
			addRoute(route)
		}
	}
	if userRouteCmd != nil {
		routes, _ := userRouteCmd.Result()
		for _, route := range routes {
			addRoute(route)
		}
	}
	if len(connIDs) == 0 {
		return 0, sessionIDs, nil
	}

	connPipe := r.rdb.Pipeline()
	connCmds := make([]*redis.SliceCmd, 0, len(connIDs))
	for _, cid := range connIDs {
		connCmds = append(connCmds, connPipe.HMGet(ctx, "im:presence:conn:"+cid, "user_id", "session_id"))
	}
	_, _ = connPipe.Exec(ctx)

	seenSess := make(map[uint64]struct{}, len(sessionIDs))
	mergedSessions := make([]uint64, 0, len(sessionIDs))
	for _, sid := range sessionIDs {
		if sid > 0 {
			if _, ok := seenSess[sid]; !ok {
				seenSess[sid] = struct{}{}
				mergedSessions = append(mergedSessions, sid)
			}
		}
	}

	var liveCount int64
	userIDStr := strconv.FormatUint(userID, 10)
	for _, cmd := range connCmds {
		vals := cmd.Val()
		if len(vals) < 2 {
			continue
		}
		uStr, ok1 := vals[0].(string)
		sStr, ok2 := vals[1].(string)
		if !ok1 || !ok2 || uStr != userIDStr {
			continue
		}
		sid, parseErr := strconv.ParseUint(sStr, 10, 64)
		if parseErr != nil || sid == 0 {
			continue
		}
		if _, exists := seenSess[sid]; exists || includeUserRoutes {
			liveCount++
			if _, ok := seenSess[sid]; !ok {
				seenSess[sid] = struct{}{}
				mergedSessions = append(mergedSessions, sid)
			}
		}
	}
	return liveCount, mergedSessions, nil
}

func (r *GormAdminRepository) BlacklistAccessJTIs(ctx context.Context, jtis []string, reason string, ttl time.Duration) error {
	return NewSessionRepository(r.db, r.rdb).BlacklistAccessJTIs(ctx, jtis, reason, ttl)
}

func (r *GormAdminRepository) CollectStreamsOverview(ctx context.Context) (AdminStreamsSummary, error) {
	if r == nil || r.rdb == nil {
		return AdminStreamsSummary{}, errors.New("redis client is nil")
	}
	pipe := r.rdb.Pipeline()
	msgLenCmd := pipe.XLen(ctx, "im:stream:messages")
	evtLenCmd := pipe.XLen(ctx, "im:stream:events")
	deadMsgLenCmd := pipe.XLen(ctx, "im:stream:dead:messages")
	deadEvtLenCmd := pipe.XLen(ctx, "im:stream:dead:events")
	if _, err := pipe.Exec(ctx); err != nil {
		return AdminStreamsSummary{}, err
	}

	var pendingTotal int64
	for _, stream := range []string{"im:stream:messages", "im:stream:events"} {
		groups, err := r.rdb.XInfoGroups(ctx, stream).Result()
		if err != nil {
			if isRedisStreamMissingErr(err) {
				continue
			}
			return AdminStreamsSummary{}, err
		}
		for _, g := range groups {
			pendingTotal += g.Pending
		}
	}

	return AdminStreamsSummary{
		MessagesLen:     msgLenCmd.Val(),
		EventsLen:       evtLenCmd.Val(),
		PendingTotal:    pendingTotal,
		DeadLetterCount: deadMsgLenCmd.Val() + deadEvtLenCmd.Val(),
	}, nil
}

func (r *GormAdminRepository) CollectStreamConsumers(ctx context.Context) ([]AdminStreamConsumerRow, error) {
	if r == nil || r.rdb == nil {
		return nil, errors.New("redis client is nil")
	}
	nowMs := time.Now().UTC().UnixMilli()
	streamGroups := []struct {
		stream string
		groups []string
	}{
		{stream: "im:stream:messages", groups: []string{"im-msg-persist", "im-msg-deliver"}},
		{stream: "im:stream:events", groups: []string{"im-event-deliver", "im-event-audit"}},
	}

	rows := make([]AdminStreamConsumerRow, 0, 8)
	for _, sg := range streamGroups {
		infoGroups, err := r.rdb.XInfoGroups(ctx, sg.stream).Result()
		if err != nil {
			if isRedisStreamMissingErr(err) {
				for _, g := range sg.groups {
					rows = append(rows, AdminStreamConsumerRow{
						Stream:          sg.stream,
						Group:           g,
						Consumer:        "-",
						LastDeliveredID: "0-0",
					})
				}
				continue
			}
			return nil, err
		}

		groupMap := make(map[string]redis.XInfoGroup, len(infoGroups))
		for _, g := range infoGroups {
			groupMap[g.Name] = g
		}
		for _, gName := range sg.groups {
			gInfo, exists := groupMap[gName]
			if !exists {
				rows = append(rows, AdminStreamConsumerRow{
					Stream:          sg.stream,
					Group:           gName,
					Consumer:        "-",
					LastDeliveredID: "0-0",
				})
				continue
			}

			var oldestAgeMs int64
			if gInfo.Pending > 0 {
				pend, pErr := r.rdb.XPending(ctx, sg.stream, gName).Result()
				if pErr == nil && pend != nil && pend.Count > 0 {
					oldestAgeMs = computeStreamIDAgeMs(pend.Lower, nowMs)
				}
			}

			consumers, cErr := r.rdb.XInfoConsumers(ctx, sg.stream, gName).Result()
			if cErr != nil && !isRedisStreamMissingErr(cErr) {
				return nil, cErr
			}
			if len(consumers) == 0 {
				rows = append(rows, AdminStreamConsumerRow{
					Stream:           sg.stream,
					Group:            gName,
					Consumer:         "-",
					LastDeliveredID:  defaultStreamID(gInfo.LastDeliveredID),
					PendingCount:     gInfo.Pending,
					OldestPendingAge: oldestAgeMs,
				})
				continue
			}
			for _, c := range consumers {
				ageMs := int64(0)
				if c.Pending > 0 {
					ageMs = max(oldestAgeMs, c.Idle.Milliseconds())
				}
				rows = append(rows, AdminStreamConsumerRow{
					Stream:           sg.stream,
					Group:            gName,
					Consumer:         c.Name,
					LastDeliveredID:  defaultStreamID(gInfo.LastDeliveredID),
					PendingCount:     c.Pending,
					OldestPendingAge: ageMs,
				})
			}
		}
	}
	return rows, nil
}

func (r *GormAdminRepository) batchGetUsers(ctx context.Context, userIDs []uint64) (map[uint64]model.User, error) {
	unique := dedupePositiveUint64(userIDs)
	out := make(map[uint64]model.User, len(unique))
	if len(unique) == 0 {
		return out, nil
	}
	var users []model.User
	if err := r.db.WithContext(ctx).
		Select(selfUserSelectColumns).
		Where("id IN ?", unique).
		Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

func parseRedisVersion(info string) string {
	for _, line := range strings.Split(info, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "redis_version:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "redis_version:"))
		}
	}
	return ""
}

func isRedisStreamMissingErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such key") || strings.Contains(msg, "NOGROUP")
}

func computeStreamIDAgeMs(id string, nowMs int64) int64 {
	tsPart, _, ok := strings.Cut(strings.TrimSpace(id), "-")
	if !ok || tsPart == "" {
		return 0
	}
	entryMs, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil || entryMs <= 0 || nowMs <= entryMs {
		return 0
	}
	return nowMs - entryMs
}

func defaultStreamID(id string) string {
	if strings.TrimSpace(id) == "" {
		return "0-0"
	}
	return id
}

func truncateRunesRepo(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}
