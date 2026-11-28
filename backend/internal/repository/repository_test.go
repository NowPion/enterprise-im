package repository

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
)

func testLiveRedisDB() int {
	for _, key := range []string{"IM_REDIS_DB", "REDIS_DB"} {
		if v := os.Getenv(key); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
				return parsed
			}
		}
	}
	return 0
}

func TestStartupConnectivityFailuresFailFast(t *testing.T) {
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	if _, err := NewMySQL(config.MySQLConfig{
		Host:        "127.0.0.1",
		Port:        1,
		User:        "root",
		Database:    "enterprise_im",
		DialTimeout: 300 * time.Millisecond,
	}, log); err == nil {
		t.Fatal("expected NewMySQL to fail on unreachable port")
	}

	if _, err := NewRedis(config.RedisConfig{
		Addr:        "127.0.0.1:1",
		DialTimeout: 300 * time.Millisecond,
	}); err == nil {
		t.Fatal("expected NewRedis to fail on unreachable port")
	}

	if err := PingMySQL(context.Background(), nil); err == nil {
		t.Fatal("expected PingMySQL(nil) to fail")
	}
	if err := PingRedis(context.Background(), nil); err == nil {
		t.Fatal("expected PingRedis(nil) to fail")
	}
}

func TestLiveMySQLAndRedisModelsIntegration(t *testing.T) {
	mysqlPass := os.Getenv("MYSQL_PASSWORD")
	redisPass := os.Getenv("REDIS_PASSWORD")
	if mysqlPass == "" && redisPass == "" {
		t.Skip("skipping live MySQL/Redis integration test: MYSQL_PASSWORD/REDIS_PASSWORD not set")
	}

	host := envOrDefault("MYSQL_HOST", "127.0.0.1")
	port := 3306
	if p := os.Getenv("MYSQL_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	user := envOrDefault("MYSQL_USER", "root")
	dbName := envOrDefault("MYSQL_DATABASE", "enterprise_im")
	redisAddr := envOrDefault("REDIS_ADDR", "127.0.0.1:6379")

	log, err := logger.New(config.LogConfig{Level: "warn", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	db, err := NewMySQL(config.MySQLConfig{
		Host:            host,
		Port:            port,
		User:            user,
		Password:        mysqlPass,
		Database:        dbName,
		Charset:         "utf8mb4",
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		DialTimeout:     3 * time.Second,
	}, log)
	if err != nil {
		t.Fatalf("NewMySQL live connect failed: %v", err)
	}
	defer func() { _ = CloseMySQL(db) }()

	rdb, err := NewRedis(config.RedisConfig{
		Addr:         redisAddr,
		Password:     redisPass,
		DB:           testLiveRedisDB(),
		PoolSize:     5,
		MinIdleConns: 1,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRedis live connect failed: %v", err)
	}
	defer func() { _ = CloseRedis(rdb) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := PingMySQL(ctx, db); err != nil {
		t.Fatalf("PingMySQL failed: %v", err)
	}
	if err := PingRedis(ctx, rdb); err != nil {
		t.Fatalf("PingRedis failed: %v", err)
	}

	// Verify all 12 frozen tables exist and match GORM models.
	models := []any{
		&model.User{},
		&model.UserSession{},
		&model.FriendRequest{},
		&model.Friendship{},
		&model.Group{},
		&model.GroupMember{},
		&model.Conversation{},
		&model.ConversationMember{},
		&model.Message{},
		&model.MessageReceipt{},
		&model.Asset{},
		&model.AuditLog{},
	}
	for _, m := range models {
		if !db.Migrator().HasTable(m) {
			t.Fatalf("expected table for %T to exist in %s", m, dbName)
		}
	}

	// Verify seeded admin exists via GORM User model.
	var admin model.User
	if err := db.WithContext(ctx).Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatalf("query seeded admin failed: %v", err)
	}
	if admin.Role != model.UserRoleAdmin || admin.Status != model.UserStatusActive {
		t.Fatalf("unexpected seeded admin metadata: role=%q status=%d", admin.Role, admin.Status)
	}

	// Verify Snowflake Message insert & AuditLog append inside a rolled-back transaction (zero dirty state).
	gen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatalf("snowflake NewGenerator: %v", err)
	}
	msgID, err := gen.NextID()
	if err != nil {
		t.Fatalf("snowflake NextID: %v", err)
	}

	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatalf("begin tx: %v", tx.Error)
	}
	defer tx.Rollback()

	msg := model.Message{
		ID:             msgID,
		ConversationID: 999999,
		Seq:            1,
		SenderID:       admin.ID,
		ClientMsgID:    "stage3-tx-check-001",
		MsgType:        model.MessageTypeText,
		Content:        "stage3 model check",
		Extra:          model.NullableJSON(`{"check":true}`),
		Status:         model.MessageStatusNormal,
	}
	if err := tx.Create(&msg).Error; err != nil {
		t.Fatalf("tx create message failed: %v", err)
	}
	if msg.ID != msgID {
		t.Fatalf("message id changed unexpectedly: got %d want %d", msg.ID, msgID)
	}

	audit := model.AuditLog{
		EventID:    "stage3-audit-tx-check-001",
		Scope:      model.AuditScopeSystem,
		Action:     "stage3.schema_check",
		ActorID:    admin.ID,
		ActorRole:  model.UserRoleAdmin,
		TargetType: model.AuditTargetSystem,
		TargetID:   0,
		Reason:     "stage3 verification",
		Detail:     model.NullableJSON(`{"rolled_back":true}`),
	}
	if err := tx.Create(&audit).Error; err != nil && err != gorm.ErrRecordNotFound {
		t.Fatalf("tx create audit_log failed: %v", err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
