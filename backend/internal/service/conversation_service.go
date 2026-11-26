package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

const (
	DefaultConversationListLimit = 20
	MaxConversationListLimit     = 100

	conversationListCursorScope   = "conversations"
	conversationMemberCursorScope = "conv_members"
)

// ConversationStore defines persistence queries required by ConversationService.
// *repository.ConversationRepository satisfies this interface directly.
type ConversationStore interface {
	ListUserConversations(ctx context.Context, userID uint64, includeHidden bool, cursor *repository.ConversationListCursor, limit int) ([]repository.ConversationListItemRow, error)
	GetConversationByID(ctx context.Context, convID uint64) (*model.Conversation, error)
	GetConversationMember(ctx context.Context, convID, userID uint64) (*model.ConversationMember, error)
	ListConversationMembers(ctx context.Context, convID uint64, convType string, groupID *uint64, cursorID uint64, limit int) ([]model.ConversationMember, error)
	UpdateMemberSettings(ctx context.Context, convID, userID uint64, isPinned, isMuted, isVisible *bool, now time.Time) (*model.ConversationMember, error)
	BatchGetUsersByIDs(ctx context.Context, userIDs []uint64) (map[uint64]model.User, error)
	BatchGetFriendshipsForUser(ctx context.Context, userID uint64, peerIDs []uint64) (map[uint64]string, map[uint64]bool, error)
	BatchGetGroupsByIDs(ctx context.Context, groupIDs []uint64) (map[uint64]model.Group, map[uint64]uint32, error)
	GetGroupByID(ctx context.Context, groupID uint64) (*model.Group, uint32, error)
	GetGroupMember(ctx context.Context, groupID, userID uint64) (*model.GroupMember, error)
	BatchGetGroupMembers(ctx context.Context, groupID uint64, userIDs []uint64) (map[uint64]model.GroupMember, error)
}

// OnlineStatusProvider resolves merged multi-device online status for batches of users.
// *repository.FriendshipRepository satisfies this interface directly.
type OnlineStatusProvider interface {
	GetOnlineStatusBatch(ctx context.Context, userIDs []uint64) (map[uint64]bool, error)
}

// ChatUserDTO matches the frozen `User` object in docs/api.md.
type ChatUserDTO struct {
	ID           uint64  `json:"id,string"`
	Username     string  `json:"username"`
	Nickname     string  `json:"nickname"`
	AvatarURL    string  `json:"avatar_url"`
	Signature    string  `json:"signature"`
	Online       bool    `json:"online"`
	LastOnlineAt *string `json:"last_online_at"`
}

// GroupDTO matches the frozen `Group` object in docs/api.md.
type GroupDTO struct {
	ID             uint64  `json:"id,string"`
	ConversationID uint64  `json:"conversation_id,string"`
	Name           string  `json:"name"`
	AvatarURL      string  `json:"avatar_url"`
	Announcement   *string `json:"announcement"`
	OwnerID        uint64  `json:"owner_id,string"`
	Status         string  `json:"status"`
	MemberCount    uint32  `json:"member_count"`
	MaxMembers     uint32  `json:"max_members"`
}

// ConversationDTO matches the frozen `Conversation` object in docs/api.md.
type ConversationDTO struct {
	ID               uint64       `json:"id,string"`
	Type             string       `json:"type"`
	Peer             *ChatUserDTO `json:"peer"`
	GroupID          *string      `json:"group_id"`
	Title            string       `json:"title"`
	AvatarURL        string       `json:"avatar_url"`
	LastMsgID        uint64       `json:"last_msg_id,string"`
	LastSeq          uint64       `json:"last_seq,string"`
	LastMsgPreview   string       `json:"last_msg_preview"`
	LastMsgAt        *string      `json:"last_msg_at"`
	LastDeliveredSeq uint64       `json:"last_delivered_seq,string"`
	LastReadSeq      uint64       `json:"last_read_seq,string"`
	UnreadCount      uint32       `json:"unread_count"`
	IsPinned         bool         `json:"is_pinned"`
	IsMuted          bool         `json:"is_muted"`
	IsVisible        bool         `json:"is_visible"`
	CanSend          bool         `json:"can_send"`
}

// ConversationListPageDTO matches GET /api/v1/conversations response data.
type ConversationListPageDTO struct {
	Items      []ConversationDTO `json:"items"`
	NextCursor *string           `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
}

// ConversationDetailDTO matches GET /api/v1/conversations/{conversation_id} response data.
type ConversationDetailDTO struct {
	Conversation ConversationDTO `json:"conversation"`
	Role         *string         `json:"role"`
	Peer         *ChatUserDTO    `json:"peer"`
	Group        *GroupDTO       `json:"group"`
}

// UpdateConversationSettingsInput holds validated PATCH settings fields.
type UpdateConversationSettingsInput struct {
	IsPinned  *bool
	IsMuted   *bool
	IsVisible *bool
}

// ConversationSettingsDTO matches PATCH /api/v1/conversations/{conversation_id}/settings response data.
type ConversationSettingsDTO struct {
	ConversationID uint64 `json:"conversation_id,string"`
	IsPinned       bool   `json:"is_pinned"`
	IsMuted        bool   `json:"is_muted"`
	IsVisible      bool   `json:"is_visible"`
	UpdatedAt      string `json:"updated_at"`
}

// ConversationMemberDTO matches a member item in GET /api/v1/conversations/{conversation_id}/members.
type ConversationMemberDTO struct {
	User             ChatUserDTO `json:"user"`
	Role             *string     `json:"role"`
	NicknameInGroup  string      `json:"nickname_in_group"`
	Online           bool        `json:"online"`
	JoinedAt         string      `json:"joined_at"`
	LastDeliveredSeq uint64      `json:"last_delivered_seq,string"`
	LastReadSeq      uint64      `json:"last_read_seq,string"`
}

// ConversationMemberPageDTO matches GET /api/v1/conversations/{conversation_id}/members response data.
type ConversationMemberPageDTO struct {
	Items      []ConversationMemberDTO `json:"items"`
	NextCursor *string                 `json:"next_cursor"`
	HasMore    bool                    `json:"has_more"`
}

type conversationListCursorPayload struct {
	Version        int    `json:"v"`
	Scope          string `json:"s"`
	UserID         uint64 `json:"u,string"`
	IncludeHidden  bool   `json:"h"`
	IsPinned       bool   `json:"p"`
	UpdatedAtMs    int64  `json:"t"`
	ConversationID uint64 `json:"id,string"`
}

type conversationMemberCursorPayload struct {
	Version        int    `json:"v"`
	Scope          string `json:"s"`
	UserID         uint64 `json:"u,string"`
	ConversationID uint64 `json:"c,string"`
	LastID         uint64 `json:"id,string"`
}

// ConversationService implements conversation list, detail, settings, and member list queries.
type ConversationService struct {
	store    ConversationStore
	presence OnlineStatusProvider
	log      *slog.Logger
}

// NewConversationService constructs a ConversationService.
func NewConversationService(store ConversationStore, presence OnlineStatusProvider, log *slog.Logger) *ConversationService {
	if log == nil {
		log = slog.Default()
	}
	return &ConversationService{
		store:    store,
		presence: presence,
		log:      log,
	}
}

// ListConversations returns userID's paginated conversation list with batch-assembled peers, groups, and presence.
func (s *ConversationService) ListConversations(
	ctx context.Context,
	userID uint64,
	includeHidden bool,
	cursor string,
	limit int,
) (ConversationListPageDTO, *errcode.Error) {
	empty := ConversationListPageDTO{
		Items:      []ConversationDTO{},
		NextCursor: nil,
		HasMore:    false,
	}
	if userID == 0 {
		return empty, errcode.ErrAccessTokenInvalid
	}
	if limit == 0 {
		limit = DefaultConversationListLimit
	}
	if limit < 1 || limit > MaxConversationListLimit {
		return empty, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}
	if s == nil || s.store == nil {
		return empty, errcode.ErrMySQLUnavailable
	}

	var parsedCursor *repository.ConversationListCursor
	if trimmed := strings.TrimSpace(cursor); trimmed != "" {
		decoded, decErr := decodeConversationListCursor(trimmed, userID, includeHidden)
		if decErr != nil {
			return empty, decErr
		}
		parsedCursor = decoded
	}

	rows, err := s.store.ListUserConversations(ctx, userID, includeHidden, parsedCursor, limit)
	if err != nil {
		return empty, s.mapStoreError(ctx, userID, "list_user_conversations", err)
	}
	if len(rows) == 0 {
		return empty, nil
	}

	hasMore := false
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	peerIDs := make([]uint64, 0, len(rows))
	peerByConvID := make(map[uint64]uint64, len(rows))
	groupIDs := make([]uint64, 0, len(rows))
	for _, row := range rows {
		switch row.Type {
		case model.ConversationTypeSingle:
			if peerID, ok := repository.ParseDirectKeyPeer(row.DirectKey, userID); ok {
				peerIDs = append(peerIDs, peerID)
				peerByConvID[row.ConversationID] = peerID
			}
		case model.ConversationTypeGroup:
			if row.GroupID != nil && *row.GroupID > 0 {
				groupIDs = append(groupIDs, *row.GroupID)
			}
		}
	}

	usersMap, err := s.store.BatchGetUsersByIDs(ctx, peerIDs)
	if err != nil {
		return empty, s.mapStoreError(ctx, userID, "batch_get_conversation_peers", err)
	}
	remarksMap, activeFriendsMap, err := s.store.BatchGetFriendshipsForUser(ctx, userID, peerIDs)
	if err != nil {
		return empty, s.mapStoreError(ctx, userID, "batch_get_conversation_friendships", err)
	}
	groupsMap, _, err := s.store.BatchGetGroupsByIDs(ctx, groupIDs)
	if err != nil {
		return empty, s.mapStoreError(ctx, userID, "batch_get_conversation_groups", err)
	}

	onlineMap := s.lookupOnlineBatch(ctx, peerIDs)

	items := make([]ConversationDTO, 0, len(rows))
	for _, row := range rows {
		dto := ConversationDTO{
			ID:               row.ConversationID,
			Type:             row.Type,
			LastMsgID:        row.LastMsgID,
			LastSeq:          row.LastSeq,
			LastMsgPreview:   row.LastMsgPreview,
			LastMsgAt:        FormatUTCMillisPtr(row.LastMsgAt),
			LastDeliveredSeq: row.LastDeliveredSeq,
			LastReadSeq:      row.LastReadSeq,
			UnreadCount:      row.UnreadCount,
			IsPinned:         row.IsPinned,
			IsMuted:          row.IsMuted,
			IsVisible:        row.IsVisible,
		}

		if row.Type == model.ConversationTypeSingle {
			if peerID, ok := peerByConvID[row.ConversationID]; ok {
				if u, exists := usersMap[peerID]; exists {
					peerDTO := toChatUserDTO(u, onlineMap[peerID])
					dto.Peer = &peerDTO
					dto.AvatarURL = u.AvatarURL
					if remark := remarksMap[peerID]; remark != "" {
						dto.Title = remark
					} else {
						dto.Title = u.Nickname
					}
				}
				dto.CanSend = activeFriendsMap[peerID]
			}
		} else if row.Type == model.ConversationTypeGroup && row.GroupID != nil && *row.GroupID > 0 {
			gidStr := strconv.FormatUint(*row.GroupID, 10)
			dto.GroupID = &gidStr
			if g, exists := groupsMap[*row.GroupID]; exists {
				dto.Title = g.Name
				dto.AvatarURL = g.AvatarURL
				dto.CanSend = g.Status == model.GroupStatusActive
			}
		}

		items = append(items, dto)
	}

	var nextCursor *string
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		nc := encodeConversationListCursor(userID, includeHidden, last.IsPinned, last.MemberUpdatedAt, last.ConversationID)
		nextCursor = &nc
	}

	return ConversationListPageDTO{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// GetConversationDetail returns full conversation metadata, caller role, and peer or group details.
func (s *ConversationService) GetConversationDetail(
	ctx context.Context,
	userID uint64,
	convID uint64,
) (*ConversationDetailDTO, *errcode.Error) {
	if userID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid conversation_id")
	}
	if s == nil || s.store == nil {
		return nil, errcode.ErrMySQLUnavailable
	}

	conv, err := s.store.GetConversationByID(ctx, convID)
	if err != nil {
		return nil, s.mapStoreError(ctx, userID, "get_conversation_by_id", err)
	}
	member, err := s.store.GetConversationMember(ctx, convID, userID)
	if err != nil {
		return nil, s.mapStoreError(ctx, userID, "get_conversation_member", err)
	}

	dto := ConversationDTO{
		ID:               conv.ID,
		Type:             conv.Type,
		LastMsgID:        conv.LastMsgID,
		LastSeq:          conv.LastSeq,
		LastMsgPreview:   conv.LastMsgPreview,
		LastMsgAt:        FormatUTCMillisPtr(conv.LastMsgAt),
		LastDeliveredSeq: member.LastDeliveredSeq,
		LastReadSeq:      member.LastReadSeq,
		UnreadCount:      member.UnreadCount,
		IsPinned:         member.IsPinned,
		IsMuted:          member.IsMuted,
		IsVisible:        member.IsVisible,
	}

	detail := &ConversationDetailDTO{}
	switch conv.Type {
	case model.ConversationTypeSingle:
		if peerID, ok := repository.ParseDirectKeyPeer(conv.DirectKey, userID); ok {
			usersMap, uErr := s.store.BatchGetUsersByIDs(ctx, []uint64{peerID})
			if uErr != nil {
				return nil, s.mapStoreError(ctx, userID, "get_detail_peer_user", uErr)
			}
			remarksMap, activeFriendsMap, fErr := s.store.BatchGetFriendshipsForUser(ctx, userID, []uint64{peerID})
			if fErr != nil {
				return nil, s.mapStoreError(ctx, userID, "get_detail_friendship", fErr)
			}
			onlineMap := s.lookupOnlineBatch(ctx, []uint64{peerID})
			if u, exists := usersMap[peerID]; exists {
				peerDTO := toChatUserDTO(u, onlineMap[peerID])
				dto.Peer = &peerDTO
				detail.Peer = &peerDTO
				dto.AvatarURL = u.AvatarURL
				if remark := remarksMap[peerID]; remark != "" {
					dto.Title = remark
				} else {
					dto.Title = u.Nickname
				}
			}
			dto.CanSend = activeFriendsMap[peerID]
		}
	case model.ConversationTypeGroup:
		if conv.GroupID == nil || *conv.GroupID == 0 {
			return nil, errcode.ErrGroupNotFound
		}
		gm, gmErr := s.store.GetGroupMember(ctx, *conv.GroupID, userID)
		if gmErr != nil {
			return nil, s.mapStoreError(ctx, userID, "get_detail_group_member", gmErr)
		}
		group, memberCount, gErr := s.store.GetGroupByID(ctx, *conv.GroupID)
		if gErr != nil {
			return nil, s.mapStoreError(ctx, userID, "get_detail_group", gErr)
		}
		gidStr := strconv.FormatUint(group.ID, 10)
		dto.GroupID = &gidStr
		dto.Title = group.Name
		dto.AvatarURL = group.AvatarURL
		dto.CanSend = group.Status == model.GroupStatusActive

		role := gm.Role
		detail.Role = &role
		detail.Group = &GroupDTO{
			ID:             group.ID,
			ConversationID: conv.ID,
			Name:           group.Name,
			AvatarURL:      group.AvatarURL,
			Announcement:   group.Announcement,
			OwnerID:        group.OwnerID,
			Status:         group.Status,
			MemberCount:    memberCount,
			MaxMembers:     group.MaxMembers,
		}
	default:
		return nil, errcode.ErrInternalConsistency
	}

	detail.Conversation = dto
	return detail, nil
}

// UpdateSettings updates the caller's is_pinned, is_muted, and/or is_visible settings for convID.
func (s *ConversationService) UpdateSettings(
	ctx context.Context,
	userID uint64,
	convID uint64,
	input UpdateConversationSettingsInput,
) (*ConversationSettingsDTO, *errcode.Error) {
	if userID == 0 {
		return nil, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid conversation_id")
	}
	if input.IsPinned == nil && input.IsMuted == nil && input.IsVisible == nil {
		return nil, errcode.ErrInvalidParams.WithMessage("at least one of is_pinned, is_muted, or is_visible must be provided")
	}
	if s == nil || s.store == nil {
		return nil, errcode.ErrMySQLUnavailable
	}

	updated, err := s.store.UpdateMemberSettings(ctx, convID, userID, input.IsPinned, input.IsMuted, input.IsVisible, time.Now().UTC())
	if err != nil {
		return nil, s.mapStoreError(ctx, userID, "update_conversation_settings", err)
	}

	return &ConversationSettingsDTO{
		ConversationID: updated.ConversationID,
		IsPinned:       updated.IsPinned,
		IsMuted:        updated.IsMuted,
		IsVisible:      updated.IsVisible,
		UpdatedAt:      FormatUTCMillis(updated.UpdatedAt),
	}, nil
}

// ListMembers returns a cursor-paginated list of conversation members with profiles, roles, and watermarks.
func (s *ConversationService) ListMembers(
	ctx context.Context,
	userID uint64,
	convID uint64,
	cursor string,
	limit int,
) (ConversationMemberPageDTO, *errcode.Error) {
	empty := ConversationMemberPageDTO{
		Items:      []ConversationMemberDTO{},
		NextCursor: nil,
		HasMore:    false,
	}
	if userID == 0 {
		return empty, errcode.ErrAccessTokenInvalid
	}
	if convID == 0 {
		return empty, errcode.ErrInvalidValue.WithMessage("invalid conversation_id")
	}
	if limit == 0 {
		limit = DefaultConversationListLimit
	}
	if limit < 1 || limit > MaxConversationListLimit {
		return empty, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}
	if s == nil || s.store == nil {
		return empty, errcode.ErrMySQLUnavailable
	}

	var cursorID uint64
	if trimmed := strings.TrimSpace(cursor); trimmed != "" {
		decoded, decErr := decodeConversationMemberCursor(trimmed, userID, convID)
		if decErr != nil {
			return empty, decErr
		}
		cursorID = decoded
	}

	conv, err := s.store.GetConversationByID(ctx, convID)
	if err != nil {
		return empty, s.mapStoreError(ctx, userID, "list_members_get_conv", err)
	}
	if _, err := s.store.GetConversationMember(ctx, convID, userID); err != nil {
		return empty, s.mapStoreError(ctx, userID, "list_members_check_cm", err)
	}
	if conv.Type == model.ConversationTypeGroup {
		if conv.GroupID == nil || *conv.GroupID == 0 {
			return empty, errcode.ErrGroupNotFound
		}
		if _, gmErr := s.store.GetGroupMember(ctx, *conv.GroupID, userID); gmErr != nil {
			return empty, s.mapStoreError(ctx, userID, "list_members_check_gm", gmErr)
		}
	}

	rows, err := s.store.ListConversationMembers(ctx, convID, conv.Type, conv.GroupID, cursorID, limit)
	if err != nil {
		return empty, s.mapStoreError(ctx, userID, "list_conversation_members", err)
	}
	if len(rows) == 0 {
		return empty, nil
	}

	hasMore := false
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	userIDs := make([]uint64, len(rows))
	for i, row := range rows {
		userIDs[i] = row.UserID
	}

	usersMap, err := s.store.BatchGetUsersByIDs(ctx, userIDs)
	if err != nil {
		return empty, s.mapStoreError(ctx, userID, "list_members_batch_users", err)
	}
	var gmMap map[uint64]model.GroupMember
	if conv.Type == model.ConversationTypeGroup && conv.GroupID != nil && *conv.GroupID > 0 {
		gmMap, err = s.store.BatchGetGroupMembers(ctx, *conv.GroupID, userIDs)
		if err != nil {
			return empty, s.mapStoreError(ctx, userID, "list_members_batch_gm", err)
		}
	}
	onlineMap := s.lookupOnlineBatch(ctx, userIDs)

	items := make([]ConversationMemberDTO, 0, len(rows))
	for _, row := range rows {
		u, exists := usersMap[row.UserID]
		if !exists {
			continue
		}
		online := onlineMap[row.UserID]
		var rolePtr *string
		var nickInGroup string
		joinedAt := row.JoinedAt
		if gm, ok := gmMap[row.UserID]; ok {
			r := gm.Role
			rolePtr = &r
			nickInGroup = gm.NicknameInGroup
			if !gm.JoinedAt.IsZero() {
				joinedAt = gm.JoinedAt
			}
		}

		items = append(items, ConversationMemberDTO{
			User:             toChatUserDTO(u, online),
			Role:             rolePtr,
			NicknameInGroup:  nickInGroup,
			Online:           online,
			JoinedAt:         FormatUTCMillis(joinedAt),
			LastDeliveredSeq: row.LastDeliveredSeq,
			LastReadSeq:      row.LastReadSeq,
		})
	}

	var nextCursor *string
	if hasMore && len(rows) > 0 {
		nc := encodeConversationMemberCursor(userID, convID, rows[len(rows)-1].ID)
		nextCursor = &nc
	}

	return ConversationMemberPageDTO{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

func (s *ConversationService) lookupOnlineBatch(ctx context.Context, userIDs []uint64) map[uint64]bool {
	if s.presence == nil || len(userIDs) == 0 {
		return map[uint64]bool{}
	}
	onlineMap, err := s.presence.GetOnlineStatusBatch(ctx, userIDs)
	if err != nil {
		s.log.WarnContext(ctx, "conversation_presence_lookup_failed",
			slog.Int("user_count", len(userIDs)),
			slog.Any("error", err),
		)
		return map[uint64]bool{}
	}
	return onlineMap
}

func (s *ConversationService) mapStoreError(ctx context.Context, userID uint64, op string, err error) *errcode.Error {
	switch {
	case errors.Is(err, repository.ErrConversationNotFound):
		return errcode.ErrConversationNotFound
	case errors.Is(err, repository.ErrNotConversationMember):
		return errcode.ErrNotConversationMember
	case errors.Is(err, repository.ErrGroupNotFound):
		return errcode.ErrGroupNotFound
	}
	s.log.ErrorContext(ctx, op+"_failed",
		slog.Uint64("user_id", userID),
		slog.Any("error", err),
	)
	if isInfrastructureUnavailableErr(err) {
		return errcode.ErrMySQLUnavailable
	}
	return errcode.ErrInternalConsistency
}

func toChatUserDTO(u model.User, online bool) ChatUserDTO {
	return ChatUserDTO{
		ID:           u.ID,
		Username:     u.Username,
		Nickname:     u.Nickname,
		AvatarURL:    u.AvatarURL,
		Signature:    u.Signature,
		Online:       online,
		LastOnlineAt: FormatUTCMillisPtr(u.LastOnlineAt),
	}
}

func encodeConversationListCursor(userID uint64, includeHidden, isPinned bool, updatedAt time.Time, convID uint64) string {
	raw, _ := json.Marshal(conversationListCursorPayload{
		Version:        1,
		Scope:          conversationListCursorScope,
		UserID:         userID,
		IncludeHidden:  includeHidden,
		IsPinned:       isPinned,
		UpdatedAtMs:    updatedAt.UTC().Truncate(time.Millisecond).UnixMilli(),
		ConversationID: convID,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeConversationListCursor(token string, expectedUserID uint64, expectedIncludeHidden bool) (*repository.ConversationListCursor, *errcode.Error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	var payload conversationListCursorPayload
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	if payload.Version != 1 ||
		payload.Scope != conversationListCursorScope ||
		payload.UserID != expectedUserID ||
		payload.IncludeHidden != expectedIncludeHidden ||
		payload.ConversationID == 0 ||
		payload.UpdatedAtMs <= 0 {
		return nil, errcode.ErrInvalidValue.WithMessage("cursor does not match query scope")
	}
	return &repository.ConversationListCursor{
		IsPinned:       payload.IsPinned,
		UpdatedAt:      time.UnixMilli(payload.UpdatedAtMs).UTC(),
		ConversationID: payload.ConversationID,
	}, nil
}

func encodeConversationMemberCursor(userID, convID, lastCMID uint64) string {
	raw, _ := json.Marshal(conversationMemberCursorPayload{
		Version:        1,
		Scope:          conversationMemberCursorScope,
		UserID:         userID,
		ConversationID: convID,
		LastID:         lastCMID,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeConversationMemberCursor(token string, expectedUserID, expectedConvID uint64) (uint64, *errcode.Error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	var payload conversationMemberCursorPayload
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return 0, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return 0, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	if payload.Version != 1 ||
		payload.Scope != conversationMemberCursorScope ||
		payload.UserID != expectedUserID ||
		payload.ConversationID != expectedConvID ||
		payload.LastID == 0 {
		return 0, errcode.ErrInvalidValue.WithMessage("cursor does not match query scope")
	}
	return payload.LastID, nil
}
