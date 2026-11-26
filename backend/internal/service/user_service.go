package service

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

const (
	MinNicknameRunes = 1
	MaxNicknameRunes = 64

	MaxSignatureRunes = 255

	MinSearchQueryRunes = 1
	MaxSearchQueryRunes = 64

	DefaultSearchLimit = 20
	MaxSearchLimit     = 100

	userSearchCursorVersion = 1
	userSearchCursorSort    = "id_desc"
	timestampFormatMillis   = "2006-01-02T15:04:05.000Z"
)

// UserStore defines the persistence operations required by UserService.
type UserStore interface {
	GetByID(ctx context.Context, userID uint64) (*model.User, error)
	UpdateProfile(ctx context.Context, userID uint64, updates map[string]any) (*model.User, error)
	Search(ctx context.Context, params repository.UserSearchParams) ([]model.User, error)
}

// PresenceStore defines online-presence lookup operations required by UserService.
type PresenceStore interface {
	IsUserOnline(ctx context.Context, userID uint64) (bool, error)
	GetUsersOnline(ctx context.Context, userIDs []uint64) (map[uint64]bool, error)
}

// UserProfile matches the frozen public `User` object in docs/api.md.
type UserProfile struct {
	ID           string  `json:"id"`
	Username     string  `json:"username"`
	Nickname     string  `json:"nickname"`
	AvatarURL    string  `json:"avatar_url"`
	Signature    string  `json:"signature"`
	Online       bool    `json:"online"`
	LastOnlineAt *string `json:"last_online_at"`
}

// SelfUserProfile extends UserProfile with `role` and `status` for `/api/v1/me`.
type SelfUserProfile struct {
	UserProfile
	Role   string `json:"role"`
	Status int8   `json:"status"`
}

// GetMeResult matches GET /api/v1/me response `data`.
type GetMeResult struct {
	User      SelfUserProfile `json:"user"`
	SessionID string          `json:"session_id"`
	DeviceID  string          `json:"device_id"`
}

// UpdateMeInput contains validated inputs for PATCH /api/v1/me.
type UpdateMeInput struct {
	UserID    uint64
	Nickname  *string
	Signature *string
}

// UpdateMeResult matches PATCH /api/v1/me response `data`.
type UpdateMeResult struct {
	User SelfUserProfile `json:"user"`
}

// SearchUsersInput contains query parameters for GET /api/v1/users/search.
type SearchUsersInput struct {
	ViewerUserID uint64
	Query        string
	Field        string
	Match        string
	Cursor       string
	Limit        int
}

// UserSearchPage matches the cursor-paginated `User` page in docs/api.md.
type UserSearchPage struct {
	Items      []UserProfile `json:"items"`
	NextCursor *string       `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
}

type userSearchCursor struct {
	Version int    `json:"v"`
	UserID  uint64 `json:"u"`
	Query   string `json:"q"`
	Field   string `json:"f"`
	Match   string `json:"m"`
	Sort    string `json:"s"`
	LastID  uint64 `json:"id"`
}

// UserService implements current-user profile retrieval/update and user search.
type UserService struct {
	users    UserStore
	presence PresenceStore
	log      *slog.Logger
}

// NewUserService creates a UserService backed by MySQL UserStore and Redis PresenceStore.
func NewUserService(users UserStore, presence PresenceStore, log *slog.Logger) *UserService {
	if log == nil {
		log = slog.Default()
	}
	return &UserService{
		users:    users,
		presence: presence,
		log:      log,
	}
}

// GetMe fetches the authenticated user's full profile and current session/device identifiers.
func (s *UserService) GetMe(ctx context.Context, userID uint64, sessionID, deviceID string) (GetMeResult, *errcode.Error) {
	if userID == 0 {
		return GetMeResult{}, errcode.ErrAccessTokenInvalid
	}
	if s == nil || s.users == nil {
		return GetMeResult{}, errcode.ErrInternalConsistency
	}

	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return GetMeResult{}, s.handleUserReadError(ctx, "get_me", userID, err)
	}
	if user.Status != model.UserStatusActive {
		return GetMeResult{}, errcode.ErrAccountDisabled
	}

	online := s.lookupOnlineSingle(ctx, user.ID)
	return GetMeResult{
		User:      toSelfUserProfile(user, online),
		SessionID: sessionID,
		DeviceID:  deviceID,
	}, nil
}

// UpdateMe updates nickname and/or signature for the authenticated user.
// Passing signature as an empty string clears the signature.
func (s *UserService) UpdateMe(ctx context.Context, input UpdateMeInput) (UpdateMeResult, *errcode.Error) {
	if input.UserID == 0 {
		return UpdateMeResult{}, errcode.ErrAccessTokenInvalid
	}
	if s == nil || s.users == nil {
		return UpdateMeResult{}, errcode.ErrInternalConsistency
	}
	if input.Nickname == nil && input.Signature == nil {
		return UpdateMeResult{}, errcode.ErrInvalidParams.WithMessage("at least one of nickname or signature must be provided")
	}

	updates := make(map[string]any, 2)
	if input.Nickname != nil {
		nick, bizErr := validateNickname(*input.Nickname)
		if bizErr != nil {
			return UpdateMeResult{}, bizErr
		}
		updates["nickname"] = nick
	}
	if input.Signature != nil {
		sig, bizErr := validateSignature(*input.Signature)
		if bizErr != nil {
			return UpdateMeResult{}, bizErr
		}
		updates["signature"] = sig
	}

	updated, err := s.users.UpdateProfile(ctx, input.UserID, updates)
	if err != nil {
		if errors.Is(err, repository.ErrUserDisabled) {
			return UpdateMeResult{}, errcode.ErrAccountDisabled
		}
		return UpdateMeResult{}, s.handleUserReadError(ctx, "update_me", input.UserID, err)
	}
	if updated.Status != model.UserStatusActive {
		return UpdateMeResult{}, errcode.ErrAccountDisabled
	}

	online := s.lookupOnlineSingle(ctx, updated.ID)
	return UpdateMeResult{
		User: toSelfUserProfile(updated, online),
	}, nil
}

// SearchUsers searches active users by exact or literal prefix match on username or nickname.
func (s *UserService) SearchUsers(ctx context.Context, input SearchUsersInput) (UserSearchPage, *errcode.Error) {
	if input.ViewerUserID == 0 {
		return UserSearchPage{}, errcode.ErrAccessTokenInvalid
	}
	if s == nil || s.users == nil {
		return UserSearchPage{}, errcode.ErrInternalConsistency
	}

	q, bizErr := validateSearchQuery(input.Query)
	if bizErr != nil {
		return UserSearchPage{}, bizErr
	}

	field, bizErr := normalizeSearchField(input.Field)
	if bizErr != nil {
		return UserSearchPage{}, bizErr
	}

	match, bizErr := normalizeSearchMatch(input.Match)
	if bizErr != nil {
		return UserSearchPage{}, bizErr
	}

	limit := input.Limit
	if limit == 0 {
		limit = DefaultSearchLimit
	} else if limit < 1 || limit > MaxSearchLimit {
		return UserSearchPage{}, errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100")
	}

	var cursorLastID uint64
	if trimmedCursor := strings.TrimSpace(input.Cursor); trimmedCursor != "" {
		decoded, curErr := decodeSearchCursor(trimmedCursor, input.ViewerUserID, q, field, match)
		if curErr != nil {
			return UserSearchPage{}, curErr
		}
		cursorLastID = decoded.LastID
	}

	rows, err := s.users.Search(ctx, repository.UserSearchParams{
		Query:  q,
		Field:  field,
		Match:  match,
		Cursor: cursorLastID,
		Limit:  limit + 1,
	})
	if err != nil {
		return UserSearchPage{}, s.handleSearchError(ctx, input.ViewerUserID, err)
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	userIDs := make([]uint64, len(rows))
	for i, u := range rows {
		userIDs[i] = u.ID
	}
	onlineMap := s.lookupOnlineBatch(ctx, userIDs)

	items := make([]UserProfile, 0, len(rows))
	for i := range rows {
		u := &rows[i]
		items = append(items, toUserProfile(u, onlineMap[u.ID]))
	}

	var nextCursor *string
	if hasMore && len(rows) > 0 {
		encoded, encErr := encodeSearchCursor(userSearchCursor{
			Version: userSearchCursorVersion,
			UserID:  input.ViewerUserID,
			Query:   q,
			Field:   field,
			Match:   match,
			Sort:    userSearchCursorSort,
			LastID:  rows[len(rows)-1].ID,
		})
		if encErr != nil {
			s.log.ErrorContext(ctx, "user_search_encode_cursor_failed",
				slog.Uint64("viewer_user_id", input.ViewerUserID),
				slog.Any("error", encErr),
			)
			return UserSearchPage{}, errcode.ErrInternalConsistency
		}
		nextCursor = &encoded
	}

	return UserSearchPage{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

func validateNickname(raw string) (string, *errcode.Error) {
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return "", errcode.ErrInvalidValue.WithMessage("nickname contains invalid characters")
	}
	trimmed := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(trimmed)
	if n < MinNicknameRunes || n > MaxNicknameRunes {
		return "", errcode.ErrInvalidValue.WithMessage("nickname must be between 1 and 64 characters")
	}
	return trimmed, nil
}

func validateSignature(raw string) (string, *errcode.Error) {
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return "", errcode.ErrInvalidValue.WithMessage("signature contains invalid characters")
	}
	if utf8.RuneCountInString(raw) > MaxSignatureRunes {
		return "", errcode.ErrInvalidValue.WithMessage("signature must not exceed 255 characters")
	}
	return raw, nil
}

func validateSearchQuery(raw string) (string, *errcode.Error) {
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return "", errcode.ErrInvalidValue.WithMessage("q contains invalid characters")
	}
	trimmed := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(trimmed)
	if n == 0 {
		return "", errcode.ErrInvalidParams.WithMessage("query parameter q is required")
	}
	if n > MaxSearchQueryRunes {
		return "", errcode.ErrInvalidValue.WithMessage("q must be between 1 and 64 characters")
	}
	return trimmed, nil
}

func normalizeSearchField(raw string) (string, *errcode.Error) {
	field := strings.TrimSpace(raw)
	if field == "" {
		return repository.UserSearchFieldUsername, nil
	}
	switch field {
	case repository.UserSearchFieldUsername, repository.UserSearchFieldNickname:
		return field, nil
	default:
		return "", errcode.ErrInvalidValue.WithMessage("field must be username or nickname")
	}
}

func normalizeSearchMatch(raw string) (string, *errcode.Error) {
	match := strings.TrimSpace(raw)
	if match == "" {
		return repository.UserSearchMatchPrefix, nil
	}
	switch match {
	case repository.UserSearchMatchExact, repository.UserSearchMatchPrefix:
		return match, nil
	default:
		return "", errcode.ErrInvalidValue.WithMessage("match must be exact or prefix")
	}
}

func encodeSearchCursor(c userSearchCursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeSearchCursor(token string, viewerUserID uint64, q, field, match string) (userSearchCursor, *errcode.Error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return userSearchCursor{}, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	var c userSearchCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return userSearchCursor{}, errcode.ErrInvalidValue.WithMessage("invalid pagination cursor")
	}
	if c.Version != userSearchCursorVersion ||
		c.UserID != viewerUserID ||
		c.Query != q ||
		c.Field != field ||
		c.Match != match ||
		c.Sort != userSearchCursorSort ||
		c.LastID == 0 {
		return userSearchCursor{}, errcode.ErrInvalidValue.WithMessage("cursor does not match current user or query filters")
	}
	return c, nil
}

func (s *UserService) lookupOnlineSingle(ctx context.Context, userID uint64) bool {
	if s.presence == nil || userID == 0 {
		return false
	}
	online, err := s.presence.IsUserOnline(ctx, userID)
	if err != nil {
		s.log.WarnContext(ctx, "user_presence_lookup_failed",
			slog.Uint64("user_id", userID),
			slog.Any("error", err),
		)
		return false
	}
	return online
}

func (s *UserService) lookupOnlineBatch(ctx context.Context, userIDs []uint64) map[uint64]bool {
	if s.presence == nil || len(userIDs) == 0 {
		return map[uint64]bool{}
	}
	onlineMap, err := s.presence.GetUsersOnline(ctx, userIDs)
	if err != nil {
		s.log.WarnContext(ctx, "user_presence_batch_lookup_failed",
			slog.Int("user_count", len(userIDs)),
			slog.Any("error", err),
		)
		if onlineMap == nil {
			return map[uint64]bool{}
		}
	}
	return onlineMap
}

func (s *UserService) handleUserReadError(ctx context.Context, op string, userID uint64, err error) *errcode.Error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errcode.ErrUserNotFound
	}
	bizErr := mapMySQLError(err)
	s.log.ErrorContext(ctx, "user_store_operation_failed",
		slog.String("op", op),
		slog.Uint64("user_id", userID),
		slog.Int("code", bizErr.Code),
		slog.Any("error", err),
	)
	return bizErr
}

func (s *UserService) handleSearchError(ctx context.Context, viewerUserID uint64, err error) *errcode.Error {
	if errors.Is(err, repository.ErrInvalidSearchField) || errors.Is(err, repository.ErrInvalidSearchMatch) {
		return errcode.ErrInvalidValue
	}
	bizErr := mapMySQLError(err)
	s.log.ErrorContext(ctx, "user_search_failed",
		slog.Uint64("viewer_user_id", viewerUserID),
		slog.Int("code", bizErr.Code),
		slog.Any("error", err),
	)
	return bizErr
}

func mapMySQLError(err error) *errcode.Error {
	if err == nil {
		return errcode.ErrInternalConsistency
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) {
		return errcode.ErrMySQLUnavailable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return errcode.ErrMySQLUnavailable
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "bad connection") ||
		strings.Contains(msg, "broken pipe") {
		return errcode.ErrMySQLUnavailable
	}
	return errcode.ErrInternalConsistency
}

func toUserProfile(u *model.User, online bool) UserProfile {
	return UserProfile{
		ID:           strconv.FormatUint(u.ID, 10),
		Username:     u.Username,
		Nickname:     u.Nickname,
		AvatarURL:    u.AvatarURL,
		Signature:    u.Signature,
		Online:       online,
		LastOnlineAt: formatTimestampPtr(u.LastOnlineAt),
	}
}

func toSelfUserProfile(u *model.User, online bool) SelfUserProfile {
	return SelfUserProfile{
		UserProfile: toUserProfile(u, online),
		Role:        u.Role,
		Status:      u.Status,
	}
}

func formatTimestampPtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	formatted := t.UTC().Format(timestampFormatMillis)
	return &formatted
}
