package tests

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/pkg/errcode"
)

func (publisher *stage5Publisher) expect(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	devices := 0
	if publisher.online {
		devices = 1
	}
	stage5PresenceFor(t, conn, publisher.lease.UserID, devices)
}

func TestStage5HubSlowConsumerQueueBoundary(t *testing.T) {
	const capacity = 256
	rig := newStage5Rig(t, true, 2*time.Minute)
	session := rig.session(t, rig.user(t))
	publisher := rig.publisher(t, session.UserID)
	witness := rig.session(t, rig.user(t))
	barrier := rig.publisher(t, witness.UserID)
	observer := rig.connect(t, witness)

	boundaryWrites := make(chan chan struct{}, 1)
	overflowWrites := make(chan chan struct{}, 1)
	boundary := rig.dial(t, session.token, false, boundaryWrites)
	stage5Ping(t, boundary)
	overflow := rig.dial(t, session.token, false, overflowWrites)
	stage5Ping(t, overflow)
	fast := rig.connect(t, session)
	publisher.toggle(t)
	for _, conn := range []*websocket.Conn{boundary, overflow, fast} {
		publisher.expect(t, conn)
	}

	boundaryStarted, overflowStarted := make(chan struct{}), make(chan struct{})
	boundaryWrites <- boundaryStarted
	overflowWrites <- overflowStarted
	publisher.toggle(t)
	firstDevices := 0
	if publisher.online {
		firstDevices = 1
	}
	for _, started := range []<-chan struct{}{boundaryStarted, overflowStarted} {
		select {
		case <-started:
		case <-time.After(stage5IO):
			t.Fatal("the slow peer's write pump did not start its in-flight frame")
		}
	}
	publisher.expect(t, fast)
	for range capacity {
		publisher.toggle(t)
		publisher.expect(t, fast)
	}
	barrier.toggle(t)
	barrier.expect(t, observer)
	devices := firstDevices
	for range capacity + 1 {
		stage5PresenceFor(t, boundary, publisher.lease.UserID, devices)
		devices = 1 - devices
	}
	stage5Ping(t, boundary)

	publisher.toggle(t)
	publisher.expect(t, fast)
	publisher.expect(t, boundary)
	barrier.toggle(t)
	barrier.expect(t, observer)
	stage5ExpectClose(t, overflow, websocket.CloseInternalServerErr, websocket.ClosePolicyViolation, websocket.CloseTryAgainLater)
	rig.count(t, session.UserID, 2)
	stage5Ping(t, fast)
	stage5Ping(t, boundary)
}

func stage5Kicked(t *testing.T, conn *websocket.Conn, session stage5Session) {
	t.Helper()
	stage5Check(t, conn.SetReadDeadline(time.Now().Add(stage5IO)))
	var sawFailure, sawNotice bool
	for {
		var frame stage5Frame
		err := conn.ReadJSON(&frame)
		if websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
			break
		}
		stage5Check(t, err)
		switch frame.Type {
		case "error":
			var failure struct{ Code int }
			stage5Check(t, json.Unmarshal(frame.Data, &failure))
			if failure.Code != errcode.ErrWSSessionRevoked.Code {
				t.Fatalf("kick sent error %d", failure.Code)
			}
			sawFailure = true
		case "system.notice":
			var notice struct {
				Event        string `json:"event"`
				TargetUserID string `json:"target_user_id"`
			}
			stage5Check(t, json.Unmarshal(frame.Data, &notice))
			if notice.Event != "account.kicked" || notice.TargetUserID != strconv.FormatUint(session.UserID, 10) {
				t.Fatalf("incorrect kick notice: %+v", notice)
			}
			sawNotice = true
		case "presence.update":
		default:
			t.Fatalf("unexpected %s before kick", frame.Type)
		}
	}
	if !sawFailure || !sawNotice {
		t.Fatalf("session %d closed with error frame=%v, kick notice=%v", session.ID, sawFailure, sawNotice)
	}
}

func TestStage5HubConcurrentJoinKickAndDelivery(t *testing.T) {
	rig := newStage5Rig(t, true, time.Minute)
	primary, other := rig.user(t), rig.user(t)
	publisher := rig.publisher(t, primary)
	rig.friends(t, other, publisher.lease.UserID)
	shared := rig.session(t, primary)
	stable := []*websocket.Conn{rig.connect(t, shared), rig.connect(t, shared), rig.connect(t, rig.session(t, other))}
	joins := []stage5Session{rig.session(t, primary), rig.session(t, primary), rig.session(t, other)}
	target := rig.session(t, primary)
	t.Run("concurrent", func(t *testing.T) {
		t.Run("deliver_to_all_stable_tabs", func(t *testing.T) {
			t.Parallel()
			for range 24 {
				publisher.toggle(t)
				for _, conn := range stable {
					publisher.expect(t, conn)
				}
			}
		})
		for index, session := range joins {
			t.Run("join_leave_"+strconv.Itoa(index), func(t *testing.T) {
				t.Parallel()
				for range 12 {
					conn := rig.connect(t, session)
					stage5Check(t, conn.Close())
				}
			})
		}
		t.Run("kick_both_tabs_of_one_session", func(t *testing.T) {
			t.Parallel()
			for range 8 {
				first, second := rig.connect(t, target), rig.connect(t, target)
				stage5Check(t, rig.hub.RevokeSessions(t.Context(), primary, []uint64{target.ID}, "account.kicked"))
				stage5Kicked(t, first, target)
				stage5Kicked(t, second, target)
			}
		})
	})
	rig.count(t, primary, 2)
	rig.count(t, other, 1)
	rig.checkRoutes(t, stage5SessionKey(target.ID))
	for _, conn := range stable {
		stage5Ping(t, conn)
	}
}
