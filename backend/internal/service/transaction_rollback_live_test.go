package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
)

func openLiveMySQLAndRedisForRollbackTest(t *testing.T) (*gorm.DB, *redis.Client, *slog.Logger) {
	t.Helper()

	mysqlPass := firstNonEmpty(os.Getenv("MYSQL_PASSWORD"), os.Getenv("IM_MYSQL_PASSWORD"))
	redisPass := firstNonEmpty(os.Getenv("REDIS_PASSWORD"), os.Getenv("IM_REDIS_PASSWORD"))
	if mysqlPass == "" || redisPass == "" {
		t.Skip("skipping live MySQL/Redis transaction rollback test: MYSQL_PASSWORD/REDIS_PASSWORD not set")
	}

	mysqlHost := firstNonEmpty(os.Getenv("MYSQL_HOST"), os.Getenv("IM_MYSQL_HOST"), "127.0.0.1")
	mysqlPort := 3306
	if p := firstNonEmpty(os.Getenv("MYSQL_PORT"), os.Getenv("IM_MYSQL_PORT")); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			mysqlPort = parsed
		}
	}
	mysqlUser := firstNonEmpty(os.Getenv("MYSQL_USER"), os.Getenv("IM_MYSQL_USER"), "root")
	dbName := firstNonEmpty(os.Getenv("MYSQL_DATABASE"), os.Getenv("IM_MYSQL_DATABASE"), "enterprise_im")
	redisAddr := firstNonEmpty(os.Getenv("REDIS_ADDR"), os.Getenv("IM_REDIS_ADDR"), "127.0.0.1:6379")

	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	db, err := repository.NewMySQL(config.MySQLConfig{
		Host:            mysqlHost,
		Port:            mysqlPort,
		User:            mysqlUser,
		Password:        mysqlPass,
		Database:        dbName,
		Charset:         "utf8mb4",
		MaxOpenConns:    10,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		DialTimeout:     3 * time.Second,
	}, log)
	if err != nil {
		t.Fatalf("NewMySQL failed: %v", err)
	}
	t.Cleanup(func() { _ = repository.CloseMySQL(db) })

	rdb, err := repository.NewRedis(config.RedisConfig{
		Addr:         redisAddr,
		Password:     redisPass,
		DB:           testServiceRedisDB(),
		PoolSize:     10,
		MinIdleConns: 1,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRedis failed: %v", err)
	}
	t.Cleanup(func() { _ = repository.CloseRedis(rdb) })
	return db, rdb, log
}

type rollbackInjectFriendRequestRepo struct {
	*repository.GormFriendRequestRepository
	capturedConvID *uint64
}

func (r *rollbackInjectFriendRequestRepo) CreateSingleConversation(ctx context.Context, tx *gorm.DB, directKey string) (*model.Conversation, error) {
	conv, err := r.GormFriendRequestRepository.CreateSingleConversation(ctx, tx, directKey)
	if err == nil && conv != nil && r.capturedConvID != nil {
		*r.capturedConvID = conv.ID
	}
	return conv, err
}

func (r *rollbackInjectFriendRequestRepo) CreateMessageReceipts(context.Context, *gorm.DB, []model.MessageReceipt) error {
	return errors.New("simulated mid-tx failure creating friend accept receipts")
}

func TestLiveMySQLRollbackFriendRequestAccept(t *testing.T) {
	db, rdb, log := openLiveMySQLAndRedisForRollbackTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	suffix := time.Now().UnixNano()
	u1 := model.User{Username: fmt.Sprintf("rb_fr_a_%d", suffix), PasswordHash: "x", Nickname: "RBA", Role: model.UserRoleUser, Status: model.UserStatusActive}
	u2 := model.User{Username: fmt.Sprintf("rb_fr_b_%d", suffix), PasswordHash: "x", Nickname: "RBB", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.WithContext(ctx).Create(&u1).Error; err != nil {
		t.Fatalf("create u1: %v", err)
	}
	if err := db.WithContext(ctx).Create(&u2).Error; err != nil {
		t.Fatalf("create u2: %v", err)
	}

	var reqID uint64
	var capturedConvID uint64
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		if capturedConvID > 0 {
			_ = db.WithContext(cctx).Where("conversation_id = ?", capturedConvID).Delete(&model.MessageReceipt{}).Error
			_ = db.WithContext(cctx).Where("conversation_id = ?", capturedConvID).Delete(&model.Message{}).Error
			_ = db.WithContext(cctx).Where("conversation_id = ?", capturedConvID).Delete(&model.ConversationMember{}).Error
			_ = db.WithContext(cctx).Where("id = ?", capturedConvID).Delete(&model.Conversation{}).Error
			_ = rdb.Del(cctx, fmt.Sprintf("conv:seq:%d", capturedConvID), fmt.Sprintf("conv:lock:%d", capturedConvID)).Err()
		}
		if reqID > 0 {
			_ = db.WithContext(cctx).Where("id = ?", reqID).Delete(&model.FriendRequest{}).Error
		}
		_ = db.WithContext(cctx).Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", u1.ID, u2.ID, u2.ID, u1.ID).Delete(&model.Friendship{}).Error
		_ = db.WithContext(cctx).Where("id IN ?", []uint64{u1.ID, u2.ID}).Delete(&model.User{}).Error
	})

	baseRepo := repository.NewGormFriendRequestRepository(db)
	seqAlloc := repository.NewRedisSeqAllocator(rdb, 5*time.Second)
	presence := repository.NewRedisPresenceChecker(rdb)
	idGen, err := snowflake.NewGenerator(2)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}

	createSvc := NewFriendRequestService(baseRepo, nil, seqAlloc, presence, idGen, log)
	createdReq, createErr := createSvc.Create(ctx, u1.ID, CreateFriendRequestInput{
		ToUserID: u2.ID,
		Message:  "rollback check",
	})
	if createErr != nil {
		t.Fatalf("seed friend request failed: %v", createErr)
	}
	reqID = createdReq.ID

	// Inject failure inside real MySQL transaction after friendships, conversation, members, Redis seq, and system message ran.
	failingRepo := &rollbackInjectFriendRequestRepo{
		GormFriendRequestRepository: baseRepo,
		capturedConvID:              &capturedConvID,
	}
	acceptSvc := NewFriendRequestService(failingRepo, nil, seqAlloc, presence, idGen, log)
	if _, acceptErr := acceptSvc.Accept(ctx, u2.ID, reqID); acceptErr == nil {
		t.Fatal("expected Accept to fail on injected mid-tx error")
	}
	if capturedConvID == 0 {
		t.Fatal("expected CreateSingleConversation inside tx to allocate conv ID before rollback")
	}

	// Verify real MySQL rolled back every table cleanly.
	var storedReq model.FriendRequest
	if err := db.WithContext(ctx).First(&storedReq, reqID).Error; err != nil {
		t.Fatalf("reload friend_request: %v", err)
	}
	if storedReq.Status != model.FriendRequestStatusPending || storedReq.HandledAt != nil {
		t.Fatalf("expected friend_request to roll back to pending with nil handled_at, got %+v", storedReq)
	}

	var friendCount int64
	_ = db.WithContext(ctx).Model(&model.Friendship{}).
		Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", u1.ID, u2.ID, u2.ID, u1.ID).
		Count(&friendCount).Error
	if friendCount != 0 {
		t.Fatalf("expected 0 friendship rows after MySQL rollback, got %d", friendCount)
	}

	var convCount int64
	_ = db.WithContext(ctx).Model(&model.Conversation{}).Where("direct_key = ?", repository.DirectKey(u1.ID, u2.ID)).Count(&convCount).Error
	if convCount != 0 {
		t.Fatalf("expected 0 conversation rows after MySQL rollback, got %d", convCount)
	}

	var memCount, msgCount int64
	_ = db.WithContext(ctx).Model(&model.ConversationMember{}).Where("conversation_id = ?", capturedConvID).Count(&memCount).Error
	_ = db.WithContext(ctx).Model(&model.Message{}).Where("conversation_id = ?", capturedConvID).Count(&msgCount).Error
	if memCount != 0 || msgCount != 0 {
		t.Fatalf("expected 0 conversation_members/messages after rollback, got members=%d messages=%d", memCount, msgCount)
	}

	// Verify Redis conv:seq compensation cleaned up the newly created conv:seq key.
	if _, err := rdb.Get(ctx, fmt.Sprintf("conv:seq:%d", capturedConvID)).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expected Redis conv:seq:%d deleted on rollback, got err=%v", capturedConvID, err)
	}
}

type rollbackInjectGroupRepo struct {
	*repository.GormGroupRepository
	capturedGroupID *uint64
	capturedConvID  *uint64
}

func (r *rollbackInjectGroupRepo) CreateGroup(ctx context.Context, tx *gorm.DB, group *model.Group) error {
	err := r.GormGroupRepository.CreateGroup(ctx, tx, group)
	if err == nil && group != nil && r.capturedGroupID != nil {
		*r.capturedGroupID = group.ID
	}
	return err
}

func (r *rollbackInjectGroupRepo) CreateGroupConversation(ctx context.Context, tx *gorm.DB, groupID uint64, now time.Time) (*model.Conversation, error) {
	conv, err := r.GormGroupRepository.CreateGroupConversation(ctx, tx, groupID, now)
	if err == nil && conv != nil && r.capturedConvID != nil {
		*r.capturedConvID = conv.ID
	}
	return conv, err
}

func (r *rollbackInjectGroupRepo) AppendAuditLog(context.Context, *gorm.DB, *model.AuditLog) error {
	return errors.New("simulated mid-tx failure appending group audit log")
}

type rollbackNoopGroupNotifier struct{}

func (rollbackNoopGroupNotifier) DeliverToUsers(context.Context, []uint64, string, any) error {
	return nil
}

func TestLiveMySQLRollbackGroupCreate(t *testing.T) {
	db, rdb, log := openLiveMySQLAndRedisForRollbackTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	suffix := time.Now().UnixNano()
	u1 := model.User{Username: fmt.Sprintf("rb_grp_a_%d", suffix), PasswordHash: "x", Nickname: "Owner", Role: model.UserRoleUser, Status: model.UserStatusActive}
	u2 := model.User{Username: fmt.Sprintf("rb_grp_b_%d", suffix), PasswordHash: "x", Nickname: "Member", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.WithContext(ctx).Create(&u1).Error; err != nil {
		t.Fatalf("create u1: %v", err)
	}
	if err := db.WithContext(ctx).Create(&u2).Error; err != nil {
		t.Fatalf("create u2: %v", err)
	}

	var capturedGroupID, capturedConvID uint64
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		if capturedConvID > 0 {
			_ = db.WithContext(cctx).Where("conversation_id = ?", capturedConvID).Delete(&model.Message{}).Error
			_ = db.WithContext(cctx).Where("conversation_id = ?", capturedConvID).Delete(&model.ConversationMember{}).Error
			_ = db.WithContext(cctx).Where("id = ?", capturedConvID).Delete(&model.Conversation{}).Error
			_ = rdb.Del(cctx, fmt.Sprintf("conv:seq:%d", capturedConvID), fmt.Sprintf("conv:lock:%d", capturedConvID)).Err()
		}
		if capturedGroupID > 0 {
			_ = db.WithContext(cctx).Where("group_id = ?", capturedGroupID).Delete(&model.AuditLog{}).Error
			_ = db.WithContext(cctx).Where("group_id = ?", capturedGroupID).Delete(&model.GroupMember{}).Error
			_ = db.WithContext(cctx).Where("id = ?", capturedGroupID).Delete(&model.Group{}).Error
		}
		_ = db.WithContext(cctx).Where("id IN ?", []uint64{u1.ID, u2.ID}).Delete(&model.User{}).Error
	})

	baseRepo := repository.NewGormGroupRepository(db)
	failingRepo := &rollbackInjectGroupRepo{
		GormGroupRepository: baseRepo,
		capturedGroupID:     &capturedGroupID,
		capturedConvID:      &capturedConvID,
	}
	seqAlloc := repository.NewRedisSeqAllocator(rdb, 5*time.Second)
	presence := repository.NewRedisPresenceChecker(rdb)
	idGen, err := snowflake.NewGenerator(2)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}

	svc := NewGroupService(failingRepo, seqAlloc, presence, idGen, rollbackNoopGroupNotifier{}, log)
	if _, createErr := svc.Create(ctx, u1.ID, CreateGroupInput{
		Name:      fmt.Sprintf("RollbackGroup_%d", suffix),
		MemberIDs: []uint64{u2.ID},
	}); createErr == nil {
		t.Fatal("expected GroupService.Create to fail on mid-tx audit failure")
	}
	if capturedGroupID == 0 || capturedConvID == 0 {
		t.Fatalf("expected group/conv IDs captured before rollback, got group=%d conv=%d", capturedGroupID, capturedConvID)
	}

	// Assert zero dirty rows committed across groups, group_members, conversations, conversation_members, messages, audit_logs.
	var grpCnt, gmCnt, convCnt, cmCnt, msgCnt, audCnt int64
	_ = db.WithContext(ctx).Model(&model.Group{}).Where("id = ?", capturedGroupID).Count(&grpCnt).Error
	_ = db.WithContext(ctx).Model(&model.GroupMember{}).Where("group_id = ?", capturedGroupID).Count(&gmCnt).Error
	_ = db.WithContext(ctx).Model(&model.Conversation{}).Where("id = ?", capturedConvID).Count(&convCnt).Error
	_ = db.WithContext(ctx).Model(&model.ConversationMember{}).Where("conversation_id = ?", capturedConvID).Count(&cmCnt).Error
	_ = db.WithContext(ctx).Model(&model.Message{}).Where("conversation_id = ?", capturedConvID).Count(&msgCnt).Error
	_ = db.WithContext(ctx).Model(&model.AuditLog{}).Where("group_id = ?", capturedGroupID).Count(&audCnt).Error
	if grpCnt != 0 || gmCnt != 0 || convCnt != 0 || cmCnt != 0 || msgCnt != 0 || audCnt != 0 {
		t.Fatalf("expected zero dirty rows after group create rollback, got groups=%d gm=%d conv=%d cm=%d msgs=%d audit=%d",
			grpCnt, gmCnt, convCnt, cmCnt, msgCnt, audCnt)
	}
	if _, err := rdb.Get(ctx, fmt.Sprintf("conv:seq:%d", capturedConvID)).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expected Redis conv:seq:%d removed on rollback, got err=%v", capturedConvID, err)
	}
}

type rollbackInjectMessageStore struct {
	*repository.MessageRepository
}

func (s *rollbackInjectMessageStore) UpdateMembersOnNewMessageTx(context.Context, *gorm.DB, uint64, uint64, []uint64, time.Time) error {
	return errors.New("simulated mid-tx failure updating conversation members")
}

func TestLiveMySQLRollbackMessageSend(t *testing.T) {
	db, rdb, log := openLiveMySQLAndRedisForRollbackTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	suffix := time.Now().UnixNano()
	u1 := model.User{Username: fmt.Sprintf("rb_msg_a_%d", suffix), PasswordHash: "x", Nickname: "MsgA", Role: model.UserRoleUser, Status: model.UserStatusActive}
	u2 := model.User{Username: fmt.Sprintf("rb_msg_b_%d", suffix), PasswordHash: "x", Nickname: "MsgB", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.WithContext(ctx).Create(&u1).Error; err != nil {
		t.Fatalf("create u1: %v", err)
	}
	if err := db.WithContext(ctx).Create(&u2).Error; err != nil {
		t.Fatalf("create u2: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	dk := repository.DirectKey(u1.ID, u2.ID)
	conv := model.Conversation{Type: model.ConversationTypeSingle, DirectKey: &dk, LastSeq: 5, CreatedAt: now, UpdatedAt: now}
	if err := db.WithContext(ctx).Create(&conv).Error; err != nil {
		t.Fatalf("create conv: %v", err)
	}
	if err := db.WithContext(ctx).Create(&[]model.Friendship{
		{UserID: u1.ID, FriendID: u2.ID, Status: model.FriendshipStatusActive, CreatedAt: now, UpdatedAt: now},
		{UserID: u2.ID, FriendID: u1.ID, Status: model.FriendshipStatusActive, CreatedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create friendships: %v", err)
	}
	if err := db.WithContext(ctx).Create(&[]model.ConversationMember{
		{ConversationID: conv.ID, UserID: u1.ID, UnreadCount: 0, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: conv.ID, UserID: u2.ID, UnreadCount: 2, IsVisible: true, JoinedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create conv members: %v", err)
	}

	seqKey := fmt.Sprintf("conv:seq:%d", conv.ID)
	if err := rdb.Set(ctx, seqKey, "5", 0).Err(); err != nil {
		t.Fatalf("seed Redis %s: %v", seqKey, err)
	}

	clientMsgID := "rb-msg-" + uuid.NewString()
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = db.WithContext(cctx).Where("conversation_id = ?", conv.ID).Delete(&model.MessageReceipt{}).Error
		_ = db.WithContext(cctx).Where("conversation_id = ?", conv.ID).Delete(&model.Message{}).Error
		_ = db.WithContext(cctx).Where("conversation_id = ?", conv.ID).Delete(&model.ConversationMember{}).Error
		_ = db.WithContext(cctx).Where("id = ?", conv.ID).Delete(&model.Conversation{}).Error
		_ = db.WithContext(cctx).Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", u1.ID, u2.ID, u2.ID, u1.ID).Delete(&model.Friendship{}).Error
		_ = db.WithContext(cctx).Where("id IN ?", []uint64{u1.ID, u2.ID}).Delete(&model.User{}).Error
		_ = rdb.Del(cctx, seqKey, fmt.Sprintf("conv:lock:%d", conv.ID), repository.DedupeKey(u1.ID, clientMsgID), repository.DedupeLockKey(u1.ID, clientMsgID)).Err()
	})

	convRepo := repository.NewConversationRepository(db)
	baseMsgRepo := repository.NewMessageRepository(db, rdb)
	failingMsgStore := &rollbackInjectMessageStore{MessageRepository: baseMsgRepo}
	seqAlloc := repository.NewRedisSeqAllocator(rdb, 5*time.Second)
	idGen, err := snowflake.NewGenerator(2)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}

	svc := NewMessageService(convRepo, failingMsgStore, seqAlloc, idGen, &testBroadcaster{}, 2*time.Minute, log)
	if _, sendErr := svc.SendMessage(ctx, SendMessageInput{
		ConversationID: conv.ID,
		SenderID:       u1.ID,
		ClientMsgID:    clientMsgID,
		MsgType:        model.MessageTypeText,
		Content:        "should rollback completely",
	}, nil); sendErr == nil {
		t.Fatal("expected SendMessage to fail on mid-tx error")
	}

	// Verify MySQL rolled back messages + conversations.last_seq=5 + unread_count=2.
	var msgCnt int64
	_ = db.WithContext(ctx).Model(&model.Message{}).Where("conversation_id = ?", conv.ID).Count(&msgCnt).Error
	if msgCnt != 0 {
		t.Fatalf("expected 0 messages committed after SendMessage rollback, got %d", msgCnt)
	}
	var reloadedConv model.Conversation
	if err := db.WithContext(ctx).First(&reloadedConv, conv.ID).Error; err != nil || reloadedConv.LastSeq != 5 {
		t.Fatalf("expected conversations.last_seq restored to 5, got %d err=%v", reloadedConv.LastSeq, err)
	}
	var bobMem model.ConversationMember
	if err := db.WithContext(ctx).Where("conversation_id = ? AND user_id = ?", conv.ID, u2.ID).First(&bobMem).Error; err != nil || bobMem.UnreadCount != 2 {
		t.Fatalf("expected bob unread_count unchanged at 2, got %d err=%v", bobMem.UnreadCount, err)
	}

	// Verify Redis conv:seq rolled back from 6 -> 5 and dedupe cache was NOT populated.
	if gotSeq, err := rdb.Get(ctx, seqKey).Result(); err != nil || gotSeq != "5" {
		t.Fatalf("expected Redis %s rolled back to 5, got %q err=%v", seqKey, gotSeq, err)
	}
	if _, err := rdb.Get(ctx, repository.DedupeKey(u1.ID, clientMsgID)).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expected no committed Redis dedupe key after rollback, got err=%v", err)
	}
}
