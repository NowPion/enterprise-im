package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
)

func TestDirectKeyCanonicalOrder(t *testing.T) {
	if got := DirectKey(10002, 10001); got != "10001:10002" {
		t.Fatalf("expected 10001:10002, got %q", got)
	}
	if got := DirectKey(10001, 10002); got != "10001:10002" {
		t.Fatalf("expected 10001:10002, got %q", got)
	}
}

func TestRedisSeqAllocatorAndPresenceFailFastOnInvalidClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Nil Redis client must fail fast for seq lock allocation.
	nilAlloc := NewRedisSeqAllocator(nil, 5*time.Second)
	if _, err := nilAlloc.AcquireConvLock(ctx, 10); err == nil {
		t.Fatal("expected AcquireConvLock on nil redis client to fail")
	}

	// Unreachable local port must surface a real connection error (no silent fallback).
	deadRedis := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 150 * time.Millisecond,
	})
	defer func() { _ = deadRedis.Close() }()

	deadAlloc := NewRedisSeqAllocator(deadRedis, 5*time.Second)
	if _, err := deadAlloc.AcquireConvLock(ctx, 10); err == nil {
		t.Fatal("expected AcquireConvLock on unreachable redis to fail")
	}

	// Nil presence checker returns empty map safely; dead redis surfaces error.
	nilPresence := NewRedisPresenceChecker(nil)
	onlineNil, err := nilPresence.BatchIsOnline(ctx, []uint64{1, 2})
	if err != nil {
		t.Fatalf("expected nil redis presence checker to degrade gracefully, got %v", err)
	}
	if len(onlineNil) != 0 {
		t.Fatalf("expected empty online map for nil checker, got %+v", onlineNil)
	}

	deadPresence := NewRedisPresenceChecker(deadRedis)
	if _, err := deadPresence.BatchIsOnline(ctx, []uint64{1, 2}); err == nil {
		t.Fatal("expected BatchIsOnline on unreachable redis to return error")
	}
}

func TestLiveFriendRequestRepositoryIntegration(t *testing.T) {
	mysqlPass, mysqlSet := lookupEnv("MYSQL_PASSWORD", "IM_MYSQL_PASSWORD")
	redisPass, redisSet := lookupEnv("REDIS_PASSWORD", "IM_REDIS_PASSWORD")
	if !mysqlSet && !redisSet && os.Getenv("MYSQL_HOST") == "" && os.Getenv("IM_MYSQL_HOST") == "" {
		t.Skip("skipping live MySQL/Redis friend request repository integration test: env credentials not set")
	}

	host := firstNonEmpty(os.Getenv("MYSQL_HOST"), os.Getenv("IM_MYSQL_HOST"), "127.0.0.1")
	port := 3306
	if p := firstNonEmpty(os.Getenv("MYSQL_PORT"), os.Getenv("IM_MYSQL_PORT")); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	user := firstNonEmpty(os.Getenv("MYSQL_USER"), os.Getenv("IM_MYSQL_USER"), "root")
	dbName := firstNonEmpty(os.Getenv("MYSQL_DATABASE"), os.Getenv("IM_MYSQL_DATABASE"), "enterprise_im")
	redisAddr := firstNonEmpty(os.Getenv("REDIS_ADDR"), os.Getenv("IM_REDIS_ADDR"), "127.0.0.1:6379")

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
		DB:           0,
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

	repo := NewGormFriendRequestRepository(db)
	writer := NewGormRelationWriter()
	seqAlloc := NewRedisSeqAllocator(rdb, 5*time.Second)
	presence := NewRedisPresenceChecker(rdb)

	// Create two ephemeral users and clean them up at end of test.
	suffix := time.Now().UnixNano()
	u1 := model.User{
		Username:     fmt.Sprintf("fr_repo_u1_%d", suffix),
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     "RepoU1",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	u2 := model.User{
		Username:     fmt.Sprintf("fr_repo_u2_%d", suffix),
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     "RepoU2",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	if err := db.WithContext(ctx).Create(&u1).Error; err != nil {
		t.Fatalf("create u1: %v", err)
	}
	if err := db.WithContext(ctx).Create(&u2).Error; err != nil {
		t.Fatalf("create u2: %v", err)
	}

	var createdConvID uint64
	defer func() {
		cleanupCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if createdConvID > 0 {
			_ = db.WithContext(cleanupCtx).Where("conversation_id = ?", createdConvID).Delete(&model.MessageReceipt{}).Error
			_ = db.WithContext(cleanupCtx).Where("conversation_id = ?", createdConvID).Delete(&model.Message{}).Error
			_ = db.WithContext(cleanupCtx).Where("conversation_id = ?", createdConvID).Delete(&model.ConversationMember{}).Error
			_ = db.WithContext(cleanupCtx).Where("id = ?", createdConvID).Delete(&model.Conversation{}).Error
			_ = rdb.Del(cleanupCtx, fmt.Sprintf("conv:seq:%d", createdConvID), fmt.Sprintf("conv:lock:%d", createdConvID)).Err()
		}
		_ = db.WithContext(cleanupCtx).Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", u1.ID, u2.ID, u2.ID, u1.ID).Delete(&model.Friendship{}).Error
		_ = db.WithContext(cleanupCtx).Where("(from_user_id = ? AND to_user_id = ?) OR (from_user_id = ? AND to_user_id = ?)", u1.ID, u2.ID, u2.ID, u1.ID).Delete(&model.FriendRequest{}).Error
		_ = db.WithContext(cleanupCtx).Where("id IN ?", []uint64{u1.ID, u2.ID}).Delete(&model.User{}).Error
		_ = rdb.Del(cleanupCtx, fmt.Sprintf("im:presence:user:%d", u1.ID), fmt.Sprintf("im:presence:user:%d", u2.ID)).Err()
	}()

	// Verify RedisPresenceChecker against live Redis.
	if err := rdb.Set(ctx, fmt.Sprintf("im:presence:user:%d", u1.ID), "2", 30*time.Second).Err(); err != nil {
		t.Fatalf("set presence key: %v", err)
	}
	onlineMap, err := presence.BatchIsOnline(ctx, []uint64{u1.ID, u2.ID})
	if err != nil {
		t.Fatalf("BatchIsOnline: %v", err)
	}
	if !onlineMap[u1.ID] || onlineMap[u2.ID] {
		t.Fatalf("unexpected online map: %+v", onlineMap)
	}

	// Verify transaction + RelationWriter + Conversation + RedisSeqAllocator + Snowflake Message + rollback.
	gen, err := snowflake.NewGenerator(1)
	if err != nil {
		t.Fatalf("snowflake generator: %v", err)
	}

	// 1. Successful accept flow inside WithTx.
	var firstMsgID uint64
	err = repo.WithTx(ctx, func(tx *gorm.DB) error {
		lockedUsers, lockErr := repo.LockUsersAscending(ctx, tx, u2.ID, u1.ID)
		if lockErr != nil {
			return lockErr
		}
		if len(lockedUsers) != 2 {
			return fmt.Errorf("expected 2 locked users, got %d", len(lockedUsers))
		}
		fr := &model.FriendRequest{
			FromUserID: u1.ID,
			ToUserID:   u2.ID,
			Message:    "hi",
			Status:     model.FriendRequestStatusPending,
		}
		if err := repo.CreateFriendRequest(ctx, tx, fr); err != nil {
			return err
		}
		pending, err := repo.HasPendingFriendRequest(ctx, tx, u2.ID, u1.ID)
		if err != nil || !pending {
			return fmt.Errorf("expected pending request in reverse check, pending=%v err=%v", pending, err)
		}

		now := time.Now().UTC().Truncate(time.Millisecond)
		updated, err := repo.UpdateFriendRequestStatus(ctx, tx, fr.ID, model.FriendRequestStatusPending, model.FriendRequestStatusAccepted, now)
		if err != nil || !updated {
			return fmt.Errorf("update status failed: updated=%v err=%v", updated, err)
		}

		if err := writer.EnsureBilateralFriendship(ctx, tx, u1.ID, u2.ID); err != nil {
			return err
		}

		conv, err := repo.CreateSingleConversation(ctx, tx, DirectKey(u1.ID, u2.ID))
		if err != nil {
			return err
		}
		createdConvID = conv.ID

		if err := repo.EnsureConversationMembersVisible(ctx, tx, conv.ID, []uint64{u1.ID, u2.ID}, conv.LastSeq, now); err != nil {
			return err
		}

		token, err := seqAlloc.AcquireConvLock(ctx, conv.ID)
		if err != nil {
			return err
		}
		defer func() { _ = seqAlloc.ReleaseConvLock(ctx, conv.ID, token) }()

		seqRes, err := seqAlloc.AllocateNextSeq(ctx, tx, conv.ID, token)
		if err != nil {
			return err
		}
		if seqRes.Seq != 1 {
			return fmt.Errorf("expected seq=1, got %d", seqRes.Seq)
		}

		msgID, err := gen.NextID()
		if err != nil {
			return err
		}
		firstMsgID = msgID

		msg := &model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:friend_accept:%d", fr.ID),
			MsgType:        model.MessageTypeSystem,
			Content:        "已添加为好友",
			Extra:          model.NullableJSON(fmt.Sprintf(`{"request_id":"%d"}`, fr.ID)),
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if err := repo.CreateSystemMessage(ctx, tx, msg); err != nil {
			return err
		}
		if err := repo.UpdateConversationLastMessage(ctx, tx, conv.ID, msg); err != nil {
			return err
		}
		return repo.IncrementMembersUnreadCount(ctx, tx, conv.ID, []uint64{u1.ID, u2.ID}, now)
	})
	if err != nil {
		t.Fatalf("live accept tx failed: %v", err)
	}

	// Verify bilateral friendships & system message persisted.
	var fCount int64
	if err := db.WithContext(ctx).Model(&model.Friendship{}).Where("((user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)) AND status = 1", u1.ID, u2.ID, u2.ID, u1.ID).Count(&fCount).Error; err != nil {
		t.Fatalf("count friendships: %v", err)
	}
	if fCount != 2 {
		t.Fatalf("expected 2 bilateral friendships, got %d", fCount)
	}
	var savedMsg model.Message
	if err := db.WithContext(ctx).Where("id = ?", firstMsgID).First(&savedMsg).Error; err != nil {
		t.Fatalf("load saved system message: %v", err)
	}
	if savedMsg.Seq != 1 || savedMsg.SenderID != 0 || savedMsg.MsgType != model.MessageTypeSystem {
		t.Fatalf("unexpected saved message: %+v", savedMsg)
	}

	// 2. Verify Redis seq rollback compensation when DB transaction fails after AllocateNextSeq.
	sentinelErr := errors.New("forced tx failure")
	token2, err := seqAlloc.AcquireConvLock(ctx, createdConvID)
	if err != nil {
		t.Fatalf("AcquireConvLock 2: %v", err)
	}
	err = repo.WithTx(ctx, func(tx *gorm.DB) error {
		seqRes, allocErr := seqAlloc.AllocateNextSeq(ctx, tx, createdConvID, token2)
		if allocErr != nil {
			return allocErr
		}
		if seqRes.Seq != 2 {
			return fmt.Errorf("expected tentative seq=2, got %d", seqRes.Seq)
		}
		// Simulate transaction error -> compensate Redis seq before returning error.
		if rbErr := seqRes.Rollback(ctx); rbErr != nil {
			return rbErr
		}
		return sentinelErr
	})
	_ = seqAlloc.ReleaseConvLock(ctx, createdConvID, token2)
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("expected sentinelErr, got %v", err)
	}
	seqVal, err := rdb.Get(ctx, fmt.Sprintf("conv:seq:%d", createdConvID)).Result()
	if err != nil || seqVal != "1" {
		t.Fatalf("expected Redis conv:seq restored to 1 after rollback, got %q (err=%v)", seqVal, err)
	}

	// 3. Verify dangling increment calibration: simulate process crash leaving conv:seq=99 while MySQL MAX(seq)=1.
	if err := rdb.Set(ctx, fmt.Sprintf("conv:seq:%d", createdConvID), "99", 0).Err(); err != nil {
		t.Fatalf("seed dangling conv:seq: %v", err)
	}
	token3, err := seqAlloc.AcquireConvLock(ctx, createdConvID)
	if err != nil {
		t.Fatalf("AcquireConvLock 3: %v", err)
	}
	err = repo.WithTx(ctx, func(tx *gorm.DB) error {
		seqRes, allocErr := seqAlloc.AllocateNextSeq(ctx, tx, createdConvID, token3)
		if allocErr != nil {
			return allocErr
		}
		if seqRes.Seq != 2 {
			return fmt.Errorf("expected calibrated seq=2 (not 100), got %d", seqRes.Seq)
		}
		return sentinelErr
	})
	_ = seqAlloc.ReleaseConvLock(ctx, createdConvID, token3)
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("expected sentinelErr on dangling calibration tx, got %v", err)
	}

	// 4. Verify initializing missing conv:seq deletes the newly initialized key on rollback.
	if err := rdb.Del(ctx, fmt.Sprintf("conv:seq:%d", createdConvID)).Err(); err != nil {
		t.Fatalf("del conv:seq: %v", err)
	}
	token4, err := seqAlloc.AcquireConvLock(ctx, createdConvID)
	if err != nil {
		t.Fatalf("AcquireConvLock 4: %v", err)
	}
	err = repo.WithTx(ctx, func(tx *gorm.DB) error {
		seqRes, allocErr := seqAlloc.AllocateNextSeq(ctx, tx, createdConvID, token4)
		if allocErr != nil {
			return allocErr
		}
		if seqRes.Seq != 2 {
			return fmt.Errorf("expected seq=2 after init from MySQL, got %d", seqRes.Seq)
		}
		if rbErr := seqRes.Rollback(ctx); rbErr != nil {
			return rbErr
		}
		return sentinelErr
	})
	_ = seqAlloc.ReleaseConvLock(ctx, createdConvID, token4)
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("expected sentinelErr on missing-key rollback test, got %v", err)
	}
	if _, err := rdb.Get(ctx, fmt.Sprintf("conv:seq:%d", createdConvID)).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expected newly initialized conv:seq to be deleted on rollback, got err=%v", err)
	}

	// 5. Verify lost lock token fails fast with ErrConvLockTimeout.
	err = repo.WithTx(ctx, func(tx *gorm.DB) error {
		_, allocErr := seqAlloc.AllocateNextSeq(ctx, tx, createdConvID, "wrong-or-expired-token")
		return allocErr
	})
	if !errors.Is(err, ErrConvLockTimeout) {
		t.Fatalf("expected ErrConvLockTimeout when lock not owned, got %v", err)
	}
}

func lookupEnv(primary, fallback string) (string, bool) {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		return v, true
	}
	if v, ok := os.LookupEnv(fallback); ok && v != "" {
		return v, true
	}
	return "", false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
