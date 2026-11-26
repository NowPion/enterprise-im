package repository

import (
	"context"
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

func TestReceiptRepositoryNilGuards(t *testing.T) {
	var repo *ReceiptRepository
	ctx := context.Background()

	if err := repo.WithTx(ctx, func(*gorm.DB) error { return nil }); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable on nil WithTx, got %v", err)
	}
	if _, err := repo.GetConversationByID(ctx, 1); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}
	if _, err := repo.GetConversationMember(ctx, 1, 1); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}
	if _, err := repo.GetMessageInConversation(ctx, 1, 1); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}
	if _, _, err := repo.QueryMessageReceiptDetails(ctx, &model.Conversation{ID: 1}, &model.Message{ID: 1}, "", 0, 20); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable, got %v", err)
	}

	nonNil := NewReceiptRepository(nil)
	if _, err := nonNil.LockConversationByIDTx(ctx, nil, 1); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable on nil tx, got %v", err)
	}
	if _, err := nonNil.MarkSingleReceiptDeliveredTx(ctx, nil, 1, time.Now()); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable on nil tx, got %v", err)
	}
	if _, err := nonNil.MarkReceiptsReadUpToSeqTx(ctx, nil, 1, 1, 1, time.Now()); err != ErrRepositoryUnavailable {
		t.Fatalf("expected ErrRepositoryUnavailable on nil tx, got %v", err)
	}
}

func TestReceiptRepositoryLiveMySQLQueriesAndStateTransitions(t *testing.T) {
	mysqlPass := os.Getenv("MYSQL_PASSWORD")
	if mysqlPass == "" {
		t.Skip("skipping live MySQL receipt_repo test: MYSQL_PASSWORD not set")
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ts := time.Now().UTC().UnixNano()
	u1 := model.User{Username: fmt.Sprintf("rcpt_repo_u1_%d", ts), PasswordHash: "x", Nickname: "U1", Role: model.UserRoleUser, Status: model.UserStatusActive}
	u2 := model.User{Username: fmt.Sprintf("rcpt_repo_u2_%d", ts), PasswordHash: "x", Nickname: "U2", Role: model.UserRoleUser, Status: model.UserStatusActive}
	u3 := model.User{Username: fmt.Sprintf("rcpt_repo_u3_%d", ts), PasswordHash: "x", Nickname: "U3", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.WithContext(ctx).Create(&[]model.User{u1, u2, u3}).Error; err != nil {
		t.Fatalf("create test users: %v", err)
	}
	// Reload IDs cleanly.
	var users []model.User
	if err := db.WithContext(ctx).Where("username IN ?", []string{u1.Username, u2.Username, u3.Username}).Order("id ASC").Find(&users).Error; err != nil {
		t.Fatalf("reload users: %v", err)
	}
	u1, u2, u3 = users[0], users[1], users[2]

	dk := DirectKey(u1.ID, u2.ID)
	singleConv := model.Conversation{Type: model.ConversationTypeSingle, DirectKey: &dk, LastSeq: 2}
	if err := db.WithContext(ctx).Create(&singleConv).Error; err != nil {
		t.Fatalf("create single conv: %v", err)
	}
	grp := model.Group{Name: fmt.Sprintf("rcpt-grp-%d", ts), OwnerID: u1.ID, Status: model.GroupStatusActive, MaxMembers: 500}
	if err := db.WithContext(ctx).Create(&grp).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}
	groupConv := model.Conversation{Type: model.ConversationTypeGroup, GroupID: &grp.ID, LastSeq: 2}
	if err := db.WithContext(ctx).Create(&groupConv).Error; err != nil {
		t.Fatalf("create group conv: %v", err)
	}

	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = db.WithContext(cctx).Where("conversation_id IN ?", []uint64{singleConv.ID, groupConv.ID}).Delete(&model.MessageReceipt{}).Error
		_ = db.WithContext(cctx).Where("conversation_id IN ?", []uint64{singleConv.ID, groupConv.ID}).Delete(&model.Message{}).Error
		_ = db.WithContext(cctx).Where("conversation_id IN ?", []uint64{singleConv.ID, groupConv.ID}).Delete(&model.ConversationMember{}).Error
		_ = db.WithContext(cctx).Where("group_id = ?", grp.ID).Delete(&model.GroupMember{}).Error
		_ = db.WithContext(cctx).Where("id IN ?", []uint64{singleConv.ID, groupConv.ID}).Delete(&model.Conversation{}).Error
		_ = db.WithContext(cctx).Where("id = ?", grp.ID).Delete(&model.Group{}).Error
		_ = db.WithContext(cctx).Where("id IN ?", []uint64{u1.ID, u2.ID, u3.ID}).Delete(&model.User{}).Error
	})

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := db.WithContext(ctx).Create(&[]model.ConversationMember{
		{ConversationID: singleConv.ID, UserID: u1.ID, LastDeliveredSeq: 0, LastReadSeq: 0, UnreadCount: 0, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: singleConv.ID, UserID: u2.ID, LastDeliveredSeq: 0, LastReadSeq: 0, UnreadCount: 2, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: groupConv.ID, UserID: u1.ID, LastDeliveredSeq: 1, LastReadSeq: 1, UnreadCount: 0, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: groupConv.ID, UserID: u2.ID, LastDeliveredSeq: 2, LastReadSeq: 0, UnreadCount: 1, IsVisible: true, JoinedAt: now, UpdatedAt: now},
		{ConversationID: groupConv.ID, UserID: u3.ID, LastDeliveredSeq: 2, LastReadSeq: 2, UnreadCount: 0, IsVisible: true, JoinedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create conversation_members: %v", err)
	}
	if err := db.WithContext(ctx).Create(&[]model.GroupMember{
		{GroupID: grp.ID, UserID: u1.ID, Role: model.GroupRoleOwner, JoinedAt: now, UpdatedAt: now},
		{GroupID: grp.ID, UserID: u2.ID, Role: model.GroupRoleAdmin, JoinedAt: now, UpdatedAt: now},
		{GroupID: grp.ID, UserID: u3.ID, Role: model.GroupRoleMember, JoinedAt: now, UpdatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create group_members: %v", err)
	}

	singleMsgID := uint64(ts)
	groupMsgID := uint64(ts + 1)
	if err := db.WithContext(ctx).Create(&[]model.Message{
		{ID: singleMsgID, ConversationID: singleConv.ID, Seq: 1, SenderID: u1.ID, ClientMsgID: fmt.Sprintf("s-%d", ts), MsgType: model.MessageTypeText, Content: "hi", Status: model.MessageStatusNormal, CreatedAt: now},
		{ID: groupMsgID, ConversationID: groupConv.ID, Seq: 2, SenderID: u1.ID, ClientMsgID: fmt.Sprintf("g-%d", ts), MsgType: model.MessageTypeText, Content: "hi grp", Status: model.MessageStatusNormal, CreatedAt: now},
	}).Error; err != nil {
		t.Fatalf("create messages: %v", err)
	}
	singleRec := model.MessageReceipt{
		MessageID: singleMsgID, ConversationID: singleConv.ID, Seq: 1, UserID: u2.ID,
		Status: model.ReceiptStatusSent, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.WithContext(ctx).Create(&singleRec).Error; err != nil {
		t.Fatalf("create single receipt: %v", err)
	}

	repo := NewReceiptRepository(db)

	// Verify group summary & filters (u2 delivered=2/read=0, u3 delivered=2/read=2, u1 excluded as sender).
	gSummary, gRows, err := repo.QueryMessageReceiptDetails(ctx, &groupConv, &model.Message{ID: groupMsgID, ConversationID: groupConv.ID, Seq: 2, SenderID: u1.ID}, "", 0, 10)
	if err != nil {
		t.Fatalf("query group receipts: %v", err)
	}
	if gSummary.TotalMembers != 2 || gSummary.DeliveredCount != 2 || gSummary.ReadCount != 1 || gSummary.UnreadCount != 1 {
		t.Fatalf("unexpected group summary: %+v", gSummary)
	}
	if len(gRows) != 2 {
		t.Fatalf("expected 2 group receipt rows, got %d", len(gRows))
	}

	// Verify single receipt transitions: sent -> delivered -> read, preserving delivered_at, and never downgrading read.
	t1 := now.Add(time.Second)
	t2 := now.Add(2 * time.Second)
	if err := repo.WithTx(ctx, func(tx *gorm.DB) error {
		locked, lErr := repo.LockMessageReceiptTx(ctx, tx, singleMsgID, u2.ID)
		if lErr != nil || locked == nil {
			return fmt.Errorf("lock single receipt: %v", lErr)
		}
		changed, mErr := repo.MarkSingleReceiptDeliveredTx(ctx, tx, locked.ID, t1)
		if mErr != nil || !changed {
			return fmt.Errorf("mark delivered: changed=%v err=%v", changed, mErr)
		}
		if n, rErr := repo.MarkReceiptsReadUpToSeqTx(ctx, tx, singleConv.ID, u2.ID, 1, t2); rErr != nil || n != 1 {
			return fmt.Errorf("mark read: n=%d err=%v", n, rErr)
		}
		// Calling MarkSingleReceiptDeliveredTx again after read must affect 0 rows.
		downgraded, dErr := repo.MarkSingleReceiptDeliveredTx(ctx, tx, locked.ID, t2.Add(time.Second))
		if dErr != nil || downgraded {
			return fmt.Errorf("expected no downgrade after read: downgraded=%v err=%v", downgraded, dErr)
		}
		return nil
	}); err != nil {
		t.Fatalf("single receipt transition tx failed: %v", err)
	}

	sSummary, sRows, err := repo.QueryMessageReceiptDetails(ctx, &singleConv, &model.Message{ID: singleMsgID, ConversationID: singleConv.ID, Seq: 1, SenderID: u1.ID}, ReceiptFilterRead, 0, 10)
	if err != nil {
		t.Fatalf("query single receipts: %v", err)
	}
	if sSummary.TotalMembers != 1 || sSummary.DeliveredCount != 1 || sSummary.ReadCount != 1 || sSummary.UnreadCount != 0 || len(sRows) != 1 {
		t.Fatalf("unexpected single summary/rows: %+v %+v", sSummary, sRows)
	}
	if sRows[0].DeliveredAt == nil || !sRows[0].DeliveredAt.UTC().Equal(t1) || sRows[0].ReadAt == nil || !sRows[0].ReadAt.UTC().Equal(t2) {
		t.Fatalf("expected delivered_at=%v preserved and read_at=%v, got %+v", t1, t2, sRows[0])
	}
}
