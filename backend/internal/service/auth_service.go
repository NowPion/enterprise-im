package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/pkg/password"
	"enterprise-im/backend/internal/repository"
)

const (
	LogoutScopeCurrent = "current"
	LogoutScopeAll     = "all"

	maxUsernameRunes = 64
	maxNicknameRunes = 64
	maxDeviceIDRunes = 128

	refreshLockTTL = 5 * time.Second
	timestampFmt   = "2006-01-02T15:04:05.000Z"

	// dummyBcryptHash is used when a login username does not exist to keep verification timing uniform.
	dummyBcryptHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
)

// SessionRevokeHook notifies downstream real-time hubs after session revocations commit.
type SessionRevokeHook func(ctx context.Context, userID uint64, sessionIDs []uint64, reason string) error

// NoopSessionRevokeHook is the Stage 4 default hook before a WebSocket Hub exists.
func NoopSessionRevokeHook(context.Context, uint64, []uint64, string) error {
	return nil
}

// UserView is the self User JSON object aligned with docs/api.md.
type UserView struct {
	ID           string  `json:"id"`
	Username     string  `json:"username"`
	Nickname     string  `json:"nickname"`
	AvatarURL    string  `json:"avatar_url"`
	Signature    string  `json:"signature"`
	Online       bool    `json:"online"`
	LastOnlineAt *string `json:"last_online_at"`
	Role         string  `json:"role"`
	Status       int8    `json:"status"`
}

// TokensView is the Tokens JSON object aligned with docs/api.md.
type TokensView struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
	SessionID        string `json:"session_id"`
	DeviceID         string `json:"device_id"`
}

// RegisterInput contains validated user registration parameters.
type RegisterInput struct {
	Username string
	Password string
	Nickname string
}

// RegisterOutput wraps the newly created user view.
type RegisterOutput struct {
	User UserView `json:"user"`
}

// LoginInput contains login credentials and device metadata.
type LoginInput struct {
	Username   string
	Password   string
	DeviceID   string
	DeviceType string
	IPAddress  string
	UserAgent  string
}

// LoginOutput wraps the authenticated user and issued session tokens.
type LoginOutput struct {
	User   UserView   `json:"user"`
	Tokens TokensView `json:"tokens"`
}

// RefreshInput contains refresh token rotation inputs.
type RefreshInput struct {
	RefreshToken string
	DeviceID     string
	IPAddress    string
	UserAgent    string
}

// RefreshOutput wraps newly rotated access and refresh tokens.
type RefreshOutput struct {
	Tokens TokensView `json:"tokens"`
}

// LogoutInput describes the authenticated logout request.
type LogoutInput struct {
	UserID     uint64
	SessionID  uint64
	CurrentJTI string
	Scope      string
}

// LogoutOutput reports the applied scope and number of revoked device sessions.
type LogoutOutput struct {
	Scope        string `json:"scope"`
	RevokedCount int64  `json:"revoked_count"`
}

// ChangePasswordInput describes an authenticated password change request.
type ChangePasswordInput struct {
	UserID      uint64
	CurrentJTI  string
	OldPassword string
	NewPassword string
}

// ChangePasswordOutput reports the number of revoked sessions after password rotation.
type ChangePasswordOutput struct {
	RevokedCount int64 `json:"revoked_count"`
}

// AuthDataStore abstracts persistence and Redis state operations required by AuthService.
type AuthDataStore interface {
	WithTx(ctx context.Context, fn func(tx AuthTxStore) error) error
	CreateUser(ctx context.Context, user *model.User) error
	FindUserByUsername(ctx context.Context, username string) (*model.User, error)
	FindUserByID(ctx context.Context, userID uint64) (*model.User, error)
	FindSessionByRefreshHash(ctx context.Context, hash string) (*model.UserSession, error)
	BlacklistAccessJTIs(ctx context.Context, jtis []string, reason string, ttl time.Duration) error
	AcquireRefreshLock(ctx context.Context, refreshHash string, ttl time.Duration) (string, bool, error)
	IsRefreshLockHeld(ctx context.Context, refreshHash string) (bool, error)
	ReleaseRefreshLock(ctx context.Context, refreshHash, token string) error
	IsUserOnline(ctx context.Context, userID uint64) (bool, error)
}

// AuthTxStore encapsulates transactional MySQL operations with row locking.
type AuthTxStore interface {
	LockUserByID(ctx context.Context, userID uint64) (*model.User, error)
	UpdateUserPasswordHash(ctx context.Context, userID uint64, passwordHash string) error
	LockSessionByID(ctx context.Context, sessionID uint64) (*model.UserSession, error)
	ListUnrevokedSessionsByDevice(ctx context.Context, userID uint64, deviceID string) ([]model.UserSession, error)
	ListUnrevokedSessionsByUser(ctx context.Context, userID uint64) ([]model.UserSession, error)
	CountActiveSessionsByUser(ctx context.Context, userID uint64, now time.Time) (int64, error)
	RevokeSessionsByIDs(ctx context.Context, sessionIDs []uint64, revokedAt time.Time) (int64, error)
	CreateSession(ctx context.Context, session *model.UserSession) error
	RotateSessionTokens(ctx context.Context, sessionID uint64, newRefreshHash, newAccessJTI string, newExpiresAt, activeAt time.Time, ipAddress, userAgent string) error
}

type repoAuthDataStore struct {
	authRepo    *repository.AuthRepository
	sessionRepo *repository.SessionRepository
}

type repoAuthTxStore struct {
	tx          *gorm.DB
	authRepo    *repository.AuthRepository
	sessionRepo *repository.SessionRepository
}

// NewRepoAuthDataStore builds a real MySQL+Redis AuthDataStore from repositories.
func NewRepoAuthDataStore(authRepo *repository.AuthRepository, sessionRepo *repository.SessionRepository) AuthDataStore {
	return &repoAuthDataStore{
		authRepo:    authRepo,
		sessionRepo: sessionRepo,
	}
}

func (s *repoAuthDataStore) WithTx(ctx context.Context, fn func(tx AuthTxStore) error) error {
	return s.authRepo.WithTx(ctx, func(tx *gorm.DB) error {
		return fn(&repoAuthTxStore{
			tx:          tx,
			authRepo:    s.authRepo,
			sessionRepo: s.sessionRepo,
		})
	})
}

func (s *repoAuthDataStore) CreateUser(ctx context.Context, user *model.User) error {
	return s.authRepo.CreateUser(ctx, user)
}

func (s *repoAuthDataStore) FindUserByUsername(ctx context.Context, username string) (*model.User, error) {
	return s.authRepo.FindUserByUsername(ctx, username)
}

func (s *repoAuthDataStore) FindUserByID(ctx context.Context, userID uint64) (*model.User, error) {
	return s.authRepo.FindUserByID(ctx, userID)
}

func (s *repoAuthDataStore) FindSessionByRefreshHash(ctx context.Context, hash string) (*model.UserSession, error) {
	return s.sessionRepo.FindByRefreshTokenHash(ctx, hash)
}

func (s *repoAuthDataStore) BlacklistAccessJTIs(ctx context.Context, jtis []string, reason string, ttl time.Duration) error {
	return s.sessionRepo.BlacklistAccessJTIs(ctx, jtis, reason, ttl)
}

func (s *repoAuthDataStore) AcquireRefreshLock(ctx context.Context, refreshHash string, ttl time.Duration) (string, bool, error) {
	return s.sessionRepo.AcquireRefreshLock(ctx, refreshHash, ttl)
}

func (s *repoAuthDataStore) IsRefreshLockHeld(ctx context.Context, refreshHash string) (bool, error) {
	return s.sessionRepo.IsRefreshLockHeld(ctx, refreshHash)
}

func (s *repoAuthDataStore) ReleaseRefreshLock(ctx context.Context, refreshHash, token string) error {
	return s.sessionRepo.ReleaseRefreshLock(ctx, refreshHash, token)
}

func (s *repoAuthDataStore) IsUserOnline(ctx context.Context, userID uint64) (bool, error) {
	return s.sessionRepo.IsUserOnline(ctx, userID)
}

func (t *repoAuthTxStore) LockUserByID(ctx context.Context, userID uint64) (*model.User, error) {
	return t.authRepo.FindUserByIDForUpdate(ctx, t.tx, userID)
}

func (t *repoAuthTxStore) UpdateUserPasswordHash(ctx context.Context, userID uint64, passwordHash string) error {
	return t.authRepo.UpdatePasswordHash(ctx, t.tx, userID, passwordHash)
}

func (t *repoAuthTxStore) LockSessionByID(ctx context.Context, sessionID uint64) (*model.UserSession, error) {
	return t.sessionRepo.FindByIDForUpdate(ctx, t.tx, sessionID)
}

func (t *repoAuthTxStore) ListUnrevokedSessionsByDevice(ctx context.Context, userID uint64, deviceID string) ([]model.UserSession, error) {
	return t.sessionRepo.ListUnrevokedByUserDeviceForUpdate(ctx, t.tx, userID, deviceID)
}

func (t *repoAuthTxStore) ListUnrevokedSessionsByUser(ctx context.Context, userID uint64) ([]model.UserSession, error) {
	return t.sessionRepo.ListUnrevokedByUserForUpdate(ctx, t.tx, userID)
}

func (t *repoAuthTxStore) CountActiveSessionsByUser(ctx context.Context, userID uint64, now time.Time) (int64, error) {
	return t.sessionRepo.CountActiveByUser(ctx, t.tx, userID, now)
}

func (t *repoAuthTxStore) RevokeSessionsByIDs(ctx context.Context, sessionIDs []uint64, revokedAt time.Time) (int64, error) {
	return t.sessionRepo.RevokeByIDs(ctx, t.tx, sessionIDs, revokedAt)
}

func (t *repoAuthTxStore) CreateSession(ctx context.Context, session *model.UserSession) error {
	return t.sessionRepo.Create(ctx, t.tx, session)
}

func (t *repoAuthTxStore) RotateSessionTokens(ctx context.Context, sessionID uint64, newRefreshHash, newAccessJTI string, newExpiresAt, activeAt time.Time, ipAddress, userAgent string) error {
	return t.sessionRepo.RotateTokens(ctx, t.tx, sessionID, newRefreshHash, newAccessJTI, newExpiresAt, activeAt, ipAddress, userAgent)
}

// AuthService implements account registration, multi-device login, token rotation,
// logout, and password rotation.
type AuthService struct {
	store              AuthDataStore
	jwtMgr             *pkgjwt.Manager
	maxSessionsPerUser int
	bcryptCost         int
	log                *slog.Logger
	revokeHook         SessionRevokeHook
	nowFn              func() time.Time
}

// NewAuthServiceWithRepos constructs an AuthService backed by real MySQL and Redis repositories.
func NewAuthServiceWithRepos(
	authRepo *repository.AuthRepository,
	sessionRepo *repository.SessionRepository,
	jwtMgr *pkgjwt.Manager,
	authCfg *config.AuthConfig,
	log *slog.Logger,
	revokeHook SessionRevokeHook,
) *AuthService {
	return NewAuthService(NewRepoAuthDataStore(authRepo, sessionRepo), jwtMgr, authCfg, log, revokeHook)
}

// NewAuthService constructs an AuthService with an AuthDataStore.
func NewAuthService(
	store AuthDataStore,
	jwtMgr *pkgjwt.Manager,
	authCfg *config.AuthConfig,
	log *slog.Logger,
	revokeHook SessionRevokeHook,
) *AuthService {
	maxSessions := 5
	bcryptCost := password.DefaultCost
	if authCfg != nil {
		if authCfg.MaxSessionsPerUser > 0 {
			maxSessions = authCfg.MaxSessionsPerUser
		}
		if authCfg.BcryptCost >= password.DefaultCost {
			bcryptCost = authCfg.BcryptCost
		}
	}
	if log == nil {
		log = slog.Default()
	}
	if revokeHook == nil {
		revokeHook = NoopSessionRevokeHook
	}
	return &AuthService{
		store:              store,
		jwtMgr:             jwtMgr,
		maxSessionsPerUser: maxSessions,
		bcryptCost:         bcryptCost,
		log:                log,
		revokeHook:         revokeHook,
		nowFn:              func() time.Time { return time.Now().UTC() },
	}
}

// WithClock overrides the service clock for deterministic unit tests.
func (s *AuthService) WithClock(nowFn func() time.Time) *AuthService {
	if nowFn != nil {
		s.nowFn = nowFn
	}
	return s
}

// Register creates a new user account with role=user without issuing device sessions.
func (s *AuthService) Register(ctx context.Context, input RegisterInput) (RegisterOutput, *errcode.Error) {
	if err := s.ensureReady(); err != nil {
		return RegisterOutput{}, err
	}
	username, bizErr := validateUsername(input.Username)
	if bizErr != nil {
		return RegisterOutput{}, bizErr
	}
	nickname, bizErr := validateNickname(input.Nickname)
	if bizErr != nil {
		return RegisterOutput{}, bizErr
	}
	if bizErr := validateNewPassword(input.Password); bizErr != nil {
		return RegisterOutput{}, bizErr
	}

	hashed, err := password.Hash(input.Password, s.bcryptCost)
	if err != nil {
		s.log.ErrorContext(ctx, "auth_register_hash_failed", slog.Any("error", err))
		return RegisterOutput{}, errcode.ErrInternalConsistency
	}

	user := &model.User{
		Username:     username,
		PasswordHash: hashed,
		Nickname:     nickname,
		AvatarURL:    "",
		Signature:    "",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	if err := s.store.CreateUser(ctx, user); err != nil {
		if errors.Is(err, repository.ErrDuplicateUsername) {
			return RegisterOutput{}, errcode.ErrUsernameExists
		}
		s.log.ErrorContext(ctx, "auth_register_create_failed", slog.Any("error", err))
		return RegisterOutput{}, classifyDBError(err)
	}

	return RegisterOutput{
		User: newUserView(user, false),
	}, nil
}

// Login authenticates credentials, replaces any existing session on the same device_id,
// enforces active device limits, and issues Access + Refresh tokens.
func (s *AuthService) Login(ctx context.Context, input LoginInput) (LoginOutput, *errcode.Error) {
	if err := s.ensureReady(); err != nil {
		return LoginOutput{}, err
	}
	username, bizErr := validateUsername(input.Username)
	if bizErr != nil {
		return LoginOutput{}, bizErr
	}
	if input.Password == "" {
		return LoginOutput{}, errcode.ErrInvalidParams
	}
	deviceID, bizErr := validateDeviceID(input.DeviceID)
	if bizErr != nil {
		return LoginOutput{}, bizErr
	}
	deviceType, bizErr := validateDeviceType(input.DeviceType)
	if bizErr != nil {
		return LoginOutput{}, bizErr
	}

	user, err := s.store.FindUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, repository.ErrAuthUserNotFound) {
			_ = password.Verify(input.Password, dummyBcryptHash)
			return LoginOutput{}, errcode.ErrInvalidCredentials
		}
		s.log.ErrorContext(ctx, "auth_login_lookup_failed", slog.Any("error", err))
		return LoginOutput{}, classifyDBError(err)
	}

	if err := password.Verify(input.Password, user.PasswordHash); err != nil {
		return LoginOutput{}, errcode.ErrInvalidCredentials
	}
	if user.Status != model.UserStatusActive {
		return LoginOutput{}, errcode.ErrAccountDisabled
	}

	refreshRaw, refreshHash, err := pkgjwt.GenerateRefreshToken()
	if err != nil {
		s.log.ErrorContext(ctx, "auth_login_refresh_gen_failed", slog.Any("error", err))
		return LoginOutput{}, errcode.ErrInternalConsistency
	}
	accessJTI := pkgjwt.NewJTI()
	now := s.nowFn().UTC()
	refreshExp := now.Add(s.jwtMgr.RefreshTTL())

	var (
		lockedUser         *model.User
		createdSession     *model.UserSession
		replacedSessionIDs []uint64
		replacedJTIs       []string
	)

	txErr := s.store.WithTx(ctx, func(tx AuthTxStore) error {
		var err error
		lockedUser, err = tx.LockUserByID(ctx, user.ID)
		if err != nil {
			if errors.Is(err, repository.ErrAuthUserNotFound) {
				return errcode.ErrInvalidCredentials
			}
			return classifyDBError(err)
		}
		if lockedUser.Status != model.UserStatusActive {
			return errcode.ErrAccountDisabled
		}
		if lockedUser.PasswordHash != user.PasswordHash {
			if verifyErr := password.Verify(input.Password, lockedUser.PasswordHash); verifyErr != nil {
				return errcode.ErrInvalidCredentials
			}
		}

		sameDeviceSessions, err := tx.ListUnrevokedSessionsByDevice(ctx, lockedUser.ID, deviceID)
		if err != nil {
			return classifyDBError(err)
		}
		if len(sameDeviceSessions) > 0 {
			replacedSessionIDs, replacedJTIs = collectSessionIDsAndJTIs(sameDeviceSessions)
			if _, err := tx.RevokeSessionsByIDs(ctx, replacedSessionIDs, now); err != nil {
				return classifyDBError(err)
			}
		}

		activeCount, err := tx.CountActiveSessionsByUser(ctx, lockedUser.ID, now)
		if err != nil {
			return classifyDBError(err)
		}
		if activeCount >= int64(s.maxSessionsPerUser) {
			return errcode.ErrSessionLimitReached
		}

		sess := &model.UserSession{
			UserID:           lockedUser.ID,
			DeviceID:         deviceID,
			DeviceType:       deviceType,
			RefreshTokenHash: refreshHash,
			AccessJTI:        accessJTI,
			IPAddress:        truncateString(strings.TrimSpace(input.IPAddress), 64),
			UserAgent:        truncateString(strings.TrimSpace(input.UserAgent), 512),
			ExpiresAt:        refreshExp,
			LastActiveAt:     now,
		}
		if err := tx.CreateSession(ctx, sess); err != nil {
			return classifyDBError(err)
		}
		createdSession = sess
		return nil
	})
	if txErr != nil {
		return LoginOutput{}, asBizError(txErr, s.log, ctx, "auth_login_tx_failed")
	}

	accessToken, _, _, err := s.jwtMgr.IssueAccessTokenWithJTI(lockedUser.ID, createdSession.ID, lockedUser.Role, accessJTI)
	if err != nil {
		s.log.ErrorContext(ctx, "auth_login_sign_jwt_failed", slog.Any("error", err))
		return LoginOutput{}, errcode.ErrInternalConsistency
	}

	s.revokeSideEffects(ctx, lockedUser.ID, replacedSessionIDs, replacedJTIs, "session_replaced")

	online, onlineErr := s.store.IsUserOnline(ctx, lockedUser.ID)
	if onlineErr != nil {
		s.log.WarnContext(ctx, "auth_login_presence_read_failed",
			slog.Uint64("user_id", lockedUser.ID),
			slog.Any("error", onlineErr),
		)
		online = false
	}

	return LoginOutput{
		User: newUserView(lockedUser, online),
		Tokens: TokensView{
			AccessToken:      accessToken,
			RefreshToken:     refreshRaw,
			TokenType:        "Bearer",
			ExpiresIn:        s.jwtMgr.AccessTTLSeconds(),
			RefreshExpiresIn: s.jwtMgr.RefreshTTLSeconds(),
			SessionID:        strconv.FormatUint(createdSession.ID, 10),
			DeviceID:         deviceID,
		},
	}, nil
}

// Refresh validates refresh_token and device_id, locks the session, rotates both credentials,
// and guards against concurrent rotation races with error code 11008.
func (s *AuthService) Refresh(ctx context.Context, input RefreshInput) (RefreshOutput, *errcode.Error) {
	if err := s.ensureReady(); err != nil {
		return RefreshOutput{}, err
	}
	if input.RefreshToken == "" {
		return RefreshOutput{}, errcode.ErrInvalidParams
	}
	if strings.TrimSpace(input.RefreshToken) == "" {
		return RefreshOutput{}, errcode.ErrInvalidValue
	}
	deviceID, bizErr := validateDeviceID(input.DeviceID)
	if bizErr != nil {
		return RefreshOutput{}, bizErr
	}

	oldHash := pkgjwt.HashRefreshToken(input.RefreshToken)
	now := s.nowFn().UTC()

	sess, err := s.store.FindSessionByRefreshHash(ctx, oldHash)
	if err != nil {
		if errors.Is(err, repository.ErrSessionNotFound) {
			held, lockErr := s.store.IsRefreshLockHeld(ctx, oldHash)
			if lockErr != nil {
				s.log.ErrorContext(ctx, "auth_refresh_lock_check_failed", slog.Any("error", lockErr))
				return RefreshOutput{}, errcode.ErrRedisUnavailable
			}
			if held {
				return RefreshOutput{}, errcode.ErrRefreshConflict
			}
			return RefreshOutput{}, errcode.ErrRefreshTokenInvalid
		}
		s.log.ErrorContext(ctx, "auth_refresh_lookup_failed", slog.Any("error", err))
		return RefreshOutput{}, classifyDBError(err)
	}

	if sess.RevokedAt != nil || sess.DeviceID != deviceID {
		return RefreshOutput{}, errcode.ErrSessionRevoked
	}
	if !sess.ExpiresAt.After(now) {
		return RefreshOutput{}, errcode.ErrRefreshTokenInvalid
	}

	lockToken, acquired, lockErr := s.store.AcquireRefreshLock(ctx, oldHash, refreshLockTTL)
	if lockErr != nil {
		s.log.ErrorContext(ctx, "auth_refresh_lock_acquire_failed", slog.Any("error", lockErr))
		return RefreshOutput{}, errcode.ErrRedisUnavailable
	}
	if !acquired {
		return RefreshOutput{}, errcode.ErrRefreshConflict
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if relErr := s.store.ReleaseRefreshLock(releaseCtx, oldHash, lockToken); relErr != nil {
			s.log.Warn("auth_refresh_lock_release_failed", slog.Any("error", relErr))
		}
	}()

	newRefreshRaw, newRefreshHash, err := pkgjwt.GenerateRefreshToken()
	if err != nil {
		s.log.ErrorContext(ctx, "auth_refresh_gen_failed", slog.Any("error", err))
		return RefreshOutput{}, errcode.ErrInternalConsistency
	}
	newJTI := pkgjwt.NewJTI()
	newExpiresAt := now.Add(s.jwtMgr.RefreshTTL())

	var (
		lockedUser   *model.User
		lockedSess   *model.UserSession
		oldAccessJTI string
	)

	txErr := s.store.WithTx(ctx, func(tx AuthTxStore) error {
		var err error
		lockedUser, err = tx.LockUserByID(ctx, sess.UserID)
		if err != nil {
			if errors.Is(err, repository.ErrAuthUserNotFound) {
				return errcode.ErrRefreshTokenInvalid
			}
			return classifyDBError(err)
		}
		if lockedUser.Status != model.UserStatusActive {
			return errcode.ErrAccountDisabled
		}

		lockedSess, err = tx.LockSessionByID(ctx, sess.ID)
		if err != nil {
			if errors.Is(err, repository.ErrSessionNotFound) {
				return errcode.ErrRefreshTokenInvalid
			}
			return classifyDBError(err)
		}
		if lockedSess.RefreshTokenHash != oldHash {
			return errcode.ErrRefreshConflict
		}
		if lockedSess.RevokedAt != nil || lockedSess.DeviceID != deviceID {
			return errcode.ErrSessionRevoked
		}
		if !lockedSess.ExpiresAt.After(now) {
			return errcode.ErrRefreshTokenInvalid
		}

		oldAccessJTI = lockedSess.AccessJTI
		ip := truncateString(strings.TrimSpace(input.IPAddress), 64)
		ua := truncateString(strings.TrimSpace(input.UserAgent), 512)
		if err := tx.RotateSessionTokens(ctx, lockedSess.ID, newRefreshHash, newJTI, newExpiresAt, now, ip, ua); err != nil {
			return classifyDBError(err)
		}
		return nil
	})
	if txErr != nil {
		return RefreshOutput{}, asBizError(txErr, s.log, ctx, "auth_refresh_tx_failed")
	}

	accessToken, _, _, err := s.jwtMgr.IssueAccessTokenWithJTI(lockedUser.ID, lockedSess.ID, lockedUser.Role, newJTI)
	if err != nil {
		s.log.ErrorContext(ctx, "auth_refresh_sign_jwt_failed", slog.Any("error", err))
		return RefreshOutput{}, errcode.ErrInternalConsistency
	}

	if oldAccessJTI != "" {
		if blErr := s.store.BlacklistAccessJTIs(ctx, []string{oldAccessJTI}, "rotated", s.jwtMgr.AccessTTL()); blErr != nil {
			s.log.WarnContext(ctx, "auth_refresh_blacklist_old_jti_failed",
				slog.Uint64("user_id", lockedUser.ID),
				slog.Uint64("session_id", lockedSess.ID),
				slog.Any("error", blErr),
			)
		}
	}

	return RefreshOutput{
		Tokens: TokensView{
			AccessToken:      accessToken,
			RefreshToken:     newRefreshRaw,
			TokenType:        "Bearer",
			ExpiresIn:        s.jwtMgr.AccessTTLSeconds(),
			RefreshExpiresIn: s.jwtMgr.RefreshTTLSeconds(),
			SessionID:        strconv.FormatUint(lockedSess.ID, 10),
			DeviceID:         lockedSess.DeviceID,
		},
	}, nil
}

// Logout revokes either the current device session (default) or all active sessions for the user,
// blacklists their access JTIs, and invokes the WS close hook.
func (s *AuthService) Logout(ctx context.Context, input LogoutInput) (LogoutOutput, *errcode.Error) {
	if err := s.ensureReady(); err != nil {
		return LogoutOutput{}, err
	}
	if input.UserID == 0 || input.SessionID == 0 {
		return LogoutOutput{}, errcode.ErrAccessTokenInvalid
	}
	scope, bizErr := validateLogoutScope(input.Scope)
	if bizErr != nil {
		return LogoutOutput{}, bizErr
	}

	now := s.nowFn().UTC()
	var (
		revokedSessionIDs []uint64
		revokedJTIs       []string
		revokedCount      int64
	)

	txErr := s.store.WithTx(ctx, func(tx AuthTxStore) error {
		lockedUser, err := tx.LockUserByID(ctx, input.UserID)
		if err != nil {
			if errors.Is(err, repository.ErrAuthUserNotFound) {
				return errcode.ErrAccessTokenInvalid
			}
			return classifyDBError(err)
		}
		if lockedUser.Status != model.UserStatusActive {
			return errcode.ErrAccountDisabled
		}

		if scope == LogoutScopeCurrent {
			sess, err := tx.LockSessionByID(ctx, input.SessionID)
			if err != nil {
				if errors.Is(err, repository.ErrSessionNotFound) {
					return errcode.ErrSessionRevoked
				}
				return classifyDBError(err)
			}
			if sess.UserID != input.UserID {
				return errcode.ErrSessionRevoked
			}
			if sess.RevokedAt == nil {
				revokedSessionIDs = []uint64{sess.ID}
				revokedJTIs = []string{sess.AccessJTI}
				revokedCount, err = tx.RevokeSessionsByIDs(ctx, revokedSessionIDs, now)
				if err != nil {
					return classifyDBError(err)
				}
			}
			return nil
		}

		sessions, err := tx.ListUnrevokedSessionsByUser(ctx, input.UserID)
		if err != nil {
			return classifyDBError(err)
		}
		if len(sessions) > 0 {
			revokedSessionIDs, revokedJTIs = collectSessionIDsAndJTIs(sessions)
			revokedCount, err = tx.RevokeSessionsByIDs(ctx, revokedSessionIDs, now)
			if err != nil {
				return classifyDBError(err)
			}
		}
		return nil
	})
	if txErr != nil {
		return LogoutOutput{}, asBizError(txErr, s.log, ctx, "auth_logout_tx_failed")
	}

	if input.CurrentJTI != "" {
		revokedJTIs = append(revokedJTIs, input.CurrentJTI)
	}
	s.revokeSideEffects(ctx, input.UserID, revokedSessionIDs, revokedJTIs, "logout")

	return LogoutOutput{
		Scope:        scope,
		RevokedCount: revokedCount,
	}, nil
}

// ChangePassword verifies old_password, replaces password_hash with a new bcrypt hash,
// and revokes all user sessions in the same database transaction.
func (s *AuthService) ChangePassword(ctx context.Context, input ChangePasswordInput) (ChangePasswordOutput, *errcode.Error) {
	if err := s.ensureReady(); err != nil {
		return ChangePasswordOutput{}, err
	}
	if input.UserID == 0 {
		return ChangePasswordOutput{}, errcode.ErrAccessTokenInvalid
	}
	if input.OldPassword == "" || input.NewPassword == "" {
		return ChangePasswordOutput{}, errcode.ErrInvalidParams
	}
	if bizErr := validateNewPassword(input.NewPassword); bizErr != nil {
		return ChangePasswordOutput{}, bizErr
	}

	user, err := s.store.FindUserByID(ctx, input.UserID)
	if err != nil {
		if errors.Is(err, repository.ErrAuthUserNotFound) {
			return ChangePasswordOutput{}, errcode.ErrAccessTokenInvalid
		}
		s.log.ErrorContext(ctx, "auth_change_password_lookup_failed", slog.Any("error", err))
		return ChangePasswordOutput{}, classifyDBError(err)
	}
	if user.Status != model.UserStatusActive {
		return ChangePasswordOutput{}, errcode.ErrAccountDisabled
	}
	if err := password.Verify(input.OldPassword, user.PasswordHash); err != nil {
		return ChangePasswordOutput{}, errcode.ErrInvalidCredentials
	}

	newHash, err := password.Hash(input.NewPassword, s.bcryptCost)
	if err != nil {
		s.log.ErrorContext(ctx, "auth_change_password_hash_failed", slog.Any("error", err))
		return ChangePasswordOutput{}, errcode.ErrInternalConsistency
	}

	now := s.nowFn().UTC()
	var (
		revokedSessionIDs []uint64
		revokedJTIs       []string
		revokedCount      int64
	)

	txErr := s.store.WithTx(ctx, func(tx AuthTxStore) error {
		lockedUser, err := tx.LockUserByID(ctx, input.UserID)
		if err != nil {
			if errors.Is(err, repository.ErrAuthUserNotFound) {
				return errcode.ErrAccessTokenInvalid
			}
			return classifyDBError(err)
		}
		if lockedUser.Status != model.UserStatusActive {
			return errcode.ErrAccountDisabled
		}
		if lockedUser.PasswordHash != user.PasswordHash {
			if verifyErr := password.Verify(input.OldPassword, lockedUser.PasswordHash); verifyErr != nil {
				return errcode.ErrInvalidCredentials
			}
		}

		if err := tx.UpdateUserPasswordHash(ctx, lockedUser.ID, newHash); err != nil {
			return classifyDBError(err)
		}

		sessions, err := tx.ListUnrevokedSessionsByUser(ctx, lockedUser.ID)
		if err != nil {
			return classifyDBError(err)
		}
		if len(sessions) > 0 {
			revokedSessionIDs, revokedJTIs = collectSessionIDsAndJTIs(sessions)
			revokedCount, err = tx.RevokeSessionsByIDs(ctx, revokedSessionIDs, now)
			if err != nil {
				return classifyDBError(err)
			}
		}
		return nil
	})
	if txErr != nil {
		return ChangePasswordOutput{}, asBizError(txErr, s.log, ctx, "auth_change_password_tx_failed")
	}

	if input.CurrentJTI != "" {
		revokedJTIs = append(revokedJTIs, input.CurrentJTI)
	}
	s.revokeSideEffects(ctx, input.UserID, revokedSessionIDs, revokedJTIs, "password_changed")

	return ChangePasswordOutput{
		RevokedCount: revokedCount,
	}, nil
}

func (s *AuthService) ensureReady() *errcode.Error {
	if s == nil || s.store == nil || s.jwtMgr == nil {
		return errcode.ErrInternalConsistency
	}
	return nil
}

func (s *AuthService) revokeSideEffects(ctx context.Context, userID uint64, sessionIDs []uint64, jtis []string, reason string) {
	if len(jtis) > 0 {
		if err := s.store.BlacklistAccessJTIs(ctx, jtis, reason, s.jwtMgr.AccessTTL()); err != nil {
			s.log.WarnContext(ctx, "auth_blacklist_jti_failed",
				slog.Uint64("user_id", userID),
				slog.String("reason", reason),
				slog.Any("error", err),
			)
		}
	}
	if len(sessionIDs) > 0 && s.revokeHook != nil {
		if err := s.revokeHook(ctx, userID, sessionIDs, reason); err != nil {
			s.log.WarnContext(ctx, "auth_session_revoke_hook_failed",
				slog.Uint64("user_id", userID),
				slog.String("reason", reason),
				slog.Any("error", err),
			)
		}
	}
}

func newUserView(u *model.User, online bool) UserView {
	var lastOnline *string
	if u.LastOnlineAt != nil {
		formatted := u.LastOnlineAt.UTC().Format(timestampFmt)
		lastOnline = &formatted
	}
	return UserView{
		ID:           strconv.FormatUint(u.ID, 10),
		Username:     u.Username,
		Nickname:     u.Nickname,
		AvatarURL:    u.AvatarURL,
		Signature:    u.Signature,
		Online:       online,
		LastOnlineAt: lastOnline,
		Role:         u.Role,
		Status:       u.Status,
	}
}

func collectSessionIDsAndJTIs(sessions []model.UserSession) ([]uint64, []string) {
	ids := make([]uint64, 0, len(sessions))
	jtis := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
		if sess.AccessJTI != "" {
			jtis = append(jtis, sess.AccessJTI)
		}
	}
	return ids, jtis
}

func validateUsername(raw string) (string, *errcode.Error) {
	if raw == "" {
		return "", errcode.ErrInvalidParams
	}
	if !utf8.ValidString(raw) || strings.TrimSpace(raw) != raw {
		return "", errcode.ErrInvalidValue
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errcode.ErrInvalidValue
		}
	}
	n := utf8.RuneCountInString(raw)
	if n < 1 || n > maxUsernameRunes {
		return "", errcode.ErrInvalidValue
	}
	return raw, nil
}

func validateNickname(raw string) (string, *errcode.Error) {
	if raw == "" {
		return "", errcode.ErrInvalidParams
	}
	if !utf8.ValidString(raw) {
		return "", errcode.ErrInvalidValue
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsAny(trimmed, "\r\n\x00") {
		return "", errcode.ErrInvalidValue
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", errcode.ErrInvalidValue
		}
	}
	n := utf8.RuneCountInString(trimmed)
	if n < 1 || n > maxNicknameRunes {
		return "", errcode.ErrInvalidValue
	}
	return trimmed, nil
}

func validateNewPassword(raw string) *errcode.Error {
	err := password.Validate(raw)
	if err == nil {
		return nil
	}
	if errors.Is(err, password.ErrEmpty) {
		return errcode.ErrInvalidParams
	}
	return errcode.ErrInvalidValue
}

func validateDeviceID(raw string) (string, *errcode.Error) {
	if raw == "" {
		return "", errcode.ErrInvalidParams
	}
	if !utf8.ValidString(raw) || strings.TrimSpace(raw) != raw {
		return "", errcode.ErrInvalidValue
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errcode.ErrInvalidValue
		}
	}
	n := utf8.RuneCountInString(raw)
	if n < 1 || n > maxDeviceIDRunes {
		return "", errcode.ErrInvalidValue
	}
	return raw, nil
}

func validateDeviceType(raw string) (string, *errcode.Error) {
	if raw == "" {
		return "", errcode.ErrInvalidParams
	}
	switch raw {
	case model.DeviceTypeWeb, model.DeviceTypeDesktop, model.DeviceTypeMobile:
		return raw, nil
	default:
		return "", errcode.ErrInvalidValue
	}
}

func validateLogoutScope(raw string) (string, *errcode.Error) {
	if raw == "" {
		return LogoutScopeCurrent, nil
	}
	switch raw {
	case LogoutScopeCurrent, LogoutScopeAll:
		return raw, nil
	default:
		return "", errcode.ErrInvalidValue
	}
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

func classifyDBError(err error) *errcode.Error {
	if repository.IsDBUnavailableError(err) {
		return errcode.ErrMySQLUnavailable
	}
	return errcode.ErrInternalConsistency
}

func asBizError(err error, log *slog.Logger, ctx context.Context, msg string) *errcode.Error {
	var bizErr *errcode.Error
	if errors.As(err, &bizErr) {
		return bizErr
	}
	if log != nil {
		log.ErrorContext(ctx, msg, slog.Any("error", err))
	}
	return classifyDBError(err)
}
