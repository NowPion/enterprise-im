package middleware

import (
	"context"
	"errors"
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
	UserID    uint64
	SessionID uint64
	DeviceID  string
	Role      string
	JTI       string
	ExpiresAt time.Time
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
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return func(c *gin.Context) {
		if verifier == nil || store == nil {
			errcode.Fail(c, errcode.ErrInternalConsistency)
			c.Abort()
			return
		}

		tokenStr, ok := extractBearerToken(c.GetHeader("Authorization"))
		if !ok {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}

		claims, err := verifier.ParseAccessToken(tokenStr)
		if err != nil {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}

		userID, err := claims.UserID()
		if err != nil {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		sessionID, err := claims.SessionIDUint64()
		if err != nil {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}

		ctx := c.Request.Context()

		status, role, err := store.GetUserAuthStatus(ctx, userID)
		if err != nil {
			if errors.Is(err, repository.ErrAuthUserNotFound) {
				errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			} else {
				errcode.Fail(c, errcode.ErrMySQLUnavailable)
			}
			c.Abort()
			return
		}
		if status != model.UserStatusActive {
			errcode.Fail(c, errcode.ErrAccountDisabled)
			c.Abort()
			return
		}

		blacklisted, _, err := store.IsAccessJTIBlacklisted(ctx, claims.ID)
		if err != nil {
			errcode.Fail(c, errcode.ErrRedisUnavailable)
			c.Abort()
			return
		}
		if blacklisted {
			errcode.Fail(c, errcode.ErrAccessTokenBlacklisted)
			c.Abort()
			return
		}

		sess, err := store.GetSessionAuthState(ctx, sessionID)
		if err != nil {
			if errors.Is(err, repository.ErrSessionNotFound) {
				errcode.Fail(c, errcode.ErrSessionRevoked)
			} else {
				errcode.Fail(c, errcode.ErrMySQLUnavailable)
			}
			c.Abort()
			return
		}
		if sess.UserID != userID || sess.RevokedAt != nil || !sess.ExpiresAt.After(nowFn().UTC()) {
			errcode.Fail(c, errcode.ErrSessionRevoked)
			c.Abort()
			return
		}
		if sess.AccessJTI != claims.ID {
			errcode.Fail(c, errcode.ErrAccessTokenBlacklisted)
			c.Abort()
			return
		}

		var exp time.Time
		if claims.ExpiresAt != nil {
			exp = claims.ExpiresAt.Time.UTC()
		}

		errcode.SetUserID(c, claims.Subject)
		c.Set(ContextKeyAuthContext, AuthContext{
			UserID:    userID,
			SessionID: sessionID,
			DeviceID:  sess.DeviceID,
			Role:      role,
			JTI:       claims.ID,
			ExpiresAt: exp,
		})
		c.Next()
	}
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

func extractBearerToken(header string) (string, bool) {
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
