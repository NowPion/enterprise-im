package repository

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/logger"
)

func TestFriendshipHelpersAndNilGuards(t *testing.T) {
	minID, maxID, err := OrderedUserPair(200, 100)
	if err != nil || minID != 100 || maxID != 200 {
		t.Fatalf("unexpected OrderedUserPair result: (%d, %d, %v)", minID, maxID, err)
	}
	if _, _, err := OrderedUserPair(100, 100); !errors.Is(err, ErrInvalidUserPair) {
		t.Fatalf("expected ErrInvalidUserPair for equal IDs, got %v", err)
	}
	if _, _, err := OrderedUserPair(0, 100); !errors.Is(err, ErrInvalidUserPair) {
		t.Fatalf("expected ErrInvalidUserPair for zero ID, got %v", err)
	}

	k1, err := FormatDirectKey(200, 100)
	if err != nil {
		t.Fatalf("FormatDirectKey failed: %v", err)
	}
	k2, err := FormatDirectKey(100, 200)
	if err != nil {
		t.Fatalf("FormatDirectKey reverse failed: %v", err)
	}
	if k1 != "100:200" || k1 != k2 {
		t.Fatalf("expected symmetric direct key 100:200, got %q and %q", k1, k2)
	}

	if got := extractConnIDFromRouteMember("node-1:conn-abc"); got != "conn-abc" {
		t.Fatalf("unexpected extracted conn id: %q", got)
	}
	if got := extractConnIDFromRouteMember("conn-bare"); got != "conn-bare" {
		t.Fatalf("unexpected fallback conn id: %q", got)
	}
	if got := extractConnIDFromRouteMember("  "); got != "" {
		t.Fatalf("expected empty conn id for whitespace, got %q", got)
	}

	repo := NewFriendshipRepository(nil, nil)
	ctx := context.Background()
	if _, err := repo.ListActiveFriendships(ctx, 1, 0, 20); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for ListActiveFriendships, got %v", err)
	}
	if _, err := repo.GetOnlineStatusBatch(ctx, []uint64{1}); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for GetOnlineStatusBatch, got %v", err)
	}
	if err := repo.UpdateRemark(ctx, 1, 2, "test"); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for UpdateRemark, got %v", err)
	}
	if _, err := repo.DeactivateBidirectionalFriendship(ctx, 1, 2); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for DeactivateBidirectionalFriendship, got %v", err)
	}
	if err := UpsertBidirectionalFriendshipTx(ctx, nil, 1, 2); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for UpsertBidirectionalFriendshipTx, got %v", err)
	}
	if _, err := EnsureFriendshipAndConversationTx(ctx, nil, 1, 2); !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("expected ErrRepositoryUnavailable for EnsureFriendshipAndConversationTx, got %v", err)
	}
}

func TestFriendshipRepositoryLiveMySQLAndRedis(t *testing.T) {
	db, rdb, cleanup := openLiveDepsForRepoTest(t)
	if db == nil || rdb == nil {
		return
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Test inside a rolled-back transaction for MySQL mutations to guarantee zero dirty rows,
	// plus dedicated live Redis keys with explicit cleanup.
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatalf("begin tx: %v", tx.Error)
	}
	defer tx.Rollback()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	uA := model.User{
		Username:     "repo_friend_a_" + suffix,
		PasswordHash: "hash",
		Nickname:     "Alice",
		Status:       model.UserStatusActive,
		Role:         model.UserRoleUser,
	}
	uB := model.User{
		Username:     "repo_friend_b_" + suffix,
		PasswordHash: "hash",
		Nickname:     "Bob",
		Status:       model.UserStatusActive,
		Role:         model.UserRoleUser,
	}
	uC := model.User{
		Username:     "repo_friend_c_" + suffix,
		PasswordHash: "hash",
		Nickname:     "Charlie",
		Status:       model.UserStatusActive,
		Role:         model.UserRoleUser,
	}
	for _, u := range []*model.User{&uA, &uB, &uC} {
		if err := tx.Create(u).Error; err != nil {
			t.Fatalf("create user %s: %v", u.Username, err)
		}
	}

	// EnsureFriendshipAndConversationTx creates bidirectional friendships + single conversation + both members.
	convAB, err := EnsureFriendshipAndConversationTx(ctx, tx, uA.ID, uB.ID)
	if err != nil {
		t.Fatalf("EnsureFriendshipAndConversationTx(A,B): %v", err)
	}
	if convAB.ID == 0 || convAB.Type != model.ConversationTypeSingle || convAB.DirectKey == nil {
		t.Fatalf("unexpected conversation created: %+v", convAB)
	}
	if _, err := EnsureFriendshipAndConversationTx(ctx, tx, uA.ID, uC.ID); err != nil {
		t.Fatalf("EnsureFriendshipAndConversationTx(A,C): %v", err)
	}

	activeAB, err := AreActiveFriendsTx(ctx, tx, uA.ID, uB.ID)
	if err != nil || !activeAB {
		t.Fatalf("expected active bidirectional friendship (A,B), got (%v, %v)", activeAB, err)
	}

	repoTx := NewFriendshipRepository(tx, rdb)

	// ListActiveFriendships returns DESC order with limit+1 probe.
	rows, err := repoTx.ListActiveFriendships(ctx, uA.ID, 0, 1)
	if err != nil {
		t.Fatalf("ListActiveFriendships page1: %v", err)
	}
	if len(rows) != 2 || rows[0].FriendID != uC.ID {
		t.Fatalf("expected 2 rows (limit+1) starting with Charlie(%d), got %+v", uC.ID, rows)
	}
	page2, err := repoTx.ListActiveFriendships(ctx, uA.ID, rows[0].ID, 1)
	if err != nil {
		t.Fatalf("ListActiveFriendships page2: %v", err)
	}
	if len(page2) != 1 || page2[0].FriendID != uB.ID {
		t.Fatalf("expected 1 row on page2 with Bob(%d), got %+v", uB.ID, page2)
	}

	// Batch user and conversation lookup.
	usersMap, err := repoTx.BatchGetUsersByIDs(ctx, []uint64{uB.ID, uC.ID, uB.ID})
	if err != nil || len(usersMap) != 2 {
		t.Fatalf("BatchGetUsersByIDs failed: len=%d err=%v", len(usersMap), err)
	}
	dkAB, _ := FormatDirectKey(uA.ID, uB.ID)
	convMap, err := repoTx.BatchGetSingleConversationsByDirectKeys(ctx, []string{dkAB})
	if err != nil || convMap[dkAB].ID != convAB.ID {
		t.Fatalf("BatchGetSingleConversationsByDirectKeys failed: %+v err=%v", convMap, err)
	}

	// UpdateRemark inside tx updates only caller's row.
	if err := tx.Model(&model.Friendship{}).
		Where("user_id = ? AND friend_id = ?", uA.ID, uB.ID).
		Update("remark", "MyBob").Error; err != nil {
		t.Fatalf("update remark: %v", err)
	}
	fBA, err := GetFriendshipTx(ctx, tx, uB.ID, uA.ID)
	if err != nil || fBA.Remark != "" {
		t.Fatalf("expected reverse remark empty, got %+v err=%v", fBA, err)
	}

	// Live Redis batch online status check across multiple devices + stale route protection.
	connKeyB := redisPresenceConnKeyPrefix + "stage4-repo-b-" + suffix
	routeKeyB := redisRouteUserKeyPrefix + strconv.FormatUint(uB.ID, 10)
	userKeyB := redisPresenceUserKeyPrefix + strconv.FormatUint(uB.ID, 10)
	routeKeyC := redisRouteUserKeyPrefix + strconv.FormatUint(uC.ID, 10)
	userKeyC := redisPresenceUserKeyPrefix + strconv.FormatUint(uC.ID, 10)

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanCancel()
		_ = rdb.Del(cleanCtx, connKeyB, routeKeyB, userKeyB, routeKeyC, userKeyC).Err()
	})

	// User B: 2 route members, one dead conn + one live conn -> online=true.
	if err := rdb.HSet(ctx, connKeyB, "user_id", uB.ID, "conn_id", "stage4-repo-b-"+suffix).Err(); err != nil {
		t.Fatalf("redis HSet connKeyB: %v", err)
	}
	_ = rdb.Expire(ctx, connKeyB, 30*time.Second).Err()
	_ = rdb.SAdd(ctx, routeKeyB, "node-1:dead-conn-"+suffix, "node-1:stage4-repo-b-"+suffix).Err()
	_ = rdb.Expire(ctx, routeKeyB, 30*time.Second).Err()
	_ = rdb.Set(ctx, userKeyB, "2", 30*time.Second).Err()

	// User C: stale route member with NO live conn lease even though userKeyC="1" -> online=false.
	_ = rdb.SAdd(ctx, routeKeyC, "node-1:expired-conn-"+suffix).Err()
	_ = rdb.Expire(ctx, routeKeyC, 30*time.Second).Err()
	_ = rdb.Set(ctx, userKeyC, "1", 30*time.Second).Err()

	onlineMap, err := repoTx.GetOnlineStatusBatch(ctx, []uint64{uB.ID, uC.ID, uA.ID})
	if err != nil {
		t.Fatalf("GetOnlineStatusBatch failed: %v", err)
	}
	if !onlineMap[uB.ID] || onlineMap[uC.ID] || onlineMap[uA.ID] {
		t.Fatalf("unexpected online map: B=%v C=%v A=%v", onlineMap[uB.ID], onlineMap[uC.ID], onlineMap[uA.ID])
	}
}

func TestFriendshipRepositoryLiveTxCommitAndDeleteRestore(t *testing.T) {
	db, rdb, cleanup := openLiveDepsForRepoTest(t)
	if db == nil || rdb == nil {
		return
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	uA := model.User{
		Username:     "repo_tx_a_" + suffix,
		PasswordHash: "hash",
		Nickname:     "TxAlice",
		Status:       model.UserStatusActive,
		Role:         model.UserRoleUser,
	}
	uB := model.User{
		Username:     "repo_tx_b_" + suffix,
		PasswordHash: "hash",
		Nickname:     "TxBob",
		Status:       model.UserStatusActive,
		Role:         model.UserRoleUser,
	}
	if err := db.WithContext(ctx).Create(&uA).Error; err != nil {
		t.Fatalf("create uA: %v", err)
	}
	if err := db.WithContext(ctx).Create(&uB).Error; err != nil {
		t.Fatalf("create uB: %v", err)
	}

	var convID uint64
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		if convID > 0 {
			_ = db.WithContext(cleanCtx).Where("conversation_id = ?", convID).Delete(&model.ConversationMember{}).Error
			_ = db.WithContext(cleanCtx).Where("id = ?", convID).Delete(&model.Conversation{}).Error
		}
		_ = db.WithContext(cleanCtx).
			Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", uA.ID, uB.ID, uB.ID, uA.ID).
			Delete(&model.Friendship{}).Error
		_ = db.WithContext(cleanCtx).Where("id IN ?", []uint64{uA.ID, uB.ID}).Delete(&model.User{}).Error
	})

	err := WithTx(ctx, db, func(tx *gorm.DB) error {
		conv, err := EnsureFriendshipAndConversationTx(ctx, tx, uA.ID, uB.ID)
		if err != nil {
			return err
		}
		convID = conv.ID
		return nil
	})
	if err != nil {
		t.Fatalf("EnsureFriendshipAndConversationTx commit failed: %v", err)
	}

	repo := NewFriendshipRepository(db, rdb)

	// Remark update affects only caller side.
	if err := repo.UpdateRemark(ctx, uA.ID, uB.ID, "Colleague"); err != nil {
		t.Fatalf("UpdateRemark failed: %v", err)
	}
	rowAB, err := repo.GetFriendship(ctx, uA.ID, uB.ID)
	if err != nil || rowAB.Remark != "Colleague" {
		t.Fatalf("expected A->B remark Colleague, got %+v err=%v", rowAB, err)
	}
	rowBA, err := repo.GetFriendship(ctx, uB.ID, uA.ID)
	if err != nil || rowBA.Remark != "" {
		t.Fatalf("expected B->A remark empty, got %+v err=%v", rowBA, err)
	}

	// Delete deactivates both friendships and hides both conversation_members.
	delOutcome, err := repo.DeactivateBidirectionalFriendship(ctx, uA.ID, uB.ID)
	if err != nil {
		t.Fatalf("DeactivateBidirectionalFriendship failed: %v", err)
	}
	if delOutcome.FriendID != uB.ID || delOutcome.ConversationID == nil || *delOutcome.ConversationID != convID || delOutcome.IsVisible {
		t.Fatalf("unexpected delete outcome: %+v", delOutcome)
	}

	active, err := repo.AreActiveFriends(ctx, uA.ID, uB.ID)
	if err != nil || active {
		t.Fatalf("expected inactive friendship after delete, got (%v, %v)", active, err)
	}
	var members []model.ConversationMember
	if err := db.WithContext(ctx).Where("conversation_id = ?", convID).Find(&members).Error; err != nil {
		t.Fatalf("query conversation_members: %v", err)
	}
	if len(members) != 2 || members[0].IsVisible || members[1].IsVisible {
		t.Fatalf("expected both conversation_members preserved and hidden, got %+v", members)
	}

	// Second delete returns ErrFriendshipInactive.
	if _, err := repo.DeactivateBidirectionalFriendship(ctx, uA.ID, uB.ID); !errors.Is(err, ErrFriendshipInactive) {
		t.Fatalf("expected ErrFriendshipInactive on second delete, got %v", err)
	}

	// Re-accept restores active friendship and SAME conversation_id with is_visible=true.
	var restoredConvID uint64
	err = WithTx(ctx, db, func(tx *gorm.DB) error {
		conv, err := EnsureFriendshipAndConversationTx(ctx, tx, uB.ID, uA.ID)
		if err != nil {
			return err
		}
		restoredConvID = conv.ID
		return nil
	})
	if err != nil || restoredConvID != convID {
		t.Fatalf("expected restored convID=%d, got %d err=%v", convID, restoredConvID, err)
	}

	members = nil
	if err := db.WithContext(ctx).Where("conversation_id = ?", convID).Find(&members).Error; err != nil {
		t.Fatalf("query restored conversation_members: %v", err)
	}
	if len(members) != 2 || !members[0].IsVisible || !members[1].IsVisible {
		t.Fatalf("expected both conversation_members visible after restore, got %+v", members)
	}
}

func openLiveDepsForRepoTest(t *testing.T) (*gorm.DB, *redis.Client, func()) {
	t.Helper()

	mysqlPass := firstNonEmptyEnv("MYSQL_PASSWORD", "IM_MYSQL_PASSWORD")
	redisPass := firstNonEmptyEnv("REDIS_PASSWORD", "IM_REDIS_PASSWORD")
	if mysqlPass == "" && redisPass == "" && os.Getenv("IM_LIVE_TESTS") == "" {
		t.Skip("skipping live MySQL/Redis friendship repository test: MYSQL_PASSWORD/REDIS_PASSWORD not set")
		return nil, nil, nil
	}

	host := firstNonEmptyEnv("MYSQL_HOST", "IM_MYSQL_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := 3306
	if p := firstNonEmptyEnv("MYSQL_PORT", "IM_MYSQL_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	user := firstNonEmptyEnv("MYSQL_USER", "IM_MYSQL_USER")
	if user == "" {
		user = "root"
	}
	dbName := firstNonEmptyEnv("MYSQL_DATABASE", "IM_MYSQL_DATABASE")
	if dbName == "" {
		dbName = "enterprise_im"
	}
	redisAddr := firstNonEmptyEnv("REDIS_ADDR", "IM_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}

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
		_ = CloseMySQL(db)
		t.Fatalf("NewRedis live connect failed: %v", err)
	}

	return db, rdb, func() {
		_ = CloseRedis(rdb)
		_ = CloseMySQL(db)
	}
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
