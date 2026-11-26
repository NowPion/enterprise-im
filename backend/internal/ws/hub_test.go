package ws

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
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
