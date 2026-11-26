package jwt

import (
	"errors"
	"sync"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"

	"enterprise-im/backend/internal/config"
)

func testJWTConfig() config.JWTConfig {
	return config.JWTConfig{
		Secret:     "unit-test-jwt-secret-key-32bytes!",
		Issuer:     "enterprise-im-test",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}
}

func TestIssueAndParseAccessToken(t *testing.T) {
	fixedNow := time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC)
	now := fixedNow
	mgr, err := NewManagerWithClock(testJWTConfig(), func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewManagerWithClock failed: %v", err)
	}

	tok, jti, exp, err := mgr.IssueAccessToken(10001, 20002, "admin")
	if err != nil {
		t.Fatalf("IssueAccessToken failed: %v", err)
	}
	if tok == "" || jti == "" {
		t.Fatal("expected non-empty token and jti")
	}
	if !exp.Equal(fixedNow.Add(15 * time.Minute)) {
		t.Fatalf("unexpected exp %v", exp)
	}

	claims, err := mgr.ParseAccessToken(tok)
	if err != nil {
		t.Fatalf("ParseAccessToken failed: %v", err)
	}
	if claims.Subject != "10001" || claims.SessionID != "20002" || claims.ID != jti || claims.Role != "admin" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	uid, err := claims.UserID()
	if err != nil || uid != 10001 {
		t.Fatalf("unexpected UserID(): %d err=%v", uid, err)
	}
	sid, err := claims.SessionIDUint64()
	if err != nil || sid != 20002 {
		t.Fatalf("unexpected SessionIDUint64(): %d err=%v", sid, err)
	}

	// Advance past expiration -> ErrTokenExpired
	now = fixedNow.Add(16 * time.Minute)
	if _, err := mgr.ParseAccessToken(tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestParseAccessTokenRejectsTamperedAlgNoneAndMissingClaims(t *testing.T) {
	mgr, err := NewManager(testJWTConfig())
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	otherCfg := testJWTConfig()
	otherCfg.Secret = "different-secret-key-at-least-32-b"
	otherMgr, _ := NewManager(otherCfg)
	foreignTok, _, _, _ := otherMgr.IssueAccessToken(1, 2, "user")
	if _, err := mgr.ParseAccessToken(foreignTok); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("expected ErrTokenSignature for wrong secret, got %v", err)
	}

	// Reject alg=none token.
	noneTok, err := jwtv5.NewWithClaims(jwtv5.SigningMethodNone, Claims{
		SessionID: "2",
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    testJWTConfig().Issuer,
			Subject:   "1",
			ID:        "jti-none",
			ExpiresAt: jwtv5.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString(jwtv5.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign alg=none: %v", err)
	}
	if _, err := mgr.ParseAccessToken(noneTok); err == nil {
		t.Fatal("expected alg=none token to be rejected")
	}

	// Reject token missing session_id or jti.
	missingClaimTok, err := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, Claims{
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    testJWTConfig().Issuer,
			Subject:   "1",
			ID:        "jti-1",
			ExpiresAt: jwtv5.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString([]byte(testJWTConfig().Secret))
	if err != nil {
		t.Fatalf("sign missing-claim token: %v", err)
	}
	if _, err := mgr.ParseAccessToken(missingClaimTok); !errors.Is(err, ErrTokenInvalidClaims) {
		t.Fatalf("expected ErrTokenInvalidClaims, got %v", err)
	}
}

func TestGenerateRefreshTokenUniquenessAndHash(t *testing.T) {
	const n = 32
	seenRaw := make(map[string]struct{}, n)
	seenHash := make(map[string]struct{}, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			raw, digest, err := GenerateRefreshToken()
			if err != nil {
				t.Errorf("GenerateRefreshToken failed: %v", err)
				return
			}
			if len(digest) != 64 || digest != HashRefreshToken(raw) {
				t.Errorf("invalid sha256 digest length or mismatch: %q", digest)
				return
			}
			mu.Lock()
			seenRaw[raw] = struct{}{}
			seenHash[digest] = struct{}{}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seenRaw) != n || len(seenHash) != n {
		t.Fatalf("expected %d unique tokens/hashes, got raw=%d hash=%d", n, len(seenRaw), len(seenHash))
	}
}
