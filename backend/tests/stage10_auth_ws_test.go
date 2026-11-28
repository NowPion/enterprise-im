package tests

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
)

func loginAdditionalDevice(t *testing.T, rig *stage6Rig, base stage6User, deviceID, deviceType string) stage6User {
	t.Helper()
	st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    base.Username,
		"password":    base.Password,
		"device_id":   deviceID,
		"device_type": deviceType,
	})
	assertStage6OK(t, "login additional device "+deviceID, st, env, http.StatusOK)

	loginData := decodeStage6JSON[struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			SessionID    string `json:"session_id"`
			DeviceID     string `json:"device_id"`
		} `json:"tokens"`
	}](t, env.Data)
	sid, err := strconv.ParseUint(loginData.Tokens.SessionID, 10, 64)
	if err != nil || sid == 0 || loginData.Tokens.AccessToken == "" || loginData.Tokens.RefreshToken == "" {
		t.Fatalf("incomplete additional device login response: %s", string(env.Data))
	}
	u := base
	u.DeviceID = loginData.Tokens.DeviceID
	u.SessionID = sid
	u.AccessToken = loginData.Tokens.AccessToken
	u.RefreshToken = loginData.Tokens.RefreshToken
	return u
}

func assertWSRevokedAndClosed(t *testing.T, desc string, wsClient *stage6WSClient, expectedNoticeEvent string) {
	t.Helper()

	errFrame := wsClient.awaitFrame(desc+" error 18006", stage6IOTimeout, func(f stage6WSFrame) bool {
		return f.Type == "error"
	})
	var wsErr stage6WSErrorData
	if err := json.Unmarshal(errFrame.Data, &wsErr); err != nil || wsErr.Code != errcode.ErrWSSessionRevoked.Code {
		t.Fatalf("%s expected error code 18006, got %s (err=%v)", desc, string(errFrame.Data), err)
	}

	noticeFrame := wsClient.awaitFrame(desc+" notice "+expectedNoticeEvent, stage6IOTimeout, func(f stage6WSFrame) bool {
		return f.Type == "system.notice"
	})
	var notice struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(noticeFrame.Data, &notice); err != nil || notice.Event != expectedNoticeEvent {
		t.Fatalf("%s expected system.notice event=%q, got %s (err=%v)", desc, expectedNoticeEvent, string(noticeFrame.Data), err)
	}

	select {
	case <-wsClient.done:
	case <-time.After(stage6IOTimeout):
		t.Fatalf("%s timed out waiting for kicked WS readLoop shutdown", desc)
	}

	wsClient.mu.Lock()
	readErr := wsClient.readErr
	wsClient.mu.Unlock()
	if !websocket.IsCloseError(readErr, websocket.ClosePolicyViolation) {
		t.Fatalf("%s expected WS close code 1008 (ClosePolicyViolation), got %v", desc, readErr)
	}
}

func TestStage10AuthAdminDisableAndKickLiveWS(t *testing.T) {
	rig := newStage6Rig(t)
	ctx := t.Context()

	adminCandidate := rig.registerAndLogin("s10_adm", "Stage10 Admin")
	targetDevA := rig.registerAndLogin("s10_tgt", "Stage10 Target")
	targetDevB := loginAdditionalDevice(t, rig, targetDevA, "dev-b-"+uuid.NewString()[:8], "desktop")

	// Promote admin user in MySQL and re-login so JWT carries role=admin.
	if err := rig.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", adminCandidate.ID).Update("role", model.UserRoleAdmin).Error; err != nil {
		t.Fatalf("promote admin user: %v", err)
	}
	admin := loginAdditionalDevice(t, rig, adminCandidate, "admin-dev-"+uuid.NewString()[:8], "web")

	// Connect both devices concurrently to real /ws.
	wsA := rig.dialWS(targetDevA)
	wsB := rig.dialWS(targetDevB)

	presenceKey := fmt.Sprintf("im:presence:user:%d", targetDevA.ID)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if val, _ := rig.redis.Get(ctx, presenceKey).Result(); val == "2" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := rig.redis.Get(ctx, presenceKey).Val(); got != "2" {
		t.Fatalf("expected multi-device presence count 2, got %q", got)
	}

	// 1. Admin kicks single session (dev-a) -> wsA kicked immediately with 18006 + account.kicked + 1008, wsB stays alive.
	kickSt, _, kickEnv, _ := rig.doRequest(
		http.MethodPost,
		fmt.Sprintf("/api/v1/admin/users/%d/kick", targetDevA.ID),
		admin.AccessToken,
		map[string]any{
			"session_id": strconv.FormatUint(targetDevA.SessionID, 10),
			"reason":     "stage10 single-session kick test",
		},
	)
	assertStage6OK(t, "admin kick single session", kickSt, kickEnv, http.StatusOK)
	assertWSRevokedAndClosed(t, "kicked dev-a WS", wsA, "account.kicked")

	// Surviving dev-b remains functional over both WS and HTTP.
	wsB.ping()
	meBSt, _, meBEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/me", targetDevB.AccessToken, nil)
	assertStage6OK(t, "surviving dev-b GET /me", meBSt, meBEnv, http.StatusOK)

	// Kicked dev-a access token is blacklisted & session revoked (HTTP & refresh & WS handshake denied).
	meASt, _, meAEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/me", targetDevA.AccessToken, nil)
	assertStage6BizErr(t, "kicked dev-a GET /me", meASt, meAEnv, errcode.ErrAccessTokenBlacklisted.Code, errcode.ErrSessionRevoked.Code, errcode.ErrAccessTokenInvalid.Code)

	refASt, _, refAEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": targetDevA.RefreshToken,
		"device_id":     targetDevA.DeviceID,
	})
	assertStage6BizErr(t, "kicked dev-a refresh", refASt, refAEnv, errcode.ErrSessionRevoked.Code, errcode.ErrRefreshTokenInvalid.Code)

	// 2. Admin disables user account -> remaining live WS connection (dev-b) immediately kicked with account.disabled + 1008.
	disSt, _, disEnv, _ := rig.doRequest(
		http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/users/%d/status", targetDevA.ID),
		admin.AccessToken,
		map[string]any{
			"status": model.UserStatusDisabled,
			"reason": "stage10 disable kick test",
		},
	)
	assertStage6OK(t, "admin disable target user", disSt, disEnv, http.StatusOK)
	assertWSRevokedAndClosed(t, "disabled dev-b WS", wsB, "account.disabled")

	// Presence lease must drop to 0/nil and DB status=0 + all user_sessions revoked + audit rows persisted.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		val, err := rig.redis.Get(ctx, presenceKey).Result()
		if errors.Is(err, redis.Nil) || val == "0" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if val, err := rig.redis.Get(ctx, presenceKey).Result(); !errors.Is(err, redis.Nil) && val != "0" {
		t.Fatalf("expected target offline presence after disable, got val=%q err=%v", val, err)
	}

	var dbUser model.User
	if err := rig.db.WithContext(ctx).First(&dbUser, targetDevA.ID).Error; err != nil || dbUser.Status != model.UserStatusDisabled {
		t.Fatalf("expected MySQL users.status=0 after disable, got status=%d err=%v", dbUser.Status, err)
	}
	var activeSess int64
	if err := rig.db.WithContext(ctx).Model(&model.UserSession{}).Where("user_id = ? AND revoked_at IS NULL", targetDevA.ID).Count(&activeSess).Error; err != nil || activeSess != 0 {
		t.Fatalf("expected 0 active sessions after disable, got %d (err=%v)", activeSess, err)
	}
	var disableAuditCount int64
	if err := rig.db.WithContext(ctx).Model(&model.AuditLog{}).
		Where("scope = ? AND action = ? AND actor_id = ? AND target_id = ?", model.AuditScopeAdmin, "user.disable", admin.ID, targetDevA.ID).
		Count(&disableAuditCount).Error; err != nil || disableAuditCount != 1 {
		t.Fatalf("expected 1 admin user.disable audit_logs row, got %d (err=%v)", disableAuditCount, err)
	}

	// Disabled account cannot login, refresh, call protected HTTP, or upgrade /ws.
	loginDisSt, _, loginDisEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    targetDevA.Username,
		"password":    targetDevA.Password,
		"device_id":   "dev-c",
		"device_type": "web",
	})
	assertStage6BizErr(t, "disabled user login", loginDisSt, loginDisEnv, errcode.ErrAccountDisabled.Code)

	meBDisSt, _, meBDisEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/me", targetDevB.AccessToken, nil)
	assertStage6BizErr(t, "disabled user old access token", meBDisSt, meBDisEnv, errcode.ErrAccessTokenBlacklisted.Code, errcode.ErrAccountDisabled.Code, errcode.ErrSessionRevoked.Code)

	dialer := websocket.Dialer{HandshakeTimeout: stage6IOTimeout}
	if conn, resp, err := dialer.Dial(rig.wsBaseURL+"/ws?access_token="+url.QueryEscape(targetDevB.AccessToken), nil); err == nil {
		_ = conn.Close()
		t.Fatal("expected disabled user WS handshake to be rejected")
	} else if resp != nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401/403 rejecting disabled user WS upgrade, got %d", resp.StatusCode)
		}
	}

	// 3. Re-enable account -> user can log in again and connect live WS cleanly.
	enSt, _, enEnv, _ := rig.doRequest(
		http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/users/%d/status", targetDevA.ID),
		admin.AccessToken,
		map[string]any{
			"status": model.UserStatusActive,
			"reason": "stage10 re-enable check",
		},
	)
	assertStage6OK(t, "admin re-enable user", enSt, enEnv, http.StatusOK)

	restored := loginAdditionalDevice(t, rig, targetDevA, "dev-restored-"+uuid.NewString()[:8], "web")
	wsRestored := rig.dialWS(restored)
	wsRestored.ping()
}

func TestStage10ConcurrentWSRegistrationAndDisconnectRace(t *testing.T) {
	rig := newStage6Rig(t)
	ctx := t.Context()

	alice := rig.registerAndLogin("s10_wsa", "Alice WS Race")
	bob := rig.registerAndLogin("s10_wsb", "Bob WS Race")
	_, convID, _ := rig.makeFriends(alice, bob)

	// Prepare 5 distinct device sessions (max_sessions_per_user=5) and run 8 concurrent WS tab workers.
	const maxSessions = 5
	const deviceWorkers = 8
	const roundsPerWorker = 3
	sessions := make([]stage6User, 0, maxSessions)
	sessions = append(sessions, alice)
	for i := 1; i < maxSessions; i++ {
		sessions = append(sessions, loginAdditionalDevice(t, rig, alice, fmt.Sprintf("conc-dev-%d-%s", i, uuid.NewString()[:6]), "web"))
	}

	// Concurrent Bob sender exercises server-to-multi-device delivery concurrently with Alice join/leave churn.
	stopBob := make(chan struct{})
	bobDone := make(chan struct{})
	go func() {
		defer close(bobDone)
		for idx := 0; ; idx++ {
			select {
			case <-stopBob:
				return
			default:
			}
			_, _, _, _ = func() (int, http.Header, stage6Envelope, []byte) {
				return rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", bob.AccessToken, map[string]any{
					"client_msg_id": fmt.Sprintf("bob-conc-%d-%s", idx, uuid.NewString()),
					"msg_type":      model.MessageTypeText,
					"content":       fmt.Sprintf("concurrent fanout %d", idx),
					"extra":         map[string]any{},
				})
			}()
			time.Sleep(20 * time.Millisecond)
		}
	}()

	dialer := websocket.Dialer{HandshakeTimeout: stage6IOTimeout}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var errCount atomic.Int32
	var errMu sync.Mutex
	var firstErr error
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errCount.Add(1)
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}

	for w := 0; w < deviceWorkers; w++ {
		wg.Add(1)
		workerSession := sessions[w%len(sessions)]
		go func(idx int, user stage6User) {
			defer wg.Done()
			<-start
			wsURL := rig.wsBaseURL + "/ws?access_token=" + url.QueryEscape(user.AccessToken)
			for round := 0; round < roundsPerWorker; round++ {
				conn, resp, err := dialer.Dial(wsURL, nil)
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				if err != nil {
					recordErr(fmt.Errorf("worker %d round %d dial: %w", idx, round, err))
					continue
				}

				readerDone := make(chan struct{})
				pongCh := make(chan string, 4)
				go func() {
					defer close(readerDone)
					for {
						var frame stage6WSFrame
						if err := conn.ReadJSON(&frame); err != nil {
							return
						}
						if frame.Type == "pong" {
							select {
							case pongCh <- frame.ReqID:
							default:
							}
						}
					}
				}()

				pingID := fmt.Sprintf("p-%d-%d-%s", idx, round, uuid.NewString()[:6])
				_ = conn.SetWriteDeadline(time.Now().Add(stage6IOTimeout))
				if err := conn.WriteJSON(map[string]any{
					"type":      "ping",
					"req_id":    pingID,
					"timestamp": time.Now().UnixMilli(),
					"data":      map[string]string{"nonce": pingID},
				}); err != nil {
					_ = conn.Close()
					<-readerDone
					recordErr(fmt.Errorf("worker %d round %d ping write: %w", idx, round, err))
					continue
				}

				select {
				case gotID := <-pongCh:
					if gotID != pingID {
						recordErr(fmt.Errorf("worker %d round %d pong mismatch: got %q want %q", idx, round, gotID, pingID))
					}
				case <-readerDone:
					recordErr(fmt.Errorf("worker %d round %d closed before pong", idx, round))
				case <-time.After(stage6IOTimeout):
					recordErr(fmt.Errorf("worker %d round %d timed out waiting for pong", idx, round))
				}

				if (idx+round)%2 == 0 {
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second))
				}
				_ = conn.Close()
				<-readerDone
			}
		}(w, workerSession)
	}

	close(start)
	wg.Wait()
	close(stopBob)
	<-bobDone

	if errCount.Load() > 0 {
		t.Fatalf("concurrent multi-device WS race encountered %d error(s), first: %v", errCount.Load(), firstErr)
	}

	// Verify presence count converges to 0 once all device tabs disconnect, and a subsequent connection works cleanly.
	presenceKey := fmt.Sprintf("im:presence:user:%d", alice.ID)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		val, err := rig.redis.Get(ctx, presenceKey).Result()
		if errors.Is(err, redis.Nil) || val == "0" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if val, err := rig.redis.Get(ctx, presenceKey).Result(); !errors.Is(err, redis.Nil) && val != "0" {
		t.Fatalf("expected Alice presence count to return to 0 after concurrent disconnects, got %q (err=%v)", val, err)
	}

	finalWS := rig.dialWS(alice)
	finalWS.ping()
}
