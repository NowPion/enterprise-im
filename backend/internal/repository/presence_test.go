package repository

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
)

func TestLiveReapLastLeaseAndRetryOfflineTimestamp(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if (cfg.MySQL.Password == "" || cfg.Redis.Password == "") && os.Getenv("IM_RUN_LIVE_TESTS") == "" {
		t.Skip("MySQL and Redis credentials not supplied")
	}
	db, err := NewMySQL(cfg.MySQL, nil)
	if err != nil {
		t.Fatal("connecting to live MySQL failed")
	}
	defer CloseMySQL(db)
	rdb, err := NewRedis(cfg.Redis)
	if err != nil {
		t.Fatal("connecting to live Redis failed")
	}
	defer rdb.Close()
	ctx := context.Background()
	user := model.User{Username: "ws-" + uuid.NewString(), Nickname: "lease test", PasswordHash: uuid.NewString()}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Delete(&user)
	store := NewPresenceRepository(db, rdb, 300*time.Millisecond)
	lease := ConnLease{UserID: user.ID, SessionID: user.ID, DeviceID: "web", NodeID: "lease-test", ConnID: uuid.NewString()}
	uid := strconv.FormatUint(user.ID, 10)
	defer rdb.Del(ctx, "im:presence:user:"+uid)
	if err := store.Open(ctx, lease); err != nil {
		t.Fatal(err)
	}
	defer store.Release(ctx, lease)
	time.Sleep(350 * time.Millisecond)
	if err := store.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&user, user.ID).Error; err != nil || user.LastOnlineAt == nil {
		t.Fatal("expired lease did not persist last_online_at", err)
	}
	if rdb.Get(ctx, "im:presence:user:"+uid).Val() != "0" {
		t.Fatal("expired lease still online")
	}
	want := *user.LastOnlineAt
	if err := rdb.ZAdd(ctx, offlinePendingKey, redis.Z{Member: uid, Score: float64(want.Add(-time.Second).UnixMilli())}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&user, user.ID).Error; err != nil || !user.LastOnlineAt.Equal(want) {
		t.Fatal("retry replaced a later offline transition", err)
	}
	if err := rdb.ZScore(ctx, offlinePendingKey, uid).Err(); !errors.Is(err, redis.Nil) {
		t.Fatal("offline update not acknowledged", err)
	}
}

func TestLiveConnectionLeaseOwnershipAndTTL(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Password == "" && os.Getenv("IM_RUN_LIVE_TESTS") == "" {
		t.Skip("Redis credentials not supplied")
	}
	rdb, err := NewRedis(cfg.Redis)
	if err != nil {
		t.Fatal("connecting to live Redis failed")
	}
	defer rdb.Close()
	ctx := context.Background()
	store := NewPresenceRepository(nil, rdb, 75*time.Second)
	user := uint64(time.Now().UnixNano())
	userID := strconv.FormatUint(user, 10)
	first := ConnLease{UserID: user, SessionID: user, DeviceID: "web", NodeID: "A", ConnID: uuid.NewString()}
	second := first
	second.ConnID = uuid.NewString()
	userKey, routeKey, sessKey := "im:presence:user:"+userID, "im:route:user:"+userID, "im:route:session:"+userID
	defer func() {
		for _, lease := range []ConnLease{first, second} {
			_, _ = store.change(ctx, "close", lease)
		}
		rdb.Del(ctx, userKey)
		rdb.ZRem(ctx, offlinePendingKey, userID)
	}()
	for _, lease := range []ConnLease{first, second} {
		if err := store.Open(ctx, lease); err != nil {
			t.Fatal(err)
		}
	}
	connKey := "im:presence:conn:" + first.ConnID
	fields, err := rdb.HGetAll(ctx, connKey).Result()
	if err != nil || fields["user_id"] != userID || fields["session_id"] != userID || fields["device_id"] != "web" || fields["node_id"] != "A" || fields["conn_id"] != first.ConnID || fields["last_seen_at"] == "" {
		t.Fatalf("unexpected lease fields %v, %v", fields, err)
	}
	for key, want := range map[string]time.Duration{connKey: 75 * time.Second, userKey: 75 * time.Second, routeKey: 90 * time.Second, sessKey: 90 * time.Second} {
		got, err := rdb.PTTL(ctx, key).Result()
		if err != nil || got <= want-2*time.Second || got > want {
			t.Fatalf("unexpected TTL %v, want %v (%v)", got, want, err)
		}
		if err := rdb.PExpire(ctx, key, 5*time.Second).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Renew(ctx, first); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{connKey, userKey, routeKey, sessKey} {
		if rdb.TTL(ctx, key).Val() < 74*time.Second {
			t.Fatal("ping did not renew", key)
		}
	}
	wrong := first
	wrong.DeviceID = "another-device"
	if err := store.Release(ctx, wrong); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("mismatched release: %v", err)
	}
	if got := rdb.Get(ctx, userKey).Val(); got != "2" {
		t.Fatalf("want two tabs, got %q", got)
	}
	if err := store.Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Renew(ctx, first); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale renewal: %v", err)
	}
	if err := store.Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	if rdb.Get(ctx, userKey).Val() != "1" || rdb.SCard(ctx, routeKey).Val() != 1 || !rdb.SIsMember(ctx, sessKey, "A:"+second.ConnID).Val() {
		t.Fatal("stale tab changed surviving leases")
	}
}
