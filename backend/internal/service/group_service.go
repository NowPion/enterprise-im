package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
)

const (
	DefaultGroupMaxMembers  uint32 = 500
	DefaultGroupListLimit          = 20
	MaxGroupListLimit              = 100
	MaxGroupNameRunes              = 100
	MaxGroupAnnounceRunes          = 2000
	MaxGroupNicknameRunes          = 64
	MaxAssetIDLength               = 36
	memberOnlineScanChunk          = 200

	groupListCursorScope   = "groups"
	groupMemberCursorScope = "group_members"
	groupAuditCursorScope  = "group_audit"
)

// GroupRealtimeNotifier delivers post-commit group system messages and system.notice events.
type GroupRealtimeNotifier interface {
	DeliverToUsers(ctx context.Context, targetUserIDs []uint64, frameType string, data any) error
}

// GroupAuditMeta carries HTTP request context recorded in immutable group audit logs.
type GroupAuditMeta struct {
	RequestID string
	IPAddress string
	UserAgent string
}

// GroupDTO matches the frozen Group object in docs/api.md.
type GroupDTO struct {
	ID             uint64  `json:"id,string"`
	ConversationID uint64  `json:"conversation_id,string"`
	Name           string  `json:"name"`
	AvatarURL      string  `json:"avatar_url"`
	Announcement   *string `json:"announcement"`
	OwnerID        uint64  `json:"owner_id,string"`
	Status         string  `json:"status"`
	MemberCount    int     `json:"member_count"`
	MaxMembers     uint32  `json:"max_members"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

// GroupSummaryDTO represents a Group summary item in GET /api/v1/groups.
type GroupSummaryDTO struct {
	GroupDTO
	Role string `json:"role"`
}

// GroupMemberDTO matches the frozen GroupMember object in docs/api.md.
type GroupMemberDTO struct {
	UserID          uint64 `json:"user_id,string"`
	Nickname        string `json:"nickname"`
	AvatarURL       string `json:"avatar_url"`
	NicknameInGroup string `json:"nickname_in_group"`
	Role            string `json:"role"`
	Online          bool   `json:"online"`
	JoinedAt        string `json:"joined_at"`
}

// GroupConversationDTO matches the frozen Conversation object for group chats in docs/api.md.
type GroupConversationDTO struct {
	ID               uint64  `json:"id,string"`
	Type             string  `json:"type"`
	Peer             any     `json:"peer"`
	GroupID          *string `json:"group_id"`
	Title            string  `json:"title"`
	AvatarURL        string  `json:"avatar_url"`
	LastMsgID        uint64  `json:"last_msg_id,string"`
	LastSeq          uint64  `json:"last_seq,string"`
	LastMsgPreview   string  `json:"last_msg_preview"`
	LastMsgAt        *string `json:"last_msg_at"`
	LastDeliveredSeq uint64  `json:"last_delivered_seq,string"`
	LastReadSeq      uint64  `json:"last_read_seq,string"`
	UnreadCount      uint32  `json:"unread_count"`
	IsPinned         bool    `json:"is_pinned"`
	IsMuted          bool    `json:"is_muted"`
	IsVisible        bool    `json:"is_visible"`
	CanSend          bool    `json:"can_send"`
}

// GroupMessageDTO matches the frozen Message object in docs/api.md.
type GroupMessageDTO struct {
	MsgID          uint64             `json:"msg_id,string"`
	ClientMsgID    string             `json:"client_msg_id"`
	ConversationID uint64             `json:"conversation_id,string"`
	Seq            uint64             `json:"seq,string"`
	SenderID       uint64             `json:"sender_id,string"`
	MsgType        string             `json:"msg_type"`
	Content        string             `json:"content"`
	Extra          model.NullableJSON `json:"extra"`
	Status         string             `json:"status"`
	CreatedAt      string             `json:"created_at"`
	RecalledAt     *string            `json:"recalled_at"`
}

// GroupConversationMemberDTO represents a synchronized conversation_members entry returned on member add.
type GroupConversationMemberDTO struct {
	ConversationID   uint64 `json:"conversation_id,string"`
	UserID           uint64 `json:"user_id,string"`
	LastDeliveredSeq uint64 `json:"last_delivered_seq,string"`
	LastReadSeq      uint64 `json:"last_read_seq,string"`
	UnreadCount      uint32 `json:"unread_count"`
	IsVisible        bool   `json:"is_visible"`
	JoinedAt         string `json:"joined_at"`
}

// GroupAuditLogDTO matches an immutable group audit entry returned by GET /api/v1/groups/{group_id}/audit.
type GroupAuditLogDTO struct {
	ID             uint64             `json:"id,string"`
	EventID        string             `json:"event_id"`
	Scope          string             `json:"scope"`
	Action         string             `json:"action"`
	ActorID        uint64             `json:"actor_id,string"`
	ActorRole      string             `json:"actor_role"`
	TargetType     string             `json:"target_type"`
	TargetID       uint64             `json:"target_id,string"`
	GroupID        *string            `json:"group_id"`
	ConversationID *string            `json:"conversation_id"`
	Reason         string             `json:"reason"`
	Detail         model.NullableJSON `json:"detail"`
	RequestID      string             `json:"request_id"`
	CreatedAt      string             `json:"created_at"`
}

// CreateGroupInput holds validated parameters for POST /api/v1/groups.
type CreateGroupInput struct {
	Name          string
	MemberIDs     []uint64
	AvatarAssetID *string
	Announcement  *string
	Meta          GroupAuditMeta
}

// CreateGroupResult matches POST /api/v1/groups data.
type CreateGroupResult struct {
	Group         GroupDTO             `json:"group"`
	Conversation  GroupConversationDTO `json:"conversation"`
	Members       []GroupMemberDTO     `json:"members"`
	SystemMessage GroupMessageDTO      `json:"system_message"`
}

// GroupListPage matches GET /api/v1/groups data.
type GroupListPage struct {
	Items      []GroupSummaryDTO `json:"items"`
	NextCursor *string           `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
}

// GetGroupResult matches GET /api/v1/groups/{group_id} data.
type GetGroupResult struct {
	Group        GroupDTO             `json:"group"`
	Conversation GroupConversationDTO `json:"conversation"`
	Role         string               `json:"role"`
	MemberCount  int                  `json:"member_count"`
}

// UpdateGroupInput holds validated parameters for PATCH /api/v1/groups/{group_id}.
type UpdateGroupInput struct {
	Name            *string
	HasAvatarAsset  bool
	AvatarAssetID   *string
	HasAnnouncement bool
	Announcement    *string
	Meta            GroupAuditMeta
}

// UpdateGroupResult matches PATCH /api/v1/groups/{group_id} data.
type UpdateGroupResult struct {
	Group         GroupDTO        `json:"group"`
	SystemMessage GroupMessageDTO `json:"system_message"`
}

// DisbandGroupResult matches DELETE /api/v1/groups/{group_id} data.
type DisbandGroupResult struct {
	GroupID        uint64          `json:"group_id,string"`
	ConversationID uint64          `json:"conversation_id,string"`
	Status         string          `json:"status"`
	SystemMessage  GroupMessageDTO `json:"system_message"`
}

// ListGroupMembersInput defines query filters for GET /api/v1/groups/{group_id}/members.
type ListGroupMembersInput struct {
	Role       string
	OnlineOnly bool
	Cursor     string
	Limit      int
}

// GroupMemberPage matches GET /api/v1/groups/{group_id}/members data.
type GroupMemberPage struct {
	Items      []GroupMemberDTO `json:"items"`
	NextCursor *string          `json:"next_cursor"`
	HasMore    bool             `json:"has_more"`
}

// AddGroupMembersResult matches POST /api/v1/groups/{group_id}/members data.
type AddGroupMembersResult struct {
	GroupID             uint64                       `json:"group_id,string"`
	ConversationID      uint64                       `json:"conversation_id,string"`
	Members             []GroupMemberDTO             `json:"members"`
	ConversationMembers []GroupConversationMemberDTO `json:"conversation_members"`
	SystemMessage       GroupMessageDTO              `json:"system_message"`
}

// RemoveGroupMemberResult matches DELETE /api/v1/groups/{group_id}/members/{user_id} data.
type RemoveGroupMemberResult struct {
	GroupID        uint64          `json:"group_id,string"`
	ConversationID uint64          `json:"conversation_id,string"`
	UserID         uint64          `json:"user_id,string"`
	RemovedAt      string          `json:"removed_at"`
	SystemMessage  GroupMessageDTO `json:"system_message"`
}

// LeaveGroupResult matches POST /api/v1/groups/{group_id}/leave data.
type LeaveGroupResult struct {
	GroupID        uint64          `json:"group_id,string"`
	ConversationID uint64          `json:"conversation_id,string"`
	UserID         uint64          `json:"user_id,string"`
	LeftAt         string          `json:"left_at"`
	SystemMessage  GroupMessageDTO `json:"system_message"`
}

// TransferGroupResult matches POST /api/v1/groups/{group_id}/transfer data.
type TransferGroupResult struct {
	GroupID         uint64          `json:"group_id,string"`
	OwnerID         uint64          `json:"owner_id,string"`
	PreviousOwnerID uint64          `json:"previous_owner_id,string"`
	SystemMessage   GroupMessageDTO `json:"system_message"`
}

// UpdateMemberRoleResult matches PATCH /api/v1/groups/{group_id}/members/{user_id}/role data.
type UpdateMemberRoleResult struct {
	GroupID       uint64          `json:"group_id,string"`
	UserID        uint64          `json:"user_id,string"`
	Role          string          `json:"role"`
	Member        GroupMemberDTO  `json:"member"`
	SystemMessage GroupMessageDTO `json:"system_message"`
}

// UpdateMyMemberResult matches PATCH /api/v1/groups/{group_id}/members/me data.
type UpdateMyMemberResult struct {
	Member GroupMemberDTO `json:"member"`
}

// ListGroupAuditInput defines query filters for GET /api/v1/groups/{group_id}/audit.
type ListGroupAuditInput struct {
	From   *time.Time
	To     *time.Time
	Cursor string
	Limit  int
}

// GroupAuditPage matches GET /api/v1/groups/{group_id}/audit data.
type GroupAuditPage struct {
	Items      []GroupAuditLogDTO `json:"items"`
	NextCursor *string            `json:"next_cursor"`
	HasMore    bool               `json:"has_more"`
}

// GroupService implements Stage 6 group lifecycle, membership, role permissions,
// asset reference validation, system messages, audit logs, and Hub notifications.
type GroupService struct {
	repo         repository.GroupRepository
	seqAllocator repository.SeqAllocator
	presence     repository.PresenceChecker
	idGen        *snowflake.Generator
	notifier     GroupRealtimeNotifier
	log          *slog.Logger
}

// NewGroupService constructs a GroupService.
func NewGroupService(
	repo repository.GroupRepository,
	seqAllocator repository.SeqAllocator,
	presence repository.PresenceChecker,
	idGen *snowflake.Generator,
	notifier GroupRealtimeNotifier,
	log *slog.Logger,
) *GroupService {
	if presence == nil {
		presence = repository.NewRedisPresenceChecker(nil)
	}
	if log == nil {
		log = slog.Default()
	}
	return &GroupService{
		repo:         repo,
		seqAllocator: seqAllocator,
		presence:     presence,
		idGen:        idGen,
		notifier:     notifier,
		log:          log,
	}
}

// Create creates a new group, its unique group conversation, member rows,
// initial system message, and audit log inside one atomic transaction.
func (s *GroupService) Create(ctx context.Context, creatorID uint64, input CreateGroupInput) (*CreateGroupResult, *errcode.Error) {
	if err := s.requireWriteDeps(); err != nil {
		return nil, err
	}
	if creatorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}

	name, bizErr := validateGroupName(input.Name)
	if bizErr != nil {
		return nil, bizErr
	}
	announcement, bizErr := validateGroupAnnouncement(input.Announcement)
	if bizErr != nil {
		return nil, bizErr
	}
	assetID, bizErr := normalizeOptionalAssetID(input.AvatarAssetID)
	if bizErr != nil {
		return nil, bizErr
	}

	otherMemberIDs := dedupeExcludingSelf(input.MemberIDs, creatorID)
	if len(otherMemberIDs) == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("member_ids must contain at least one user other than creator")
	}
	sort.Slice(otherMemberIDs, func(i, j int) bool { return otherMemberIDs[i] < otherMemberIDs[j] })
	if uint32(1+len(otherMemberIDs)) > DefaultGroupMaxMembers {
		return nil, errcode.ErrGroupFull
	}

	allUserIDs := append([]uint64{creatorID}, otherMemberIDs...)

	var (
		createdGroup  model.Group
		createdConv   model.Conversation
		memberModels  []model.GroupMember
		sysMsg        model.Message
		usersByID     map[uint64]model.User
		lockedConvID  uint64
		lockToken     string
		seqRes        *repository.SeqReservation
		creatorCM     model.ConversationMember
		now           time.Time
	)

	releaseLock := func() {
		if lockedConvID > 0 && lockToken != "" {
			rbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if relErr := s.seqAllocator.ReleaseConvLock(rbCtx, lockedConvID, lockToken); relErr != nil {
				s.log.WarnContext(ctx, "group_create_release_conv_lock_failed", slog.Uint64("conversation_id", lockedConvID), slog.Any("error", relErr))
			}
			lockedConvID = 0
			lockToken = ""
		}
	}
	defer releaseLock()

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		users, lockErr := s.repo.LockUsersAscending(ctx, tx, allUserIDs)
		if lockErr != nil {
			return s.mapDBError(ctx, "group_create_lock_users", lockErr)
		}
		usersByID = users

		creator, hasCreator := users[creatorID]
		if !hasCreator {
			return errcode.ErrUserNotFound
		}
		if creator.Status != model.UserStatusActive {
			return errcode.ErrAccountDisabled
		}
		for _, uid := range otherMemberIDs {
			u, ok := users[uid]
			if !ok {
				return errcode.ErrUserNotFound
			}
			if u.Status != model.UserStatusActive {
				return errcode.ErrAccountDisabled
			}
		}

		avatarURL := ""
		if assetID != "" {
			url, valErr := s.validateGroupAvatarAssetTx(ctx, tx, creatorID, 0, assetID)
			if valErr != nil {
				return valErr
			}
			avatarURL = url
		}

		now = time.Now().UTC().Truncate(time.Millisecond)
		createdGroup = model.Group{
			Name:         name,
			AvatarURL:    avatarURL,
			OwnerID:      creatorID,
			Announcement: announcement,
			MaxMembers:   DefaultGroupMaxMembers,
			Status:       model.GroupStatusActive,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if createErr := s.repo.CreateGroup(ctx, tx, &createdGroup); createErr != nil {
			return s.mapDBError(ctx, "group_create_insert_group", createErr)
		}

		if assetID != "" {
			if bindErr := s.repo.BindGroupAvatarAsset(ctx, tx, assetID, createdGroup.ID, now); bindErr != nil {
				return s.mapDBError(ctx, "group_create_bind_avatar", bindErr)
			}
		}

		memberModels = make([]model.GroupMember, 0, len(allUserIDs))
		memberModels = append(memberModels, model.GroupMember{
			GroupID:   createdGroup.ID,
			UserID:    creatorID,
			Role:      model.GroupRoleOwner,
			JoinedAt:  now,
			UpdatedAt: now,
		})
		for _, uid := range otherMemberIDs {
			memberModels = append(memberModels, model.GroupMember{
				GroupID:   createdGroup.ID,
				UserID:    uid,
				Role:      model.GroupRoleMember,
				JoinedAt:  now,
				UpdatedAt: now,
			})
		}
		if gmErr := s.repo.CreateGroupMembers(ctx, tx, memberModels); gmErr != nil {
			return s.mapDBError(ctx, "group_create_insert_members", gmErr)
		}

		conv, convErr := s.repo.CreateGroupConversation(ctx, tx, createdGroup.ID, now)
		if convErr != nil {
			return s.mapDBError(ctx, "group_create_insert_conversation", convErr)
		}

		tok, acqErr := s.seqAllocator.AcquireConvLock(ctx, conv.ID)
		if acqErr != nil {
			return s.mapSeqError(ctx, "group_create_acquire_conv_lock", acqErr)
		}
		lockedConvID = conv.ID
		lockToken = tok

		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
		if seqErr != nil {
			return s.mapSeqError(ctx, "group_create_allocate_seq", seqErr)
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			s.log.ErrorContext(ctx, "group_create_snowflake_failed", slog.Any("error", genErr))
			return errcode.ErrInternalConsistency
		}

		content := fmt.Sprintf("%s 创建了群聊", displayActorName(creator))
		extra := marshalExtraJSON(map[string]any{
			"event":      "group.created",
			"group_id":   strconv.FormatUint(createdGroup.ID, 10),
			"actor_id":   strconv.FormatUint(creatorID, 10),
			"member_ids": formatUint64Slice(allUserIDs),
		})
		sysMsg = model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:group_create:%d", msgID),
			MsgType:        model.MessageTypeSystem,
			Content:        content,
			Extra:          extra,
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if msgErr := s.repo.CreateSystemMessage(ctx, tx, &sysMsg); msgErr != nil {
			return s.mapDBError(ctx, "group_create_system_message", msgErr)
		}
		if lastErr := s.repo.UpdateConversationLastMessage(ctx, tx, conv.ID, &sysMsg); lastErr != nil {
			return s.mapDBError(ctx, "group_create_update_conversation", lastErr)
		}

		conv.LastMsgID = sysMsg.ID
		conv.LastSeq = sysMsg.Seq
		conv.LastMsgPreview = truncatePreview(sysMsg.Content, 255)
		conv.LastMsgAt = &now
		createdConv = *conv

		cmRows := make([]model.ConversationMember, 0, len(allUserIDs))
		creatorCM = model.ConversationMember{
			ConversationID:   conv.ID,
			UserID:           creatorID,
			LastDeliveredSeq: sysMsg.Seq,
			LastReadSeq:      sysMsg.Seq,
			UnreadCount:      0,
			IsVisible:        true,
			JoinedAt:         now,
			UpdatedAt:        now,
		}
		cmRows = append(cmRows, creatorCM)
		for _, uid := range otherMemberIDs {
			cmRows = append(cmRows, model.ConversationMember{
				ConversationID:   conv.ID,
				UserID:           uid,
				LastDeliveredSeq: 0,
				LastReadSeq:      0,
				UnreadCount:      1,
				IsVisible:        true,
				JoinedAt:         now,
				UpdatedAt:        now,
			})
		}
		if cmErr := s.repo.CreateConversationMembers(ctx, tx, cmRows); cmErr != nil {
			return s.mapDBError(ctx, "group_create_insert_conv_members", cmErr)
		}

		audit := buildGroupAuditEntry(
			"group.create",
			creatorID,
			model.GroupRoleOwner,
			model.AuditTargetGroup,
			createdGroup.ID,
			createdGroup.ID,
			conv.ID,
			"",
			map[string]any{
				"name":       createdGroup.Name,
				"member_ids": formatUint64Slice(allUserIDs),
			},
			input.Meta,
			now,
		)
		if auditErr := s.repo.AppendAuditLog(ctx, tx, audit); auditErr != nil {
			return s.mapDBError(ctx, "group_create_audit", auditErr)
		}
		return nil
	})
	if txErr != nil {
		s.rollbackSeq(ctx, lockedConvID, seqRes)
		return nil, s.asErrCode(ctx, "group_create", txErr)
	}
	releaseLock()

	onlineMap := s.lookupOnlineBestEffort(ctx, allUserIDs)
	memberDTOs := make([]GroupMemberDTO, 0, len(memberModels))
	for _, gm := range memberModels {
		memberDTOs = append(memberDTOs, toGroupMemberDTO(gm, usersByID[gm.UserID], onlineMap[gm.UserID]))
	}

	msgDTO := toGroupMessageDTO(sysMsg)
	s.notifyGroupUsers(ctx, allUserIDs, &msgDTO, noticeInput{
		Event:          "group.created",
		ConversationID: createdConv.ID,
		ActorID:        creatorID,
		Payload: map[string]any{
			"group_id":      strconv.FormatUint(createdGroup.ID, 10),
			"name":          createdGroup.Name,
			"system_msg_id": strconv.FormatUint(sysMsg.ID, 10),
			"seq":           strconv.FormatUint(sysMsg.Seq, 10),
		},
	})

	return &CreateGroupResult{
		Group:         toGroupDTO(createdGroup, createdConv.ID, len(memberDTOs)),
		Conversation:  toGroupConversationDTO(createdConv, createdGroup, &creatorCM, true),
		Members:       memberDTOs,
		SystemMessage: msgDTO,
	}, nil
}

// List returns cursor-paginated group summaries the caller belongs to.
func (s *GroupService) List(ctx context.Context, userID uint64, status, cursor string, limit int) (*GroupListPage, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if userID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	trimmedStatus := strings.TrimSpace(status)
	if trimmedStatus != "" && trimmedStatus != model.GroupStatusActive && trimmedStatus != model.GroupStatusDisbanded {
		return nil, errcode.ErrInvalidValue.WithMessage("status must be active or disbanded")
	}
	if limit == 0 {
		limit = DefaultGroupListLimit
	}
	if limit < 1 || limit > MaxGroupListLimit {
		return nil, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}

	var cursorID uint64
	if trimmedCursor := strings.TrimSpace(cursor); trimmedCursor != "" {
		var c groupListCursor
		if err := decodeScopedCursor(trimmedCursor, &c); err != nil ||
			c.Version != 1 || c.Scope != groupListCursorScope || c.UserID != userID ||
			c.Status != trimmedStatus || c.Sort != "id_desc" || c.LastID == 0 {
			return nil, errcode.ErrInvalidValue.WithMessage("invalid cursor")
		}
		cursorID = c.LastID
	}

	rows, hasMore, err := s.repo.ListUserGroups(ctx, repository.UserGroupListFilter{
		UserID:   userID,
		Status:   trimmedStatus,
		CursorID: cursorID,
		Limit:    limit,
	})
	if err != nil {
		return nil, s.mapDBError(ctx, "group_list", err)
	}

	items := make([]GroupSummaryDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, GroupSummaryDTO{
			GroupDTO: toGroupDTO(row.Group, row.ConversationID, row.MemberCount),
			Role:     row.Role,
		})
	}

	var nextCursor *string
	if hasMore && len(rows) > 0 {
		encoded, encErr := encodeCursor(groupListCursor{
			Version: 1,
			Scope:   groupListCursorScope,
			UserID:  userID,
			Status:  trimmedStatus,
			Sort:    "id_desc",
			LastID:  rows[len(rows)-1].Group.ID,
		})
		if encErr != nil {
			return nil, errcode.ErrInternalConsistency
		}
		nextCursor = &encoded
	}

	return &GroupListPage{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// Get returns full Group, Conversation, caller role, and member_count for a member.
func (s *GroupService) Get(ctx context.Context, userID, groupID uint64) (*GetGroupResult, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if userID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}

	group, conv, err := s.repo.GetGroupAndConversation(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_get", err)
	}

	member, err := s.repo.GetGroupMember(ctx, groupID, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrNotGroupMember
		}
		return nil, s.mapDBError(ctx, "group_get_member", err)
	}
	if conv == nil {
		return nil, errcode.ErrInternalConsistency
	}

	cm, err := s.repo.GetConversationMember(ctx, conv.ID, userID)
	if err != nil {
		return nil, s.mapDBError(ctx, "group_get_conv_member", err)
	}
	memberCount, err := s.repo.CountGroupMembers(ctx, groupID)
	if err != nil {
		return nil, s.mapDBError(ctx, "group_get_member_count", err)
	}

	canSend := group.Status == model.GroupStatusActive
	return &GetGroupResult{
		Group:        toGroupDTO(*group, conv.ID, memberCount),
		Conversation: toGroupConversationDTO(*conv, *group, cm, canSend),
		Role:         member.Role,
		MemberCount:  memberCount,
	}, nil
}

// Update modifies any combination of name, avatar_asset_id, and announcement (owner or admin).
func (s *GroupService) Update(ctx context.Context, actorID, groupID uint64, input UpdateGroupInput) (*UpdateGroupResult, *errcode.Error) {
	if err := s.requireWriteDeps(); err != nil {
		return nil, err
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}
	if input.Name == nil && !input.HasAvatarAsset && !input.HasAnnouncement {
		return nil, errcode.ErrInvalidParams.WithMessage("at least one of name, avatar_asset_id, or announcement must be provided")
	}

	var validatedName string
	if input.Name != nil {
		var bizErr *errcode.Error
		validatedName, bizErr = validateGroupName(*input.Name)
		if bizErr != nil {
			return nil, bizErr
		}
	}
	var validatedAnnouncement *string
	if input.HasAnnouncement {
		var bizErr *errcode.Error
		validatedAnnouncement, bizErr = validateGroupAnnouncement(input.Announcement)
		if bizErr != nil {
			return nil, bizErr
		}
	}
	var validatedAssetID string
	if input.HasAvatarAsset {
		var bizErr *errcode.Error
		validatedAssetID, bizErr = normalizeOptionalAssetID(input.AvatarAssetID)
		if bizErr != nil {
			return nil, bizErr
		}
	}

	_, conv, err := s.repo.GetGroupAndConversation(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_update_preread", err)
	}
	if conv == nil {
		return nil, errcode.ErrInternalConsistency
	}

	lockToken, lockErr := s.seqAllocator.AcquireConvLock(ctx, conv.ID)
	if lockErr != nil {
		return nil, s.mapSeqError(ctx, "group_update_acquire_conv_lock", lockErr)
	}
	defer s.releaseConvLock(ctx, conv.ID, lockToken)

	var (
		updatedGroup model.Group
		sysMsg       model.Message
		memberCount  int
		notifyIDs    []uint64
		seqRes       *repository.SeqReservation
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		lockedGroup, members, actorMember, prepErr := s.lockActiveGroupAndActor(ctx, tx, groupID, actorID, true)
		if prepErr != nil {
			return prepErr
		}
		memberCount = len(members)

		users, uErr := s.repo.LockUsersAscending(ctx, tx, []uint64{actorID})
		if uErr != nil {
			return s.mapDBError(ctx, "group_update_lock_actor", uErr)
		}

		now := time.Now().UTC().Truncate(time.Millisecond)
		updates := map[string]any{"updated_at": now}
		changedFields := make([]string, 0, 3)

		if input.Name != nil {
			lockedGroup.Name = validatedName
			updates["name"] = validatedName
			changedFields = append(changedFields, "name")
		}
		if input.HasAvatarAsset {
			if validatedAssetID == "" {
				if unbindErr := s.repo.UnbindGroupAvatarAssets(ctx, tx, groupID, now); unbindErr != nil {
					return s.mapDBError(ctx, "group_update_unbind_avatar", unbindErr)
				}
				lockedGroup.AvatarURL = ""
				updates["avatar_url"] = ""
			} else {
				newURL, assetErr := s.validateGroupAvatarAssetTx(ctx, tx, actorID, groupID, validatedAssetID)
				if assetErr != nil {
					return assetErr
				}
				if bindErr := s.repo.BindGroupAvatarAsset(ctx, tx, validatedAssetID, groupID, now); bindErr != nil {
					return s.mapDBError(ctx, "group_update_bind_avatar", bindErr)
				}
				lockedGroup.AvatarURL = newURL
				updates["avatar_url"] = newURL
			}
			changedFields = append(changedFields, "avatar")
		}
		if input.HasAnnouncement {
			lockedGroup.Announcement = validatedAnnouncement
			if validatedAnnouncement == nil {
				updates["announcement"] = nil
			} else {
				updates["announcement"] = *validatedAnnouncement
			}
			changedFields = append(changedFields, "announcement")
		}
		lockedGroup.UpdatedAt = now

		if updErr := s.repo.UpdateGroupFields(ctx, tx, groupID, updates); updErr != nil {
			return s.mapDBError(ctx, "group_update_fields", updErr)
		}

		if _, convErr := s.repo.LockGroupConversationByGroupID(ctx, tx, groupID); convErr != nil {
			return s.mapDBError(ctx, "group_update_lock_conv", convErr)
		}
		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
		if seqErr != nil {
			return s.mapSeqError(ctx, "group_update_allocate_seq", seqErr)
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			return errcode.ErrInternalConsistency
		}

		actorName := displayActorName(users[actorID])
		content := buildGroupUpdateSystemContent(actorName, changedFields, lockedGroup.Name)
		extra := marshalExtraJSON(map[string]any{
			"event":          "group.updated",
			"group_id":       strconv.FormatUint(groupID, 10),
			"actor_id":       strconv.FormatUint(actorID, 10),
			"changed_fields": changedFields,
		})
		sysMsg = model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:group_update:%d", msgID),
			MsgType:        model.MessageTypeSystem,
			Content:        content,
			Extra:          extra,
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if msgErr := s.repo.CreateSystemMessage(ctx, tx, &sysMsg); msgErr != nil {
			return s.mapDBError(ctx, "group_update_system_message", msgErr)
		}
		if lastErr := s.repo.UpdateConversationLastMessage(ctx, tx, conv.ID, &sysMsg); lastErr != nil {
			return s.mapDBError(ctx, "group_update_conv_last_msg", lastErr)
		}

		notifyIDs = collectMemberUserIDs(members, 0)
		unreadTargets := collectMemberUserIDs(members, actorID)
		if unreadErr := s.repo.IncrementActiveGroupMembersUnread(ctx, tx, conv.ID, unreadTargets, now); unreadErr != nil {
			return s.mapDBError(ctx, "group_update_unread", unreadErr)
		}

		audit := buildGroupAuditEntry(
			"group.update",
			actorID,
			actorMember.Role,
			model.AuditTargetGroup,
			groupID,
			groupID,
			conv.ID,
			"",
			map[string]any{
				"changed_fields": changedFields,
				"name":           lockedGroup.Name,
				"avatar_url":     lockedGroup.AvatarURL,
				"announcement":   lockedGroup.Announcement,
			},
			input.Meta,
			now,
		)
		if auditErr := s.repo.AppendAuditLog(ctx, tx, audit); auditErr != nil {
			return s.mapDBError(ctx, "group_update_audit", auditErr)
		}

		updatedGroup = *lockedGroup
		return nil
	})
	if txErr != nil {
		s.rollbackSeq(ctx, conv.ID, seqRes)
		return nil, s.asErrCode(ctx, "group_update", txErr)
	}

	msgDTO := toGroupMessageDTO(sysMsg)
	s.notifyGroupUsers(ctx, notifyIDs, &msgDTO, noticeInput{
		Event:          "group.updated",
		ConversationID: conv.ID,
		ActorID:        actorID,
		Payload: map[string]any{
			"group_id":      strconv.FormatUint(groupID, 10),
			"name":          updatedGroup.Name,
			"avatar_url":    updatedGroup.AvatarURL,
			"announcement":  updatedGroup.Announcement,
			"system_msg_id": strconv.FormatUint(sysMsg.ID, 10),
			"seq":           strconv.FormatUint(sysMsg.Seq, 10),
		},
	})

	return &UpdateGroupResult{
		Group:         toGroupDTO(updatedGroup, conv.ID, memberCount),
		SystemMessage: msgDTO,
	}, nil
}

// Disband marks an active group disbanded (owner only), preserves read-only members/conversation,
// writes a disband system message, records an audit entry, and notifies active members.
func (s *GroupService) Disband(ctx context.Context, actorID, groupID uint64, meta GroupAuditMeta) (*DisbandGroupResult, *errcode.Error) {
	if err := s.requireWriteDeps(); err != nil {
		return nil, err
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}

	_, conv, err := s.repo.GetGroupAndConversation(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_disband_preread", err)
	}
	if conv == nil {
		return nil, errcode.ErrInternalConsistency
	}

	lockToken, lockErr := s.seqAllocator.AcquireConvLock(ctx, conv.ID)
	if lockErr != nil {
		return nil, s.mapSeqError(ctx, "group_disband_acquire_conv_lock", lockErr)
	}
	defer s.releaseConvLock(ctx, conv.ID, lockToken)

	var (
		sysMsg    model.Message
		notifyIDs []uint64
		seqRes    *repository.SeqReservation
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		lockedGroup, members, actorMember, prepErr := s.lockActiveGroupAndActor(ctx, tx, groupID, actorID, false)
		if prepErr != nil {
			return prepErr
		}
		if actorMember.Role != model.GroupRoleOwner || lockedGroup.OwnerID != actorID {
			return errcode.ErrGroupRoleDenied
		}

		now := time.Now().UTC().Truncate(time.Millisecond)
		if updErr := s.repo.UpdateGroupFields(ctx, tx, groupID, map[string]any{
			"status":     model.GroupStatusDisbanded,
			"updated_at": now,
		}); updErr != nil {
			return s.mapDBError(ctx, "group_disband_update_status", updErr)
		}

		if _, convErr := s.repo.LockGroupConversationByGroupID(ctx, tx, groupID); convErr != nil {
			return s.mapDBError(ctx, "group_disband_lock_conv", convErr)
		}
		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
		if seqErr != nil {
			return s.mapSeqError(ctx, "group_disband_allocate_seq", seqErr)
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			return errcode.ErrInternalConsistency
		}
		extra := marshalExtraJSON(map[string]any{
			"event":    "group.disbanded",
			"group_id": strconv.FormatUint(groupID, 10),
			"actor_id": strconv.FormatUint(actorID, 10),
		})
		sysMsg = model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:group_disband:%d", msgID),
			MsgType:        model.MessageTypeSystem,
			Content:        "群主已解散群聊",
			Extra:          extra,
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if msgErr := s.repo.CreateSystemMessage(ctx, tx, &sysMsg); msgErr != nil {
			return s.mapDBError(ctx, "group_disband_system_message", msgErr)
		}
		if lastErr := s.repo.UpdateConversationLastMessage(ctx, tx, conv.ID, &sysMsg); lastErr != nil {
			return s.mapDBError(ctx, "group_disband_conv_last_msg", lastErr)
		}

		notifyIDs = collectMemberUserIDs(members, 0)
		unreadTargets := collectMemberUserIDs(members, actorID)
		if unreadErr := s.repo.IncrementActiveGroupMembersUnread(ctx, tx, conv.ID, unreadTargets, now); unreadErr != nil {
			return s.mapDBError(ctx, "group_disband_unread", unreadErr)
		}

		audit := buildGroupAuditEntry(
			"group.disband",
			actorID,
			model.GroupRoleOwner,
			model.AuditTargetGroup,
			groupID,
			groupID,
			conv.ID,
			"",
			map[string]any{
				"member_count": len(members),
			},
			meta,
			now,
		)
		if auditErr := s.repo.AppendAuditLog(ctx, tx, audit); auditErr != nil {
			return s.mapDBError(ctx, "group_disband_audit", auditErr)
		}
		return nil
	})
	if txErr != nil {
		s.rollbackSeq(ctx, conv.ID, seqRes)
		return nil, s.asErrCode(ctx, "group_disband", txErr)
	}

	msgDTO := toGroupMessageDTO(sysMsg)
	s.notifyGroupUsers(ctx, notifyIDs, &msgDTO, noticeInput{
		Event:          "group.disbanded",
		ConversationID: conv.ID,
		ActorID:        actorID,
		Payload: map[string]any{
			"group_id":      strconv.FormatUint(groupID, 10),
			"system_msg_id": strconv.FormatUint(sysMsg.ID, 10),
			"seq":           strconv.FormatUint(sysMsg.Seq, 10),
		},
	})

	return &DisbandGroupResult{
		GroupID:        groupID,
		ConversationID: conv.ID,
		Status:         model.GroupStatusDisbanded,
		SystemMessage:  msgDTO,
	}, nil
}

// ListMembers returns cursor-paginated group members with batch-loaded profiles and online status.
func (s *GroupService) ListMembers(ctx context.Context, actorID, groupID uint64, input ListGroupMembersInput) (*GroupMemberPage, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}

	roleFilter := strings.TrimSpace(input.Role)
	if roleFilter != "" && roleFilter != model.GroupRoleOwner && roleFilter != model.GroupRoleAdmin && roleFilter != model.GroupRoleMember {
		return nil, errcode.ErrInvalidValue.WithMessage("role must be owner, admin, or member")
	}
	limit := input.Limit
	if limit == 0 {
		limit = DefaultGroupListLimit
	}
	if limit < 1 || limit > MaxGroupListLimit {
		return nil, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}

	var cursorID uint64
	if trimmedCursor := strings.TrimSpace(input.Cursor); trimmedCursor != "" {
		var c groupMemberCursor
		if err := decodeScopedCursor(trimmedCursor, &c); err != nil ||
			c.Version != 1 || c.Scope != groupMemberCursorScope || c.UserID != actorID ||
			c.GroupID != groupID || c.Role != roleFilter || c.OnlineOnly != input.OnlineOnly ||
			c.Sort != "id_desc" || c.LastID == 0 {
			return nil, errcode.ErrInvalidValue.WithMessage("invalid cursor")
		}
		cursorID = c.LastID
	}

	if _, _, err := s.repo.GetGroupAndConversation(ctx, groupID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_list_members_group", err)
	}
	if _, err := s.repo.GetGroupMember(ctx, groupID, actorID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrNotGroupMember
		}
		return nil, s.mapDBError(ctx, "group_list_members_auth", err)
	}

	var (
		selected []model.GroupMember
		online   map[uint64]bool
		hasMore  bool
	)

	if !input.OnlineOnly {
		rows, err := s.repo.ListGroupMembers(ctx, repository.GroupMemberListFilter{
			GroupID:  groupID,
			Role:     roleFilter,
			CursorID: cursorID,
			Limit:    limit + 1,
		})
		if err != nil {
			return nil, s.mapDBError(ctx, "group_list_members", err)
		}
		if len(rows) > limit {
			hasMore = true
			rows = rows[:limit]
		}
		selected = rows
		online = s.lookupOnlineBestEffort(ctx, memberUserIDs(selected))
	} else {
		if s.presence == nil {
			return nil, errcode.ErrRedisUnavailable
		}
		online = make(map[uint64]bool)
		scanCursor := cursorID
		for len(selected) <= limit {
			chunk, err := s.repo.ListGroupMembers(ctx, repository.GroupMemberListFilter{
				GroupID:  groupID,
				Role:     roleFilter,
				CursorID: scanCursor,
				Limit:    memberOnlineScanChunk,
			})
			if err != nil {
				return nil, s.mapDBError(ctx, "group_list_members_online_scan", err)
			}
			if len(chunk) == 0 {
				break
			}
			chunkOnline, pErr := s.presence.BatchIsOnline(ctx, memberUserIDs(chunk))
			if pErr != nil {
				s.log.ErrorContext(ctx, "group_list_members_presence_failed", slog.Any("error", pErr))
				return nil, errcode.ErrRedisUnavailable
			}
			for _, m := range chunk {
				if chunkOnline[m.UserID] {
					online[m.UserID] = true
					selected = append(selected, m)
					if len(selected) > limit {
						break
					}
				}
			}
			if len(chunk) < memberOnlineScanChunk || len(selected) > limit {
				break
			}
			scanCursor = chunk[len(chunk)-1].ID
		}
		if len(selected) > limit {
			hasMore = true
			selected = selected[:limit]
		}
	}

	usersMap, err := s.repo.BatchGetUsersByIDs(ctx, memberUserIDs(selected))
	if err != nil {
		return nil, s.mapDBError(ctx, "group_list_members_users", err)
	}

	items := make([]GroupMemberDTO, 0, len(selected))
	for _, gm := range selected {
		items = append(items, toGroupMemberDTO(gm, usersMap[gm.UserID], online[gm.UserID]))
	}

	var nextCursor *string
	if hasMore && len(selected) > 0 {
		encoded, encErr := encodeCursor(groupMemberCursor{
			Version:    1,
			Scope:      groupMemberCursorScope,
			UserID:     actorID,
			GroupID:    groupID,
			Role:       roleFilter,
			OnlineOnly: input.OnlineOnly,
			Sort:       "id_desc",
			LastID:     selected[len(selected)-1].ID,
		})
		if encErr != nil {
			return nil, errcode.ErrInternalConsistency
		}
		nextCursor = &encoded
	}

	return &GroupMemberPage{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// AddMembers invites new users to an active group (owner or admin), synchronizes group_members
// and conversation_members with pre-join watermark baseline, and writes a system message.
func (s *GroupService) AddMembers(ctx context.Context, actorID, groupID uint64, userIDs []uint64, meta GroupAuditMeta) (*AddGroupMembersResult, *errcode.Error) {
	if err := s.requireWriteDeps(); err != nil {
		return nil, err
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}
	targetIDs := dedupePositiveUint64(userIDs)
	if len(targetIDs) == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("user_ids must contain at least one valid user_id")
	}
	sort.Slice(targetIDs, func(i, j int) bool { return targetIDs[i] < targetIDs[j] })

	_, conv, err := s.repo.GetGroupAndConversation(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_add_members_preread", err)
	}
	if conv == nil {
		return nil, errcode.ErrInternalConsistency
	}

	lockToken, lockErr := s.seqAllocator.AcquireConvLock(ctx, conv.ID)
	if lockErr != nil {
		return nil, s.mapSeqError(ctx, "group_add_members_acquire_conv_lock", lockErr)
	}
	defer s.releaseConvLock(ctx, conv.ID, lockToken)

	var (
		addedModels []model.GroupMember
		persistedCM []model.ConversationMember
		usersByID   map[uint64]model.User
		sysMsg      model.Message
		notifyIDs   []uint64
		seqRes      *repository.SeqReservation
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		lockedGroup, existingMembers, actorMember, prepErr := s.lockActiveGroupAndActor(ctx, tx, groupID, actorID, true)
		if prepErr != nil {
			return prepErr
		}

		existingSet := make(map[uint64]model.GroupMember, len(existingMembers))
		for _, m := range existingMembers {
			existingSet[m.UserID] = m
		}

		lockUserIDs := append([]uint64{actorID}, targetIDs...)
		users, lockErr := s.repo.LockUsersAscending(ctx, tx, lockUserIDs)
		if lockErr != nil {
			return s.mapDBError(ctx, "group_add_members_lock_users", lockErr)
		}
		usersByID = users

		for _, uid := range targetIDs {
			u, exists := users[uid]
			if !exists {
				return errcode.ErrUserNotFound
			}
			if _, already := existingSet[uid]; already {
				return errcode.ErrAlreadyGroupMember
			}
			if u.Status != model.UserStatusActive {
				return errcode.ErrAccountDisabled
			}
		}

		if uint32(len(existingMembers)+len(targetIDs)) > lockedGroup.MaxMembers {
			return errcode.ErrGroupFull
		}

		lockedConv, convErr := s.repo.LockGroupConversationByGroupID(ctx, tx, groupID)
		if convErr != nil {
			return s.mapDBError(ctx, "group_add_members_lock_conv", convErr)
		}
		preJoinLastSeq := lockedConv.LastSeq

		now := time.Now().UTC().Truncate(time.Millisecond)
		addedModels = make([]model.GroupMember, 0, len(targetIDs))
		for _, uid := range targetIDs {
			addedModels = append(addedModels, model.GroupMember{
				GroupID:   groupID,
				UserID:    uid,
				Role:      model.GroupRoleMember,
				JoinedAt:  now,
				UpdatedAt: now,
			})
		}
		if gmErr := s.repo.CreateGroupMembers(ctx, tx, addedModels); gmErr != nil {
			return s.mapDBError(ctx, "group_add_members_insert", gmErr)
		}

		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
		if seqErr != nil {
			return s.mapSeqError(ctx, "group_add_members_allocate_seq", seqErr)
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			return errcode.ErrInternalConsistency
		}

		actorName := displayActorName(users[actorID])
		targetNames := make([]string, 0, len(targetIDs))
		for _, uid := range targetIDs {
			targetNames = append(targetNames, displayActorName(users[uid]))
		}
		content := fmt.Sprintf("%s 邀请 %s 加入群聊", actorName, strings.Join(targetNames, "、"))
		extra := marshalExtraJSON(map[string]any{
			"event":    "group.member_added",
			"group_id": strconv.FormatUint(groupID, 10),
			"actor_id": strconv.FormatUint(actorID, 10),
			"user_ids": formatUint64Slice(targetIDs),
		})
		sysMsg = model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:group_member_add:%d", msgID),
			MsgType:        model.MessageTypeSystem,
			Content:        content,
			Extra:          extra,
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if msgErr := s.repo.CreateSystemMessage(ctx, tx, &sysMsg); msgErr != nil {
			return s.mapDBError(ctx, "group_add_members_system_message", msgErr)
		}
		if lastErr := s.repo.UpdateConversationLastMessage(ctx, tx, conv.ID, &sysMsg); lastErr != nil {
			return s.mapDBError(ctx, "group_add_members_conv_last_msg", lastErr)
		}

		existingNotifyIDs := collectMemberUserIDs(existingMembers, 0)
		unreadTargets := collectMemberUserIDs(existingMembers, actorID)
		if unreadErr := s.repo.IncrementActiveGroupMembersUnread(ctx, tx, conv.ID, unreadTargets, now); unreadErr != nil {
			return s.mapDBError(ctx, "group_add_members_unread", unreadErr)
		}

		cmRows, cmErr := s.repo.UpsertConversationMembersOnJoin(ctx, tx, conv.ID, targetIDs, preJoinLastSeq, 1, now)
		if cmErr != nil {
			return s.mapDBError(ctx, "group_add_members_upsert_cm", cmErr)
		}
		persistedCM = cmRows
		notifyIDs = append(existingNotifyIDs, targetIDs...)

		audit := buildGroupAuditEntry(
			"group.member_add",
			actorID,
			actorMember.Role,
			model.AuditTargetGroup,
			groupID,
			groupID,
			conv.ID,
			"",
			map[string]any{
				"user_ids":          formatUint64Slice(targetIDs),
				"pre_join_last_seq": strconv.FormatUint(preJoinLastSeq, 10),
			},
			meta,
			now,
		)
		if auditErr := s.repo.AppendAuditLog(ctx, tx, audit); auditErr != nil {
			return s.mapDBError(ctx, "group_add_members_audit", auditErr)
		}
		return nil
	})
	if txErr != nil {
		s.rollbackSeq(ctx, conv.ID, seqRes)
		return nil, s.asErrCode(ctx, "group_add_members", txErr)
	}

	onlineMap := s.lookupOnlineBestEffort(ctx, targetIDs)
	memberDTOs := make([]GroupMemberDTO, 0, len(addedModels))
	for _, gm := range addedModels {
		memberDTOs = append(memberDTOs, toGroupMemberDTO(gm, usersByID[gm.UserID], onlineMap[gm.UserID]))
	}
	cmDTOs := make([]GroupConversationMemberDTO, 0, len(persistedCM))
	for _, cm := range persistedCM {
		cmDTOs = append(cmDTOs, toGroupConversationMemberDTO(cm))
	}

	msgDTO := toGroupMessageDTO(sysMsg)
	var targetPtr *uint64
	if len(targetIDs) == 1 {
		v := targetIDs[0]
		targetPtr = &v
	}
	s.notifyGroupUsers(ctx, notifyIDs, &msgDTO, noticeInput{
		Event:          "group.member_added",
		ConversationID: conv.ID,
		ActorID:        actorID,
		TargetUserID:   targetPtr,
		Payload: map[string]any{
			"group_id":      strconv.FormatUint(groupID, 10),
			"user_ids":      formatUint64Slice(targetIDs),
			"system_msg_id": strconv.FormatUint(sysMsg.ID, 10),
			"seq":           strconv.FormatUint(sysMsg.Seq, 10),
		},
	})

	return &AddGroupMembersResult{
		GroupID:             groupID,
		ConversationID:      conv.ID,
		Members:             memberDTOs,
		ConversationMembers: cmDTOs,
		SystemMessage:       msgDTO,
	}, nil
}

// RemoveMember removes targetUserID from an active group (owner or admin, with strict role hierarchy),
// deletes group_members, hides conversation_members, writes a system message, and notifies relevant users.
func (s *GroupService) RemoveMember(ctx context.Context, actorID, groupID, targetUserID uint64, meta GroupAuditMeta) (*RemoveGroupMemberResult, *errcode.Error) {
	if err := s.requireWriteDeps(); err != nil {
		return nil, err
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 || targetUserID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id or user_id")
	}

	_, conv, err := s.repo.GetGroupAndConversation(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_remove_member_preread", err)
	}
	if conv == nil {
		return nil, errcode.ErrInternalConsistency
	}

	lockToken, lockErr := s.seqAllocator.AcquireConvLock(ctx, conv.ID)
	if lockErr != nil {
		return nil, s.mapSeqError(ctx, "group_remove_member_acquire_conv_lock", lockErr)
	}
	defer s.releaseConvLock(ctx, conv.ID, lockToken)

	var (
		removedAt    time.Time
		sysMsg       model.Message
		remainingIDs []uint64
		seqRes       *repository.SeqReservation
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		lockedGroup, members, actorMember, prepErr := s.lockActiveGroupAndActor(ctx, tx, groupID, actorID, true)
		if prepErr != nil {
			return prepErr
		}

		targetMember, hasTarget := findMember(members, targetUserID)
		if !hasTarget {
			return errcode.ErrGroupMemberNotFound
		}
		if targetMember.Role == model.GroupRoleOwner || targetUserID == lockedGroup.OwnerID {
			if actorID == targetUserID {
				return errcode.ErrOwnerLeaveForbidden
			}
			return errcode.ErrGroupRoleDenied
		}
		if actorMember.Role == model.GroupRoleAdmin && targetMember.Role == model.GroupRoleAdmin {
			return errcode.ErrGroupRoleDenied
		}

		users, uErr := s.repo.LockUsersAscending(ctx, tx, []uint64{actorID, targetUserID})
		if uErr != nil {
			return s.mapDBError(ctx, "group_remove_member_lock_users", uErr)
		}

		removedAt = time.Now().UTC().Truncate(time.Millisecond)
		if delErr := s.repo.DeleteGroupMember(ctx, tx, groupID, targetUserID); delErr != nil {
			return s.mapDBError(ctx, "group_remove_member_delete", delErr)
		}
		if hideErr := s.repo.HideConversationMember(ctx, tx, conv.ID, targetUserID, removedAt); hideErr != nil {
			return s.mapDBError(ctx, "group_remove_member_hide_cm", hideErr)
		}

		if _, convErr := s.repo.LockGroupConversationByGroupID(ctx, tx, groupID); convErr != nil {
			return s.mapDBError(ctx, "group_remove_member_lock_conv", convErr)
		}
		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
		if seqErr != nil {
			return s.mapSeqError(ctx, "group_remove_member_allocate_seq", seqErr)
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			return errcode.ErrInternalConsistency
		}

		content := fmt.Sprintf("%s 将 %s 移出群聊", displayActorName(users[actorID]), displayActorName(users[targetUserID]))
		extra := marshalExtraJSON(map[string]any{
			"event":          "group.member_removed",
			"group_id":       strconv.FormatUint(groupID, 10),
			"actor_id":       strconv.FormatUint(actorID, 10),
			"target_user_id": strconv.FormatUint(targetUserID, 10),
			"reason":         "removed",
		})
		sysMsg = model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:group_member_remove:%d", msgID),
			MsgType:        model.MessageTypeSystem,
			Content:        content,
			Extra:          extra,
			Status:         model.MessageStatusNormal,
			CreatedAt:      removedAt,
		}
		if msgErr := s.repo.CreateSystemMessage(ctx, tx, &sysMsg); msgErr != nil {
			return s.mapDBError(ctx, "group_remove_member_system_message", msgErr)
		}
		if lastErr := s.repo.UpdateConversationLastMessage(ctx, tx, conv.ID, &sysMsg); lastErr != nil {
			return s.mapDBError(ctx, "group_remove_member_conv_last_msg", lastErr)
		}

		remainingIDs = collectMemberUserIDs(members, targetUserID)
		unreadTargets := collectExcluding(remainingIDs, actorID)
		if unreadErr := s.repo.IncrementActiveGroupMembersUnread(ctx, tx, conv.ID, unreadTargets, removedAt); unreadErr != nil {
			return s.mapDBError(ctx, "group_remove_member_unread", unreadErr)
		}

		audit := buildGroupAuditEntry(
			"group.member_remove",
			actorID,
			actorMember.Role,
			model.AuditTargetUser,
			targetUserID,
			groupID,
			conv.ID,
			"",
			map[string]any{
				"removed_user_id": strconv.FormatUint(targetUserID, 10),
				"removed_role":    targetMember.Role,
			},
			meta,
			removedAt,
		)
		if auditErr := s.repo.AppendAuditLog(ctx, tx, audit); auditErr != nil {
			return s.mapDBError(ctx, "group_remove_member_audit", auditErr)
		}
		return nil
	})
	if txErr != nil {
		s.rollbackSeq(ctx, conv.ID, seqRes)
		return nil, s.asErrCode(ctx, "group_remove_member", txErr)
	}

	msgDTO := toGroupMessageDTO(sysMsg)
	notice := noticeInput{
		Event:          "group.member_removed",
		ConversationID: conv.ID,
		ActorID:        actorID,
		TargetUserID:   &targetUserID,
		Payload: map[string]any{
			"group_id":      strconv.FormatUint(groupID, 10),
			"user_id":       strconv.FormatUint(targetUserID, 10),
			"reason":        "removed",
			"system_msg_id": strconv.FormatUint(sysMsg.ID, 10),
			"seq":           strconv.FormatUint(sysMsg.Seq, 10),
		},
	}
	// Remaining active members receive msg.push + system.notice;
	// removed user receives ONLY system.notice (no msg.push since membership is revoked).
	s.notifyGroupUsers(ctx, remainingIDs, &msgDTO, notice)
	s.notifyGroupUsers(ctx, []uint64{targetUserID}, nil, notice)

	return &RemoveGroupMemberResult{
		GroupID:        groupID,
		ConversationID: conv.ID,
		UserID:         targetUserID,
		RemovedAt:      FormatUTCMillis(removedAt),
		SystemMessage:  msgDTO,
	}, nil
}

// Leave removes the caller from an active group (owner must transfer or disband first -> 15008).
func (s *GroupService) Leave(ctx context.Context, actorID, groupID uint64, meta GroupAuditMeta) (*LeaveGroupResult, *errcode.Error) {
	if err := s.requireWriteDeps(); err != nil {
		return nil, err
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}

	_, conv, err := s.repo.GetGroupAndConversation(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_leave_preread", err)
	}
	if conv == nil {
		return nil, errcode.ErrInternalConsistency
	}

	lockToken, lockErr := s.seqAllocator.AcquireConvLock(ctx, conv.ID)
	if lockErr != nil {
		return nil, s.mapSeqError(ctx, "group_leave_acquire_conv_lock", lockErr)
	}
	defer s.releaseConvLock(ctx, conv.ID, lockToken)

	var (
		leftAt       time.Time
		sysMsg       model.Message
		remainingIDs []uint64
		seqRes       *repository.SeqReservation
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		lockedGroup, members, actorMember, prepErr := s.lockActiveGroupAndActor(ctx, tx, groupID, actorID, false)
		if prepErr != nil {
			return prepErr
		}
		if actorMember.Role == model.GroupRoleOwner || lockedGroup.OwnerID == actorID {
			return errcode.ErrOwnerLeaveForbidden
		}

		users, uErr := s.repo.LockUsersAscending(ctx, tx, []uint64{actorID})
		if uErr != nil {
			return s.mapDBError(ctx, "group_leave_lock_user", uErr)
		}

		leftAt = time.Now().UTC().Truncate(time.Millisecond)
		if delErr := s.repo.DeleteGroupMember(ctx, tx, groupID, actorID); delErr != nil {
			return s.mapDBError(ctx, "group_leave_delete", delErr)
		}
		if hideErr := s.repo.HideConversationMember(ctx, tx, conv.ID, actorID, leftAt); hideErr != nil {
			return s.mapDBError(ctx, "group_leave_hide_cm", hideErr)
		}

		if _, convErr := s.repo.LockGroupConversationByGroupID(ctx, tx, groupID); convErr != nil {
			return s.mapDBError(ctx, "group_leave_lock_conv", convErr)
		}
		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
		if seqErr != nil {
			return s.mapSeqError(ctx, "group_leave_allocate_seq", seqErr)
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			return errcode.ErrInternalConsistency
		}

		content := fmt.Sprintf("%s 退出了群聊", displayActorName(users[actorID]))
		extra := marshalExtraJSON(map[string]any{
			"event":          "group.member_removed",
			"group_id":       strconv.FormatUint(groupID, 10),
			"actor_id":       strconv.FormatUint(actorID, 10),
			"target_user_id": strconv.FormatUint(actorID, 10),
			"reason":         "left",
		})
		sysMsg = model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:group_leave:%d", msgID),
			MsgType:        model.MessageTypeSystem,
			Content:        content,
			Extra:          extra,
			Status:         model.MessageStatusNormal,
			CreatedAt:      leftAt,
		}
		if msgErr := s.repo.CreateSystemMessage(ctx, tx, &sysMsg); msgErr != nil {
			return s.mapDBError(ctx, "group_leave_system_message", msgErr)
		}
		if lastErr := s.repo.UpdateConversationLastMessage(ctx, tx, conv.ID, &sysMsg); lastErr != nil {
			return s.mapDBError(ctx, "group_leave_conv_last_msg", lastErr)
		}

		remainingIDs = collectMemberUserIDs(members, actorID)
		if unreadErr := s.repo.IncrementActiveGroupMembersUnread(ctx, tx, conv.ID, remainingIDs, leftAt); unreadErr != nil {
			return s.mapDBError(ctx, "group_leave_unread", unreadErr)
		}

		audit := buildGroupAuditEntry(
			"group.member_leave",
			actorID,
			actorMember.Role,
			model.AuditTargetUser,
			actorID,
			groupID,
			conv.ID,
			"",
			map[string]any{
				"role": actorMember.Role,
			},
			meta,
			leftAt,
		)
		if auditErr := s.repo.AppendAuditLog(ctx, tx, audit); auditErr != nil {
			return s.mapDBError(ctx, "group_leave_audit", auditErr)
		}
		return nil
	})
	if txErr != nil {
		s.rollbackSeq(ctx, conv.ID, seqRes)
		return nil, s.asErrCode(ctx, "group_leave", txErr)
	}

	msgDTO := toGroupMessageDTO(sysMsg)
	notice := noticeInput{
		Event:          "group.member_removed",
		ConversationID: conv.ID,
		ActorID:        actorID,
		TargetUserID:   &actorID,
		Payload: map[string]any{
			"group_id":      strconv.FormatUint(groupID, 10),
			"user_id":       strconv.FormatUint(actorID, 10),
			"reason":        "left",
			"system_msg_id": strconv.FormatUint(sysMsg.ID, 10),
			"seq":           strconv.FormatUint(sysMsg.Seq, 10),
		},
	}
	s.notifyGroupUsers(ctx, remainingIDs, &msgDTO, notice)
	s.notifyGroupUsers(ctx, []uint64{actorID}, nil, notice)

	return &LeaveGroupResult{
		GroupID:        groupID,
		ConversationID: conv.ID,
		UserID:         actorID,
		LeftAt:         FormatUTCMillis(leftAt),
		SystemMessage:  msgDTO,
	}, nil
}

// Transfer transfers group ownership to an eligible active group member (owner only).
func (s *GroupService) Transfer(ctx context.Context, actorID, groupID, targetUserID uint64, meta GroupAuditMeta) (*TransferGroupResult, *errcode.Error) {
	if err := s.requireWriteDeps(); err != nil {
		return nil, err
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 || targetUserID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id or target_user_id")
	}

	_, conv, err := s.repo.GetGroupAndConversation(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_transfer_preread", err)
	}
	if conv == nil {
		return nil, errcode.ErrInternalConsistency
	}

	lockToken, lockErr := s.seqAllocator.AcquireConvLock(ctx, conv.ID)
	if lockErr != nil {
		return nil, s.mapSeqError(ctx, "group_transfer_acquire_conv_lock", lockErr)
	}
	defer s.releaseConvLock(ctx, conv.ID, lockToken)

	var (
		sysMsg      model.Message
		notifyIDs   []uint64
		seqRes      *repository.SeqReservation
		oldTargetRole string
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		lockedGroup, members, actorMember, prepErr := s.lockActiveGroupAndActor(ctx, tx, groupID, actorID, false)
		if prepErr != nil {
			return prepErr
		}
		if actorMember.Role != model.GroupRoleOwner || lockedGroup.OwnerID != actorID {
			return errcode.ErrGroupRoleDenied
		}
		if targetUserID == actorID {
			return errcode.ErrTransferIneligible
		}
		targetMember, hasTarget := findMember(members, targetUserID)
		if !hasTarget {
			return errcode.ErrTransferIneligible
		}
		oldTargetRole = targetMember.Role

		users, uErr := s.repo.LockUsersAscending(ctx, tx, []uint64{actorID, targetUserID})
		if uErr != nil {
			return s.mapDBError(ctx, "group_transfer_lock_users", uErr)
		}
		targetUser, hasUser := users[targetUserID]
		if !hasUser || targetUser.Status != model.UserStatusActive {
			return errcode.ErrTransferIneligible
		}

		now := time.Now().UTC().Truncate(time.Millisecond)
		if updErr := s.repo.UpdateGroupFields(ctx, tx, groupID, map[string]any{
			"owner_id":   targetUserID,
			"updated_at": now,
		}); updErr != nil {
			return s.mapDBError(ctx, "group_transfer_update_owner", updErr)
		}
		if roleErr := s.repo.UpdateGroupMemberRole(ctx, tx, groupID, actorID, model.GroupRoleMember, now); roleErr != nil {
			return s.mapDBError(ctx, "group_transfer_demote_old_owner", roleErr)
		}
		if roleErr := s.repo.UpdateGroupMemberRole(ctx, tx, groupID, targetUserID, model.GroupRoleOwner, now); roleErr != nil {
			return s.mapDBError(ctx, "group_transfer_promote_new_owner", roleErr)
		}

		if _, convErr := s.repo.LockGroupConversationByGroupID(ctx, tx, groupID); convErr != nil {
			return s.mapDBError(ctx, "group_transfer_lock_conv", convErr)
		}
		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
		if seqErr != nil {
			return s.mapSeqError(ctx, "group_transfer_allocate_seq", seqErr)
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			return errcode.ErrInternalConsistency
		}

		content := fmt.Sprintf("%s 将群主转让给 %s", displayActorName(users[actorID]), displayActorName(targetUser))
		extra := marshalExtraJSON(map[string]any{
			"event":             "group.role_changed",
			"group_id":          strconv.FormatUint(groupID, 10),
			"actor_id":          strconv.FormatUint(actorID, 10),
			"previous_owner_id": strconv.FormatUint(actorID, 10),
			"new_owner_id":      strconv.FormatUint(targetUserID, 10),
			"old_role":          oldTargetRole,
			"new_role":          model.GroupRoleOwner,
		})
		sysMsg = model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:group_transfer:%d", msgID),
			MsgType:        model.MessageTypeSystem,
			Content:        content,
			Extra:          extra,
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if msgErr := s.repo.CreateSystemMessage(ctx, tx, &sysMsg); msgErr != nil {
			return s.mapDBError(ctx, "group_transfer_system_message", msgErr)
		}
		if lastErr := s.repo.UpdateConversationLastMessage(ctx, tx, conv.ID, &sysMsg); lastErr != nil {
			return s.mapDBError(ctx, "group_transfer_conv_last_msg", lastErr)
		}

		notifyIDs = collectMemberUserIDs(members, 0)
		unreadTargets := collectMemberUserIDs(members, actorID)
		if unreadErr := s.repo.IncrementActiveGroupMembersUnread(ctx, tx, conv.ID, unreadTargets, now); unreadErr != nil {
			return s.mapDBError(ctx, "group_transfer_unread", unreadErr)
		}

		audit := buildGroupAuditEntry(
			"group.transfer",
			actorID,
			model.GroupRoleOwner,
			model.AuditTargetUser,
			targetUserID,
			groupID,
			conv.ID,
			"",
			map[string]any{
				"previous_owner_id": strconv.FormatUint(actorID, 10),
				"new_owner_id":      strconv.FormatUint(targetUserID, 10),
			},
			meta,
			now,
		)
		if auditErr := s.repo.AppendAuditLog(ctx, tx, audit); auditErr != nil {
			return s.mapDBError(ctx, "group_transfer_audit", auditErr)
		}
		return nil
	})
	if txErr != nil {
		s.rollbackSeq(ctx, conv.ID, seqRes)
		return nil, s.asErrCode(ctx, "group_transfer", txErr)
	}

	msgDTO := toGroupMessageDTO(sysMsg)
	s.notifyGroupUsers(ctx, notifyIDs, &msgDTO, noticeInput{
		Event:          "group.role_changed",
		ConversationID: conv.ID,
		ActorID:        actorID,
		TargetUserID:   &targetUserID,
		Payload: map[string]any{
			"group_id":          strconv.FormatUint(groupID, 10),
			"user_id":           strconv.FormatUint(targetUserID, 10),
			"old_role":          oldTargetRole,
			"new_role":          model.GroupRoleOwner,
			"previous_owner_id": strconv.FormatUint(actorID, 10),
			"system_msg_id":     strconv.FormatUint(sysMsg.ID, 10),
			"seq":               strconv.FormatUint(sysMsg.Seq, 10),
		},
	})

	return &TransferGroupResult{
		GroupID:         groupID,
		OwnerID:         targetUserID,
		PreviousOwnerID: actorID,
		SystemMessage:   msgDTO,
	}, nil
}

// UpdateMemberRole promotes/demotes a non-owner member between admin and member (owner only).
func (s *GroupService) UpdateMemberRole(ctx context.Context, actorID, groupID, targetUserID uint64, newRole string, meta GroupAuditMeta) (*UpdateMemberRoleResult, *errcode.Error) {
	if err := s.requireWriteDeps(); err != nil {
		return nil, err
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 || targetUserID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id or user_id")
	}
	role := strings.TrimSpace(newRole)
	if role != model.GroupRoleAdmin && role != model.GroupRoleMember {
		return nil, errcode.ErrInvalidValue.WithMessage("role must be admin or member")
	}

	_, conv, err := s.repo.GetGroupAndConversation(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_role_change_preread", err)
	}
	if conv == nil {
		return nil, errcode.ErrInternalConsistency
	}

	lockToken, lockErr := s.seqAllocator.AcquireConvLock(ctx, conv.ID)
	if lockErr != nil {
		return nil, s.mapSeqError(ctx, "group_role_change_acquire_conv_lock", lockErr)
	}
	defer s.releaseConvLock(ctx, conv.ID, lockToken)

	var (
		updatedMember model.GroupMember
		targetUser    model.User
		oldRole       string
		sysMsg        model.Message
		notifyIDs     []uint64
		seqRes        *repository.SeqReservation
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		lockedGroup, members, actorMember, prepErr := s.lockActiveGroupAndActor(ctx, tx, groupID, actorID, false)
		if prepErr != nil {
			return prepErr
		}
		if actorMember.Role != model.GroupRoleOwner || lockedGroup.OwnerID != actorID {
			return errcode.ErrGroupRoleDenied
		}

		targetMember, hasTarget := findMember(members, targetUserID)
		if !hasTarget {
			return errcode.ErrGroupMemberNotFound
		}
		if targetMember.Role == model.GroupRoleOwner || targetUserID == lockedGroup.OwnerID {
			return errcode.ErrCannotDemoteOwner
		}
		oldRole = targetMember.Role

		users, uErr := s.repo.LockUsersAscending(ctx, tx, []uint64{actorID, targetUserID})
		if uErr != nil {
			return s.mapDBError(ctx, "group_role_change_lock_users", uErr)
		}
		targetUser = users[targetUserID]

		now := time.Now().UTC().Truncate(time.Millisecond)
		if updErr := s.repo.UpdateGroupMemberRole(ctx, tx, groupID, targetUserID, role, now); updErr != nil {
			return s.mapDBError(ctx, "group_role_change_update", updErr)
		}
		targetMember.Role = role
		targetMember.UpdatedAt = now
		updatedMember = targetMember

		if _, convErr := s.repo.LockGroupConversationByGroupID(ctx, tx, groupID); convErr != nil {
			return s.mapDBError(ctx, "group_role_change_lock_conv", convErr)
		}
		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
		if seqErr != nil {
			return s.mapSeqError(ctx, "group_role_change_allocate_seq", seqErr)
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			return errcode.ErrInternalConsistency
		}

		actorName := displayActorName(users[actorID])
		targetName := displayActorName(targetUser)
		content := fmt.Sprintf("%s 取消了 %s 的管理员身份", actorName, targetName)
		if role == model.GroupRoleAdmin {
			content = fmt.Sprintf("%s 将 %s 设为管理员", actorName, targetName)
		}
		extra := marshalExtraJSON(map[string]any{
			"event":          "group.role_changed",
			"group_id":       strconv.FormatUint(groupID, 10),
			"actor_id":       strconv.FormatUint(actorID, 10),
			"target_user_id": strconv.FormatUint(targetUserID, 10),
			"old_role":       oldRole,
			"new_role":       role,
		})
		sysMsg = model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:group_role_change:%d", msgID),
			MsgType:        model.MessageTypeSystem,
			Content:        content,
			Extra:          extra,
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if msgErr := s.repo.CreateSystemMessage(ctx, tx, &sysMsg); msgErr != nil {
			return s.mapDBError(ctx, "group_role_change_system_message", msgErr)
		}
		if lastErr := s.repo.UpdateConversationLastMessage(ctx, tx, conv.ID, &sysMsg); lastErr != nil {
			return s.mapDBError(ctx, "group_role_change_conv_last_msg", lastErr)
		}

		notifyIDs = collectMemberUserIDs(members, 0)
		unreadTargets := collectMemberUserIDs(members, actorID)
		if unreadErr := s.repo.IncrementActiveGroupMembersUnread(ctx, tx, conv.ID, unreadTargets, now); unreadErr != nil {
			return s.mapDBError(ctx, "group_role_change_unread", unreadErr)
		}

		audit := buildGroupAuditEntry(
			"group.role_change",
			actorID,
			model.GroupRoleOwner,
			model.AuditTargetUser,
			targetUserID,
			groupID,
			conv.ID,
			"",
			map[string]any{
				"user_id":  strconv.FormatUint(targetUserID, 10),
				"old_role": oldRole,
				"new_role": role,
			},
			meta,
			now,
		)
		if auditErr := s.repo.AppendAuditLog(ctx, tx, audit); auditErr != nil {
			return s.mapDBError(ctx, "group_role_change_audit", auditErr)
		}
		return nil
	})
	if txErr != nil {
		s.rollbackSeq(ctx, conv.ID, seqRes)
		return nil, s.asErrCode(ctx, "group_role_change", txErr)
	}

	onlineMap := s.lookupOnlineBestEffort(ctx, []uint64{targetUserID})
	msgDTO := toGroupMessageDTO(sysMsg)
	s.notifyGroupUsers(ctx, notifyIDs, &msgDTO, noticeInput{
		Event:          "group.role_changed",
		ConversationID: conv.ID,
		ActorID:        actorID,
		TargetUserID:   &targetUserID,
		Payload: map[string]any{
			"group_id":      strconv.FormatUint(groupID, 10),
			"user_id":       strconv.FormatUint(targetUserID, 10),
			"old_role":      oldRole,
			"new_role":      role,
			"system_msg_id": strconv.FormatUint(sysMsg.ID, 10),
			"seq":           strconv.FormatUint(sysMsg.Seq, 10),
		},
	})

	return &UpdateMemberRoleResult{
		GroupID:       groupID,
		UserID:        targetUserID,
		Role:          role,
		Member:        toGroupMemberDTO(updatedMember, targetUser, onlineMap[targetUserID]),
		SystemMessage: msgDTO,
	}, nil
}

// UpdateMyMember updates the caller's own `nickname_in_group` inside an active group.
func (s *GroupService) UpdateMyMember(ctx context.Context, actorID, groupID uint64, nicknameInGroup string) (*UpdateMyMemberResult, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}
	cleanedNick, bizErr := validateGroupNickname(nicknameInGroup)
	if bizErr != nil {
		return nil, bizErr
	}

	var (
		member model.GroupMember
		user   model.User
	)

	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		lockedGroup, gErr := s.repo.LockGroupByID(ctx, tx, groupID)
		if gErr != nil {
			if errors.Is(gErr, gorm.ErrRecordNotFound) {
				return errcode.ErrGroupNotFound
			}
			return s.mapDBError(ctx, "group_update_me_lock_group", gErr)
		}
		gm, mErr := s.repo.LockGroupMember(ctx, tx, groupID, actorID)
		if mErr != nil {
			if errors.Is(mErr, gorm.ErrRecordNotFound) {
				return errcode.ErrNotGroupMember
			}
			return s.mapDBError(ctx, "group_update_me_lock_member", mErr)
		}
		if lockedGroup.Status != model.GroupStatusActive {
			return errcode.ErrGroupDisbanded
		}

		now := time.Now().UTC().Truncate(time.Millisecond)
		if updErr := s.repo.UpdateGroupMemberNickname(ctx, tx, groupID, actorID, cleanedNick, now); updErr != nil {
			return s.mapDBError(ctx, "group_update_me_nickname", updErr)
		}
		gm.NicknameInGroup = cleanedNick
		gm.UpdatedAt = now
		member = *gm

		users, uErr := s.repo.LockUsersAscending(ctx, tx, []uint64{actorID})
		if uErr != nil {
			return s.mapDBError(ctx, "group_update_me_lock_user", uErr)
		}
		user = users[actorID]
		return nil
	})
	if txErr != nil {
		return nil, s.asErrCode(ctx, "group_update_me", txErr)
	}

	onlineMap := s.lookupOnlineBestEffort(ctx, []uint64{actorID})
	return &UpdateMyMemberResult{
		Member: toGroupMemberDTO(member, user, onlineMap[actorID]),
	}, nil
}

// ListAudit returns cursor-paginated immutable group audit events (owner or admin only).
func (s *GroupService) ListAudit(ctx context.Context, actorID, groupID uint64, input ListGroupAuditInput) (*GroupAuditPage, *errcode.Error) {
	if s == nil || s.repo == nil {
		return nil, errcode.ErrInternalConsistency
	}
	if actorID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if groupID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid group_id")
	}
	if input.From != nil && input.To != nil && input.From.After(*input.To) {
		return nil, errcode.ErrInvalidValue.WithMessage("from must not be after to")
	}
	limit := input.Limit
	if limit == 0 {
		limit = DefaultGroupListLimit
	}
	if limit < 1 || limit > MaxGroupListLimit {
		return nil, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}

	fromStr := formatTimeFilterKey(input.From)
	toStr := formatTimeFilterKey(input.To)

	var cursorID uint64
	if trimmedCursor := strings.TrimSpace(input.Cursor); trimmedCursor != "" {
		var c groupAuditCursor
		if err := decodeScopedCursor(trimmedCursor, &c); err != nil ||
			c.Version != 1 || c.Scope != groupAuditCursorScope || c.UserID != actorID ||
			c.GroupID != groupID || c.From != fromStr || c.To != toStr ||
			c.Sort != "id_desc" || c.LastID == 0 {
			return nil, errcode.ErrInvalidValue.WithMessage("invalid cursor")
		}
		cursorID = c.LastID
	}

	if _, _, err := s.repo.GetGroupAndConversation(ctx, groupID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrGroupNotFound
		}
		return nil, s.mapDBError(ctx, "group_audit_get_group", err)
	}
	member, err := s.repo.GetGroupMember(ctx, groupID, actorID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrNotGroupMember
		}
		return nil, s.mapDBError(ctx, "group_audit_get_member", err)
	}
	if member.Role != model.GroupRoleOwner && member.Role != model.GroupRoleAdmin {
		return nil, errcode.ErrGroupRoleDenied
	}

	rows, hasMore, err := s.repo.ListGroupAuditLogs(ctx, repository.GroupAuditListFilter{
		GroupID:  groupID,
		From:     input.From,
		To:       input.To,
		CursorID: cursorID,
		Limit:    limit,
	})
	if err != nil {
		return nil, s.mapDBError(ctx, "group_audit_list", err)
	}

	items := make([]GroupAuditLogDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toGroupAuditLogDTO(row))
	}

	var nextCursor *string
	if hasMore && len(rows) > 0 {
		encoded, encErr := encodeCursor(groupAuditCursor{
			Version: 1,
			Scope:   groupAuditCursorScope,
			UserID:  actorID,
			GroupID: groupID,
			From:    fromStr,
			To:      toStr,
			Sort:    "id_desc",
			LastID:  rows[len(rows)-1].ID,
		})
		if encErr != nil {
			return nil, errcode.ErrInternalConsistency
		}
		nextCursor = &encoded
	}

	return &GroupAuditPage{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

func (s *GroupService) requireWriteDeps() *errcode.Error {
	if s == nil || s.repo == nil || s.idGen == nil {
		return errcode.ErrInternalConsistency
	}
	if s.seqAllocator == nil {
		return errcode.ErrRedisUnavailable
	}
	return nil
}

func (s *GroupService) lockActiveGroupAndActor(
	ctx context.Context,
	tx *gorm.DB,
	groupID uint64,
	actorID uint64,
	requireAdminOrOwner bool,
) (*model.Group, []model.GroupMember, model.GroupMember, *errcode.Error) {
	lockedGroup, gErr := s.repo.LockGroupByID(ctx, tx, groupID)
	if gErr != nil {
		if errors.Is(gErr, gorm.ErrRecordNotFound) {
			return nil, nil, model.GroupMember{}, errcode.ErrGroupNotFound
		}
		return nil, nil, model.GroupMember{}, s.mapDBError(ctx, "group_lock", gErr)
	}
	members, mErr := s.repo.LockGroupMembers(ctx, tx, groupID)
	if mErr != nil {
		return nil, nil, model.GroupMember{}, s.mapDBError(ctx, "group_lock_members", mErr)
	}
	actorMember, isMember := findMember(members, actorID)
	if !isMember {
		return nil, nil, model.GroupMember{}, errcode.ErrNotGroupMember
	}
	if lockedGroup.Status != model.GroupStatusActive {
		return nil, nil, model.GroupMember{}, errcode.ErrGroupDisbanded
	}
	if requireAdminOrOwner && actorMember.Role != model.GroupRoleOwner && actorMember.Role != model.GroupRoleAdmin {
		return nil, nil, model.GroupMember{}, errcode.ErrGroupRoleDenied
	}
	return lockedGroup, members, actorMember, nil
}

func (s *GroupService) validateGroupAvatarAssetTx(
	ctx context.Context,
	tx *gorm.DB,
	actorID uint64,
	groupID uint64,
	assetID string,
) (string, *errcode.Error) {
	asset, err := s.repo.LockAssetByAssetID(ctx, tx, assetID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", errcode.ErrAssetNotFound
		}
		return "", s.mapDBError(ctx, "group_avatar_lock_asset", err)
	}
	if asset.Status != model.AssetStatusActive {
		return "", errcode.ErrAssetNotFound
	}
	if asset.Purpose != model.AssetPurposeGroupAvatar {
		return "", errcode.ErrInvalidValue.WithMessage("avatar_asset_id must reference a group_avatar asset")
	}

	alreadyBoundToThisGroup := groupID > 0 &&
		asset.RefType == model.AssetRefGroupAvatar &&
		asset.RefID != nil && *asset.RefID == groupID

	if !alreadyBoundToThisGroup {
		if asset.OwnerID != actorID {
			return "", errcode.ErrAssetAccessDenied
		}
		if asset.RefType != model.AssetRefNone {
			return "", errcode.ErrAssetAccessDenied
		}
	}
	return fmt.Sprintf("/api/v1/uploads/%s/download", asset.AssetID), nil
}

func (s *GroupService) releaseConvLock(ctx context.Context, convID uint64, lockToken string) {
	if s.seqAllocator == nil || convID == 0 || lockToken == "" {
		return
	}
	rbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.seqAllocator.ReleaseConvLock(rbCtx, convID, lockToken); err != nil {
		s.log.WarnContext(ctx, "group_release_conv_lock_failed", slog.Uint64("conversation_id", convID), slog.Any("error", err))
	}
}

func (s *GroupService) rollbackSeq(ctx context.Context, convID uint64, seqRes *repository.SeqReservation) {
	if seqRes == nil || seqRes.Rollback == nil {
		return
	}
	rbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if rbErr := seqRes.Rollback(rbCtx); rbErr != nil {
		s.log.WarnContext(ctx, "group_seq_rollback_failed", slog.Uint64("conversation_id", convID), slog.Any("error", rbErr))
	}
}

type noticeInput struct {
	Event          string
	ConversationID uint64
	ActorID        uint64
	TargetUserID   *uint64
	Payload        map[string]any
}

func (s *GroupService) notifyGroupUsers(ctx context.Context, userIDs []uint64, sysMsg *GroupMessageDTO, notice noticeInput) {
	if s == nil || s.notifier == nil || len(userIDs) == 0 {
		return
	}
	deliverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()

	if sysMsg != nil {
		pushPayload := map[string]any{
			"push_type":       "message",
			"message":         sysMsg,
			"delivery_status": model.ReceiptStatusSent,
		}
		if err := s.notifier.DeliverToUsers(deliverCtx, userIDs, "msg.push", pushPayload); err != nil {
			s.log.WarnContext(ctx, "group_notify_msg_push_failed", slog.String("event", notice.Event), slog.Any("error", err))
		}
	}

	if notice.Event != "" {
		convStr := strconv.FormatUint(notice.ConversationID, 10)
		actorStr := strconv.FormatUint(notice.ActorID, 10)
		var targetStr *string
		if notice.TargetUserID != nil && *notice.TargetUserID > 0 {
			v := strconv.FormatUint(*notice.TargetUserID, 10)
			targetStr = &v
		}
		noticePayload := map[string]any{
			"event":           notice.Event,
			"notice_id":       "n_" + uuid.NewString(),
			"conversation_id": convStr,
			"actor_id":        actorStr,
			"target_user_id":  targetStr,
			"payload":         notice.Payload,
		}
		if err := s.notifier.DeliverToUsers(deliverCtx, userIDs, "system.notice", noticePayload); err != nil {
			s.log.WarnContext(ctx, "group_notify_system_notice_failed", slog.String("event", notice.Event), slog.Any("error", err))
		}
	}
}

func (s *GroupService) lookupOnlineBestEffort(ctx context.Context, userIDs []uint64) map[uint64]bool {
	if s.presence == nil || len(userIDs) == 0 {
		return map[uint64]bool{}
	}
	res, err := s.presence.BatchIsOnline(ctx, userIDs)
	if err != nil {
		s.log.WarnContext(ctx, "group_presence_lookup_failed", slog.Any("error", err))
		return map[uint64]bool{}
	}
	return res
}

func (s *GroupService) mapSeqError(ctx context.Context, op string, err error) *errcode.Error {
	if errors.Is(err, repository.ErrConvLockTimeout) {
		s.log.WarnContext(ctx, op, slog.Any("error", err))
		return errcode.ErrInternalConsistency
	}
	s.log.ErrorContext(ctx, op, slog.Any("error", err))
	return errcode.ErrRedisUnavailable
}

func (s *GroupService) mapDBError(ctx context.Context, op string, err error) *errcode.Error {
	var bizErr *errcode.Error
	if errors.As(err, &bizErr) {
		return bizErr
	}
	s.log.ErrorContext(ctx, op, slog.Any("error", err))
	if isInfrastructureUnavailableErr(err) {
		return errcode.ErrMySQLUnavailable
	}
	return errcode.ErrInternalConsistency
}

func (s *GroupService) asErrCode(ctx context.Context, op string, err error) *errcode.Error {
	var bizErr *errcode.Error
	if errors.As(err, &bizErr) {
		return bizErr
	}
	return s.mapDBError(ctx, op, err)
}

func validateGroupName(raw string) (string, *errcode.Error) {
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return "", errcode.ErrInvalidValue.WithMessage("name contains invalid characters")
	}
	trimmed := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(trimmed)
	if n < 1 || n > MaxGroupNameRunes {
		return "", errcode.ErrInvalidValue.WithMessage("name must be between 1 and 100 characters")
	}
	return trimmed, nil
}

func validateGroupAnnouncement(raw *string) (*string, *errcode.Error) {
	if raw == nil {
		return nil, nil
	}
	val := *raw
	if !utf8.ValidString(val) || strings.ContainsRune(val, 0) {
		return nil, errcode.ErrInvalidValue.WithMessage("announcement contains invalid characters")
	}
	if utf8.RuneCountInString(val) > MaxGroupAnnounceRunes {
		return nil, errcode.ErrInvalidValue.WithMessage("announcement must be at most 2000 characters")
	}
	cp := strings.Clone(val)
	return &cp, nil
}

func validateGroupNickname(raw string) (string, *errcode.Error) {
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return "", errcode.ErrInvalidValue.WithMessage("nickname_in_group contains invalid characters")
	}
	trimmed := strings.TrimSpace(raw)
	if utf8.RuneCountInString(trimmed) > MaxGroupNicknameRunes {
		return "", errcode.ErrInvalidValue.WithMessage("nickname_in_group must be at most 64 characters")
	}
	return trimmed, nil
}

func normalizeOptionalAssetID(raw *string) (string, *errcode.Error) {
	if raw == nil {
		return "", nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return "", nil
	}
	if !utf8.ValidString(trimmed) || strings.ContainsAny(trimmed, " \t\r\n\x00") || len(trimmed) > MaxAssetIDLength {
		return "", errcode.ErrInvalidValue.WithMessage("invalid avatar_asset_id")
	}
	return trimmed, nil
}

func dedupeExcludingSelf(ids []uint64, self uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 || id == self {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func dedupePositiveUint64(ids []uint64) []uint64 {
	return dedupeExcludingSelf(ids, 0)
}

func findMember(members []model.GroupMember, userID uint64) (model.GroupMember, bool) {
	for _, m := range members {
		if m.UserID == userID {
			return m, true
		}
	}
	return model.GroupMember{}, false
}

func memberUserIDs(members []model.GroupMember) []uint64 {
	ids := make([]uint64, 0, len(members))
	for _, m := range members {
		if m.UserID > 0 {
			ids = append(ids, m.UserID)
		}
	}
	return ids
}

func collectMemberUserIDs(members []model.GroupMember, excludeUserID uint64) []uint64 {
	ids := make([]uint64, 0, len(members))
	for _, m := range members {
		if m.UserID > 0 && m.UserID != excludeUserID {
			ids = append(ids, m.UserID)
		}
	}
	return ids
}

func collectExcluding(ids []uint64, exclude uint64) []uint64 {
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && id != exclude {
			out = append(out, id)
		}
	}
	return out
}

func displayActorName(u model.User) string {
	if strings.TrimSpace(u.Nickname) != "" {
		return u.Nickname
	}
	if strings.TrimSpace(u.Username) != "" {
		return u.Username
	}
	return strconv.FormatUint(u.ID, 10)
}

func buildGroupUpdateSystemContent(actorName string, changedFields []string, groupName string) string {
	parts := make([]string, 0, len(changedFields))
	for _, field := range changedFields {
		switch field {
		case "name":
			parts = append(parts, fmt.Sprintf("修改群名为“%s”", groupName))
		case "avatar":
			parts = append(parts, "更新了群头像")
		case "announcement":
			parts = append(parts, "更新了群公告")
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%s 更新了群资料", actorName)
	}
	return actorName + " " + strings.Join(parts, "、")
}

func buildGroupAuditEntry(
	action string,
	actorID uint64,
	actorRole string,
	targetType string,
	targetID uint64,
	groupID uint64,
	convID uint64,
	reason string,
	detail map[string]any,
	meta GroupAuditMeta,
	now time.Time,
) *model.AuditLog {
	gid := groupID
	cid := convID
	return &model.AuditLog{
		EventID:        "evt-" + uuid.NewString(),
		Scope:          model.AuditScopeGroup,
		Action:         action,
		ActorID:        actorID,
		ActorRole:      actorRole,
		TargetType:     targetType,
		TargetID:       targetID,
		GroupID:        &gid,
		ConversationID: &cid,
		Reason:         truncateRunes(reason, 512),
		Detail:         marshalExtraJSON(detail),
		RequestID:      truncateRunes(meta.RequestID, 64),
		IPAddress:      truncateRunes(meta.IPAddress, 64),
		UserAgent:      truncateRunes(meta.UserAgent, 512),
		CreatedAt:      now,
	}
}

func marshalExtraJSON(payload map[string]any) model.NullableJSON {
	if len(payload) == 0 {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return model.NullableJSON(raw)
}

func formatUint64Slice(ids []uint64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatUint(id, 10))
	}
	return out
}

func truncatePreview(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return strings.Clone(s)
	}
	return string(runes[:maxRunes])
}

func truncateRunes(s string, maxRunes int) string {
	return truncatePreview(s, maxRunes)
}

func toGroupDTO(g model.Group, convID uint64, memberCount int) GroupDTO {
	return GroupDTO{
		ID:             g.ID,
		ConversationID: convID,
		Name:           g.Name,
		AvatarURL:      g.AvatarURL,
		Announcement:   g.Announcement,
		OwnerID:        g.OwnerID,
		Status:         g.Status,
		MemberCount:    memberCount,
		MaxMembers:     g.MaxMembers,
		CreatedAt:      FormatUTCMillis(g.CreatedAt),
		UpdatedAt:      FormatUTCMillis(g.UpdatedAt),
	}
}

func toGroupMemberDTO(gm model.GroupMember, u model.User, online bool) GroupMemberDTO {
	return GroupMemberDTO{
		UserID:          gm.UserID,
		Nickname:        u.Nickname,
		AvatarURL:       u.AvatarURL,
		NicknameInGroup: gm.NicknameInGroup,
		Role:            gm.Role,
		Online:          online,
		JoinedAt:        FormatUTCMillis(gm.JoinedAt),
	}
}

func toGroupConversationDTO(c model.Conversation, g model.Group, cm *model.ConversationMember, canSend bool) GroupConversationDTO {
	gid := strconv.FormatUint(g.ID, 10)
	dto := GroupConversationDTO{
		ID:             c.ID,
		Type:           model.ConversationTypeGroup,
		Peer:           nil,
		GroupID:        &gid,
		Title:          g.Name,
		AvatarURL:      g.AvatarURL,
		LastMsgID:      c.LastMsgID,
		LastSeq:        c.LastSeq,
		LastMsgPreview: c.LastMsgPreview,
		LastMsgAt:      FormatUTCMillisPtr(c.LastMsgAt),
		IsVisible:      true,
		CanSend:        canSend,
	}
	if cm != nil {
		dto.LastDeliveredSeq = cm.LastDeliveredSeq
		dto.LastReadSeq = cm.LastReadSeq
		dto.UnreadCount = cm.UnreadCount
		dto.IsPinned = cm.IsPinned
		dto.IsMuted = cm.IsMuted
		dto.IsVisible = cm.IsVisible
	}
	return dto
}

func toGroupMessageDTO(m model.Message) GroupMessageDTO {
	return GroupMessageDTO{
		MsgID:          m.ID,
		ClientMsgID:    m.ClientMsgID,
		ConversationID: m.ConversationID,
		Seq:            m.Seq,
		SenderID:       m.SenderID,
		MsgType:        m.MsgType,
		Content:        m.Content,
		Extra:          m.Extra,
		Status:         m.Status,
		CreatedAt:      FormatUTCMillis(m.CreatedAt),
		RecalledAt:     FormatUTCMillisPtr(m.RecalledAt),
	}
}

func toGroupConversationMemberDTO(cm model.ConversationMember) GroupConversationMemberDTO {
	return GroupConversationMemberDTO{
		ConversationID:   cm.ConversationID,
		UserID:           cm.UserID,
		LastDeliveredSeq: cm.LastDeliveredSeq,
		LastReadSeq:      cm.LastReadSeq,
		UnreadCount:      cm.UnreadCount,
		IsVisible:        cm.IsVisible,
		JoinedAt:         FormatUTCMillis(cm.JoinedAt),
	}
}

func toGroupAuditLogDTO(a model.AuditLog) GroupAuditLogDTO {
	var gidStr *string
	if a.GroupID != nil && *a.GroupID > 0 {
		v := strconv.FormatUint(*a.GroupID, 10)
		gidStr = &v
	}
	var cidStr *string
	if a.ConversationID != nil && *a.ConversationID > 0 {
		v := strconv.FormatUint(*a.ConversationID, 10)
		cidStr = &v
	}
	return GroupAuditLogDTO{
		ID:             a.ID,
		EventID:        a.EventID,
		Scope:          a.Scope,
		Action:         a.Action,
		ActorID:        a.ActorID,
		ActorRole:      a.ActorRole,
		TargetType:     a.TargetType,
		TargetID:       a.TargetID,
		GroupID:        gidStr,
		ConversationID: cidStr,
		Reason:         a.Reason,
		Detail:         a.Detail,
		RequestID:      a.RequestID,
		CreatedAt:      FormatUTCMillis(a.CreatedAt),
	}
}

type groupListCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"s"`
	UserID  uint64 `json:"u,string"`
	Status  string `json:"st"`
	Sort    string `json:"so"`
	LastID  uint64 `json:"id,string"`
}

type groupMemberCursor struct {
	Version    int    `json:"v"`
	Scope      string `json:"s"`
	UserID     uint64 `json:"u,string"`
	GroupID    uint64 `json:"g,string"`
	Role       string `json:"r"`
	OnlineOnly bool   `json:"o"`
	Sort       string `json:"so"`
	LastID     uint64 `json:"id,string"`
}

type groupAuditCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"s"`
	UserID  uint64 `json:"u,string"`
	GroupID uint64 `json:"g,string"`
	From    string `json:"f"`
	To      string `json:"t"`
	Sort    string `json:"so"`
	LastID  uint64 `json:"id,string"`
}

func encodeCursor(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeScopedCursor(raw string, dst any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("trailing cursor bytes")
	}
	return nil
}

func formatTimeFilterKey(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
