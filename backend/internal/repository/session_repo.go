package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

const (
	redisBlacklistKeyPrefix   = "im:token:blacklist:"
	redisRefreshLockKeyPrefix = "im:auth:refresh:lock:"
	redisPresenceUserPrefix   = "im:presence:user:"
)

var (
	// ErrSessionNotFound indicates the target session record does not exist.
	ErrSessionNotFound = errors.New("user session not found")

	releaseRefreshLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)
)

// SessionAuthState contains the minimal session fields verified by JWT middleware.
type SessionAuthState struct {
	ID        uint64
	UserID    uint64
	DeviceID  string
	AccessJTI string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// SessionRepository manages multi-device sessions in MySQL and token revocation state in Redis.
type SessionRepository struct {
	db  *gorm.DB
	rdb *redis.Client
}

// NewSessionRepository constructs a SessionRepository backed by MySQL and Redis.
func NewSessionRepository(db *gorm.DB, rdb *redis.Client) *SessionRepository {
	return &SessionRepository{
		db:  db,
		rdb: rdb,
	}
}

// TokenBlacklistKey formats the frozen Redis blacklist key for a JWT ID.
func TokenBlacklistKey(jti string) string {
	return redisBlacklistKeyPrefix + jti
}

// RefreshLockKey formats the Redis in-flight refresh lock key for a refresh token SHA-256 digest.
func RefreshLockKey(refreshHash string) string {
	return redisRefreshLockKeyPrefix + refreshHash
}

// FindByRefreshTokenHash retrieves a session by its SHA-256 refresh token digest.
func (r *SessionRepository) FindByRefreshTokenHash(ctx context.Context, hash string) (*model.UserSession, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("session repository db is nil")
	}
	var session model.UserSession
	err := r.db.WithContext(ctx).
		Where("refresh_token_hash = ?", hash).
		First(&session).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("find session by refresh hash: %w", err)
	}
	return &session, nil
}

// FindByIDForUpdate locks and loads a session row by primary key inside tx.
func (r *SessionRepository) FindByIDForUpdate(ctx context.Context, tx *gorm.DB, sessionID uint64) (*model.UserSession, error) {
	if tx == nil {
		return nil, errors.New("transaction handle is nil")
	}
	var session model.UserSession
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", sessionID).
		First(&session).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("lock session by id: %w", err)
	}
	return &session, nil
}

// GetSessionAuthState loads session fields needed for JWT authentication middleware.
func (r *SessionRepository) GetSessionAuthState(ctx context.Context, sessionID uint64) (SessionAuthState, error) {
	if r == nil || r.db == nil {
		return SessionAuthState{}, errors.New("session repository db is nil")
	}
	var row struct {
		ID        uint64     `gorm:"column:id"`
		UserID    uint64     `gorm:"column:user_id"`
		DeviceID  string     `gorm:"column:device_id"`
		AccessJTI string     `gorm:"column:access_jti"`
		ExpiresAt time.Time  `gorm:"column:expires_at"`
		RevokedAt *time.Time `gorm:"column:revoked_at"`
	}
	err := r.db.WithContext(ctx).
		Model(&model.UserSession{}).
		Select("id", "user_id", "device_id", "access_jti", "expires_at", "revoked_at").
		Where("id = ?", sessionID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return SessionAuthState{}, ErrSessionNotFound
		}
		return SessionAuthState{}, fmt.Errorf("query session auth state: %w", err)
	}
	return SessionAuthState{
		ID:        row.ID,
		UserID:    row.UserID,
		DeviceID:  row.DeviceID,
		AccessJTI: row.AccessJTI,
		ExpiresAt: row.ExpiresAt,
		RevokedAt: row.RevokedAt,
	}, nil
}

// ListUnrevokedByUserDeviceForUpdate locks and returns all non-revoked sessions for (userID, deviceID).
func (r *SessionRepository) ListUnrevokedByUserDeviceForUpdate(
	ctx context.Context,
	tx *gorm.DB,
	userID uint64,
	deviceID string,
) ([]model.UserSession, error) {
	if tx == nil {
		return nil, errors.New("transaction handle is nil")
	}
	var sessions []model.UserSession
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND device_id = ? AND revoked_at IS NULL", userID, deviceID).
		Find(&sessions).Error
	if err != nil {
		return nil, fmt.Errorf("list unrevoked sessions by device: %w", err)
	}
	return sessions, nil
}

// ListUnrevokedByUserForUpdate locks and returns all non-revoked sessions for userID.
func (r *SessionRepository) ListUnrevokedByUserForUpdate(
	ctx context.Context,
	tx *gorm.DB,
	userID uint64,
) ([]model.UserSession, error) {
	if tx == nil {
		return nil, errors.New("transaction handle is nil")
	}
	var sessions []model.UserSession
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Find(&sessions).Error
	if err != nil {
		return nil, fmt.Errorf("list unrevoked sessions by user: %w", err)
	}
	return sessions, nil
}

// CountActiveByUser counts non-revoked, non-expired sessions for userID inside tx.
func (r *SessionRepository) CountActiveByUser(ctx context.Context, tx *gorm.DB, userID uint64, now time.Time) (int64, error) {
	if tx == nil {
		return 0, errors.New("transaction handle is nil")
	}
	var count int64
	err := tx.WithContext(ctx).
		Model(&model.UserSession{}).
		Where("user_id = ? AND revoked_at IS NULL AND expires_at > ?", userID, now.UTC()).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("count active sessions: %w", err)
	}
	return count, nil
}

// RevokeByIDs marks the specified unrevoked sessions as revoked at revokedAt.
func (r *SessionRepository) RevokeByIDs(ctx context.Context, tx *gorm.DB, sessionIDs []uint64, revokedAt time.Time) (int64, error) {
	if tx == nil {
		return 0, errors.New("transaction handle is nil")
	}
	if len(sessionIDs) == 0 {
		return 0, nil
	}
	res := tx.WithContext(ctx).
		Model(&model.UserSession{}).
		Where("id IN ? AND revoked_at IS NULL", sessionIDs).
		Update("revoked_at", revokedAt.UTC())
	if res.Error != nil {
		return 0, fmt.Errorf("revoke sessions by ids: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// Create inserts a new user_sessions row inside tx.
func (r *SessionRepository) Create(ctx context.Context, tx *gorm.DB, session *model.UserSession) error {
	if tx == nil {
		return errors.New("transaction handle is nil")
	}
	if err := tx.WithContext(ctx).Create(session).Error; err != nil {
		return fmt.Errorf("create user session: %w", err)
	}
	return nil
}

// RotateTokens updates refresh_token_hash, access_jti, expires_at, and activity metadata for sessionID.
func (r *SessionRepository) RotateTokens(
	ctx context.Context,
	tx *gorm.DB,
	sessionID uint64,
	newRefreshHash string,
	newAccessJTI string,
	newExpiresAt time.Time,
	activeAt time.Time,
	ipAddress string,
	userAgent string,
) error {
	if tx == nil {
		return errors.New("transaction handle is nil")
	}
	updates := map[string]any{
		"refresh_token_hash": newRefreshHash,
		"access_jti":         newAccessJTI,
		"expires_at":         newExpiresAt.UTC(),
		"last_active_at":     activeAt.UTC(),
		"ip_address":         ipAddress,
		"user_agent":         userAgent,
	}
	res := tx.WithContext(ctx).
		Model(&model.UserSession{}).
		Where("id = ? AND revoked_at IS NULL", sessionID).
		Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("rotate session tokens: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// BlacklistAccessJTI writes a revoked access token JTI to Redis with TTL.
func (r *SessionRepository) BlacklistAccessJTI(ctx context.Context, jti, reason string, ttl time.Duration) error {
	if jti == "" || ttl <= 0 {
		return nil
	}
	return r.BlacklistAccessJTIs(ctx, []string{jti}, reason, ttl)
}

// BlacklistAccessJTIs writes a batch of revoked access token JTIs to Redis using a pipeline.
func (r *SessionRepository) BlacklistAccessJTIs(ctx context.Context, jtis []string, reason string, ttl time.Duration) error {
	if r == nil || r.rdb == nil {
		return errors.New("session repository redis client is nil")
	}
	if ttl <= 0 {
		return nil
	}
	if reason == "" {
		reason = "revoked"
	}

	unique := make([]string, 0, len(jtis))
	seen := make(map[string]struct{}, len(jtis))
	for _, jti := range jtis {
		if jti == "" {
			continue
		}
		if _, ok := seen[jti]; ok {
			continue
		}
		seen[jti] = struct{}{}
		unique = append(unique, jti)
	}
	if len(unique) == 0 {
		return nil
	}

	pipe := r.rdb.Pipeline()
	for _, jti := range unique {
		pipe.Set(ctx, TokenBlacklistKey(jti), reason, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("blacklist access jtis: %w", err)
	}
	return nil
}

// IsAccessJTIBlacklisted checks whether jti exists in Redis im:token:blacklist:{jti}.
func (r *SessionRepository) IsAccessJTIBlacklisted(ctx context.Context, jti string) (bool, string, error) {
	if r == nil || r.rdb == nil {
		return false, "", errors.New("session repository redis client is nil")
	}
	if jti == "" {
		return false, "", nil
	}
	val, err := r.rdb.Get(ctx, TokenBlacklistKey(jti)).Result()
	if errors.Is(err, redis.Nil) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("check access jti blacklist: %w", err)
	}
	return true, val, nil
}

// AcquireRefreshLock attempts to acquire a short-lived Redis mutex for rotating refreshHash.
func (r *SessionRepository) AcquireRefreshLock(ctx context.Context, refreshHash string, ttl time.Duration) (string, bool, error) {
	if r == nil || r.rdb == nil {
		return "", false, errors.New("session repository redis client is nil")
	}
	token, err := randomLockToken()
	if err != nil {
		return "", false, err
	}
	ok, err := r.rdb.SetNX(ctx, RefreshLockKey(refreshHash), token, ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("acquire refresh lock: %w", err)
	}
	return token, ok, nil
}

// IsRefreshLockHeld reports whether an in-flight refresh rotation currently holds the lock for refreshHash.
func (r *SessionRepository) IsRefreshLockHeld(ctx context.Context, refreshHash string) (bool, error) {
	if r == nil || r.rdb == nil {
		return false, errors.New("session repository redis client is nil")
	}
	n, err := r.rdb.Exists(ctx, RefreshLockKey(refreshHash)).Result()
	if err != nil {
		return false, fmt.Errorf("check refresh lock: %w", err)
	}
	return n > 0, nil
}

// ReleaseRefreshLock removes the refresh lock only if its stored token matches.
func (r *SessionRepository) ReleaseRefreshLock(ctx context.Context, refreshHash, token string) error {
	if r == nil || r.rdb == nil || token == "" {
		return nil
	}
	if err := releaseRefreshLockScript.Run(ctx, r.rdb, []string{RefreshLockKey(refreshHash)}, token).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("release refresh lock: %w", err)
	}
	return nil
}

// IsUserOnline checks Redis presence lease count (im:presence:user:{user_id}) if present.
func (r *SessionRepository) IsUserOnline(ctx context.Context, userID uint64) (bool, error) {
	if r == nil || r.rdb == nil {
		return false, nil
	}
	val, err := r.rdb.Get(ctx, redisPresenceUserPrefix+strconv.FormatUint(userID, 10)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read user presence: %w", err)
	}
	cnt, convErr := strconv.ParseInt(val, 10, 64)
	if convErr != nil {
		return false, nil
	}
	return cnt > 0, nil
}

func randomLockToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate lock token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
