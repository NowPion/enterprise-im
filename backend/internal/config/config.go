package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// Config holds all runtime settings loaded from YAML and environment variables.
type Config struct {
	App       AppConfig       `mapstructure:"app"`
	Server    ServerConfig    `mapstructure:"server"`
	CORS      CORSConfig      `mapstructure:"cors"`
	Log       LogConfig       `mapstructure:"log"`
	MySQL     MySQLConfig     `mapstructure:"mysql"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Snowflake SnowflakeConfig `mapstructure:"snowflake"`
	Health    HealthConfig    `mapstructure:"health"`
	WS        WSConfig        `mapstructure:"ws"`
	Upload    UploadConfig    `mapstructure:"upload"`
	Chat      ChatConfig      `mapstructure:"chat"`
}

// UploadConfig holds local media storage paths, size/dimension/duration limits, and signed download TTL.
type UploadConfig struct {
	Dir                string        `mapstructure:"dir"`
	TempDir            string        `mapstructure:"temp_dir"`
	MaxImageBytes      int64         `mapstructure:"max_image_bytes"`
	MaxFileBytes       int64         `mapstructure:"max_file_bytes"`
	MaxVoiceBytes      int64         `mapstructure:"max_voice_bytes"`
	MaxAvatarBytes     int64         `mapstructure:"max_avatar_bytes"`
	MaxImageWidth      int           `mapstructure:"max_image_width"`
	MaxImageHeight     int           `mapstructure:"max_image_height"`
	MaxImagePixels     int64         `mapstructure:"max_image_pixels"`
	MaxVoiceDurationMs uint32        `mapstructure:"max_voice_duration_ms"`
	ThumbnailMaxDim    int           `mapstructure:"thumbnail_max_dim"`
	DownloadURLTTL     time.Duration `mapstructure:"download_url_ttl"`
}

// MaxPossibleBytes returns the largest per-purpose upload size limit configured.
func (u UploadConfig) MaxPossibleBytes() int64 {
	maxBytes := u.MaxImageBytes
	for _, v := range []int64{u.MaxFileBytes, u.MaxVoiceBytes, u.MaxAvatarBytes} {
		if v > maxBytes {
			maxBytes = v
		}
	}
	return maxBytes
}

// ChatConfig holds chat messaging and recall policy settings.
type ChatConfig struct {
	RecallWindow       time.Duration `mapstructure:"recall_window"`
	DirectPushFallback bool          `mapstructure:"direct_push_fallback"`
}

type WSConfig struct {
	HeartbeatTimeout time.Duration `mapstructure:"heartbeat_timeout"`
	WriteTimeout     time.Duration `mapstructure:"write_timeout"`
	MaxFrameBytes    int64         `mapstructure:"max_frame_bytes"`
}

// AppConfig holds application-level identity and runtime environment settings.
type AppConfig struct {
	Name   string `mapstructure:"name"`
	Env    string `mapstructure:"env"`
	NodeID string `mapstructure:"node_id"`
}

// IsProduction reports whether the application runs in production mode.
func (a AppConfig) IsProduction() bool {
	return strings.EqualFold(a.Env, "production")
}

// ServerConfig holds HTTP server bind address, timeouts, and payload caps.
type ServerConfig struct {
	Addr            string        `mapstructure:"addr"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	IdleTimeout     time.Duration `mapstructure:"idle_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	MaxBodyBytes    int64         `mapstructure:"max_body_bytes"`
}

// CORSConfig holds allowed cross-origin request origins.
type CORSConfig struct {
	AllowedOrigins []string `mapstructure:"allowed_origins"`
}

// LogConfig holds structured logging settings.
type LogConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
}

// MySQLConfig holds MySQL 8.0 connection and pool settings.
type MySQLConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	User            string        `mapstructure:"user"`
	Password        string        `mapstructure:"password"`
	Database        string        `mapstructure:"database"`
	Charset         string        `mapstructure:"charset"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"`
	DialTimeout     time.Duration `mapstructure:"dial_timeout"`
}

// DSN builds the go-sql-driver/mysql DSN without exposing credentials to logs.
func (m MySQLConfig) DSN() string {
	timeout := m.DialTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	charset := m.Charset
	if charset == "" {
		charset = "utf8mb4"
	}
	params := url.Values{}
	params.Set("charset", charset)
	params.Set("parseTime", "true")
	params.Set("loc", "UTC")
	params.Set("timeout", timeout.String())
	params.Set("collation", "utf8mb4_0900_ai_ci")
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?%s",
		m.User, m.Password, m.Host, m.Port, m.Database, params.Encode())
}

// RedisConfig holds Redis 7 client and pool settings.
type RedisConfig struct {
	Addr         string        `mapstructure:"addr"`
	Password     string        `mapstructure:"password"`
	DB           int           `mapstructure:"db"`
	PoolSize     int           `mapstructure:"pool_size"`
	MinIdleConns int           `mapstructure:"min_idle_conns"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

// SnowflakeConfig holds the unique Snowflake worker node identifier.
type SnowflakeConfig struct {
	NodeID int64 `mapstructure:"node_id"`
}

// HealthConfig holds dependency probe settings for /healthz.
type HealthConfig struct {
	ProbeTimeout time.Duration `mapstructure:"probe_timeout"`
}

// Load reads configuration from YAML (if present or explicitly specified) and
// applies environment variable overrides, then validates the resulting Config.
func Load(configPath string) (*Config, error) {
	v := viper.New()
	setDefaults(v)
	bindEnvs(v)

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

	var cfg Config
	decodeOpt := viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
	))
	if err := v.Unmarshal(&cfg, decodeOpt); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) normalize() {
	c.App.Env = strings.ToLower(strings.TrimSpace(c.App.Env))
	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	c.Log.Format = strings.ToLower(strings.TrimSpace(c.Log.Format))
	c.Upload.Dir = strings.TrimSpace(c.Upload.Dir)
	c.Upload.TempDir = strings.TrimSpace(c.Upload.TempDir)

	cleaned := make([]string, 0, len(c.CORS.AllowedOrigins))
	for _, origin := range c.CORS.AllowedOrigins {
		trimmed := strings.TrimSpace(origin)
		if trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	c.CORS.AllowedOrigins = cleaned
}

// Validate checks configuration invariants before startup.
func (c *Config) Validate() error {
	switch c.App.Env {
	case "development", "production":
	default:
		return fmt.Errorf("invalid app.env %q: must be development or production", c.App.Env)
	}
	if strings.TrimSpace(c.App.NodeID) == "" {
		return errors.New("app.node_id must not be empty")
	}
	if strings.TrimSpace(c.Server.Addr) == "" {
		return errors.New("server.addr must not be empty")
	}
	if c.Server.ShutdownTimeout <= 0 {
		return errors.New("server.shutdown_timeout must be positive")
	}
	if c.Server.MaxBodyBytes <= 0 {
		return errors.New("server.max_body_bytes must be positive")
	}
	if c.WS.HeartbeatTimeout < time.Millisecond || c.WS.WriteTimeout <= 0 || c.WS.MaxFrameBytes <= 0 || c.WS.MaxFrameBytes >= 1<<62 {
		return errors.New("ws requires a heartbeat_timeout of at least 1ms, positive write_timeout and valid max_frame_bytes")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log.level %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		return fmt.Errorf("invalid log.format %q", c.Log.Format)
	}
	if strings.TrimSpace(c.MySQL.Host) == "" {
		return errors.New("mysql.host must not be empty")
	}
	if c.MySQL.Port < 1 || c.MySQL.Port > 65535 {
		return fmt.Errorf("invalid mysql.port %d", c.MySQL.Port)
	}
	if strings.TrimSpace(c.MySQL.User) == "" {
		return errors.New("mysql.user must not be empty")
	}
	if strings.TrimSpace(c.MySQL.Database) == "" {
		return errors.New("mysql.database must not be empty")
	}
	if strings.TrimSpace(c.Redis.Addr) == "" {
		return errors.New("redis.addr must not be empty")
	}
	if c.Redis.DB < 0 {
		return fmt.Errorf("invalid redis.db %d", c.Redis.DB)
	}
	if c.Snowflake.NodeID < 0 || c.Snowflake.NodeID > 1023 {
		return fmt.Errorf("invalid snowflake.node_id %d: must be in [0, 1023]", c.Snowflake.NodeID)
	}
	if c.Health.ProbeTimeout <= 0 {
		return errors.New("health.probe_timeout must be positive")
	}
	if strings.TrimSpace(c.Upload.Dir) == "" {
		return errors.New("upload.dir must not be empty")
	}
	if c.Upload.MaxImageBytes <= 0 || c.Upload.MaxFileBytes <= 0 || c.Upload.MaxVoiceBytes <= 0 || c.Upload.MaxAvatarBytes <= 0 {
		return errors.New("upload max byte limits must be positive")
	}
	if c.Upload.MaxImageWidth <= 0 || c.Upload.MaxImageHeight <= 0 || c.Upload.MaxImagePixels <= 0 {
		return errors.New("upload image dimension and pixel limits must be positive")
	}
	if c.Upload.MaxVoiceDurationMs == 0 {
		return errors.New("upload.max_voice_duration_ms must be positive")
	}
	if c.Upload.ThumbnailMaxDim <= 0 {
		return errors.New("upload.thumbnail_max_dim must be positive")
	}
	if c.Upload.DownloadURLTTL <= 0 {
		return errors.New("upload.download_url_ttl must be positive")
	}
	if c.Chat.RecallWindow <= 0 {
		return errors.New("chat.recall_window must be positive")
	}
	return nil
}

// RedactedSummary returns non-sensitive runtime metadata safe for startup logs.
func (c *Config) RedactedSummary() map[string]any {
	return map[string]any{
		"app_name":           c.App.Name,
		"app_env":            c.App.Env,
		"node_id":            c.App.NodeID,
		"server_addr":        c.Server.Addr,
		"shutdown_timeout":   c.Server.ShutdownTimeout.String(),
		"log_level":          c.Log.Level,
		"log_format":         c.Log.Format,
		"mysql_host":         c.MySQL.Host,
		"mysql_port":         c.MySQL.Port,
		"mysql_database":     c.MySQL.Database,
		"mysql_password":     "[REDACTED]",
		"redis_addr":         c.Redis.Addr,
		"redis_db":           c.Redis.DB,
		"redis_password":     "[REDACTED]",
		"snowflake_node_id":  c.Snowflake.NodeID,
		"allowed_origins":          c.CORS.AllowedOrigins,
		"chat_recall_window":       c.Chat.RecallWindow.String(),
		"chat_direct_push_fallback": c.Chat.DirectPushFallback,
	}
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("app.name", "enterprise-im")
	v.SetDefault("app.env", "development")
	v.SetDefault("app.node_id", "node-1")

	v.SetDefault("server.addr", ":8080")
	v.SetDefault("server.read_timeout", "10s")
	v.SetDefault("server.write_timeout", "15s")
	v.SetDefault("server.idle_timeout", "60s")
	v.SetDefault("server.shutdown_timeout", "15s")
	v.SetDefault("server.max_body_bytes", 1048576)
	v.SetDefault("ws.heartbeat_timeout", "75s")
	v.SetDefault("ws.write_timeout", "5s")
	v.SetDefault("ws.max_frame_bytes", 1048576)

	v.SetDefault("cors.allowed_origins", []string{"http://localhost:5173", "http://127.0.0.1:5173"})

	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")

	v.SetDefault("mysql.host", "127.0.0.1")
	v.SetDefault("mysql.port", 3306)
	v.SetDefault("mysql.user", "root")
	v.SetDefault("mysql.password", "")
	v.SetDefault("mysql.database", "enterprise_im")
	v.SetDefault("mysql.charset", "utf8mb4")
	v.SetDefault("mysql.max_open_conns", 50)
	v.SetDefault("mysql.max_idle_conns", 10)
	v.SetDefault("mysql.conn_max_lifetime", "30m")
	v.SetDefault("mysql.conn_max_idle_time", "10m")
	v.SetDefault("mysql.dial_timeout", "5s")

	v.SetDefault("redis.addr", "127.0.0.1:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.pool_size", 50)
	v.SetDefault("redis.min_idle_conns", 10)
	v.SetDefault("redis.dial_timeout", "5s")
	v.SetDefault("redis.read_timeout", "3s")
	v.SetDefault("redis.write_timeout", "3s")

	v.SetDefault("snowflake.node_id", 1)
	v.SetDefault("health.probe_timeout", "2s")

	v.SetDefault("upload.dir", "./uploads")
	v.SetDefault("upload.temp_dir", "")
	v.SetDefault("upload.max_image_bytes", 10485760)
	v.SetDefault("upload.max_file_bytes", 52428800)
	v.SetDefault("upload.max_voice_bytes", 10485760)
	v.SetDefault("upload.max_avatar_bytes", 5242880)
	v.SetDefault("upload.max_image_width", 8192)
	v.SetDefault("upload.max_image_height", 8192)
	v.SetDefault("upload.max_image_pixels", 25000000)
	v.SetDefault("upload.max_voice_duration_ms", 300000)
	v.SetDefault("upload.thumbnail_max_dim", 320)
	v.SetDefault("upload.download_url_ttl", "15m")
	v.SetDefault("chat.recall_window", "2m")
	v.SetDefault("chat.direct_push_fallback", false)
}

func bindEnvs(v *viper.Viper) {
	bindings := map[string][]string{
		"chat.recall_window":        {"IM_CHAT_RECALL_WINDOW", "CHAT_RECALL_WINDOW"},
		"chat.direct_push_fallback": {"IM_CHAT_DIRECT_PUSH_FALLBACK", "CHAT_DIRECT_PUSH_FALLBACK"},
		"ws.heartbeat_timeout":         {"IM_WS_HEARTBEAT_TIMEOUT", "WS_HEARTBEAT_TIMEOUT"},
		"ws.write_timeout":             {"IM_WS_WRITE_TIMEOUT", "WS_WRITE_TIMEOUT"},
		"ws.max_frame_bytes":           {"IM_WS_MAX_FRAME_BYTES", "WS_MAX_FRAME_BYTES"},
		"app.name":                     {"IM_APP_NAME", "APP_NAME"},
		"app.env":                      {"IM_APP_ENV", "APP_ENV"},
		"app.node_id":                  {"IM_APP_NODE_ID", "APP_NODE_ID"},
		"server.addr":                  {"IM_SERVER_ADDR", "SERVER_ADDR"},
		"server.read_timeout":          {"IM_SERVER_READ_TIMEOUT", "SERVER_READ_TIMEOUT"},
		"server.write_timeout":         {"IM_SERVER_WRITE_TIMEOUT", "SERVER_WRITE_TIMEOUT"},
		"server.idle_timeout":          {"IM_SERVER_IDLE_TIMEOUT", "SERVER_IDLE_TIMEOUT"},
		"server.shutdown_timeout":      {"IM_SERVER_SHUTDOWN_TIMEOUT", "SERVER_SHUTDOWN_TIMEOUT"},
		"server.max_body_bytes":        {"IM_SERVER_MAX_BODY_BYTES", "SERVER_MAX_BODY_BYTES"},
		"cors.allowed_origins":         {"IM_CORS_ALLOWED_ORIGINS", "CORS_ALLOWED_ORIGINS"},
		"log.level":                    {"IM_LOG_LEVEL", "LOG_LEVEL"},
		"log.format":                   {"IM_LOG_FORMAT", "LOG_FORMAT"},
		"mysql.host":                   {"IM_MYSQL_HOST", "MYSQL_HOST"},
		"mysql.port":                   {"IM_MYSQL_PORT", "MYSQL_PORT"},
		"mysql.user":                   {"IM_MYSQL_USER", "MYSQL_USER"},
		"mysql.password":               {"IM_MYSQL_PASSWORD", "MYSQL_PASSWORD"},
		"mysql.database":               {"IM_MYSQL_DATABASE", "MYSQL_DATABASE"},
		"mysql.charset":                {"IM_MYSQL_CHARSET", "MYSQL_CHARSET"},
		"mysql.max_open_conns":         {"IM_MYSQL_MAX_OPEN_CONNS", "MYSQL_MAX_OPEN_CONNS"},
		"mysql.max_idle_conns":         {"IM_MYSQL_MAX_IDLE_CONNS", "MYSQL_MAX_IDLE_CONNS"},
		"mysql.conn_max_lifetime":      {"IM_MYSQL_CONN_MAX_LIFETIME", "MYSQL_CONN_MAX_LIFETIME"},
		"mysql.conn_max_idle_time":     {"IM_MYSQL_CONN_MAX_IDLE_TIME", "MYSQL_CONN_MAX_IDLE_TIME"},
		"mysql.dial_timeout":           {"IM_MYSQL_DIAL_TIMEOUT", "MYSQL_DIAL_TIMEOUT"},
		"redis.addr":                   {"IM_REDIS_ADDR", "REDIS_ADDR"},
		"redis.password":               {"IM_REDIS_PASSWORD", "REDIS_PASSWORD"},
		"redis.db":                     {"IM_REDIS_DB", "REDIS_DB"},
		"redis.pool_size":              {"IM_REDIS_POOL_SIZE", "REDIS_POOL_SIZE"},
		"redis.min_idle_conns":         {"IM_REDIS_MIN_IDLE_CONNS", "REDIS_MIN_IDLE_CONNS"},
		"redis.dial_timeout":           {"IM_REDIS_DIAL_TIMEOUT", "REDIS_DIAL_TIMEOUT"},
		"redis.read_timeout":           {"IM_REDIS_READ_TIMEOUT", "REDIS_READ_TIMEOUT"},
		"redis.write_timeout":          {"IM_REDIS_WRITE_TIMEOUT", "REDIS_WRITE_TIMEOUT"},
		"snowflake.node_id":            {"IM_SNOWFLAKE_NODE_ID", "SNOWFLAKE_NODE_ID"},
		"health.probe_timeout":         {"IM_HEALTH_PROBE_TIMEOUT", "HEALTH_PROBE_TIMEOUT"},
		"upload.dir":                   {"IM_UPLOAD_DIR", "UPLOAD_DIR"},
		"upload.temp_dir":              {"IM_UPLOAD_TEMP_DIR", "UPLOAD_TEMP_DIR"},
		"upload.max_image_bytes":       {"IM_UPLOAD_MAX_IMAGE_BYTES", "UPLOAD_MAX_IMAGE_BYTES"},
		"upload.max_file_bytes":        {"IM_UPLOAD_MAX_FILE_BYTES", "UPLOAD_MAX_FILE_BYTES"},
		"upload.max_voice_bytes":       {"IM_UPLOAD_MAX_VOICE_BYTES", "UPLOAD_MAX_VOICE_BYTES"},
		"upload.max_avatar_bytes":      {"IM_UPLOAD_MAX_AVATAR_BYTES", "UPLOAD_MAX_AVATAR_BYTES"},
		"upload.max_image_width":       {"IM_UPLOAD_MAX_IMAGE_WIDTH", "UPLOAD_MAX_IMAGE_WIDTH"},
		"upload.max_image_height":      {"IM_UPLOAD_MAX_IMAGE_HEIGHT", "UPLOAD_MAX_IMAGE_HEIGHT"},
		"upload.max_image_pixels":      {"IM_UPLOAD_MAX_IMAGE_PIXELS", "UPLOAD_MAX_IMAGE_PIXELS"},
		"upload.max_voice_duration_ms": {"IM_UPLOAD_MAX_VOICE_DURATION_MS", "UPLOAD_MAX_VOICE_DURATION_MS"},
		"upload.thumbnail_max_dim":     {"IM_UPLOAD_THUMBNAIL_MAX_DIM", "UPLOAD_THUMBNAIL_MAX_DIM"},
		"upload.download_url_ttl":      {"IM_UPLOAD_DOWNLOAD_URL_TTL", "UPLOAD_DOWNLOAD_URL_TTL"},
	}
	for key, envs := range bindings {
		args := append([]string{key}, envs...)
		_ = v.BindEnv(args...)
	}
}
