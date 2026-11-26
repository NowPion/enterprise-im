package middleware

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/repository"
)

const ContextKeyAuthContext = "auth_context"

// AuthContext holds the verified user, session, and token identity attached by JWTAuth.
type AuthContext struct {
	UserID           uint64
	SessionID        uint64
	DeviceID         string
	Role             string
	JTI              string
	ExpiresAt        time.Time
	SessionExpiresAt time.Time
}

// AccessTokenVerifier verifies and parses Access JWT tokens.
type AccessTokenVerifier interface {
	ParseAccessToken(tokenString string) (*pkgjwt.Claims, error)
}

// AuthStateStore checks user account status, Redis JTI blacklist, and active session state.
type AuthStateStore interface {
	GetUserAuthStatus(ctx context.Context, userID uint64) (status int8, role string, err error)
	IsAccessJTIBlacklisted(ctx context.Context, jti string) (bool, string, error)
	GetSessionAuthState(ctx context.Context, sessionID uint64) (repository.SessionAuthState, error)
}

type compositeAuthStateStore struct {
	authRepo    *repository.AuthRepository
	sessionRepo *repository.SessionRepository
}

func (s compositeAuthStateStore) GetUserAuthStatus(ctx context.Context, userID uint64) (int8, string, error) {
	return s.authRepo.GetUserAuthStatus(ctx, userID)
}

func (s compositeAuthStateStore) IsAccessJTIBlacklisted(ctx context.Context, jti string) (bool, string, error) {
	return s.sessionRepo.IsAccessJTIBlacklisted(ctx, jti)
}

func (s compositeAuthStateStore) GetSessionAuthState(ctx context.Context, sessionID uint64) (repository.SessionAuthState, error) {
	return s.sessionRepo.GetSessionAuthState(ctx, sessionID)
}

// NewAuthStateStore adapts AuthRepository and SessionRepository into an AuthStateStore.
func NewAuthStateStore(authRepo *repository.AuthRepository, sessionRepo *repository.SessionRepository) AuthStateStore {
	return compositeAuthStateStore{
		authRepo:    authRepo,
		sessionRepo: sessionRepo,
	}
}

// JWTAuth validates the Bearer Access JWT, verifies user status (not disabled),
// checks the Redis JTI blacklist, and validates the backing user_sessions row and access_jti.
func JWTAuth(verifier AccessTokenVerifier, store AuthStateStore) gin.HandlerFunc {
	return JWTAuthWithClock(verifier, store, nil)
}

// JWTAuthWithClock creates a JWTAuth middleware with an optional UTC clock function.
func JWTAuthWithClock(verifier AccessTokenVerifier, store AuthStateStore, nowFn func() time.Time) gin.HandlerFunc {
	auth := NewAccessAuthenticator(verifier, store)
	if nowFn != nil {
		auth.now = nowFn
	}
	return func(c *gin.Context) {
		token, _ := BearerToken(c.GetHeader("Authorization"))
		identity, failure := auth.Authenticate(c.Request.Context(), token)
		if failure != nil {
			errcode.Fail(c, failure)
			c.Abort()
			return
		}
		errcode.SetUserID(c, strconv.FormatUint(identity.UserID, 10))
		c.Set(ContextKeyAuthContext, identity)
		c.Next()
	}
}

type AccessAuthenticator struct {
	verifier AccessTokenVerifier
	store    AuthStateStore
	now      func() time.Time
}

func NewAccessAuthenticator(verifier AccessTokenVerifier, store AuthStateStore) *AccessAuthenticator {
	return &AccessAuthenticator{verifier: verifier, store: store, now: time.Now}
}

func (auth *AccessAuthenticator) Authenticate(ctx context.Context, token string) (AuthContext, *errcode.Error) {
	if auth.verifier == nil || auth.store == nil {
		return AuthContext{}, errcode.ErrInternalConsistency
	}
	claims, err := auth.verifier.ParseAccessToken(token)
	if err != nil || claims == nil || claims.ExpiresAt == nil || claims.ID == "" {
		return AuthContext{}, errcode.ErrAccessTokenInvalid
	}
	userID, userErr := claims.UserID()
	sessionID, sessionErr := claims.SessionIDUint64()
	if userErr != nil || sessionErr != nil {
		return AuthContext{}, errcode.ErrAccessTokenInvalid
	}
	identity, _, failure := auth.Check(ctx, AuthContext{UserID: userID, SessionID: sessionID, JTI: claims.ID, ExpiresAt: claims.ExpiresAt.Time.UTC()})
	return identity, failure
}

func (auth *AccessAuthenticator) Check(ctx context.Context, identity AuthContext) (AuthContext, string, *errcode.Error) {
	if !identity.ExpiresAt.After(auth.now()) {
		return identity, "", errcode.ErrAccessTokenInvalid
	}
	status, role, err := auth.store.GetUserAuthStatus(ctx, identity.UserID)
	if errors.Is(err, repository.ErrAuthUserNotFound) {
		return identity, "", errcode.ErrAccessTokenInvalid
	}
	if err != nil {
		return identity, "", errcode.ErrMySQLUnavailable
	}
	if status != model.UserStatusActive {
		return identity, "account.disabled", errcode.ErrAccountDisabled
	}
	blocked, reason, err := auth.store.IsAccessJTIBlacklisted(ctx, identity.JTI)
	if err != nil {
		return identity, "", errcode.ErrRedisUnavailable
	}
	if blocked {
		return identity, reason, errcode.ErrAccessTokenBlacklisted
	}
	session, err := auth.store.GetSessionAuthState(ctx, identity.SessionID)
	if errors.Is(err, repository.ErrSessionNotFound) {
		return identity, "", errcode.ErrSessionRevoked
	}
	if err != nil {
		return identity, "", errcode.ErrMySQLUnavailable
	}
	if session.UserID != identity.UserID || session.RevokedAt != nil || !session.ExpiresAt.After(auth.now()) ||
		session.DeviceID == "" || (identity.DeviceID != "" && identity.DeviceID != session.DeviceID) {
		return identity, "", errcode.ErrSessionRevoked
	}
	if session.AccessJTI != identity.JTI {
		return identity, "", errcode.ErrAccessTokenBlacklisted
	}
	identity.Role, identity.DeviceID, identity.SessionExpiresAt = role, session.DeviceID, session.ExpiresAt.UTC()
	return identity, "", nil
}

// GetAuthContext retrieves the authenticated identity attached by JWTAuth.
func GetAuthContext(c *gin.Context) (AuthContext, bool) {
	if c == nil {
		return AuthContext{}, false
	}
	v, ok := c.Get(ContextKeyAuthContext)
	if !ok {
		return AuthContext{}, false
	}
	authCtx, ok := v.(AuthContext)
	return authCtx, ok
}

// GetAuthUserID returns the authenticated user's uint64 ID if present in context.
func GetAuthUserID(c *gin.Context) (uint64, bool) {
	authCtx, ok := GetAuthContext(c)
	if !ok || authCtx.UserID == 0 {
		return 0, false
	}
	return authCtx.UserID, true
}

// GetAuthSessionID returns the authenticated session's uint64 ID if present in context.
func GetAuthSessionID(c *gin.Context) (uint64, bool) {
	authCtx, ok := GetAuthContext(c)
	if !ok || authCtx.SessionID == 0 {
		return 0, false
	}
	return authCtx.SessionID, true
}

func BearerToken(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}
