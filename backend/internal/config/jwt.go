package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

const (
	minJWTSecretBytes  = 32
	minBcryptCost      = 10
	maxBcryptCost      = 14
	defaultJWTSecretPL = "replace-with-32byte-jwt-secret-in-env"
)

// JWTConfig holds JWT signing and token lifetime configuration.
type JWTConfig struct {
	Secret     string        `mapstructure:"secret"`
	Issuer     string        `mapstructure:"issuer"`
	AccessTTL  time.Duration `mapstructure:"access_ttl"`
	RefreshTTL time.Duration `mapstructure:"refresh_ttl"`
}

// AuthConfig holds JWT settings and authentication policy limits.
type AuthConfig struct {
	AppEnv             string    `mapstructure:"-"`
	JWT                JWTConfig `mapstructure:"jwt"`
	MaxSessionsPerUser int       `mapstructure:"max_sessions_per_user"`
	BcryptCost         int       `mapstructure:"bcrypt_cost"`
}

type authYAMLWrapper struct {
	App struct {
		Env string `mapstructure:"env"`
	} `mapstructure:"app"`
	JWT  JWTConfig `mapstructure:"jwt"`
	Auth struct {
		MaxSessionsPerUser int `mapstructure:"max_sessions_per_user"`
		BcryptCost         int `mapstructure:"bcrypt_cost"`
	} `mapstructure:"auth"`
}

// LoadJWT loads JWTConfig from YAML and environment variables using the same
// config path resolution rules as Load.
func LoadJWT(configPath string) (JWTConfig, error) {
	cfg, err := LoadAuthConfig(configPath)
	if err != nil {
		return JWTConfig{}, err
	}
	return cfg.JWT, nil
}

// LoadAuthConfig loads JWT and authentication policy settings from YAML and environment variables.
func LoadAuthConfig(configPath string) (*AuthConfig, error) {
	v := viper.New()
	setJWTDefaults(v)
	bindJWTEnvs(v)

	path := strings.TrimSpace(configPath)
	if path == "" {
		if envPath := strings.TrimSpace(os.Getenv("IM_CONFIG_FILE")); envPath != "" {
			path = envPath
		} else if envPath := strings.TrimSpace(os.Getenv("CONFIG_FILE")); envPath != "" {
			path = envPath
		}
	}

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config file %q: %w", path, err)
		}
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		v.AddConfigPath("./backend/configs")
		v.AddConfigPath(".")
		if err := v.ReadInConfig(); err != nil {
			var notFound viper.ConfigFileNotFoundError
			if !errors.As(err, &notFound) {
				return nil, fmt.Errorf("read default config: %w", err)
			}
		}
	}

	var raw authYAMLWrapper
	decodeOpt := viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
	))
	if err := v.Unmarshal(&raw, decodeOpt); err != nil {
		return nil, fmt.Errorf("unmarshal jwt/auth config: %w", err)
	}

	cfg := &AuthConfig{
		AppEnv:             strings.ToLower(strings.TrimSpace(raw.App.Env)),
		JWT:                raw.JWT,
		MaxSessionsPerUser: raw.Auth.MaxSessionsPerUser,
		BcryptCost:         raw.Auth.BcryptCost,
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *AuthConfig) normalize() {
	if c.AppEnv == "" {
		c.AppEnv = "development"
	}
	c.JWT.Secret = strings.TrimSpace(c.JWT.Secret)
	c.JWT.Issuer = strings.TrimSpace(c.JWT.Issuer)
}

// Validate checks JWT and auth policy invariants.
func (c *AuthConfig) Validate() error {
	if err := c.JWT.Validate(c.AppEnv); err != nil {
		return err
	}
	if c.MaxSessionsPerUser < 1 {
		return fmt.Errorf("invalid auth.max_sessions_per_user %d: must be >= 1", c.MaxSessionsPerUser)
	}
	if c.BcryptCost < minBcryptCost || c.BcryptCost > maxBcryptCost {
		return fmt.Errorf("invalid auth.bcrypt_cost %d: must be in [%d, %d]", c.BcryptCost, minBcryptCost, maxBcryptCost)
	}
	return nil
}

// Validate checks JWT configuration invariants for the given runtime environment.
func (j JWTConfig) Validate(appEnv string) error {
	secret := strings.TrimSpace(j.Secret)
	if secret == "" {
		return errors.New("jwt.secret must not be empty")
	}
	if len(secret) < minJWTSecretBytes {
		return fmt.Errorf("jwt.secret length must be at least %d bytes", minJWTSecretBytes)
	}
	if strings.EqualFold(strings.TrimSpace(appEnv), "production") && IsPlaceholderJWTSecret(secret) {
		return errors.New("jwt.secret must not use a placeholder value in production")
	}
	if strings.TrimSpace(j.Issuer) == "" {
		return errors.New("jwt.issuer must not be empty")
	}
	if j.AccessTTL <= 0 {
		return errors.New("jwt.access_ttl must be positive")
	}
	if j.RefreshTTL <= 0 {
		return errors.New("jwt.refresh_ttl must be positive")
	}
	if j.RefreshTTL <= j.AccessTTL {
		return errors.New("jwt.refresh_ttl must be greater than jwt.access_ttl")
	}
	return nil
}

// IsPlaceholderJWTSecret reports whether s matches a non-production placeholder pattern.
func IsPlaceholderJWTSecret(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" || lower == defaultJWTSecretPL {
		return true
	}
	for _, marker := range []string{"replace-with", "replace_with", "placeholder", "change-me", "changeme"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// RedactedSummary returns non-sensitive auth configuration metadata safe for logging.
func (c *AuthConfig) RedactedSummary() map[string]any {
	return map[string]any{
		"jwt_secret":            "[REDACTED]",
		"jwt_issuer":            c.JWT.Issuer,
		"jwt_access_ttl":        c.JWT.AccessTTL.String(),
		"jwt_refresh_ttl":       c.JWT.RefreshTTL.String(),
		"max_sessions_per_user": c.MaxSessionsPerUser,
		"bcrypt_cost":           c.BcryptCost,
	}
}

func setJWTDefaults(v *viper.Viper) {
	v.SetDefault("app.env", "development")
	v.SetDefault("jwt.secret", defaultJWTSecretPL)
	v.SetDefault("jwt.issuer", "enterprise-im")
	v.SetDefault("jwt.access_ttl", "15m")
	v.SetDefault("jwt.refresh_ttl", "168h")
	v.SetDefault("auth.max_sessions_per_user", 5)
	v.SetDefault("auth.bcrypt_cost", minBcryptCost)
}

func bindJWTEnvs(v *viper.Viper) {
	bindings := map[string][]string{
		"app.env":                    {"IM_APP_ENV", "APP_ENV"},
		"jwt.secret":                 {"IM_JWT_SECRET", "JWT_SECRET"},
		"jwt.issuer":                 {"IM_JWT_ISSUER", "JWT_ISSUER"},
		"jwt.access_ttl":             {"IM_JWT_ACCESS_TTL", "JWT_ACCESS_TTL"},
		"jwt.refresh_ttl":            {"IM_JWT_REFRESH_TTL", "JWT_REFRESH_TTL"},
		"auth.max_sessions_per_user": {"IM_AUTH_MAX_SESSIONS_PER_USER", "AUTH_MAX_SESSIONS_PER_USER"},
		"auth.bcrypt_cost":           {"IM_AUTH_BCRYPT_COST", "AUTH_BCRYPT_COST"},
	}
	for key, envs := range bindings {
		args := append([]string{key}, envs...)
		_ = v.BindEnv(args...)
	}
}
