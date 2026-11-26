package tests

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/repository"
)

type stage5Presence struct {
	UserID       string     `json:"user_id"`
	Online       *bool      `json:"online"`
	Devices      *int       `json:"device_count"`
	LastOnlineAt *time.Time `json:"last_online_at"`
}

func stage5PresenceData(t *testing.T, frame stage5Frame) stage5Presence {
	t.Helper()
	var state stage5Presence
	stage5Check(t, json.Unmarshal(frame.Data, &state))
	if state.UserID == "" || state.Online == nil || state.Devices == nil || *state.Online != (*state.Devices > 0) || *state.Devices < 0 {
		t.Fatalf("invalid presence.update: %s", frame.Data)
	}
	return state
}

func stage5PresenceFor(t *testing.T, conn *websocket.Conn, userID uint64, devices int, pendingDevices ...int) stage5Presence {
	t.Helper()
	user := strconv.FormatUint(userID, 10)
	var state stage5Presence
	stage5Await(t, conn, "presence.update", func(frame stage5Frame) bool {
		state = stage5PresenceData(t, frame)
		if state.UserID != user {
			return false
		}
		if *state.Devices == devices {
			return true
		}
		if !slices.Contains(pendingDevices, *state.Devices) {
			t.Fatalf("presence for %s reported %d devices while waiting for %d", user, *state.Devices, devices)
		}
		return false
	})
	return state
}

func (rig *stage5Rig) connID(t *testing.T, session stage5Session, previous ...string) string {
	t.Helper()
	routes, err := rig.redis.SMembers(t.Context(), stage5SessionKey(session.ID)).Result()
	stage5Check(t, err)
	if len(routes) != len(previous)+1 {
		t.Fatalf("session %d routes=%v, want one more than %v", session.ID, routes, previous)
	}
	for _, route := range routes {
		connID, ok := strings.CutPrefix(route, rig.node+":")
		if !ok || connID == "" {
			t.Fatalf("invalid node_id:conn_id route %q", route)
		}
		if !slices.Contains(previous, connID) {
			return connID
		}
	}
	t.Fatalf("session %d has no new connection", session.ID)
	return ""
}

func (rig *stage5Rig) checkRoutes(t *testing.T, key string, connections ...string) {
	t.Helper()
	got, err := rig.redis.SMembers(t.Context(), key).Result()
	stage5Check(t, err)
	var want []string
	for _, connID := range connections {
		want = append(want, rig.node+":"+connID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s=%v, want %v", key, got, want)
	}
}

func (rig *stage5Rig) leaseFields(t *testing.T, session stage5Session, connID string) {
	t.Helper()
	fields, err := rig.redis.HGetAll(t.Context(), "im:presence:conn:"+connID).Result()
	stage5Check(t, err)
	if fields["user_id"] != strconv.FormatUint(session.UserID, 10) || fields["session_id"] != strconv.FormatUint(session.ID, 10) ||
		fields["device_id"] != session.DeviceID || fields["node_id"] != rig.node || fields["conn_id"] != connID || fields["last_seen_at"] == "" {
		t.Fatalf("connection %s lease fields=%v", connID, fields)
	}
}

func (rig *stage5Rig) checkTTL(t *testing.T, key string, want time.Duration) {
	t.Helper()
	got, err := rig.redis.PTTL(t.Context(), key).Result()
	stage5Check(t, err)
	if got < want-stage5IO || got > want {
		t.Fatalf("%s PTTL=%v, want a fresh %v TTL", key, got, want)
	}
}

func (rig *stage5Rig) lastOnline(t *testing.T, userID uint64) *time.Time {
	t.Helper()
	var user model.User
	stage5Check(t, rig.db.WithContext(t.Context()).Select("last_online_at").First(&user, userID).Error)
	return user.LastOnlineAt
}

func (rig *stage5Rig) absentLeases(t *testing.T, connections ...string) {
	t.Helper()
	for _, connID := range connections {
		got, err := rig.redis.Exists(t.Context(), "im:presence:conn:"+connID).Result()
		stage5Check(t, err)
		if got != 0 {
			t.Fatalf("connection %s retained its lease", connID)
		}
	}
}

func TestStage5PresenceMultipleDevicesAndTabs(t *testing.T) {
	rig := newStage5Rig(t, true, 0)
	user, friend := rig.user(t), rig.user(t)
	rig.friends(t, user, friend)
	marker := rig.publisher(t, friend)
	observer := rig.connect(t, rig.session(t, friend))
	web, phone := rig.session(t, user), rig.session(t, user)
	first := rig.connect(t, web)
	firstID := rig.connID(t, web)
	stage5PresenceFor(t, observer, user, 1)
	second := rig.connect(t, web)
	secondID := rig.connID(t, web, firstID)
	mobile := rig.connect(t, phone)
	mobileID := rig.connID(t, phone)
	stage5PresenceFor(t, observer, user, 2, 1)
	rig.count(t, user, 3)
	rig.checkRoutes(t, stage5Routes(user), firstID, secondID, mobileID)
	rig.checkRoutes(t, stage5SessionKey(web.ID), firstID, secondID)
	rig.checkRoutes(t, stage5SessionKey(phone.ID), mobileID)
	rig.leaseFields(t, web, firstID)
	rig.leaseFields(t, web, secondID)
	rig.leaseFields(t, phone, mobileID)

	keys := map[string]time.Duration{
		stage5UserKey(user): 75 * time.Second, "im:presence:conn:" + secondID: 75 * time.Second,
		stage5Routes(user): 90 * time.Second, stage5SessionKey(web.ID): 90 * time.Second,
	}
	const shortened = 30 * time.Second
	for key, want := range keys {
		rig.checkTTL(t, key, want)
		stage5Check(t, rig.redis.PExpire(t.Context(), key, shortened).Err())
	}
	siblingKey := "im:presence:conn:" + firstID
	stage5Check(t, rig.redis.PExpire(t.Context(), siblingKey, shortened).Err())
	stage5Ping(t, second)
	for key, want := range keys {
		rig.checkTTL(t, key, want)
	}
	rig.checkTTL(t, siblingKey, shortened)

	stage5Check(t, first.Close())
	rig.count(t, user, 2)
	rig.absentLeases(t, firstID)
	rig.checkRoutes(t, stage5SessionKey(web.ID), secondID)
	rig.checkRoutes(t, stage5Routes(user), secondID, mobileID)
	stage5Check(t, mobile.Close())
	rig.count(t, user, 1)
	rig.absentLeases(t, mobileID)
	rig.checkRoutes(t, stage5SessionKey(phone.ID))
	rig.checkRoutes(t, stage5Routes(user), secondID)
	stage5PresenceFor(t, observer, user, 1, 2)
	stage5Ping(t, second)
	if rig.lastOnline(t, user) != nil {
		t.Fatal("partial disconnect updated last_online_at")
	}
	marker.toggle(t)
	stage5Await(t, observer, "presence.update", func(frame stage5Frame) bool {
		state := stage5PresenceData(t, frame)
		if state.UserID == strconv.FormatUint(user, 10) && !*state.Online {
			t.Fatal("friend received offline while a tab was still connected")
		}
		return state.UserID == strconv.FormatUint(marker.lease.UserID, 10) && *state.Online
	})

	stage5Check(t, second.Close())
	rig.count(t, user, 0)
	rig.checkRoutes(t, stage5Routes(user))
	rig.checkRoutes(t, stage5SessionKey(web.ID))
	rig.absentLeases(t, firstID, secondID, mobileID)
	offline := stage5PresenceFor(t, observer, user, 0)
	if offline.LastOnlineAt == nil {
		t.Fatal("offline presence omitted last_online_at")
	}
	stage5Eventually(t, "last_online_at after final disconnect", func() bool { return rig.lastOnline(t, user) != nil })
}

func TestStage5PresenceRecountsExpiredConnection(t *testing.T) {
	rig := newStage5Rig(t, true, time.Minute)
	session := rig.session(t, rig.user(t))
	active := rig.connect(t, session)
	activeID := rig.connID(t, session)
	stale := repository.ConnLease{UserID: session.UserID, SessionID: session.ID, DeviceID: session.DeviceID, NodeID: rig.node, ConnID: uuid.NewString()}
	stage5Check(t, rig.store.Open(t.Context(), stale))
	rig.cleanup = append(rig.cleanup, func(ctx context.Context) error { return rig.store.Release(ctx, stale) })
	rig.count(t, session.UserID, 2)
	staleKey := "im:presence:conn:" + stale.ConnID
	stage5Check(t, rig.redis.PExpire(t.Context(), staleKey, time.Millisecond).Err())
	stage5Eventually(t, "the abandoned tab's lease to expire", func() bool {
		count, err := rig.redis.Exists(t.Context(), staleKey).Result()
		stage5Check(t, err)
		return count == 0
	})
	stage5Ping(t, active)
	rig.count(t, session.UserID, 1)
	for range 2 {
		stage5Check(t, rig.store.Release(t.Context(), stale))
	}
	rig.count(t, session.UserID, 1)
	rig.checkRoutes(t, stage5SessionKey(session.ID), activeID)
	rig.checkRoutes(t, stage5Routes(session.UserID), activeID)
	rig.leaseFields(t, session, activeID)
	if rig.lastOnline(t, session.UserID) != nil {
		t.Fatal("stale cleanup marked the active tab offline")
	}
}

func TestStage5WSHeartbeatEvictsOnlyIdleTab(t *testing.T) {
	const timeout = 1500 * time.Millisecond
	rig := newStage5Rig(t, true, timeout)
	session := rig.session(t, rig.user(t))
	idle := rig.connect(t, session)
	idleID := rig.connID(t, session)
	active := rig.connect(t, session)
	activeID := rig.connID(t, session, idleID)
	rig.count(t, session.UserID, 2)
	for until := time.Now().Add(3 * timeout / 2); time.Now().Before(until); {
		time.Sleep(timeout / 4)
		stage5Ping(t, active)
	}
	stage5ExpectClose(t, idle, websocket.ClosePolicyViolation, websocket.CloseGoingAway)
	rig.count(t, session.UserID, 1)
	rig.absentLeases(t, idleID)
	rig.checkRoutes(t, stage5SessionKey(session.ID), activeID)
	stage5Ping(t, active)
}
