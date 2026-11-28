package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/logger"
)

func TestEscapeLikeLiteralAndGuards(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"alice", "alice"},
		{"alice_1", "alice!_1"},
		{"100%", "100!%"},
		{"a!b_c%d", "a!!b!_c!%d"},
		{`a\b`, `a\b`},
	}
	for _, tc := range cases {
		if got := EscapeLikeLiteral(tc.in); got != tc.want {
			t.Fatalf("EscapeLikeLiteral(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if got := PresenceUserKey(10001); got != "im:presence:user:10001" {
		t.Fatalf("unexpected presence key: %q", got)
	}

	nilRepo := NewUserRepository(nil)
	if _, err := nilRepo.GetByID(context.Background(), 1); err == nil {
		t.Fatal("expected GetByID on nil db to fail")
	}
	if _, err := nilRepo.UpdateProfile(context.Background(), 1, map[string]any{"nickname": "x"}); err == nil {
		t.Fatal("expected UpdateProfile on nil db to fail")
	}
	if _, err := nilRepo.Search(context.Background(), UserSearchParams{Query: "a", Field: UserSearchFieldUsername, Match: UserSearchMatchExact, Limit: 10}); err == nil {
		t.Fatal("expected Search on nil db to fail")
	}
	if _, err := nilRepo.Search(context.Background(), UserSearchParams{Query: "a", Field: "invalid", Match: UserSearchMatchExact, Limit: 10}); !errors.Is(err, ErrInvalidSearchField) {
		t.Fatalf("expected ErrInvalidSearchField, got %v", err)
	}
	if _, err := nilRepo.Search(context.Background(), UserSearchParams{Query: "a", Field: UserSearchFieldUsername, Match: "invalid", Limit: 10}); !errors.Is(err, ErrInvalidSearchMatch) {
		t.Fatalf("expected ErrInvalidSearchMatch, got %v", err)
	}

	nilPresence := NewUserPresenceRepository(nil)
	if _, err := nilPresence.GetUsersOnline(context.Background(), []uint64{1}); err == nil {
		t.Fatal("expected GetUsersOnline on nil redis to fail")
	}
	if online, err := nilPresence.IsUserOnline(context.Background(), 0); err != nil || online {
		t.Fatalf("expected user 0 online=false, nil; got %v, %v", online, err)
	}
}

func TestLiveUserRepositoryAndPresenceIntegration(t *testing.T) {
	mysqlPass := os.Getenv("MYSQL_PASSWORD")
	if mysqlPass == "" {
		mysqlPass = os.Getenv("IM_MYSQL_PASSWORD")
	}
	redisPass := os.Getenv("REDIS_PASSWORD")
	if redisPass == "" {
		redisPass = os.Getenv("IM_REDIS_PASSWORD")
	}
	if mysqlPass == "" && redisPass == "" {
		t.Skip("skipping live user repository integration test: MYSQL_PASSWORD/REDIS_PASSWORD not set")
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
		t.Fatalf("NewMySQL failed: %v", err)
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
		t.Fatalf("NewRedis failed: %v", err)
	}
	defer func() { _ = CloseRedis(rdb) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run all MySQL mutations in an outer transaction that always rolls back so zero dirty rows remain.
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatalf("begin tx: %v", tx.Error)
	}
	defer tx.Rollback()

	prefix := fmt.Sprintf("s4u_%d", time.Now().UnixNano())
	u1 := model.User{
		Username:     prefix + "_alice_1",
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     prefix + "_Nick_100%",
		Signature:    "initial sig",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	u2 := model.User{
		Username:     prefix + "_aliceA1",
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     prefix + "_NickA100X",
		Signature:    "second sig",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	uDisabled := model.User{
		Username:     prefix + "_alice_disabled",
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     prefix + "_Nick_disabled",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusDisabled,
	}
	for _, u := range []*model.User{&u1, &u2, &uDisabled} {
		if err := tx.Create(u).Error; err != nil {
			t.Fatalf("seed test user: %v", err)
		}
	}
	if err := tx.Model(&uDisabled).Update("status", model.UserStatusDisabled).Error; err != nil {
		t.Fatalf("disable seeded test user: %v", err)
	}

	repo := NewUserRepository(tx)

	// 1. GetByID
	got, err := repo.GetByID(ctx, u1.ID)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if got.PasswordHash != "" {
		t.Fatal("expected GetByID not to populate password_hash")
	}
	if got.Username != u1.Username || got.Nickname != u1.Nickname {
		t.Fatalf("unexpected user returned: %+v", got)
	}

	// 2. UpdateProfile (update nickname and clear signature with empty string)
	updated, err := repo.UpdateProfile(ctx, u1.ID, map[string]any{
		"nickname":  prefix + "_Updated",
		"signature": "",
	})
	if err != nil {
		t.Fatalf("UpdateProfile failed: %v", err)
	}
	if updated.Nickname != prefix+"_Updated" || updated.Signature != "" {
		t.Fatalf("unexpected updated user: %+v", updated)
	}

	// Disabled user update fails with ErrUserDisabled; missing user fails with ErrRecordNotFound.
	if _, err := repo.UpdateProfile(ctx, uDisabled.ID, map[string]any{"nickname": "x"}); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("expected ErrUserDisabled for disabled user, got %v", err)
	}
	if _, err := repo.UpdateProfile(ctx, ^uint64(0), map[string]any{"nickname": "x"}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound for nonexistent user, got %v", err)
	}

	// 3. Literal prefix search (underscores and % must match literally, not as wildcards; disabled excluded)
	resLiteral, err := repo.Search(ctx, UserSearchParams{
		Query: prefix + "_alice_1",
		Field: UserSearchFieldUsername,
		Match: UserSearchMatchPrefix,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search literal prefix failed: %v", err)
	}
	if len(resLiteral) != 1 || resLiteral[0].ID != u1.ID {
		t.Fatalf("expected only u1 (%d) for literal underscore prefix, got %+v", u1.ID, resLiteral)
	}

	// Search by nickname with % literal
	resNickPct, err := repo.Search(ctx, UserSearchParams{
		Query: prefix + "_NickA100X",
		Field: UserSearchFieldNickname,
		Match: UserSearchMatchExact,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search exact nickname failed: %v", err)
	}
	if len(resNickPct) != 1 || resNickPct[0].ID != u2.ID {
		t.Fatalf("expected only u2 (%d) for exact nickname, got %+v", u2.ID, resNickPct)
	}

	// Search prefix across active users ordered by id DESC + cursor
	page1, err := repo.Search(ctx, UserSearchParams{
		Query: prefix + "_alice",
		Field: UserSearchFieldUsername,
		Match: UserSearchMatchPrefix,
		Limit: 1,
	})
	if err != nil || len(page1) != 1 || page1[0].ID != u2.ID {
		t.Fatalf("expected page1=[u2], got %+v err=%v", page1, err)
	}
	page2, err := repo.Search(ctx, UserSearchParams{
		Query:  prefix + "_alice",
		Field:  UserSearchFieldUsername,
		Match:  UserSearchMatchPrefix,
		Cursor: page1[0].ID,
		Limit:  5,
	})
	if err != nil || len(page2) != 1 || page2[0].ID != u1.ID {
		t.Fatalf("expected page2=[u1], got %+v err=%v", page2, err)
	}

	// 4. Live Redis presence check with cleanup
	k1 := PresenceUserKey(u1.ID)
	k2 := PresenceUserKey(u2.ID)
	defer func() { _ = rdb.Del(context.Background(), k1, k2).Err() }()

	if err := rdb.Set(ctx, k1, "2", 30*time.Second).Err(); err != nil {
		t.Fatalf("set redis presence: %v", err)
	}
	if err := rdb.Set(ctx, k2, "0", 30*time.Second).Err(); err != nil {
		t.Fatalf("set redis zero presence: %v", err)
	}

	presenceRepo := NewUserPresenceRepository(rdb)
	onlineMap, err := presenceRepo.GetUsersOnline(ctx, []uint64{u1.ID, u2.ID, 999999999})
	if err != nil {
		t.Fatalf("GetUsersOnline failed: %v", err)
	}
	if !onlineMap[u1.ID] || onlineMap[u2.ID] || onlineMap[999999999] {
		t.Fatalf("unexpected onlineMap: %+v", onlineMap)
	}
}
