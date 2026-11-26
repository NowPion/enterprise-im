package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
)

func TestGormGroupRepositoryNilGuards(t *testing.T) {
	ctx := context.Background()
	var nilRepo *GormGroupRepository

	if err := nilRepo.WithTx(ctx, func(*gorm.DB) error { return nil }); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil repo WithTx, got %v", err)
	}
	if _, err := nilRepo.BatchGetUsersByIDs(ctx, []uint64{1}); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil repo BatchGetUsersByIDs, got %v", err)
	}
	if _, _, err := nilRepo.GetGroupAndConversation(ctx, 1); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil repo GetGroupAndConversation, got %v", err)
	}
	if _, _, err := nilRepo.ListUserGroups(ctx, UserGroupListFilter{UserID: 1, Limit: 20}); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil repo ListUserGroups, got %v", err)
	}
	if _, _, err := nilRepo.ListGroupAuditLogs(ctx, GroupAuditListFilter{GroupID: 1, Limit: 20}); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil repo ListGroupAuditLogs, got %v", err)
	}

	emptyRepo := NewGormGroupRepository(nil)
	if _, err := emptyRepo.LockUsersAscending(ctx, nil, []uint64{1}); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil tx LockUsersAscending, got %v", err)
	}
	if err := emptyRepo.CreateGroup(ctx, nil, &model.Group{}); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil tx CreateGroup, got %v", err)
	}
	if err := emptyRepo.AppendAuditLog(ctx, nil, &model.AuditLog{}); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for nil tx AppendAuditLog, got %v", err)
	}
}

func TestLiveGroupRepositoryIntegration(t *testing.T) {
	mysqlPass, mysqlSet := lookupEnv("MYSQL_PASSWORD", "IM_MYSQL_PASSWORD")
	redisPass, redisSet := lookupEnv("REDIS_PASSWORD", "IM_REDIS_PASSWORD")
	if !mysqlSet && !redisSet && os.Getenv("MYSQL_HOST") == "" && os.Getenv("IM_MYSQL_HOST") == "" {
		t.Skip("skipping live MySQL/Redis group repository integration test: env credentials not set")
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

	repo := NewGormGroupRepository(db)
	seqAlloc := NewRedisSeqAllocator(rdb, 5*time.Second)
	idGen, err := snowflake.NewGenerator(2)
	if err != nil {
		t.Fatalf("snowflake generator: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	suffix := uuid.NewString()[:8]

	u1 := model.User{Username: "grp_repo_1_" + suffix, PasswordHash: "hash", Nickname: "Owner", Role: model.UserRoleUser, Status: model.UserStatusActive}
	u2 := model.User{Username: "grp_repo_2_" + suffix, PasswordHash: "hash", Nickname: "Member", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.Create(&u1).Error; err != nil {
		t.Fatalf("seed u1: %v", err)
	}
	if err := db.Create(&u2).Error; err != nil {
		t.Fatalf("seed u2: %v", err)
	}

	var createdGroupID, createdConvID uint64
	defer func() {
		if createdConvID > 0 {
			_ = rdb.Del(ctx, fmt.Sprintf("conv:seq:%d", createdConvID), fmt.Sprintf("conv:lock:%d", createdConvID)).Err()
			_ = db.Where("conversation_id = ?", createdConvID).Delete(&model.Message{}).Error
			_ = db.Where("conversation_id = ?", createdConvID).Delete(&model.ConversationMember{}).Error
			_ = db.Where("id = ?", createdConvID).Delete(&model.Conversation{}).Error
		}
		if createdGroupID > 0 {
			_ = db.Where("group_id = ?", createdGroupID).Delete(&model.AuditLog{}).Error
			_ = db.Where("group_id = ?", createdGroupID).Delete(&model.GroupMember{}).Error
			_ = db.Where("id = ?", createdGroupID).Delete(&model.Group{}).Error
		}
		_ = db.Where("id IN ?", []uint64{u1.ID, u2.ID}).Delete(&model.User{}).Error
	}()

	err = repo.WithTx(ctx, func(tx *gorm.DB) error {
		users, lockErr := repo.LockUsersAscending(ctx, tx, []uint64{u2.ID, u1.ID})
		if lockErr != nil || len(users) != 2 {
			return fmt.Errorf("lock users failed: %w (len=%d)", lockErr, len(users))
		}
		g := &model.Group{
			Name:       "Live Repo Group",
			OwnerID:    u1.ID,
			MaxMembers: 500,
			Status:     model.GroupStatusActive,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if createErr := repo.CreateGroup(ctx, tx, g); createErr != nil {
			return createErr
		}
		createdGroupID = g.ID

		conv, convErr := repo.CreateGroupConversation(ctx, tx, g.ID, now)
		if convErr != nil {
			return convErr
		}
		createdConvID = conv.ID

		tok, acqErr := seqAlloc.AcquireConvLock(ctx, conv.ID)
		if acqErr != nil {
			return acqErr
		}
		defer func() { _ = seqAlloc.ReleaseConvLock(ctx, conv.ID, tok) }()

		seqRes, seqErr := seqAlloc.AllocateNextSeq(ctx, tx, conv.ID, tok)
		if seqErr != nil {
			return seqErr
		}
		msgID, genErr := idGen.NextID()
		if genErr != nil {
			return genErr
		}
		msg := &model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:live_repo:%d", msgID),
			MsgType:        model.MessageTypeSystem,
			Content:        "Owner 创建了群聊",
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if msgErr := repo.CreateSystemMessage(ctx, tx, msg); msgErr != nil {
			return msgErr
		}
		if lastErr := repo.UpdateConversationLastMessage(ctx, tx, conv.ID, msg); lastErr != nil {
			return lastErr
		}
		if gmErr := repo.CreateGroupMembers(ctx, tx, []model.GroupMember{
			{GroupID: g.ID, UserID: u1.ID, Role: model.GroupRoleOwner, JoinedAt: now, UpdatedAt: now},
			{GroupID: g.ID, UserID: u2.ID, Role: model.GroupRoleMember, JoinedAt: now, UpdatedAt: now},
		}); gmErr != nil {
			return gmErr
		}
		return repo.CreateConversationMembers(ctx, tx, []model.ConversationMember{
			{ConversationID: conv.ID, UserID: u1.ID, LastDeliveredSeq: 1, LastReadSeq: 1, UnreadCount: 0, IsVisible: true, JoinedAt: now, UpdatedAt: now},
			{ConversationID: conv.ID, UserID: u2.ID, LastDeliveredSeq: 0, LastReadSeq: 0, UnreadCount: 1, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		})
	})
	if err != nil {
		t.Fatalf("live group tx failed: %v", err)
	}

	rows, hasMore, err := repo.ListUserGroups(ctx, UserGroupListFilter{UserID: u1.ID, Limit: 20})
	if err != nil {
		t.Fatalf("ListUserGroups failed: %v", err)
	}
	if hasMore || len(rows) != 1 || rows[0].MemberCount != 2 || rows[0].ConversationID != createdConvID {
		t.Fatalf("unexpected ListUserGroups result: %+v hasMore=%v", rows, hasMore)
	}
}
