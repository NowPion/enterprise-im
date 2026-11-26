package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadAuthConfigFromYAMLAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlContent := `app:
  env: development
jwt:
  secret: "dev-yaml-jwt-secret-placeholder-32bytes"
  issuer: yaml-issuer
  access_ttl: 10m
  refresh_ttl: 24h
auth:
  max_sessions_per_user: 3
  bcrypt_cost: 10
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	t.Setenv("JWT_SECRET", "override-secret-at-least-32-bytes-long!")
	t.Setenv("JWT_ISSUER", "env-issuer")
	t.Setenv("JWT_ACCESS_TTL", "5m")
	t.Setenv("JWT_REFRESH_TTL", "72h")
	t.Setenv("AUTH_MAX_SESSIONS_PER_USER", "8")
	t.Setenv("AUTH_BCRYPT_COST", "11")

	cfg, err := LoadAuthConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadAuthConfig failed: %v", err)
	}

	if cfg.JWT.Secret != "override-secret-at-least-32-bytes-long!" {
		t.Fatalf("expected env JWT_SECRET override, got %q", cfg.JWT.Secret)
	}
	if cfg.JWT.Issuer != "env-issuer" {
		t.Fatalf("expected env-issuer, got %q", cfg.JWT.Issuer)
	}
	if cfg.JWT.AccessTTL != 5*time.Minute || cfg.JWT.RefreshTTL != 72*time.Hour {
		t.Fatalf("unexpected TTLs: access=%v refresh=%v", cfg.JWT.AccessTTL, cfg.JWT.RefreshTTL)
	}
	if cfg.MaxSessionsPerUser != 8 || cfg.BcryptCost != 11 {
		t.Fatalf("unexpected auth limits: maxSessions=%d bcryptCost=%d", cfg.MaxSessionsPerUser, cfg.BcryptCost)
	}

	summary := cfg.RedactedSummary()
	if summary["jwt_secret"] != "[REDACTED]" {
		t.Fatalf("expected jwt_secret redacted, got %#v", summary["jwt_secret"])
	}
}

func TestLoadAuthConfigRepoDefaultAndProductionPlaceholderCheck(t *testing.T) {
	// Default repository config.yaml loads cleanly in development mode and also keeps config.Load working.
	t.Setenv("APP_ENV", "development")
	baseCfg, err := Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("base config.Load failed: %v", err)
	}
	if baseCfg.App.Name != "enterprise-im" {
		t.Fatalf("unexpected app name: %q", baseCfg.App.Name)
	}

	authCfg, err := LoadAuthConfig("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("LoadAuthConfig on repo default failed in development: %v", err)
	}
	if !IsPlaceholderJWTSecret(authCfg.JWT.Secret) {
		t.Fatalf("expected repo default jwt.secret to be marked as placeholder")
	}

	// Production mode must reject placeholder JWT secret.
	t.Setenv("APP_ENV", "production")
	if _, err := LoadAuthConfig("../../configs/config.yaml"); err == nil {
		t.Fatal("expected production mode to reject placeholder jwt.secret")
	}

	// Production mode succeeds with a real non-placeholder >= 32 byte secret via env.
	t.Setenv("IM_JWT_SECRET", "prod-test-key-with-sufficient-entropy-32b")
	prodCfg, err := LoadAuthConfig("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("expected production LoadAuthConfig to succeed with env secret: %v", err)
	}
	if prodCfg.JWT.AccessTTL != 15*time.Minute || prodCfg.JWT.RefreshTTL != 168*time.Hour {
		t.Fatalf("unexpected default TTLs: %+v", prodCfg.JWT)
	}
}

func TestLoadJWTValidationErrors(t *testing.T) {
	t.Setenv("APP_ENV", "development")

	t.Run("short_secret", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "too-short")
		if _, err := LoadJWT("../../configs/config.yaml"); err == nil {
			t.Fatal("expected error for short jwt.secret")
		}
	})

	t.Run("refresh_not_greater_than_access", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "valid-secret-that-exceeds-32-bytes-len")
		t.Setenv("JWT_ACCESS_TTL", "1h")
		t.Setenv("JWT_REFRESH_TTL", "1h")
		if _, err := LoadJWT("../../configs/config.yaml"); err == nil {
			t.Fatal("expected error when refresh_ttl <= access_ttl")
		}
	})

	t.Run("bcrypt_cost_out_of_range", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "valid-secret-that-exceeds-32-bytes-len")
		t.Setenv("AUTH_BCRYPT_COST", "9")
		if _, err := LoadAuthConfig("../../configs/config.yaml"); err == nil {
			t.Fatal("expected error when bcrypt_cost < 10")
		}
	})
}
