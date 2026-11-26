package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

const (
	UserSearchFieldUsername = "username"
	UserSearchFieldNickname = "nickname"

	UserSearchMatchExact  = "exact"
	UserSearchMatchPrefix = "prefix"

	presenceUserKeyPrefix = "im:presence:user:"
)

var (
	// ErrUserDisabled indicates the target account exists in MySQL with status != active.
	ErrUserDisabled = errors.New("user account is disabled")
	// ErrInvalidSearchField indicates an unsupported search column.
	ErrInvalidSearchField = errors.New("invalid user search field")
	// ErrInvalidSearchMatch indicates an unsupported search match mode.
	ErrInvalidSearchMatch = errors.New("invalid user search match mode")

	selfUserSelectColumns = []string{
		"id",
		"username",
		"nickname",
		"avatar_url",
		"signature",
		"role",
		"status",
		"last_online_at",
		"created_at",
		"updated_at",
	}

	publicUserSelectColumns = []string{
		"id",
		"username",
		"nickname",
		"avatar_url",
		"signature",
		"last_online_at",
	}

	likeLiteralEscaper = strings.NewReplacer(
		"!", "!!",
		"%", "!%",
		"_", "!_",
	)
)

// UserSearchParams defines repository-level query filters and keyset cursor bounds.
type UserSearchParams struct {
	Query  string
	Field  string
	Match  string
	Cursor uint64
	Limit  int
}

// UserRepository provides MySQL persistence and search queries against `users`.
type UserRepository struct {
	db *gorm.DB
}

// NewUserRepository creates a UserRepository backed by a GORM MySQL handle.
func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

// GetByID loads a user profile by primary key without reading password_hash.
func (r *UserRepository) GetByID(ctx context.Context, userID uint64) (*model.User, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("mysql db handle is nil")
	}
	if userID == 0 {
		return nil, gorm.ErrRecordNotFound
	}

	var user model.User
	err := r.db.WithContext(ctx).
		Select(selfUserSelectColumns).
		Where("id = ?", userID).
		Take(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// UpdateProfile atomically validates account existence/status and updates allowed profile fields.
func (r *UserRepository) UpdateProfile(ctx context.Context, userID uint64, updates map[string]any) (*model.User, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("mysql db handle is nil")
	}
	if userID == 0 {
		return nil, gorm.ErrRecordNotFound
	}

	var updated model.User
	err := WithTx(ctx, r.db, func(tx *gorm.DB) error {
		var current model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select(selfUserSelectColumns).
			Where("id = ?", userID).
			Take(&current).Error; err != nil {
			return err
		}
		if current.Status != model.UserStatusActive {
			return ErrUserDisabled
		}

		if len(updates) > 0 {
			if err := tx.Model(&model.User{}).
				Where("id = ?", userID).
				Updates(updates).Error; err != nil {
				return err
			}
		}

		return tx.Select(selfUserSelectColumns).
			Where("id = ?", userID).
			Take(&updated).Error
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// Search queries active users by exact or literal prefix match on username or nickname,
// ordered by id DESC with keyset pagination.
func (r *UserRepository) Search(ctx context.Context, params UserSearchParams) ([]model.User, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("mysql db handle is nil")
	}

	var column string
	switch params.Field {
	case UserSearchFieldUsername:
		column = "username"
	case UserSearchFieldNickname:
		column = "nickname"
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidSearchField, params.Field)
	}

	q := r.db.WithContext(ctx).
		Model(&model.User{}).
		Select(publicUserSelectColumns).
		Where("status = ?", model.UserStatusActive)

	switch params.Match {
	case UserSearchMatchExact:
		q = q.Where(column+" = ?", params.Query)
	case UserSearchMatchPrefix:
		q = q.Where(column+" LIKE ? ESCAPE '!'", EscapeLikeLiteral(params.Query)+"%")
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidSearchMatch, params.Match)
	}

	if params.Cursor > 0 {
		q = q.Where("id < ?", params.Cursor)
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}

	var users []model.User
	if err := q.Order("id DESC").Limit(limit).Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// EscapeLikeLiteral escapes SQL LIKE special characters using '!' as the escape character
// so user input performs a literal prefix match.
func EscapeLikeLiteral(s string) string {
	return likeLiteralEscaper.Replace(s)
}

// PresenceUserKey returns the frozen Redis key storing the active connection count for a user.
func PresenceUserKey(userID uint64) string {
	return presenceUserKeyPrefix + strconv.FormatUint(userID, 10)
}

// UserPresenceRepository reads merged online status from Redis `im:presence:user:{user_id}`.
type UserPresenceRepository struct {
	rdb *redis.Client
}

// NewUserPresenceRepository creates a UserPresenceRepository backed by Redis.
func NewUserPresenceRepository(rdb *redis.Client) *UserPresenceRepository {
	return &UserPresenceRepository{rdb: rdb}
}

// IsUserOnline checks whether a single user has at least one active connection lease in Redis.
func (p *UserPresenceRepository) IsUserOnline(ctx context.Context, userID uint64) (bool, error) {
	if userID == 0 {
		return false, nil
	}
	statuses, err := p.GetUsersOnline(ctx, []uint64{userID})
	if err != nil {
		return false, err
	}
	return statuses[userID], nil
}

// GetUsersOnline fetches online flags for a batch of users using a single Redis Pipeline.
func (p *UserPresenceRepository) GetUsersOnline(ctx context.Context, userIDs []uint64) (map[uint64]bool, error) {
	result := make(map[uint64]bool, len(userIDs))
	if len(userIDs) == 0 {
		return result, nil
	}
	if p == nil || p.rdb == nil {
		return nil, errors.New("redis client is nil")
	}

	uniqueIDs := make([]uint64, 0, len(userIDs))
	for _, uid := range userIDs {
		if uid == 0 {
			continue
		}
		if _, exists := result[uid]; exists {
			continue
		}
		result[uid] = false
		uniqueIDs = append(uniqueIDs, uid)
	}
	if len(uniqueIDs) == 0 {
		return result, nil
	}

	pipe := p.rdb.Pipeline()
	cmds := make(map[uint64]*redis.StringCmd, len(uniqueIDs))
	for _, uid := range uniqueIDs {
		cmds[uid] = pipe.Get(ctx, PresenceUserKey(uid))
	}
	_, execErr := pipe.Exec(ctx)

	var firstErr error
	for _, uid := range uniqueIDs {
		val, err := cmds[uid].Result()
		if err != nil {
			if !errors.Is(err, redis.Nil) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		count, parseErr := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if parseErr == nil && count > 0 {
			result[uid] = true
		}
	}

	if firstErr != nil {
		return result, firstErr
	}
	if execErr != nil && !errors.Is(execErr, redis.Nil) {
		return result, execErr
	}
	return result, nil
}
