package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromYAMLAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlContent := `app:
  name: enterprise-im
  env: development
  node_id: node-yaml
server:
  addr: ":9090"
  shutdown_timeout: 12s
cors:
  allowed_origins:
    - "http://localhost:5173"
log:
  level: debug
  format: text
mysql:
  host: 127.0.0.1
  port: 3306
  user: tester
  password: ""
  database: enterprise_im
redis:
  addr: 127.0.0.1:6379
  password: ""
  db: 1
snowflake:
  node_id: 7
health:
  probe_timeout: 3s
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	t.Setenv("APP_ENV", "production")
	t.Setenv("SERVER_ADDR", ":18080")
	t.Setenv("MYSQL_PASSWORD", "env-mysql-pass")
	t.Setenv("REDIS_PASSWORD", "env-redis-pass")
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://im.example.com, https://admin.example.com")
	t.Setenv("SNOWFLAKE_NODE_ID", "42")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if !cfg.App.IsProduction() {
		t.Fatalf("expected production env, got %q", cfg.App.Env)
	}
	if cfg.Server.Addr != ":18080" {
		t.Fatalf("expected server addr :18080, got %q", cfg.Server.Addr)
	}
	if cfg.MySQL.Password != "env-mysql-pass" {
		t.Fatalf("expected MYSQL_PASSWORD override")
	}
	if cfg.Redis.Password != "env-redis-pass" {
		t.Fatalf("expected REDIS_PASSWORD override")
	}
	if cfg.Snowflake.NodeID != 42 {
		t.Fatalf("expected snowflake node_id 42, got %d", cfg.Snowflake.NodeID)
	}
	if len(cfg.CORS.AllowedOrigins) != 2 ||
		cfg.CORS.AllowedOrigins[0] != "https://im.example.com" ||
		cfg.CORS.AllowedOrigins[1] != "https://admin.example.com" {
		t.Fatalf("unexpected cors origins: %#v", cfg.CORS.AllowedOrigins)
	}

	summary := cfg.RedactedSummary()
	if summary["mysql_password"] != "[REDACTED]" || summary["redis_password"] != "[REDACTED]" {
		t.Fatalf("expected passwords redacted in summary: %#v", summary)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	if _, err := Load("../../configs/config.yaml"); err == nil {
		t.Fatal("expected error for invalid app.env")
	}

	t.Setenv("APP_ENV", "development")
	t.Setenv("SNOWFLAKE_NODE_ID", "1024")
	if _, err := Load("../../configs/config.yaml"); err == nil {
		t.Fatal("expected error for out-of-range snowflake.node_id")
	}

	t.Setenv("SNOWFLAKE_NODE_ID", "1")
	t.Setenv("UPLOAD_MAX_IMAGE_BYTES", "0")
	if _, err := Load("../../configs/config.yaml"); err == nil {
		t.Fatal("expected error for non-positive upload.max_image_bytes")
	}
}

func TestLoadUploadConfigOverrides(t *testing.T) {
	t.Setenv("UPLOAD_DIR", "/tmp/custom-uploads")
	t.Setenv("UPLOAD_MAX_IMAGE_BYTES", "2097152")
	t.Setenv("UPLOAD_MAX_VOICE_DURATION_MS", "60000")
	t.Setenv("UPLOAD_DOWNLOAD_URL_TTL", "5m")

	cfg, err := Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Upload.Dir != "/tmp/custom-uploads" {
		t.Fatalf("unexpected upload.dir: %q", cfg.Upload.Dir)
	}
	if cfg.Upload.MaxImageBytes != 2097152 {
		t.Fatalf("unexpected upload.max_image_bytes: %d", cfg.Upload.MaxImageBytes)
	}
	if cfg.Upload.MaxVoiceDurationMs != 60000 {
		t.Fatalf("unexpected upload.max_voice_duration_ms: %d", cfg.Upload.MaxVoiceDurationMs)
	}
	if cfg.Upload.DownloadURLTTL.Minutes() != 5 {
		t.Fatalf("unexpected upload.download_url_ttl: %v", cfg.Upload.DownloadURLTTL)
	}
}

func TestLoadMQConfigDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("Load default config failed: %v", err)
	}
	if cfg.MQ.StreamMaxLen != 100000 || cfg.MQ.ReadCount != 100 || cfg.MQ.MaxRetries != 5 ||
		cfg.MQ.StreamRetention.Hours() != 168 || cfg.MQ.BlockTimeout.Milliseconds() != 2000 ||
		cfg.MQ.ReclaimInterval.Seconds() != 30 || cfg.MQ.ReclaimMinIdle.Seconds() != 30 ||
		cfg.MQ.MaxPendingAge.Minutes() != 15 {
		t.Fatalf("unexpected default MQ config: %+v", cfg.MQ)
	}

	t.Setenv("MQ_STREAM_MAX_LEN", "25000")
	t.Setenv("MQ_READ_COUNT", "50")
	t.Setenv("MQ_MAX_RETRIES", "3")
	t.Setenv("MQ_RECLAIM_MIN_IDLE", "10s")
	t.Setenv("MQ_WORKER_COUNT", "2")
	cfg, err = Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("Load overridden MQ config failed: %v", err)
	}
	if cfg.MQ.StreamMaxLen != 25000 || cfg.MQ.ReadCount != 50 || cfg.MQ.MaxRetries != 3 ||
		cfg.MQ.ReclaimMinIdle.Seconds() != 10 || cfg.MQ.WorkerCount != 2 {
		t.Fatalf("unexpected overridden MQ config: %+v", cfg.MQ)
	}

	t.Setenv("MQ_MAX_RETRIES", "0")
	if _, err := Load("../../configs/config.yaml"); err == nil {
		t.Fatal("expected error when MQ_MAX_RETRIES=0")
	}
}
