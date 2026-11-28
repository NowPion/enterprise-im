package repository

import (
	"context"
	"encoding/json"
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
	store := NewPresenceRepository(db, rdb, 300*time.Millisecond, WithPresenceKeyPrefix("test:pres:"+uuid.NewString()+":"))
	lease := ConnLease{UserID: user.ID, SessionID: user.ID, DeviceID: "web", NodeID: "lease-test", ConnID: uuid.NewString()}
	uid := strconv.FormatUint(user.ID, 10)
	defer rdb.Del(ctx, store.userKey(uid), store.DeadlinesKey(), store.ActiveKey(), store.OfflinePendingKey())
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
	if rdb.Get(ctx, store.userKey(uid)).Val() != "0" {
		t.Fatal("expired lease still online")
	}
	want := *user.LastOnlineAt
	if err := rdb.ZAdd(ctx, store.OfflinePendingKey(), redis.Z{Member: uid, Score: float64(want.Add(-time.Second).UnixMilli())}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&user, user.ID).Error; err != nil || !user.LastOnlineAt.Equal(want) {
		t.Fatal("retry replaced a later offline transition", err)
	}
	if err := rdb.ZScore(ctx, store.OfflinePendingKey(), uid).Err(); !errors.Is(err, redis.Nil) {
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
	store := NewPresenceRepository(nil, rdb, 75*time.Second, WithPresenceKeyPrefix("test:pres:"+uuid.NewString()+":"))
	user := uint64(time.Now().UnixNano())
	userID := strconv.FormatUint(user, 10)
	first := ConnLease{UserID: user, SessionID: user, DeviceID: "web", NodeID: "A", ConnID: uuid.NewString()}
	second := first
	second.ConnID = uuid.NewString()
	userKey, routeKey, sessKey := store.userKey(userID), store.routeUserKey(userID), store.routeSessionKey(userID)
	defer func() {
		for _, lease := range []ConnLease{first, second} {
			_, _ = store.change(ctx, "close", lease)
		}
		rdb.Del(ctx, userKey, store.DeadlinesKey(), store.ActiveKey(), store.OfflinePendingKey())
	}()
	for _, lease := range []ConnLease{first, second} {
		if err := store.Open(ctx, lease); err != nil {
			t.Fatal(err)
		}
	}
	connKey := store.connKey(first.ConnID)
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

type presencePubSubEvent struct {
	UserID      string `json:"user_id"`
	DeviceCount int    `json:"device_count"`
	At          int64  `json:"at"`
}

func readPresencePubSubForUser(t *testing.T, ch <-chan *redis.Message, uid string) presencePubSubEvent {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatal("presence pubsub channel closed unexpectedly")
			}
			var event presencePubSubEvent
			if err := json.Unmarshal([]byte(msg.Payload), &event); err == nil && event.UserID == uid {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for presence event for user %s", uid)
		}
	}
}

func assertNoPresencePubSubForUser(t *testing.T, ch <-chan *redis.Message, uid string, wait time.Duration) {
	t.Helper()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var event presencePubSubEvent
			if err := json.Unmarshal([]byte(msg.Payload), &event); err == nil && event.UserID == uid {
				t.Fatalf("unexpected extra presence event for user %s: %+v", uid, event)
			}
		case <-timer.C:
			return
		}
	}
}

func TestLiveReconnectAfterLastLeaseExpiryPublishesOfflineThenOnline(t *testing.T) {
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
	user := model.User{Username: "ws-m1-" + uuid.NewString(), Nickname: "m1 test", PasswordHash: uuid.NewString()}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Delete(&user)

	uid := strconv.FormatUint(user.ID, 10)
	store := NewPresenceRepository(db, rdb, 250*time.Millisecond, WithPresenceKeyPrefix("test:pres:"+uuid.NewString()+":"))
	sub, err := store.Subscribe(ctx, "m1-node-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	ch := sub.Channel()

	lease1 := ConnLease{UserID: user.ID, SessionID: user.ID + 1, DeviceID: "web", NodeID: "m1-node", ConnID: uuid.NewString()}
	lease2 := ConnLease{UserID: user.ID, SessionID: user.ID + 2, DeviceID: "desktop", NodeID: "m1-node", ConnID: uuid.NewString()}
	lease3 := ConnLease{UserID: user.ID, SessionID: user.ID + 3, DeviceID: "mobile", NodeID: "m1-node", ConnID: uuid.NewString()}
	lease4 := ConnLease{UserID: user.ID, SessionID: user.ID + 4, DeviceID: "web", NodeID: "m1-node", ConnID: uuid.NewString()}

	defer func() {
		for _, lease := range []ConnLease{lease1, lease2, lease3, lease4} {
			_, _ = store.change(ctx, "close", lease)
		}
		rdb.Del(ctx, store.userKey(uid), store.routeUserKey(uid), store.DeadlinesKey(), store.ActiveKey(), store.OfflinePendingKey())
	}()

	// 1. Open first connection and let its lease naturally expire without running ReapExpired.
	if err := store.Open(ctx, lease1); err != nil {
		t.Fatal(err)
	}
	if ev := readPresencePubSubForUser(t, ch, uid); ev.DeviceCount != 1 {
		t.Fatalf("expected initial online event with device_count=1, got %+v", ev)
	}
	time.Sleep(320 * time.Millisecond)

	// 2. Reconnect before ReapExpired runs: must emit offline(0) then online(1), not two consecutive online events.
	if err := store.Open(ctx, lease2); err != nil {
		t.Fatal(err)
	}
	if ev := readPresencePubSubForUser(t, ch, uid); ev.DeviceCount != 0 {
		t.Fatalf("expected missed offline transition device_count=0 before reconnect online, got %+v", ev)
	}
	if ev := readPresencePubSubForUser(t, ch, uid); ev.DeviceCount != 1 {
		t.Fatalf("expected reconnect online event device_count=1 after offline, got %+v", ev)
	}
	var refreshed model.User
	if err := db.First(&refreshed, user.ID).Error; err != nil || refreshed.LastOnlineAt == nil {
		t.Fatalf("expected last_online_at persisted after expired-lease reconnect, err=%v user=%+v", err, refreshed)
	}

	// 3. Delayed ReapExpired must clean lease1 route/deadline without emitting any extra presence event.
	if err := store.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoPresencePubSubForUser(t, ch, uid, 100*time.Millisecond)
	if got := rdb.Get(ctx, store.userKey(uid)).Val(); got != "1" {
		t.Fatalf("expected active connection count 1 after reap, got %q", got)
	}
	if got := rdb.SCard(ctx, store.routeUserKey(uid)).Val(); got != 1 || !rdb.SIsMember(ctx, store.routeUserKey(uid), "m1-node:"+lease2.ConnID).Val() {
		t.Fatalf("expected only lease2 route remaining after reap, card=%d", got)
	}
	if err := rdb.ZScore(ctx, store.OfflinePendingKey(), uid).Err(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expected offline pending key acknowledged, got %v", err)
	}

	// 4. Control: opening another device while lease2 is alive must emit device_count=2 without offline.
	if err := store.Open(ctx, lease3); err != nil {
		t.Fatal(err)
	}
	if ev := readPresencePubSubForUser(t, ch, uid); ev.DeviceCount != 2 {
		t.Fatalf("expected online device_count=2 when adding live device, got %+v", ev)
	}

	// 5. Let both live devices naturally expire before ReapExpired, then reconnect with lease4.
	time.Sleep(320 * time.Millisecond)
	if err := store.Open(ctx, lease4); err != nil {
		t.Fatal(err)
	}
	if ev := readPresencePubSubForUser(t, ch, uid); ev.DeviceCount != 0 {
		t.Fatalf("expected offline device_count=0 after all leases expired, got %+v", ev)
	}
	if ev := readPresencePubSubForUser(t, ch, uid); ev.DeviceCount != 1 {
		t.Fatalf("expected online device_count=1 after offline, got %+v", ev)
	}
	if err := store.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoPresencePubSubForUser(t, ch, uid, 100*time.Millisecond)
}
