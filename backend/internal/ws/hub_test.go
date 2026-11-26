package ws

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/repository"
)

func TestDelayedRotationOnlyClosesMatchingTokenAndSession(t *testing.T) {
	var hub Hub
	clients := make([]*Client, 3)
	for idx := range clients {
		client := newClient(context.Background(), nil)
		client.lease.UserID, client.lease.SessionID, client.lease.ConnID = 42, 81, strconv.Itoa(idx)
		client.auth.JTI = "old"
		hub.peers.add(42, client.lease.ConnID, client)
		clients[idx] = client
	}
	clients[1].auth.JTI = "replacement"
	clients[2].lease.SessionID = 82
	hub.closeSession(sessionEvent{UserID: 42, SessionID: 81, ConnIDs: []string{"0", "1", "2"}, Reason: "rotated", OnlyJTI: "old"})
	end, ok := context.Cause(clients[0].ctx).(termination)
	if !ok || end.code != 1008 || len(end.frames) != 2 || end.frames[0].Data.(frameError).Code != 18006 || end.frames[1].Type != "system.notice" {
		t.Fatalf("invalid revocation: %+v", end)
	}
	if clients[1].ctx.Err() != nil || clients[2].ctx.Err() != nil {
		t.Fatal("rotation closed a newer token or another device session")
	}
}

func TestHeartbeatExpiryAndDrainingSignals(t *testing.T) {
	client := newClient(context.Background(), nil)
	client.auth.ExpiresAt = time.Now().Add(time.Hour)
	client.auth.SessionExpiresAt = client.auth.ExpiresAt
	client.readFailed(os.ErrDeadlineExceeded)
	end := context.Cause(client.ctx).(termination)
	if end.code != 1008 || end.reason != "heartbeat timeout" {
		t.Fatal(end)
	}
	client.drain()
	if context.Cause(client.ctx).(termination).code != 1008 {
		t.Fatal("second close replaced the first cause")
	}
	open := newClient(context.Background(), nil)
	open.drain()
	end = context.Cause(open.ctx).(termination)
	if end.code != 1000 || len(end.frames) != 1 || end.frames[0].Data.(map[string]any)["event"] != "server.draining" {
		t.Fatal(end)
	}
}

func TestRegistrySnapshotsAndSlowClientIsolation(t *testing.T) {
	var peers registry
	slow := newClient(context.Background(), nil)
	fast := newClient(context.Background(), nil)
	peers.add(17, "slow", slow)
	peers.add(17, "fast", fast)
	if len(peers.shards) != 64 || cap(slow.send) != 256 {
		t.Fatal("unexpected shard count or queue capacity")
	}
	for range cap(slow.send) {
		slow.enqueue(frame{Type: "pong"})
	}
	for _, client := range peers.snapshot(17) {
		client.enqueue(frame{Type: "presence.update"})
	}
	if slow.ctx.Err() == nil || fast.ctx.Err() != nil || (<-fast.send).Type != "presence.update" {
		t.Fatal("overflow must evict only the slow connection")
	}
	peers.remove(17, "slow", slow)
	peers.add(17, "slow", fast)
	peers.remove(17, "slow", slow)
	if len(peers.snapshot(17)) != 2 {
		t.Fatal("stale removal affected a replacement")
	}
}

func TestRegistryConcurrentJoinDeliverKickAndLeave(t *testing.T) {
	var peers registry
	var workers sync.WaitGroup
	for index := range 96 {
		workers.Go(func() {
			id := strconv.Itoa(index)
			user := uint64(index%4 + 1)
			client := newClient(context.Background(), nil)
			peers.add(user, id, client)
			for _, peer := range peers.snapshot(user) {
				peer.enqueue(frame{Type: "pong"})
				peer.stop(termination{code: 1008})
			}
			peers.remove(user, id, client)
		})
	}
	workers.Wait()
	if len(peers.all()) != 0 {
		t.Fatal("registry retained disconnected clients")
	}
}

type presenceFrameExpectation struct {
	online      bool
	deviceCount int
}

func readFriendPresenceFrame(t *testing.T, ch <-chan frame, targetUserID string) presenceFrameExpectation {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case msg := <-ch:
			if msg.Type != "presence.update" {
				continue
			}
			payload, ok := msg.Data.(map[string]any)
			if !ok || payload["user_id"] != targetUserID {
				continue
			}
			online, _ := payload["online"].(bool)
			count, _ := payload["device_count"].(int)
			lastOnline, hasLastOnline := payload["last_online_at"].(*time.Time)
			if !online && (!hasLastOnline || lastOnline == nil) {
				t.Fatalf("offline presence.update missing last_online_at: %+v", payload)
			}
			if online && lastOnline != nil {
				t.Fatalf("online presence.update must have nil last_online_at: %+v", payload)
			}
			return presenceFrameExpectation{online: online, deviceCount: count}
		case <-timer.C:
			t.Fatalf("timed out waiting for presence.update frame for user %s", targetUserID)
		}
	}
}

func TestLiveReconnectAfterLastLeaseExpiryDeliversOfflineThenOnlineToFriend(t *testing.T) {
	cfg, err := config.Load("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if (cfg.MySQL.Password == "" || cfg.Redis.Password == "") && os.Getenv("IM_RUN_LIVE_TESTS") == "" {
		t.Skip("MySQL and Redis credentials not supplied")
	}
	db, err := repository.NewMySQL(cfg.MySQL, nil)
	if err != nil {
		t.Fatal("connecting to live MySQL failed")
	}
	defer repository.CloseMySQL(db)
	rdb, err := repository.NewRedis(cfg.Redis)
	if err != nil {
		t.Fatal("connecting to live Redis failed")
	}
	defer rdb.Close()

	ctx := context.Background()
	target := model.User{Username: "ws-target-" + uuid.NewString(), Nickname: "target", PasswordHash: uuid.NewString()}
	friend := model.User{Username: "ws-friend-" + uuid.NewString(), Nickname: "friend", PasswordHash: uuid.NewString()}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Delete(&target)
	if err := db.Create(&friend).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Delete(&friend)

	pair := []model.Friendship{
		{UserID: target.ID, FriendID: friend.ID, Status: model.FriendshipStatusActive},
		{UserID: friend.ID, FriendID: target.ID, Status: model.FriendshipStatusActive},
	}
	if err := db.Create(&pair).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", target.ID, friend.ID, friend.ID, target.ID).Delete(&model.Friendship{})

	friendStore := repository.NewPresenceRepository(db, rdb, 10*time.Second)
	targetStore := repository.NewPresenceRepository(db, rdb, 250*time.Millisecond)

	nodeID := "ws-m1-node-" + uuid.NewString()
	cfgCopy := *cfg
	cfgCopy.App.NodeID = nodeID
	hub := NewHub(&cfgCopy, nil, friendStore, nil)

	sub, err := friendStore.Subscribe(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer sub.Close()
		hub.listen(runCtx, sub)
	}()
	defer func() {
		cancel()
		<-done
	}()

	friendClient := newClient(ctx, nil)
	friendClient.lease = repository.ConnLease{
		UserID:    friend.ID,
		SessionID: friend.ID + 100,
		DeviceID:  "web",
		NodeID:    nodeID,
		ConnID:    uuid.NewString(),
	}
	hub.peers.add(friend.ID, friendClient.lease.ConnID, friendClient)
	defer hub.peers.remove(friend.ID, friendClient.lease.ConnID, friendClient)
	if err := friendStore.Open(ctx, friendClient.lease); err != nil {
		t.Fatal(err)
	}
	targetUID := strconv.FormatUint(target.ID, 10)
	friendUID := strconv.FormatUint(friend.ID, 10)
	defer rdb.Del(ctx, "im:presence:user:"+targetUID, "im:presence:user:"+friendUID)
	defer friendStore.Release(ctx, friendClient.lease)

	lease1 := repository.ConnLease{UserID: target.ID, SessionID: target.ID + 200, DeviceID: "web", NodeID: nodeID, ConnID: uuid.NewString()}
	lease2 := repository.ConnLease{UserID: target.ID, SessionID: target.ID + 201, DeviceID: "desktop", NodeID: nodeID, ConnID: uuid.NewString()}
	defer targetStore.Release(ctx, lease1)
	defer targetStore.Release(ctx, lease2)

	if err := targetStore.Open(ctx, lease1); err != nil {
		t.Fatal(err)
	}
	if got := readFriendPresenceFrame(t, friendClient.send, targetUID); !got.online || got.deviceCount != 1 {
		t.Fatalf("expected initial friend notification online=true device_count=1, got %+v", got)
	}

	// Allow lease1 to expire naturally before ReapExpired runs, then reconnect with lease2.
	time.Sleep(320 * time.Millisecond)
	if err := targetStore.Open(ctx, lease2); err != nil {
		t.Fatal(err)
	}
	if got := readFriendPresenceFrame(t, friendClient.send, targetUID); got.online || got.deviceCount != 0 {
		t.Fatalf("expected friend notification online=false device_count=0 before reconnect online, got %+v", got)
	}
	if got := readFriendPresenceFrame(t, friendClient.send, targetUID); !got.online || got.deviceCount != 1 {
		t.Fatalf("expected friend notification online=true device_count=1 after offline, got %+v", got)
	}

	if err := targetStore.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(120 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case extra := <-friendClient.send:
			if payload, ok := extra.Data.(map[string]any); ok && extra.Type == "presence.update" && payload["user_id"] == targetUID {
				t.Fatalf("unexpected duplicate presence notification after reap: %+v", payload)
			}
		case <-timer.C:
			return
		}
	}
}

func TestDeliverToUsersEnqueuesFramesAndDeduplicatesTargets(t *testing.T) {
	var hub Hub
	c1 := newClient(context.Background(), nil)
	c2 := newClient(context.Background(), nil)
	c3 := newClient(context.Background(), nil)
	c1.lease = repository.ConnLease{UserID: 101, ConnID: "c1"}
	c2.lease = repository.ConnLease{UserID: 101, ConnID: "c2"}
	c3.lease = repository.ConnLease{UserID: 102, ConnID: "c3"}
	hub.peers.add(101, "c1", c1)
	hub.peers.add(101, "c2", c2)
	hub.peers.add(102, "c3", c3)

	payload := map[string]any{"event": "group.member_added", "group_id": "1"}
	if err := hub.DeliverToUsers(context.Background(), []uint64{0, 101, 101, 102}, "system.notice", payload); err != nil {
		t.Fatalf("DeliverToUsers failed: %v", err)
	}

	for name, ch := range map[string]<-chan frame{"c1": c1.send, "c2": c2.send, "c3": c3.send} {
		select {
		case msg := <-ch:
			if msg.Type != "system.notice" {
				t.Fatalf("%s expected system.notice, got %q", name, msg.Type)
			}
		default:
			t.Fatalf("%s did not receive delivered frame", name)
		}
		select {
		case extra := <-ch:
			t.Fatalf("%s received duplicate frame: %+v", name, extra)
		default:
		}
	}
}
