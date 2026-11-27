package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

const (
	defaultAdminListLimit   = 20
	maxAdminListLimit       = 100
	maxAdminReasonRunes     = 512
	maxAdminAuditRange      = 90 * 24 * time.Hour
	maxAdminAuditKeywordLen = 200

	adminUserCursorScope         = "admin_users"
	adminGroupCursorScope        = "admin_groups"
	adminMessageAuditCursorScope = "admin_message_audit"
)

// AdminSessionRevoker kicks active WebSocket connections for revoked sessions.
// *ws.Hub satisfies this interface directly.
type AdminSessionRevoker interface {
	RevokeSessions(ctx context.Context, userID uint64, sessionIDs []uint64, reason string) error
}

// AdminEventNotifier delivers group disband notifications to online members.
// *ws.Hub satisfies this interface directly.
type AdminEventNotifier interface {
	DeliverToUsers(ctx context.Context, targetUserIDs []uint64, frameType string, data any) error
}

// AdminUserSummaryDTO matches AdminUserSummary in docs/api.md and frontend/src/types/admin.ts.
type AdminUserSummaryDTO struct {
	ID           uint64  `json:"id,string"`
	Username     string  `json:"username"`
	Nickname     string  `json:"nickname"`
	AvatarURL    string  `json:"avatar_url"`
	Signature    string  `json:"signature"`
	Role         string  `json:"role"`
	Status       int8    `json:"status"`
	Online       bool    `json:"online"`
	LastOnlineAt *string `json:"last_online_at"`
	SessionCount int     `json:"session_count"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// AdminUserSessionDTO matches AdminUserSession in frontend/src/types/admin.ts.
type AdminUserSessionDTO struct {
	ID           uint64  `json:"id,string"`
	DeviceID     string  `json:"device_id"`
	DeviceType   string  `json:"device_type"`
	IPAddress    string  `json:"ip_address"`
	UserAgent    string  `json:"user_agent"`
	ExpiresAt    string  `json:"expires_at"`
	RevokedAt    *string `json:"revoked_at"`
	LastActiveAt string  `json:"last_active_at"`
	CreatedAt    string  `json:"created_at"`
}

// AdminUserPage matches GET /api/v1/admin/users data.
type AdminUserPage struct {
	Items      []AdminUserSummaryDTO `json:"items"`
	NextCursor *string               `json:"next_cursor"`
	HasMore    bool                  `json:"has_more"`
}

// AdminUserDetailDTO matches GET /api/v1/admin/users/{user_id} data.
type AdminUserDetailDTO struct {
	User         AdminUserSummaryDTO   `json:"user"`
	Sessions     []AdminUserSessionDTO `json:"sessions"`
	FriendCount  int64                 `json:"friend_count"`
	GroupCount   int64                 `json:"group_count"`
	AuditSummary []GroupAuditLogDTO    `json:"audit_summary"`
}

// AdminUpdateUserStatusResult matches PATCH /api/v1/admin/users/{user_id}/status data.
type AdminUpdateUserStatusResult struct {
	UserID                uint64 `json:"user_id,string"`
	Status                int8   `json:"status"`
	RevokedSessionCount   int64  `json:"revoked_session_count"`
	KickedConnectionCount int64  `json:"kicked_connection_count"`
}

// AdminKickUserResult matches POST /api/v1/admin/users/{user_id}/kick data.
type AdminKickUserResult struct {
	UserID                uint64 `json:"user_id,string"`
	RevokedSessionCount   int64  `json:"revoked_session_count"`
	KickedConnectionCount int64  `json:"kicked_connection_count"`
}

// AdminGroupOwnerDTO represents summary info of a group's owner.
type AdminGroupOwnerDTO struct {
	ID        uint64 `json:"id,string"`
	Username  string `json:"username"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url"`
}

// AdminGroupSummaryDTO matches GET /api/v1/admin/groups item.
type AdminGroupSummaryDTO struct {
	ID             uint64              `json:"id,string"`
	ConversationID uint64              `json:"conversation_id,string"`
	Name           string              `json:"name"`
	AvatarURL      string              `json:"avatar_url"`
	Announcement   *string             `json:"announcement"`
	OwnerID        uint64              `json:"owner_id,string"`
	Owner          *AdminGroupOwnerDTO `json:"owner,omitempty"`
	Status         string              `json:"status"`
	MemberCount    int                 `json:"member_count"`
	MaxMembers     uint32              `json:"max_members"`
	LastSeq        uint64              `json:"last_seq,string"`
	CreatedAt      string              `json:"created_at"`
	UpdatedAt      string              `json:"updated_at"`
}

// AdminGroupPage matches GET /api/v1/admin/groups data.
type AdminGroupPage struct {
	Items      []AdminGroupSummaryDTO `json:"items"`
	NextCursor *string                `json:"next_cursor"`
	HasMore    bool                   `json:"has_more"`
}

// AdminGroupDetailDTO matches GET /api/v1/admin/groups/{group_id} data.
type AdminGroupDetailDTO struct {
	Group        AdminGroupSummaryDTO `json:"group"`
	Members      []GroupMemberDTO     `json:"members"`
	SystemEvents []GroupAuditLogDTO   `json:"system_events"`
}

// AdminDisbandGroupResult matches DELETE /api/v1/admin/groups/{group_id} data.
type AdminDisbandGroupResult struct {
	GroupID        uint64           `json:"group_id,string"`
	ConversationID uint64           `json:"conversation_id,string"`
	Status         string           `json:"status"`
	AuditLog       GroupAuditLogDTO `json:"audit_log"`
}

// AdminMessageAuditItemDTO matches GET /api/v1/admin/messages/audit item.
type AdminMessageAuditItemDTO struct {
	MsgID          uint64             `json:"msg_id,string"`
	ClientMsgID    string             `json:"client_msg_id"`
	ConversationID uint64             `json:"conversation_id,string"`
	Seq            uint64             `json:"seq,string"`
	SenderID       uint64             `json:"sender_id,string"`
	SenderUsername string             `json:"sender_username,omitempty"`
	SenderNickname string             `json:"sender_nickname,omitempty"`
	MsgType        string             `json:"msg_type"`
	Content        string             `json:"content"`
	Extra          model.NullableJSON `json:"extra"`
	Status         string             `json:"status"`
	CreatedAt      string             `json:"created_at"`
	RecalledAt     *string            `json:"recalled_at"`
}

// AdminMessageAuditPage matches GET /api/v1/admin/messages/audit data.
type AdminMessageAuditPage struct {
	Items      []AdminMessageAuditItemDTO `json:"items"`
	NextCursor *string                    `json:"next_cursor"`
	HasMore    bool                       `json:"has_more"`
}

// AdminMetricsOverviewDTO matches GET /api/v1/admin/metrics/overview data.
type AdminMetricsOverviewDTO struct {
	OnlineUsers       int64                          `json:"online_users"`
	WSConnections     int64                          `json:"ws_connections"`
	MessagesPerMinute int64                          `json:"messages_per_minute"`
	Streams           repository.AdminStreamsSummary `json:"streams"`
	Goroutines        int                            `json:"goroutines"`
	HeapBytes         uint64                         `json:"heap_bytes"`
	Timestamp         string                         `json:"timestamp"`
}

// AdminStreamMetricsPage matches GET /api/v1/admin/metrics/streams data.
type AdminStreamMetricsPage struct {
	Items []repository.AdminStreamConsumerRow `json:"items"`
}

// DependencyHealthItemDTO matches DependencyHealthItem in frontend/src/types/admin.ts.
type DependencyHealthItemDTO struct {
	Status      string  `json:"status"`
	LatencyMs   *int64  `json:"latency_ms"`
	Version     string  `json:"version"`
	LastErrorAt *string `json:"last_error_at"`
	LastError   *string `json:"last_error"`
}

// AdminDependenciesHealthDTO matches GET /api/v1/admin/health/dependencies data.
type AdminDependenciesHealthDTO struct {
	MySQL     DependencyHealthItemDTO `json:"mysql"`
	Redis     DependencyHealthItemDTO `json:"redis"`
	Timestamp string                  `json:"timestamp"`
}

// ListAdminUsersInput defines validated query inputs for ListUsers.
type ListAdminUsersInput struct {
	Query  string
	Status *int8
	Role   string
	Cursor string
	Limit  int
}

// ListAdminGroupsInput defines validated query inputs for ListGroups.
type ListAdminGroupsInput struct {
	Query  string
	Status string
	Cursor string
	Limit  int
}

// ListAdminMessageAuditInput defines validated query inputs for ListMessageAudit.
type ListAdminMessageAuditInput struct {
	ConversationID *uint64
	SenderID       *uint64
	Keyword        string
	From           *time.Time
	To             *time.Time
	Cursor         string
	Limit          int
}

type adminUserCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"s"`
	Query   string `json:"q"`
	Status  string `json:"st"`
	Role    string `json:"r"`
	LastID  uint64 `json:"id,string"`
}

type adminGroupCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"s"`
	Query   string `json:"q"`
	Status  string `json:"st"`
	LastID  uint64 `json:"id,string"`
}

type adminAuditMsgCursor struct {
	Version        int    `json:"v"`
	Scope          string `json:"s"`
	ConversationID string `json:"cid"`
	SenderID       string `json:"sid"`
	Keyword        string `json:"kw"`
	From           string `json:"f"`
	To             string `json:"t"`
	LastID         uint64 `json:"id,string"`
}

type depLastError struct {
	at  *time.Time
	msg *string
}

// AdminService implements Stage 9 platform administration, session revocation/kick,
// group governance, message audit search, and live dependency/stream monitoring.
type AdminService struct {
	repo         repository.AdminRepository
	seqAllocator repository.SeqAllocator
	idGen        SnowflakeIDGenerator
	revoker      AdminSessionRevoker
	notifier     AdminEventNotifier
	accessTTL    time.Duration
	log          *slog.Logger
	nowFn        func() time.Time

	errMu        sync.Mutex
	mysqlLastErr depLastError
	redisLastErr depLastError
}

// NewAdminService constructs an AdminService.
func NewAdminService(
	repo repository.AdminRepository,
	seqAllocator repository.SeqAllocator,
	idGen SnowflakeIDGenerator,
	revoker AdminSessionRevoker,
	notifier AdminEventNotifier,
	accessTTL time.Duration,
	log *slog.Logger,
) *AdminService {
	if accessTTL <= 0 {
		accessTTL = 15 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &AdminService{
		repo:         repo,
		seqAllocator: seqAllocator,
		idGen:        idGen,
		revoker:      revoker,
		notifier:     notifier,
		accessTTL:    accessTTL,
		log:          log,
		nowFn:        func() time.Time { return time.Now().UTC() },
	}
}

// ListUsers implements GET /api/v1/admin/users.
func (s *AdminService) ListUsers(ctx context.Context, input ListAdminUsersInput) (*AdminUserPage, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	limit := normalizeAdminLimit(input.Limit)
	if input.Status != nil && *input.Status != model.UserStatusDisabled && *input.Status != model.UserStatusActive {
		return nil, errcode.ErrInvalidValue.WithMessage("status must be 0 or 1")
	}
	role := strings.TrimSpace(input.Role)
	if role != "" && role != model.UserRoleUser && role != model.UserRoleAdmin {
		return nil, errcode.ErrInvalidValue.WithMessage("role must be user or admin")
	}
	q := strings.TrimSpace(input.Query)
	statusTag := ""
	if input.Status != nil {
		statusTag = strconv.Itoa(int(*input.Status))
	}

	var cursorID uint64
	if strings.TrimSpace(input.Cursor) != "" {
		var cur adminUserCursor
		if err := decodeScopedCursor(input.Cursor, &cur); err != nil ||
			cur.Version != 1 || cur.Scope != adminUserCursorScope ||
			cur.Query != q || cur.Status != statusTag || cur.Role != role || cur.LastID == 0 {
			return nil, errcode.ErrInvalidValue.WithMessage("invalid cursor")
		}
		cursorID = cur.LastID
	}

	users, hasMore, err := s.repo.ListUsers(ctx, repository.AdminUserListFilter{
		Query:    q,
		Status:   input.Status,
		Role:     role,
		CursorID: cursorID,
		Limit:    limit,
	})
	if err != nil {
		return nil, s.classifyRepoErr(ctx, "admin_list_users", err)
	}

	userIDs := make([]uint64, 0, len(users))
	for _, u := range users {
		userIDs = append(userIDs, u.ID)
	}
	now := s.nowFn()
	sessionCounts, err := s.repo.BatchCountActiveSessions(ctx, userIDs, now)
	if err != nil {
		return nil, s.classifyRepoErr(ctx, "admin_batch_session_counts", err)
	}
	onlineMap, err := s.repo.GetUsersOnline(ctx, userIDs)
	if err != nil {
		return nil, errcode.ErrRedisUnavailable
	}

	items := make([]AdminUserSummaryDTO, 0, len(users))
	for _, u := range users {
		items = append(items, toAdminUserSummaryDTO(u, onlineMap[u.ID], sessionCounts[u.ID]))
	}

	var nextCursor *string
	if hasMore && len(users) > 0 {
		encoded, encErr := encodeCursor(adminUserCursor{
			Version: 1,
			Scope:   adminUserCursorScope,
			Query:   q,
			Status:  statusTag,
			Role:    role,
			LastID:  users[len(users)-1].ID,
		})
		if encErr != nil {
			return nil, errcode.ErrInternalConsistency
		}
		nextCursor = &encoded
	}

	return &AdminUserPage{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// GetUserDetail implements GET /api/v1/admin/users/{user_id}.
func (s *AdminService) GetUserDetail(ctx context.Context, userID uint64) (*AdminUserDetailDTO, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if userID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid user_id")
	}
	user, err := s.repo.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrAdminTargetNotFound
		}
		return nil, s.classifyRepoErr(ctx, "admin_get_user", err)
	}
	sessions, err := s.repo.ListUserSessions(ctx, userID, 50)
	if err != nil {
		return nil, s.classifyRepoErr(ctx, "admin_list_user_sessions", err)
	}
	friendCount, err := s.repo.CountUserFriends(ctx, userID)
	if err != nil {
		return nil, s.classifyRepoErr(ctx, "admin_count_user_friends", err)
	}
	groupCount, err := s.repo.CountUserGroups(ctx, userID)
	if err != nil {
		return nil, s.classifyRepoErr(ctx, "admin_count_user_groups", err)
	}
	audits, err := s.repo.ListUserAuditLogs(ctx, userID, 20)
	if err != nil {
		return nil, s.classifyRepoErr(ctx, "admin_list_user_audits", err)
	}
	onlineMap, err := s.repo.GetUsersOnline(ctx, []uint64{userID})
	if err != nil {
		return nil, errcode.ErrRedisUnavailable
	}

	now := s.nowFn()
	activeSessions := 0
	sessionDTOs := make([]AdminUserSessionDTO, 0, len(sessions))
	for _, sess := range sessions {
		if sess.RevokedAt == nil && sess.ExpiresAt.After(now) {
			activeSessions++
		}
		sessionDTOs = append(sessionDTOs, AdminUserSessionDTO{
			ID:           sess.ID,
			DeviceID:     sess.DeviceID,
			DeviceType:   sess.DeviceType,
			IPAddress:    sess.IPAddress,
			UserAgent:    sess.UserAgent,
			ExpiresAt:    FormatUTCMillis(sess.ExpiresAt),
			RevokedAt:    FormatUTCMillisPtr(sess.RevokedAt),
			LastActiveAt: FormatUTCMillis(sess.LastActiveAt),
			CreatedAt:    FormatUTCMillis(sess.CreatedAt),
		})
	}
	auditDTOs := make([]GroupAuditLogDTO, 0, len(audits))
	for _, a := range audits {
		auditDTOs = append(auditDTOs, toGroupAuditLogDTO(a))
	}

	return &AdminUserDetailDTO{
		User:         toAdminUserSummaryDTO(*user, onlineMap[userID], activeSessions),
		Sessions:     sessionDTOs,
		FriendCount:  friendCount,
		GroupCount:   groupCount,
		AuditSummary: auditDTOs,
	}, nil
}

// UpdateUserStatus implements PATCH /api/v1/admin/users/{user_id}/status.
func (s *AdminService) UpdateUserStatus(
	ctx context.Context,
	adminID uint64,
	targetUserID uint64,
	status int8,
	reason string,
	meta GroupAuditMeta,
) (*AdminUpdateUserStatusResult, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if targetUserID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid user_id")
	}
	if status != model.UserStatusDisabled && status != model.UserStatusActive {
		return nil, errcode.ErrInvalidValue.WithMessage("status must be 0 or 1")
	}
	trimmedReason := truncateRunes(strings.TrimSpace(reason), maxAdminReasonRunes)

	now := s.nowFn().Truncate(time.Millisecond)
	var (
		revokedSessionIDs []uint64
		revokedJTIs       []string
		revokedCount      int64
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		target, err := s.repo.LockUserByID(ctx, tx, targetUserID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errcode.ErrAdminTargetNotFound
			}
			return err
		}
		if target.Status == status {
			return errcode.ErrAdminStateConflict
		}
		if status == model.UserStatusDisabled && target.Role == model.UserRoleAdmin {
			activeAdmins, cntErr := s.repo.CountActiveAdminsForUpdate(ctx, tx)
			if cntErr != nil {
				return cntErr
			}
			if activeAdmins <= 1 {
				return errcode.ErrCannotDisableOnlyAdmin
			}
		}

		if err := s.repo.UpdateUserStatus(ctx, tx, targetUserID, status, now); err != nil {
			return err
		}

		action := "user.enable"
		if status == model.UserStatusDisabled {
			action = "user.disable"
			sessions, sessErr := s.repo.LockUnrevokedSessionsByUser(ctx, tx, targetUserID)
			if sessErr != nil {
				return sessErr
			}
			if len(sessions) > 0 {
				revokedSessionIDs, revokedJTIs = collectSessionIDsAndJTIs(sessions)
				revokedCount, err = s.repo.RevokeSessionsByIDs(ctx, tx, revokedSessionIDs, now)
				if err != nil {
					return err
				}
			}
		}

		audit := buildAdminAuditEntry(
			action,
			adminID,
			model.AuditTargetUser,
			targetUserID,
			nil,
			nil,
			trimmedReason,
			map[string]any{
				"status":                status,
				"revoked_session_count": revokedCount,
			},
			meta,
			now,
		)
		return s.repo.AppendAuditLog(ctx, tx, audit)
	})
	if txErr != nil {
		return nil, s.asBizErr(ctx, "admin_update_user_status", txErr)
	}

	var kickedConnCount int64
	if status == model.UserStatusDisabled {
		if len(revokedJTIs) > 0 {
			if err := s.repo.BlacklistAccessJTIs(ctx, revokedJTIs, "account.disabled", s.accessTTL); err != nil {
				s.log.WarnContext(ctx, "admin_disable_blacklist_jti_failed", slog.Uint64("user_id", targetUserID), slog.Any("error", err))
			}
		}
		var kickSessionIDs []uint64
		var countErr error
		kickedConnCount, kickSessionIDs, countErr = s.repo.CountLiveConnectionsForSessions(ctx, targetUserID, revokedSessionIDs, true)
		if countErr != nil {
			s.log.WarnContext(ctx, "admin_disable_count_conns_failed", slog.Uint64("user_id", targetUserID), slog.Any("error", countErr))
			kickSessionIDs = revokedSessionIDs
		}
		if len(kickSessionIDs) > 0 && s.revoker != nil {
			if err := s.revoker.RevokeSessions(ctx, targetUserID, kickSessionIDs, "account.disabled"); err != nil {
				s.log.WarnContext(ctx, "admin_disable_revoke_ws_failed", slog.Uint64("user_id", targetUserID), slog.Any("error", err))
			}
		}
	}

	return &AdminUpdateUserStatusResult{
		UserID:                targetUserID,
		Status:                status,
		RevokedSessionCount:   revokedCount,
		KickedConnectionCount: kickedConnCount,
	}, nil
}

// KickUser implements POST /api/v1/admin/users/{user_id}/kick.
func (s *AdminService) KickUser(
	ctx context.Context,
	adminID uint64,
	targetUserID uint64,
	sessionID *uint64,
	reason string,
	meta GroupAuditMeta,
) (*AdminKickUserResult, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if targetUserID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid user_id")
	}
	if sessionID != nil && *sessionID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid session_id")
	}
	trimmedReason := truncateRunes(strings.TrimSpace(reason), maxAdminReasonRunes)

	now := s.nowFn().Truncate(time.Millisecond)
	var (
		revokedSessionIDs []uint64
		revokedJTIs       []string
		revokedCount      int64
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		if _, err := s.repo.LockUserByID(ctx, tx, targetUserID); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errcode.ErrAdminTargetNotFound
			}
			return err
		}

		if sessionID != nil {
			sess, err := s.repo.LockSessionByID(ctx, tx, *sessionID)
			if err != nil {
				if errors.Is(err, repository.ErrSessionNotFound) {
					return errcode.ErrAdminTargetNotFound
				}
				return err
			}
			if sess.UserID != targetUserID {
				return errcode.ErrAdminTargetNotFound
			}
			if sess.RevokedAt == nil {
				revokedSessionIDs = []uint64{sess.ID}
				if sess.AccessJTI != "" {
					revokedJTIs = []string{sess.AccessJTI}
				}
				revokedCount, err = s.repo.RevokeSessionsByIDs(ctx, tx, revokedSessionIDs, now)
				if err != nil {
					return err
				}
			}
		} else {
			sessions, err := s.repo.LockUnrevokedSessionsByUser(ctx, tx, targetUserID)
			if err != nil {
				return err
			}
			if len(sessions) > 0 {
				revokedSessionIDs, revokedJTIs = collectSessionIDsAndJTIs(sessions)
				revokedCount, err = s.repo.RevokeSessionsByIDs(ctx, tx, revokedSessionIDs, now)
				if err != nil {
					return err
				}
			}
		}

		targetType := model.AuditTargetUser
		targetID := targetUserID
		if sessionID != nil {
			targetType = model.AuditTargetSession
			targetID = *sessionID
		}
		audit := buildAdminAuditEntry(
			"user.kick",
			adminID,
			targetType,
			targetID,
			nil,
			nil,
			trimmedReason,
			map[string]any{
				"user_id":               strconv.FormatUint(targetUserID, 10),
				"revoked_session_count": revokedCount,
			},
			meta,
			now,
		)
		return s.repo.AppendAuditLog(ctx, tx, audit)
	})
	if txErr != nil {
		return nil, s.asBizErr(ctx, "admin_kick_user", txErr)
	}

	if len(revokedJTIs) > 0 {
		if err := s.repo.BlacklistAccessJTIs(ctx, revokedJTIs, "account.kicked", s.accessTTL); err != nil {
			s.log.WarnContext(ctx, "admin_kick_blacklist_jti_failed", slog.Uint64("user_id", targetUserID), slog.Any("error", err))
		}
	}

	targetSessions := revokedSessionIDs
	if sessionID != nil && len(targetSessions) == 0 {
		targetSessions = []uint64{*sessionID}
	}
	kickedConnCount, mergedSessions, countErr := s.repo.CountLiveConnectionsForSessions(ctx, targetUserID, targetSessions, sessionID == nil)
	if countErr != nil {
		s.log.WarnContext(ctx, "admin_kick_count_conns_failed", slog.Uint64("user_id", targetUserID), slog.Any("error", countErr))
		mergedSessions = targetSessions
	}
	if len(mergedSessions) > 0 && s.revoker != nil {
		if err := s.revoker.RevokeSessions(ctx, targetUserID, mergedSessions, "account.kicked"); err != nil {
			s.log.WarnContext(ctx, "admin_kick_revoke_ws_failed", slog.Uint64("user_id", targetUserID), slog.Any("error", err))
		}
	}

	return &AdminKickUserResult{
		UserID:                targetUserID,
		RevokedSessionCount:   revokedCount,
		KickedConnectionCount: kickedConnCount,
	}, nil
}

// ListGroups implements GET /api/v1/admin/groups.
func (s *AdminService) ListGroups(ctx context.Context, input ListAdminGroupsInput) (*AdminGroupPage, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	limit := normalizeAdminLimit(input.Limit)
	q := strings.TrimSpace(input.Query)
	status := strings.TrimSpace(input.Status)
	if status != "" && status != model.GroupStatusActive && status != model.GroupStatusDisbanded {
		return nil, errcode.ErrInvalidValue.WithMessage("status must be active or disbanded")
	}

	var cursorID uint64
	if strings.TrimSpace(input.Cursor) != "" {
		var cur adminGroupCursor
		if err := decodeScopedCursor(input.Cursor, &cur); err != nil ||
			cur.Version != 1 || cur.Scope != adminGroupCursorScope ||
			cur.Query != q || cur.Status != status || cur.LastID == 0 {
			return nil, errcode.ErrInvalidValue.WithMessage("invalid cursor")
		}
		cursorID = cur.LastID
	}

	rows, hasMore, err := s.repo.ListGroups(ctx, repository.AdminGroupListFilter{
		Query:    q,
		Status:   status,
		CursorID: cursorID,
		Limit:    limit,
	})
	if err != nil {
		return nil, s.classifyRepoErr(ctx, "admin_list_groups", err)
	}

	items := make([]AdminGroupSummaryDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toAdminGroupSummaryDTO(row))
	}

	var nextCursor *string
	if hasMore && len(rows) > 0 {
		encoded, encErr := encodeCursor(adminGroupCursor{
			Version: 1,
			Scope:   adminGroupCursorScope,
			Query:   q,
			Status:  status,
			LastID:  rows[len(rows)-1].Group.ID,
		})
		if encErr != nil {
			return nil, errcode.ErrInternalConsistency
		}
		nextCursor = &encoded
	}

	return &AdminGroupPage{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// GetGroupDetail implements GET /api/v1/admin/groups/{group_id}.
func (s *AdminService) GetGroupDetail(ctx context.Context, groupID uint64) (*AdminGroupDetailDTO, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}
	summary, members, usersByID, audits, err := s.repo.GetGroupDetail(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrAdminTargetNotFound
		}
		return nil, s.classifyRepoErr(ctx, "admin_get_group_detail", err)
	}

	memberUserIDs := make([]uint64, 0, len(members))
	for _, m := range members {
		memberUserIDs = append(memberUserIDs, m.UserID)
	}
	onlineMap, err := s.repo.GetUsersOnline(ctx, memberUserIDs)
	if err != nil {
		return nil, errcode.ErrRedisUnavailable
	}

	memberDTOs := make([]GroupMemberDTO, 0, len(members))
	for _, m := range members {
		u := usersByID[m.UserID]
		memberDTOs = append(memberDTOs, toGroupMemberDTO(m, u, onlineMap[m.UserID]))
	}
	auditDTOs := make([]GroupAuditLogDTO, 0, len(audits))
	for _, a := range audits {
		auditDTOs = append(auditDTOs, toGroupAuditLogDTO(a))
	}

	return &AdminGroupDetailDTO{
		Group:        toAdminGroupSummaryDTO(*summary),
		Members:      memberDTOs,
		SystemEvents: auditDTOs,
	}, nil
}

// ForceDisbandGroup implements DELETE /api/v1/admin/groups/{group_id}.
func (s *AdminService) ForceDisbandGroup(
	ctx context.Context,
	adminID uint64,
	groupID uint64,
	reason string,
	meta GroupAuditMeta,
) (*AdminDisbandGroupResult, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}
	trimmedReason := truncateRunes(strings.TrimSpace(reason), maxAdminReasonRunes)

	var (
		convID     uint64
		lockToken  string
		seqRes     *repository.SeqReservation
		sysMsg     *model.Message
		notifyIDs  []uint64
		auditEntry *model.AuditLog
	)

	summary, _, _, _, preErr := s.repo.GetGroupDetail(ctx, groupID)
	if preErr != nil {
		if errors.Is(preErr, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrAdminTargetNotFound
		}
		return nil, s.classifyRepoErr(ctx, "admin_disband_preread", preErr)
	}
	convID = summary.ConversationID

	if convID > 0 && s.seqAllocator != nil {
		token, lockErr := s.seqAllocator.AcquireConvLock(ctx, convID)
		if lockErr != nil {
			return nil, errcode.ErrRedisUnavailable
		}
		lockToken = token
		defer func() {
			relCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.seqAllocator.ReleaseConvLock(relCtx, convID, lockToken)
		}()
	}

	now := s.nowFn().Truncate(time.Millisecond)
	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		group, conv, members, lockErr := s.repo.LockGroupAndConversation(ctx, tx, groupID)
		if lockErr != nil {
			if errors.Is(lockErr, gorm.ErrRecordNotFound) {
				return errcode.ErrAdminTargetNotFound
			}
			return lockErr
		}
		if group.Status == model.GroupStatusDisbanded {
			return errcode.ErrAdminStateConflict
		}
		if err := s.repo.MarkGroupDisbanded(ctx, tx, groupID, now); err != nil {
			return err
		}

		for _, m := range members {
			if m.UserID > 0 {
				notifyIDs = append(notifyIDs, m.UserID)
			}
		}

		var convPtr *uint64
		if conv != nil {
			convID = conv.ID
			cCopy := conv.ID
			convPtr = &cCopy
			if s.seqAllocator != nil && s.idGen != nil && lockToken != "" {
				allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
				if seqErr != nil {
					return seqErr
				}
				seqRes = allocated
				msgID, genErr := s.idGen.NextID()
				if genErr != nil {
					return errcode.ErrInternalConsistency
				}
				content := "管理员已强制解散该群聊"
				if trimmedReason != "" {
					content = fmt.Sprintf("管理员已强制解散该群聊（原因：%s）", trimmedReason)
				}
				sysMsg = &model.Message{
					ID:             msgID,
					ConversationID: conv.ID,
					Seq:            seqRes.Seq,
					SenderID:       0,
					ClientMsgID:    fmt.Sprintf("sys:admin_group_disband:%d", msgID),
					MsgType:        model.MessageTypeSystem,
					Content:        content,
					Extra: marshalExtraJSON(map[string]any{
						"event":    "group.disbanded",
						"group_id": strconv.FormatUint(groupID, 10),
						"actor_id": strconv.FormatUint(adminID, 10),
						"reason":   trimmedReason,
					}),
					Status:    model.MessageStatusNormal,
					CreatedAt: now,
				}
				if err := s.repo.CreateSystemMessageAndUpdateConv(ctx, tx, sysMsg, conv.ID, notifyIDs, now); err != nil {
					return err
				}
			}
		}

		gidCopy := groupID
		auditEntry = buildAdminAuditEntry(
			"group.disband",
			adminID,
			model.AuditTargetGroup,
			groupID,
			&gidCopy,
			convPtr,
			trimmedReason,
			map[string]any{
				"member_count": len(members),
				"group_name":   group.Name,
			},
			meta,
			now,
		)
		return s.repo.AppendAuditLog(ctx, tx, auditEntry)
	})
	if txErr != nil {
		if seqRes != nil && seqRes.Rollback != nil {
			rbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = seqRes.Rollback(rbCtx)
			cancel()
		}
		return nil, s.asBizErr(ctx, "admin_disband_group", txErr)
	}

	if s.notifier != nil && len(notifyIDs) > 0 {
		if sysMsg != nil {
			_ = s.notifier.DeliverToUsers(ctx, notifyIDs, "msg.push", map[string]any{
				"push_type":       "message",
				"message":         toGroupMessageDTO(*sysMsg),
				"delivery_status": model.ReceiptStatusSent,
			})
		}
		var convStr *string
		if convID > 0 {
			v := strconv.FormatUint(convID, 10)
			convStr = &v
		}
		actorStr := strconv.FormatUint(adminID, 10)
		_ = s.notifier.DeliverToUsers(ctx, notifyIDs, "system.notice", map[string]any{
			"event":           "group.disbanded",
			"notice_id":       "n_" + uuid.NewString(),
			"conversation_id": convStr,
			"actor_id":        &actorStr,
			"target_user_id":  nil,
			"payload": map[string]any{
				"group_id": strconv.FormatUint(groupID, 10),
				"reason":   trimmedReason,
			},
		})
	}

	return &AdminDisbandGroupResult{
		GroupID:        groupID,
		ConversationID: convID,
		Status:         model.GroupStatusDisbanded,
		AuditLog:       toGroupAuditLogDTO(*auditEntry),
	}, nil
}

// ListMessageAudit implements GET /api/v1/admin/messages/audit.
func (s *AdminService) ListMessageAudit(ctx context.Context, input ListAdminMessageAuditInput) (*AdminMessageAuditPage, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	limit := normalizeAdminLimit(input.Limit)
	if input.ConversationID != nil && *input.ConversationID == 0 {
		return nil, errcode.ErrInvalidAuditFilter.WithMessage("invalid conversation_id")
	}
	if input.SenderID != nil && *input.SenderID == 0 {
		return nil, errcode.ErrInvalidAuditFilter.WithMessage("invalid sender_id")
	}
	kw := strings.TrimSpace(input.Keyword)
	if utf8.RuneCountInString(kw) > maxAdminAuditKeywordLen {
		return nil, errcode.ErrInvalidAuditFilter.WithMessage("keyword too long")
	}
	if input.From != nil && input.To != nil {
		if input.From.After(*input.To) {
			return nil, errcode.ErrInvalidAuditFilter.WithMessage("from must not be after to")
		}
		if input.To.Sub(*input.From) > maxAdminAuditRange {
			return nil, errcode.ErrInvalidAuditFilter.WithMessage("time range exceeds 90 days")
		}
	}

	cidTag := ""
	if input.ConversationID != nil {
		cidTag = strconv.FormatUint(*input.ConversationID, 10)
	}
	sidTag := ""
	if input.SenderID != nil {
		sidTag = strconv.FormatUint(*input.SenderID, 10)
	}
	fromTag := ""
	if input.From != nil {
		fromTag = FormatUTCMillis(*input.From)
	}
	toTag := ""
	if input.To != nil {
		toTag = FormatUTCMillis(*input.To)
	}

	var cursorID uint64
	if strings.TrimSpace(input.Cursor) != "" {
		var cur adminAuditMsgCursor
		if err := decodeScopedCursor(input.Cursor, &cur); err != nil ||
			cur.Version != 1 || cur.Scope != adminMessageAuditCursorScope ||
			cur.ConversationID != cidTag || cur.SenderID != sidTag ||
			cur.Keyword != kw || cur.From != fromTag || cur.To != toTag || cur.LastID == 0 {
			return nil, errcode.ErrInvalidAuditFilter.WithMessage("invalid cursor")
		}
		cursorID = cur.LastID
	}

	msgs, senders, hasMore, err := s.repo.ListMessageAudit(ctx, repository.AdminMessageAuditFilter{
		ConversationID: input.ConversationID,
		SenderID:       input.SenderID,
		Keyword:        kw,
		From:           input.From,
		To:             input.To,
		CursorID:       cursorID,
		Limit:          limit,
	})
	if err != nil {
		return nil, s.classifyRepoErr(ctx, "admin_list_message_audit", err)
	}

	items := make([]AdminMessageAuditItemDTO, 0, len(msgs))
	for _, m := range msgs {
		sender := senders[m.SenderID]
		items = append(items, AdminMessageAuditItemDTO{
			MsgID:          m.ID,
			ClientMsgID:    m.ClientMsgID,
			ConversationID: m.ConversationID,
			Seq:            m.Seq,
			SenderID:       m.SenderID,
			SenderUsername: sender.Username,
			SenderNickname: sender.Nickname,
			MsgType:        m.MsgType,
			Content:        m.Content,
			Extra:          m.Extra,
			Status:         m.Status,
			CreatedAt:      FormatUTCMillis(m.CreatedAt),
			RecalledAt:     FormatUTCMillisPtr(m.RecalledAt),
		})
	}

	var nextCursor *string
	if hasMore && len(msgs) > 0 {
		encoded, encErr := encodeCursor(adminAuditMsgCursor{
			Version:        1,
			Scope:          adminMessageAuditCursorScope,
			ConversationID: cidTag,
			SenderID:       sidTag,
			Keyword:        kw,
			From:           fromTag,
			To:             toTag,
			LastID:         msgs[len(msgs)-1].ID,
		})
		if encErr != nil {
			return nil, errcode.ErrInternalConsistency
		}
		nextCursor = &encoded
	}

	return &AdminMessageAuditPage{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// GetMetricsOverview implements GET /api/v1/admin/metrics/overview.
func (s *AdminService) GetMetricsOverview(ctx context.Context) (*AdminMetricsOverviewDTO, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	now := s.nowFn()
	onlineUsers, wsConns, err := s.repo.CountLivePresence(ctx)
	if err != nil {
		return nil, errcode.ErrMetricsUnavailable.WithMessage("redis presence metrics unavailable")
	}
	msgsPerMin, err := s.repo.CountMessagesSince(ctx, now.Add(-time.Minute))
	if err != nil {
		return nil, errcode.ErrMetricsUnavailable.WithMessage("mysql message metrics unavailable")
	}
	streams, err := s.repo.CollectStreamsOverview(ctx)
	if err != nil {
		return nil, errcode.ErrMetricsUnavailable.WithMessage("redis stream metrics unavailable")
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return &AdminMetricsOverviewDTO{
		OnlineUsers:       onlineUsers,
		WSConnections:     wsConns,
		MessagesPerMinute: msgsPerMin,
		Streams:           streams,
		Goroutines:        runtime.NumGoroutine(),
		HeapBytes:         mem.HeapAlloc,
		Timestamp:         FormatUTCMillis(now),
	}, nil
}

// GetMetricsStreams implements GET /api/v1/admin/metrics/streams.
func (s *AdminService) GetMetricsStreams(ctx context.Context) (*AdminStreamMetricsPage, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	rows, err := s.repo.CollectStreamConsumers(ctx)
	if err != nil {
		return nil, errcode.ErrMetricsUnavailable.WithMessage("redis stream metrics unavailable")
	}
	return &AdminStreamMetricsPage{Items: rows}, nil
}

// GetDependenciesHealth implements GET /api/v1/admin/health/dependencies.
func (s *AdminService) GetDependenciesHealth(ctx context.Context) (*AdminDependenciesHealthDTO, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	now := s.nowFn()

	var (
		mysqlVer, redisVer string
		mysqlLat, redisLat int64
		mysqlErr, redisErr error
		wg                 sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		mysqlVer, mysqlLat, mysqlErr = s.repo.ProbeMySQL(ctx)
	}()
	go func() {
		defer wg.Done()
		redisVer, redisLat, redisErr = s.repo.ProbeRedis(ctx)
	}()
	wg.Wait()

	s.errMu.Lock()
	mysqlItem := buildDependencyHealthItem(mysqlVer, mysqlLat, mysqlErr, now, &s.mysqlLastErr)
	redisItem := buildDependencyHealthItem(redisVer, redisLat, redisErr, now, &s.redisLastErr)
	s.errMu.Unlock()

	dto := &AdminDependenciesHealthDTO{
		MySQL:     mysqlItem,
		Redis:     redisItem,
		Timestamp: FormatUTCMillis(now),
	}
	if mysqlErr != nil || redisErr != nil {
		return dto, errcode.ErrMetricsUnavailable
	}
	return dto, nil
}

func buildDependencyHealthItem(
	version string,
	latencyMs int64,
	err error,
	now time.Time,
	last *depLastError,
) DependencyHealthItemDTO {
	if err != nil {
		nowCopy := now
		errMsg := err.Error()
		last.at = &nowCopy
		last.msg = &errMsg
		return DependencyHealthItemDTO{
			Status:      HealthStateUnavailable,
			LatencyMs:   nil,
			Version:     "",
			LastErrorAt: FormatUTCMillisPtr(last.at),
			LastError:   last.msg,
		}
	}
	lat := latencyMs
	return DependencyHealthItemDTO{
		Status:      HealthStateOK,
		LatencyMs:   &lat,
		Version:     version,
		LastErrorAt: FormatUTCMillisPtr(last.at),
		LastError:   last.msg,
	}
}

func (s *AdminService) classifyRepoErr(ctx context.Context, op string, err error) *errcode.Error {
	if repository.IsDBUnavailableError(err) {
		s.log.ErrorContext(ctx, op+"_mysql_unavailable", slog.Any("error", err))
		return errcode.ErrMySQLUnavailable
	}
	s.log.ErrorContext(ctx, op+"_failed", slog.Any("error", err))
	return errcode.ErrInternalConsistency
}

func (s *AdminService) asBizErr(ctx context.Context, op string, err error) *errcode.Error {
	var bizErr *errcode.Error
	if errors.As(err, &bizErr) {
		return bizErr
	}
	return s.classifyRepoErr(ctx, op, err)
}

func normalizeAdminLimit(limit int) int {
	if limit <= 0 {
		return defaultAdminListLimit
	}
	if limit > maxAdminListLimit {
		return maxAdminListLimit
	}
	return limit
}

func toAdminUserSummaryDTO(u model.User, online bool, sessionCount int) AdminUserSummaryDTO {
	return AdminUserSummaryDTO{
		ID:           u.ID,
		Username:     u.Username,
		Nickname:     u.Nickname,
		AvatarURL:    u.AvatarURL,
		Signature:    u.Signature,
		Role:         u.Role,
		Status:       u.Status,
		Online:       online,
		LastOnlineAt: FormatUTCMillisPtr(u.LastOnlineAt),
		SessionCount: sessionCount,
		CreatedAt:    FormatUTCMillis(u.CreatedAt),
		UpdatedAt:    FormatUTCMillis(u.UpdatedAt),
	}
}

func toAdminGroupSummaryDTO(row repository.AdminGroupSummaryRow) AdminGroupSummaryDTO {
	var ownerDTO *AdminGroupOwnerDTO
	if row.Owner != nil {
		ownerDTO = &AdminGroupOwnerDTO{
			ID:        row.Owner.ID,
			Username:  row.Owner.Username,
			Nickname:  row.Owner.Nickname,
			AvatarURL: row.Owner.AvatarURL,
		}
	}
	return AdminGroupSummaryDTO{
		ID:             row.Group.ID,
		ConversationID: row.ConversationID,
		Name:           row.Group.Name,
		AvatarURL:      row.Group.AvatarURL,
		Announcement:   row.Group.Announcement,
		OwnerID:        row.Group.OwnerID,
		Owner:          ownerDTO,
		Status:         row.Group.Status,
		MemberCount:    row.MemberCount,
		MaxMembers:     row.Group.MaxMembers,
		LastSeq:        row.LastSeq,
		CreatedAt:      FormatUTCMillis(row.Group.CreatedAt),
		UpdatedAt:      FormatUTCMillis(row.Group.UpdatedAt),
	}
}

func buildAdminAuditEntry(
	action string,
	actorID uint64,
	targetType string,
	targetID uint64,
	groupID *uint64,
	convID *uint64,
	reason string,
	detail map[string]any,
	meta GroupAuditMeta,
	now time.Time,
) *model.AuditLog {
	var detailJSON model.NullableJSON
	if len(detail) > 0 {
		if raw, err := json.Marshal(detail); err == nil {
			detailJSON = model.NullableJSON(raw)
		}
	}
	return &model.AuditLog{
		EventID:        "evt-" + uuid.NewString(),
		Scope:          model.AuditScopeAdmin,
		Action:         action,
		ActorID:        actorID,
		ActorRole:      model.UserRoleAdmin,
		TargetType:     targetType,
		TargetID:       targetID,
		GroupID:        groupID,
		ConversationID: convID,
		Reason:         truncateRunes(reason, maxAdminReasonRunes),
		Detail:         detailJSON,
		RequestID:      truncateRunes(meta.RequestID, 64),
		IPAddress:      truncateRunes(meta.IPAddress, 64),
		UserAgent:      truncateRunes(meta.UserAgent, 512),
		CreatedAt:      now,
	}
}
