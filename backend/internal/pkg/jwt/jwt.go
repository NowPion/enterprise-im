package jwt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"enterprise-im/backend/internal/config"
)

const (
	refreshTokenPrefix = "rt_"
	refreshRandomBytes = 32
)

var (
	// ErrInvalidConfig indicates missing or insecure JWT manager settings.
	ErrInvalidConfig = errors.New("invalid jwt manager configuration")
	// ErrTokenMalformed indicates a malformed or unsupported JWT string.
	ErrTokenMalformed = errors.New("malformed access token")
	// ErrTokenExpired indicates an expired JWT.
	ErrTokenExpired = errors.New("access token expired")
	// ErrTokenSignature indicates an invalid JWT signature or signing method.
	ErrTokenSignature = errors.New("invalid access token signature")
	// ErrTokenInvalidClaims indicates missing or invalid required claims (sub, jti, session_id, exp).
	ErrTokenInvalidClaims = errors.New("invalid access token claims")
)

// Claims represents the frozen Access JWT payload aligned with docs/api.md.
type Claims struct {
	SessionID string `json:"session_id"`
	Role      string `json:"role,omitempty"`
	jwtv5.RegisteredClaims
}

// UserID returns the parsed positive numeric user identifier from sub.
func (c *Claims) UserID() (uint64, error) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Subject), 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("%w: invalid sub", ErrTokenInvalidClaims)
	}
	return id, nil
}

// SessionIDUint64 returns the parsed positive numeric session identifier from session_id.
func (c *Claims) SessionIDUint64() (uint64, error) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.SessionID), 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("%w: invalid session_id", ErrTokenInvalidClaims)
	}
	return id, nil
}

// Manager issues and verifies Access JWTs and generates Refresh tokens with SHA-256 digests.
type Manager struct {
	secret     []byte
	issuer     string
	accessTTL  time.Duration
	refreshTTL time.Duration
	nowFn      func() time.Time
}

// NewManager constructs a JWT Manager from cfg.
func NewManager(cfg config.JWTConfig) (*Manager, error) {
	return NewManagerWithClock(cfg, nil)
}

// NewManagerWithClock constructs a JWT Manager with a custom UTC clock function (used for testing).
func NewManagerWithClock(cfg config.JWTConfig, nowFn func() time.Time) (*Manager, error) {
	secret := strings.TrimSpace(cfg.Secret)
	if len(secret) < 32 {
		return nil, fmt.Errorf("%w: secret must be at least 32 bytes", ErrInvalidConfig)
	}
	issuer := strings.TrimSpace(cfg.Issuer)
	if issuer == "" {
		return nil, fmt.Errorf("%w: issuer must not be empty", ErrInvalidConfig)
	}
	if cfg.AccessTTL <= 0 || cfg.RefreshTTL <= 0 || cfg.RefreshTTL <= cfg.AccessTTL {
		return nil, fmt.Errorf("%w: invalid access/refresh TTL", ErrInvalidConfig)
	}
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return &Manager{
		secret:     []byte(secret),
		issuer:     issuer,
		accessTTL:  cfg.AccessTTL,
		refreshTTL: cfg.RefreshTTL,
		nowFn:      nowFn,
	}, nil
}

// AccessTTL returns the configured access token lifetime.
func (m *Manager) AccessTTL() time.Duration {
	return m.accessTTL
}

// RefreshTTL returns the configured refresh token lifetime.
func (m *Manager) RefreshTTL() time.Duration {
	return m.refreshTTL
}

// AccessTTLSeconds returns the access token lifetime in whole seconds.
func (m *Manager) AccessTTLSeconds() int64 {
	return int64(m.accessTTL.Seconds())
}

// RefreshTTLSeconds returns the refresh token lifetime in whole seconds.
func (m *Manager) RefreshTTLSeconds() int64 {
	return int64(m.refreshTTL.Seconds())
}

// IssueAccessToken signs a new HS256 Access JWT bound to userID (sub), jti, sessionID, and exp.
func (m *Manager) IssueAccessToken(userID, sessionID uint64, role string) (tokenString string, jti string, expiresAt time.Time, err error) {
	if userID == 0 || sessionID == 0 {
		return "", "", time.Time{}, fmt.Errorf("%w: userID and sessionID must be positive", ErrTokenInvalidClaims)
	}
	now := m.nowFn().UTC()
	expiresAt = now.Add(m.accessTTL)
	jti = uuid.NewString()

	claims := Claims{
		SessionID: strconv.FormatUint(sessionID, 10),
		Role:      strings.TrimSpace(role),
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   strconv.FormatUint(userID, 10),
			ID:        jti,
			IssuedAt:  jwtv5.NewNumericDate(now),
			ExpiresAt: jwtv5.NewNumericDate(expiresAt),
		},
	}

	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims)
	signed, err := tok.SignedString(m.secret)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("sign access jwt: %w", err)
	}
	return signed, jti, expiresAt, nil
}

// ParseAccessToken validates HS256 signature, issuer, expiration, and required claims.
func (m *Manager) ParseAccessToken(tokenString string) (*Claims, error) {
	raw := strings.TrimSpace(tokenString)
	if raw == "" {
		return nil, ErrTokenMalformed
	}

	claims := &Claims{}
	parser := jwtv5.NewParser(
		jwtv5.WithValidMethods([]string{jwtv5.SigningMethodHS256.Alg()}),
		jwtv5.WithIssuer(m.issuer),
		jwtv5.WithTimeFunc(m.nowFn),
		jwtv5.WithExpirationRequired(),
	)

	token, err := parser.ParseWithClaims(raw, claims, func(t *jwtv5.Token) (any, error) {
		if t.Method == nil || t.Method.Alg() != jwtv5.SigningMethodHS256.Alg() {
			return nil, ErrTokenSignature
		}
		return m.secret, nil
	})
	if err != nil {
		switch {
		case errors.Is(err, jwtv5.ErrTokenExpired):
			return nil, ErrTokenExpired
		case errors.Is(err, jwtv5.ErrTokenSignatureInvalid), errors.Is(err, ErrTokenSignature):
			return nil, ErrTokenSignature
		case errors.Is(err, jwtv5.ErrTokenMalformed):
			return nil, ErrTokenMalformed
		default:
			return nil, fmt.Errorf("%w: %v", ErrTokenInvalidClaims, err)
		}
	}
	if token == nil || !token.Valid {
		return nil, ErrTokenInvalidClaims
	}
	if strings.TrimSpace(claims.ID) == "" || claims.ExpiresAt == nil {
		return nil, ErrTokenInvalidClaims
	}
	if _, err := claims.UserID(); err != nil {
		return nil, err
	}
	if _, err := claims.SessionIDUint64(); err != nil {
		return nil, err
	}
	return claims, nil
}

// GenerateRefreshToken produces a high-entropy opaque refresh token and its SHA-256 hex digest.
func GenerateRefreshToken() (raw string, sha256Hex string, err error) {
	buf := make([]byte, refreshRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate random refresh token: %w", err)
	}
	raw = refreshTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashRefreshToken(raw), nil
}

// HashRefreshToken computes the 64-char lowercase SHA-256 hex digest stored in user_sessions.
func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
