package service

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

const (
	DefaultFriendListLimit = 20
	MaxFriendListLimit     = 100
	MaxFriendRemarkLength  = 64

	friendCursorScope      = "friends"
	utcMillisRFC3339Layout = "2006-01-02T15:04:05.000Z"
)

// FriendStore defines persistence and presence queries required by FriendService.
// *repository.FriendshipRepository satisfies this interface directly.
type FriendStore interface {
	ListActiveFriendships(ctx context.Context, userID uint64, cursorID uint64, limit int) ([]model.Friendship, error)
	BatchGetUsersByIDs(ctx context.Context, userIDs []uint64) (map[uint64]model.User, error)
	BatchGetSingleConversationsByDirectKeys(ctx context.Context, directKeys []string) (map[string]model.Conversation, error)
	GetOnlineStatusBatch(ctx context.Context, userIDs []uint64) (map[uint64]bool, error)
	AreActiveFriends(ctx context.Context, userA, userB uint64) (bool, error)
	UpdateRemark(ctx context.Context, userID, friendID uint64, remark string) error
	DeactivateBidirectionalFriendship(ctx context.Context, userID, friendID uint64) (repository.DeleteFriendshipOutcome, error)
	FindSingleConversationByDirectKey(ctx context.Context, directKey string) (*model.Conversation, error)
}

// FriendUserDTO matches the public peer User fields in docs/api.md.
type FriendUserDTO struct {
	ID           uint64  `json:"id,string"`
	Username     string  `json:"username"`
	Nickname     string  `json:"nickname"`
	AvatarURL    string  `json:"avatar_url"`
	Signature    string  `json:"signature"`
	Online       bool    `json:"online"`
	LastOnlineAt *string `json:"last_online_at"`
}

// FriendListItem matches a single entry in GET /api/v1/friends data.items.
type FriendListItem struct {
	FriendID       uint64        `json:"friend_id,string"`
	User           FriendUserDTO `json:"user"`
	Remark         string        `json:"remark"`
	Online         bool          `json:"online"`
	ConversationID *string       `json:"conversation_id"`
}

// FriendListResult matches the paginated response data of GET /api/v1/friends.
type FriendListResult struct {
	Items      []FriendListItem `json:"items"`
	NextCursor *string          `json:"next_cursor"`
	HasMore    bool             `json:"has_more"`
}

// UpdateFriendRemarkResult matches the response data of PATCH /api/v1/friends/{friend_id}.
type UpdateFriendRemarkResult struct {
	FriendID uint64 `json:"friend_id,string"`
	Remark   string `json:"remark"`
}

// DeleteFriendResult matches the response data of DELETE /api/v1/friends/{friend_id}.
type DeleteFriendResult struct {
	FriendID       uint64  `json:"friend_id,string"`
	ConversationID *string `json:"conversation_id"`
	IsVisible      bool    `json:"is_visible"`
}

type friendCursorPayload struct {
	Scope  string `json:"s"`
	UserID uint64 `json:"u,string"`
	LastID uint64 `json:"id,string"`
}

// FriendService implements friend list, remark, deletion, and shared integration helpers.
type FriendService struct {
	store FriendStore
	log   *slog.Logger
}

// NewFriendService creates a FriendService backed by store.
func NewFriendService(store FriendStore, log *slog.Logger) *FriendService {
	if log == nil {
		log = slog.Default()
	}
	return &FriendService{
		store: store,
		log:   log,
	}
}

// EncodeFriendCursor encodes a user-bound, scope-bound, sort-key-bound pagination cursor.
func EncodeFriendCursor(userID, lastFriendshipID uint64) string {
	payload := friendCursorPayload{
		Scope:  friendCursorScope,
		UserID: userID,
		LastID: lastFriendshipID,
	}
	raw, _ := json.Marshal(payload)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeFriendCursor validates and decodes a friend list cursor bound to expectedUserID.
func DecodeFriendCursor(raw string, expectedUserID uint64) (uint64, *errcode.Error) {
	if expectedUserID == 0 || raw == "" {
		return 0, errcode.ErrInvalidValue
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, errcode.ErrInvalidValue
	}

	var payload friendCursorPayload
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return 0, errcode.ErrInvalidValue
	}
	// Reject trailing bytes after JSON object.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return 0, errcode.ErrInvalidValue
	}

	if payload.Scope != friendCursorScope || payload.UserID != expectedUserID || payload.LastID == 0 {
		return 0, errcode.ErrInvalidValue
	}
	return payload.LastID, nil
}

// ListFriends returns a cursor-paginated friend list with batch-loaded profiles,
// single conversation IDs, and merged multi-device Redis presence.
func (s *FriendService) ListFriends(
	ctx context.Context,
	userID uint64,
	cursor string,
	limit int,
) (FriendListResult, *errcode.Error) {
	empty := FriendListResult{
		Items:      []FriendListItem{},
		NextCursor: nil,
		HasMore:    false,
	}
	if userID == 0 {
		return empty, errcode.ErrAccessTokenInvalid
	}
	if limit < 1 || limit > MaxFriendListLimit {
		return empty, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}
	if s == nil || s.store == nil {
		return empty, errcode.ErrMySQLUnavailable
	}

	var cursorID uint64
	if cursor != "" {
		decodedID, curErr := DecodeFriendCursor(cursor, userID)
		if curErr != nil {
			return empty, curErr
		}
		cursorID = decodedID
	}

	rows, err := s.store.ListActiveFriendships(ctx, userID, cursorID, limit)
	if err != nil {
		return empty, s.mapMySQLError(ctx, userID, "list_active_friendships", err)
	}
	if len(rows) == 0 {
		return empty, nil
	}

	hasMore := false
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	friendIDs := make([]uint64, 0, len(rows))
	directKeys := make([]string, 0, len(rows))
	directKeyByFriend := make(map[uint64]string, len(rows))
	for _, row := range rows {
		friendIDs = append(friendIDs, row.FriendID)
		if dk, dkErr := repository.FormatDirectKey(userID, row.FriendID); dkErr == nil {
			directKeys = append(directKeys, dk)
			directKeyByFriend[row.FriendID] = dk
		}
	}

	usersMap, err := s.store.BatchGetUsersByIDs(ctx, friendIDs)
	if err != nil {
		return empty, s.mapMySQLError(ctx, userID, "batch_get_friend_users", err)
	}

	convMap, err := s.store.BatchGetSingleConversationsByDirectKeys(ctx, directKeys)
	if err != nil {
		return empty, s.mapMySQLError(ctx, userID, "batch_get_friend_conversations", err)
	}

	onlineMap, err := s.store.GetOnlineStatusBatch(ctx, friendIDs)
	if err != nil {
		s.log.ErrorContext(ctx, "friend_list_redis_presence_failed",
			slog.String("user_id", strconv.FormatUint(userID, 10)),
			slog.Any("error", err),
		)
		return empty, errcode.ErrRedisUnavailable
	}

	items := make([]FriendListItem, 0, len(rows))
	for _, row := range rows {
		u, ok := usersMap[row.FriendID]
		if !ok {
			continue
		}
		online := onlineMap[row.FriendID]

		var lastOnlineAt *string
		if u.LastOnlineAt != nil {
			formatted := u.LastOnlineAt.UTC().Format(utcMillisRFC3339Layout)
			lastOnlineAt = &formatted
		}

		var convIDPtr *string
		if dk, ok := directKeyByFriend[row.FriendID]; ok {
			if conv, found := convMap[dk]; found && conv.ID > 0 {
				convStr := strconv.FormatUint(conv.ID, 10)
				convIDPtr = &convStr
			}
		}

		items = append(items, FriendListItem{
			FriendID: row.FriendID,
			User: FriendUserDTO{
				ID:           u.ID,
				Username:     u.Username,
				Nickname:     u.Nickname,
				AvatarURL:    u.AvatarURL,
				Signature:    u.Signature,
				Online:       online,
				LastOnlineAt: lastOnlineAt,
			},
			Remark:         row.Remark,
			Online:         online,
			ConversationID: convIDPtr,
		})
	}

	var nextCursor *string
	if hasMore && len(rows) > 0 {
		nc := EncodeFriendCursor(userID, rows[len(rows)-1].ID)
		nextCursor = &nc
	}

	return FriendListResult{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// UpdateRemark updates ONLY the caller's remark for friendID (0-64 UTF-8 runes).
func (s *FriendService) UpdateRemark(
	ctx context.Context,
	userID uint64,
	friendID uint64,
	remark string,
) (UpdateFriendRemarkResult, *errcode.Error) {
	if userID == 0 {
		return UpdateFriendRemarkResult{}, errcode.ErrAccessTokenInvalid
	}
	if friendID == 0 {
		return UpdateFriendRemarkResult{}, errcode.ErrInvalidValue.WithMessage("invalid friend_id")
	}
	if !utf8.ValidString(remark) || utf8.RuneCountInString(remark) > MaxFriendRemarkLength {
		return UpdateFriendRemarkResult{}, errcode.ErrInvalidValue.WithMessage("remark must be 0 to 64 characters")
	}
	if s == nil || s.store == nil {
		return UpdateFriendRemarkResult{}, errcode.ErrMySQLUnavailable
	}

	if err := s.store.UpdateRemark(ctx, userID, friendID, remark); err != nil {
		return UpdateFriendRemarkResult{}, s.mapFriendshipWriteError(ctx, userID, "update_friend_remark", err)
	}

	return UpdateFriendRemarkResult{
		FriendID: friendID,
		Remark:   remark,
	}, nil
}

// DeleteFriend deactivates the bidirectional friendship and hides the single conversation for both users.
func (s *FriendService) DeleteFriend(
	ctx context.Context,
	userID uint64,
	friendID uint64,
) (DeleteFriendResult, *errcode.Error) {
	if userID == 0 {
		return DeleteFriendResult{}, errcode.ErrAccessTokenInvalid
	}
	if friendID == 0 {
		return DeleteFriendResult{}, errcode.ErrInvalidValue.WithMessage("invalid friend_id")
	}
	if s == nil || s.store == nil {
		return DeleteFriendResult{}, errcode.ErrMySQLUnavailable
	}

	outcome, err := s.store.DeactivateBidirectionalFriendship(ctx, userID, friendID)
	if err != nil {
		return DeleteFriendResult{}, s.mapFriendshipWriteError(ctx, userID, "delete_friend", err)
	}

	var convIDStr *string
	if outcome.ConversationID != nil && *outcome.ConversationID > 0 {
		sVal := strconv.FormatUint(*outcome.ConversationID, 10)
		convIDStr = &sVal
	}

	return DeleteFriendResult{
		FriendID:       outcome.FriendID,
		ConversationID: convIDStr,
		IsVisible:      outcome.IsVisible,
	}, nil
}

// AreActiveFriends reports whether an active bidirectional friendship exists between userA and userB.
// Exposed for friend-request and messaging integration.
func (s *FriendService) AreActiveFriends(ctx context.Context, userA, userB uint64) (bool, *errcode.Error) {
	if s == nil || s.store == nil {
		return false, errcode.ErrMySQLUnavailable
	}
	ok, err := s.store.AreActiveFriends(ctx, userA, userB)
	if err != nil {
		return false, s.mapMySQLError(ctx, userA, "are_active_friends", err)
	}
	return ok, nil
}

// FindSingleConversationByUsers looks up the unique single conversation between userA and userB by direct_key.
// Returns gorm.ErrRecordNotFound wrapped inside *errcode.Error (13001) when absent.
func (s *FriendService) FindSingleConversationByUsers(
	ctx context.Context,
	userA uint64,
	userB uint64,
) (*model.Conversation, *errcode.Error) {
	if s == nil || s.store == nil {
		return nil, errcode.ErrMySQLUnavailable
	}
	dk, err := repository.FormatDirectKey(userA, userB)
	if err != nil {
		return nil, errcode.ErrInvalidValue
	}
	conv, err := s.store.FindSingleConversationByDirectKey(ctx, dk)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrConversationNotFound
		}
		return nil, s.mapMySQLError(ctx, userA, "find_single_conversation", err)
	}
	return conv, nil
}

// EnsureFriendshipAndConversationTx delegates to repository.EnsureFriendshipAndConversationTx
// for Stage 4 integration when accepting a friend request inside a transaction.
func (s *FriendService) EnsureFriendshipAndConversationTx(
	ctx context.Context,
	tx *gorm.DB,
	userA uint64,
	userB uint64,
) (*model.Conversation, error) {
	return repository.EnsureFriendshipAndConversationTx(ctx, tx, userA, userB)
}

// UpsertBidirectionalFriendshipTx delegates to repository.UpsertBidirectionalFriendshipTx
// for transactional callers in Stage 4 integration.
func (s *FriendService) UpsertBidirectionalFriendshipTx(
	ctx context.Context,
	tx *gorm.DB,
	userA uint64,
	userB uint64,
) error {
	return repository.UpsertBidirectionalFriendshipTx(ctx, tx, userA, userB)
}

// EnsureSingleConversationTx delegates to repository.EnsureSingleConversationTx
// for transactional callers in Stage 4 integration.
func (s *FriendService) EnsureSingleConversationTx(
	ctx context.Context,
	tx *gorm.DB,
	userA uint64,
	userB uint64,
) (*model.Conversation, error) {
	return repository.EnsureSingleConversationTx(ctx, tx, userA, userB)
}

func (s *FriendService) mapFriendshipWriteError(
	ctx context.Context,
	userID uint64,
	op string,
	err error,
) *errcode.Error {
	switch {
	case errors.Is(err, repository.ErrFriendshipRecordNotFound),
		errors.Is(err, repository.ErrInvalidUserPair):
		return errcode.ErrFriendshipNotFound
	case errors.Is(err, repository.ErrFriendshipInactive):
		return errcode.ErrNotFriends
	default:
		return s.mapMySQLError(ctx, userID, op, err)
	}
}

func (s *FriendService) mapMySQLError(
	ctx context.Context,
	userID uint64,
	op string,
	err error,
) *errcode.Error {
	s.log.ErrorContext(ctx, op+"_failed",
		slog.String("user_id", strconv.FormatUint(userID, 10)),
		slog.Any("error", err),
	)
	if isInfrastructureUnavailableErr(err) {
		return errcode.ErrMySQLUnavailable
	}
	return errcode.ErrInternalConsistency
}

func isInfrastructureUnavailableErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, repository.ErrRepositoryUnavailable) ||
		errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "bad connection") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset")
}
