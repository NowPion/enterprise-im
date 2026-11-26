package tests

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

const (
	stage6IOTimeout    = 8 * time.Second
	stage6QuietTimeout = 400 * time.Millisecond
)

func buildStage6ServerBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "enterprise-im-server")
	cmd := exec.Command("go", "build", "-o", binPath, "../cmd/server")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ../cmd/server: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return binPath
}

type stage6Envelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	RequestID string          `json:"request_id"`
}

type stage6WSFrame struct {
	Type      string          `json:"type"`
	ReqID     string          `json:"req_id"`
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type stage6WSErrorData struct {
	Code         int    `json:"code"`
	Message      string `json:"message"`
	Retryable    *bool  `json:"retryable"`
	RelatedType  string `json:"related_type"`
	RelatedReqID string `json:"related_req_id"`
}

type stage6MessageDTO struct {
	MsgID          string          `json:"msg_id"`
	ClientMsgID    string          `json:"client_msg_id"`
	ConversationID string          `json:"conversation_id"`
	Seq            string          `json:"seq"`
	SenderID       string          `json:"sender_id"`
	MsgType        string          `json:"msg_type"`
	Content        string          `json:"content"`
	Extra          json.RawMessage `json:"extra"`
	Status         string          `json:"status"`
	DeliveryStatus string          `json:"delivery_status"`
	CreatedAt      string          `json:"created_at"`
	RecalledAt     *string         `json:"recalled_at"`
}

func (m stage6MessageDTO) seqUint64(t *testing.T) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(m.Seq, 10, 64)
	if err != nil {
		t.Fatalf("parse message seq %q: %v", m.Seq, err)
	}
	return v
}

func (m stage6MessageDTO) extraMap(t *testing.T) map[string]any {
	t.Helper()
	if len(m.Extra) == 0 || string(m.Extra) == "null" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(m.Extra, &out); err != nil {
		t.Fatalf("unmarshal message extra %s: %v", string(m.Extra), err)
	}
	return out
}

type stage6MessagePageDTO struct {
	Items      []stage6MessageDTO `json:"items"`
	NextCursor *string            `json:"next_cursor"`
	HasMore    bool               `json:"has_more"`
}

type stage6SyncDTO struct {
	Items   []stage6MessageDTO `json:"items"`
	FromSeq string             `json:"from_seq"`
	ToSeq   string             `json:"to_seq"`
	HasMore bool               `json:"has_more"`
}

type stage6ConversationDTO struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Peer             json.RawMessage `json:"peer"`
	GroupID          *string         `json:"group_id"`
	Title            string          `json:"title"`
	AvatarURL        string          `json:"avatar_url"`
	LastMsgID        string          `json:"last_msg_id"`
	LastSeq          string          `json:"last_seq"`
	LastMsgPreview   string          `json:"last_msg_preview"`
	LastMsgAt        *string         `json:"last_msg_at"`
	LastDeliveredSeq string          `json:"last_delivered_seq"`
	LastReadSeq      string          `json:"last_read_seq"`
	UnreadCount      int             `json:"unread_count"`
	IsPinned         bool            `json:"is_pinned"`
	IsMuted          bool            `json:"is_muted"`
	IsVisible        bool            `json:"is_visible"`
	CanSend          *bool           `json:"can_send"`
}

type stage6ConversationPageDTO struct {
	Items      []stage6ConversationDTO `json:"items"`
	NextCursor *string                 `json:"next_cursor"`
	HasMore    bool                    `json:"has_more"`
}

type stage6AssetDTO struct {
	AssetID      string  `json:"asset_id"`
	Purpose      string  `json:"purpose"`
	URL          string  `json:"url"`
	DownloadURL  string  `json:"download_url"`
	Filename     string  `json:"filename"`
	Mime         string  `json:"mime"`
	SizeBytes    uint64  `json:"size_bytes"`
	Width        *uint32 `json:"width"`
	Height       *uint32 `json:"height"`
	ThumbnailURL *string `json:"thumbnail_url"`
	DurationMs   *uint32 `json:"duration_ms"`
}

type stage6GroupDTO struct {
	ID             string  `json:"id"`
	ConversationID string  `json:"conversation_id"`
	Name           string  `json:"name"`
	AvatarURL      string  `json:"avatar_url"`
	Announcement   *string `json:"announcement"`
	OwnerID        string  `json:"owner_id"`
	Status         string  `json:"status"`
	MemberCount    int     `json:"member_count"`
	MaxMembers     int     `json:"max_members"`
	Role           string  `json:"role"`
}

type stage6GroupMemberDTO struct {
	UserID          string `json:"user_id"`
	Nickname        string `json:"nickname"`
	AvatarURL       string `json:"avatar_url"`
	NicknameInGroup string `json:"nickname_in_group"`
	Role            string `json:"role"`
	Online          bool   `json:"online"`
	JoinedAt        string `json:"joined_at"`
}

type stage6User struct {
	ID           uint64
	IDStr        string
	Username     string
	Nickname     string
	Password     string
	DeviceID     string
	SessionID    uint64
	AccessToken  string
	RefreshToken string
}

type stage6Rig struct {
	t          *testing.T
	baseURL    string
	wsBaseURL  string
	httpClient *http.Client
	db         *gorm.DB
	redis      *redis.Client
	workDir    string

	mu      sync.Mutex
	users   []uint64
	convs   []uint64
	groups  []uint64
	assets  []string
	cleanFn []func(context.Context)
}

func newStage6Rig(t *testing.T) *stage6Rig {
	t.Helper()

	cfgPath := os.Getenv("IM_CONFIG_FILE")
	if cfgPath == "" {
		cfgPath = os.Getenv("CONFIG_FILE")
	}
	if cfgPath == "" {
		cfgPath = "../configs/config.yaml"
	}
	absCfg, err := filepath.Abs(cfgPath)
	if err != nil {
		t.Fatalf("resolve config path: %v", err)
	}

	cfg, err := config.Load(absCfg)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	extBaseURL := strings.TrimRight(os.Getenv("IM_TEST_BASE_URL"), "/")
	if cfg.MySQL.Password == "" && cfg.Redis.Password == "" &&
		os.Getenv("IM_RUN_LIVE_TESTS") == "" && os.Getenv("IM_LIVE_TESTS") == "" && extBaseURL == "" {
		t.Skip("live Stage 6 requires MYSQL_PASSWORD/REDIS_PASSWORD (or IM_RUN_LIVE_TESTS / IM_TEST_BASE_URL)")
	}

	rig := &stage6Rig{
		t:          t,
		httpClient: &http.Client{Timeout: stage6IOTimeout},
		workDir:    t.TempDir(),
	}

	quietLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	if cfg.MySQL.Password != "" || os.Getenv("IM_RUN_LIVE_TESTS") != "" || os.Getenv("IM_LIVE_TESTS") != "" {
		rig.db, err = repository.NewMySQL(cfg.MySQL, quietLog)
		if err != nil {
			t.Fatalf("connect live mysql: %v", err)
		}
		t.Cleanup(func() { _ = repository.CloseMySQL(rig.db) })

		rig.redis, err = repository.NewRedis(cfg.Redis)
		if err != nil {
			t.Fatalf("connect live redis: %v", err)
		}
		t.Cleanup(func() { _ = repository.CloseRedis(rig.redis) })
	}

	t.Cleanup(rig.cleanupData)

	if extBaseURL != "" {
		rig.baseURL = extBaseURL
		rig.wsBaseURL = toWSBaseURL(extBaseURL)
		return rig
	}

	binPath := buildStage6ServerBinary(t)

	addr, stopFn := startStage6ServerProcess(t, binPath, absCfg, rig.workDir)
	t.Cleanup(stopFn)

	rig.baseURL = "http://" + addr
	rig.wsBaseURL = "ws://" + addr
	rig.waitHealthy()
	return rig
}

func toWSBaseURL(httpURL string) string {
	switch {
	case strings.HasPrefix(httpURL, "https://"):
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	case strings.HasPrefix(httpURL, "http://"):
		return "ws://" + strings.TrimPrefix(httpURL, "http://")
	default:
		return "ws://" + httpURL
	}
}

func startStage6ServerProcess(t *testing.T, binPath, absCfg, workDir string) (string, func()) {
	t.Helper()

	cmd := exec.Command(binPath, "-config", absCfg)
	cmd.Dir = workDir

	overrides := map[string]string{
		"SERVER_ADDR":    "127.0.0.1:0",
		"IM_SERVER_ADDR": "127.0.0.1:0",
		"LOG_FORMAT":     "json",
		"IM_LOG_FORMAT":  "json",
		"LOG_LEVEL":      "info",
		"IM_LOG_LEVEL":   "info",
		"APP_NODE_ID":    "stage6-" + uuid.NewString(),
		"IM_APP_NODE_ID": "stage6-" + uuid.NewString(),
	}
	if os.Getenv("JWT_SECRET") == "" && os.Getenv("IM_JWT_SECRET") == "" {
		secret := "stage6-qa-test-jwt-secret-" + uuid.NewString()
		overrides["JWT_SECRET"] = secret
		overrides["IM_JWT_SECRET"] = secret
	}
	cmd.Env = mergeStage6Env(os.Environ(), overrides)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderrBuf safeBuffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start server process: %v", err)
	}

	addrCh := make(chan string, 1)
	var logTail safeLines
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			logTail.add(line)
			var entry struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "http_server_listening" && entry.Addr != "" {
				select {
				case addrCh <- entry.Addr:
				default:
				}
			}
		}
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var boundAddr string
	select {
	case boundAddr = <-addrCh:
	case exitErr := <-waitDone:
		t.Fatalf("server exited prematurely: %v; stderr=%s; logs=%s", exitErr, stderrBuf.String(), logTail.joined())
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-waitDone
		t.Fatalf("timed out waiting for server http_server_listening; stderr=%s; logs=%s", stderrBuf.String(), logTail.joined())
	}

	stopFn := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-waitDone
		}
	}
	return boundAddr, stopFn
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type safeLines struct {
	mu    sync.Mutex
	lines []string
}

func (s *safeLines) add(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lines) >= 25 {
		s.lines = s.lines[1:]
	}
	s.lines = append(s.lines, line)
}

func (s *safeLines) joined() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

func (r *stage6Rig) waitHealthy() {
	r.t.Helper()
	deadline := time.Now().Add(stage6IOTimeout)
	for {
		resp, err := r.httpClient.Get(r.baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("server /healthz did not become ready")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (r *stage6Rig) trackUser(id uint64) {
	if id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.users, id) {
		r.users = append(r.users, id)
	}
}

func (r *stage6Rig) trackConv(id uint64) {
	if id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.convs, id) {
		r.convs = append(r.convs, id)
	}
}

func (r *stage6Rig) trackGroup(id uint64) {
	if id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.groups, id) {
		r.groups = append(r.groups, id)
	}
}

func (r *stage6Rig) trackAsset(assetID string) {
	if assetID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.assets, assetID) {
		r.assets = append(r.assets, assetID)
	}
}

func (r *stage6Rig) cleanupData() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	r.mu.Lock()
	cleanFns := slices.Clone(r.cleanFn)
	userIDs := slices.Clone(r.users)
	convIDs := slices.Clone(r.convs)
	groupIDs := slices.Clone(r.groups)
	assetIDs := slices.Clone(r.assets)
	r.mu.Unlock()

	for _, fn := range slices.Backward(cleanFns) {
		fn(ctx)
	}

	if r.db != nil {
		db := r.db.WithContext(ctx)
		if len(groupIDs) > 0 {
			var gConvIDs []uint64
			_ = db.Model(&model.Conversation{}).Where("group_id IN ?", groupIDs).Pluck("id", &gConvIDs).Error
			for _, cid := range gConvIDs {
				if !slices.Contains(convIDs, cid) {
					convIDs = append(convIDs, cid)
				}
			}
			_ = db.Where("group_id IN ?", groupIDs).Delete(&model.AuditLog{}).Error
			_ = db.Where("group_id IN ?", groupIDs).Delete(&model.GroupMember{}).Error
			_ = db.Where("id IN ?", groupIDs).Delete(&model.Group{}).Error
		}
		if len(convIDs) > 0 {
			_ = db.Where("conversation_id IN ?", convIDs).Delete(&model.AuditLog{}).Error
			_ = db.Where("conversation_id IN ?", convIDs).Delete(&model.MessageReceipt{}).Error
			_ = db.Where("conversation_id IN ?", convIDs).Delete(&model.Message{}).Error
			_ = db.Where("conversation_id IN ?", convIDs).Delete(&model.ConversationMember{}).Error
			_ = db.Where("id IN ?", convIDs).Delete(&model.Conversation{}).Error
		}
		if len(assetIDs) > 0 {
			_ = db.Where("asset_id IN ?", assetIDs).Delete(&model.Asset{}).Error
		}
		if len(userIDs) > 0 {
			_ = db.Where("owner_id IN ?", userIDs).Delete(&model.Asset{}).Error
			_ = db.Where("sender_id IN ?", userIDs).Delete(&model.Message{}).Error
			_ = db.Where("actor_id IN ?", userIDs).Delete(&model.AuditLog{}).Error
			_ = db.Where("user_id IN ? OR friend_id IN ?", userIDs, userIDs).Delete(&model.Friendship{}).Error
			_ = db.Where("from_user_id IN ? OR to_user_id IN ?", userIDs, userIDs).Delete(&model.FriendRequest{}).Error
		}
	}

	if r.redis != nil {
		for _, cid := range convIDs {
			_ = r.redis.Del(ctx, fmt.Sprintf("conv:seq:%d", cid), fmt.Sprintf("conv:lock:%d", cid)).Err()
		}
		for _, uid := range userIDs {
			_ = r.redis.Del(ctx,
				fmt.Sprintf("im:presence:user:%d", uid),
				fmt.Sprintf("im:route:user:%d", uid),
			).Err()
			r.scanAndDelete(ctx, fmt.Sprintf("im:dedupe:%d:*", uid))
			r.scanAndDelete(ctx, fmt.Sprintf("im:dedupe:lock:%d:*", uid))
		}
	}

	if r.db != nil && len(userIDs) > 0 {
		db := r.db.WithContext(ctx)
		var sessionIDs []uint64
		var jtis []string
		_ = db.Model(&model.UserSession{}).Where("user_id IN ?", userIDs).Pluck("id", &sessionIDs).Error
		_ = db.Model(&model.UserSession{}).Where("user_id IN ?", userIDs).Pluck("access_jti", &jtis).Error
		if r.redis != nil {
			for _, sid := range sessionIDs {
				_ = r.redis.Del(ctx, fmt.Sprintf("im:route:session:%d", sid)).Err()
			}
			for _, jti := range jtis {
				if jti != "" {
					_ = r.redis.Del(ctx, repository.TokenBlacklistKey(jti)).Err()
				}
			}
		}
		_ = db.Where("user_id IN ?", userIDs).Delete(&model.UserSession{}).Error
		_ = db.Where("id IN ?", userIDs).Delete(&model.User{}).Error
	}
}

func (r *stage6Rig) scanAndDelete(ctx context.Context, pattern string) {
	var cursor uint64
	for {
		keys, next, err := r.redis.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return
		}
		if len(keys) > 0 {
			_ = r.redis.Del(ctx, keys...).Err()
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}

func (r *stage6Rig) doRequest(method, path, token string, body any) (int, http.Header, stage6Envelope, []byte) {
	r.t.Helper()

	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			r.t.Fatalf("marshal request body for %s %s: %v", method, path, err)
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, r.baseURL+path, bodyReader)
	if err != nil {
		r.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.t.Fatalf("execute %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("read response %s %s: %v", method, path, err)
	}

	var env stage6Envelope
	if err := json.Unmarshal(rawResp, &env); err != nil {
		r.t.Fatalf("%s %s (HTTP %d) did not return valid JSON envelope: %v; body=%s", method, path, resp.StatusCode, err, string(rawResp))
	}
	if reqID := resp.Header.Get(errcode.HeaderRequestID); reqID == "" || env.RequestID != reqID {
		r.t.Fatalf("%s %s X-Request-ID mismatch: header=%q envelope=%q body=%s", method, path, reqID, env.RequestID, string(rawResp))
	}
	return resp.StatusCode, resp.Header, env, rawResp
}

func decodeStage6JSON[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode JSON %T: %v; raw=%s", out, err, string(raw))
	}
	return out
}

func assertStage6OK(t *testing.T, op string, status int, env stage6Envelope, wantStatuses ...int) {
	t.Helper()
	if len(wantStatuses) == 0 {
		wantStatuses = []int{http.StatusOK}
	}
	if !slices.Contains(wantStatuses, status) || env.Code != 0 {
		t.Fatalf("%s failed: http=%d (want %v) code=%d msg=%q data=%s", op, status, wantStatuses, env.Code, env.Message, string(env.Data))
	}
}

func assertStage6BizErr(t *testing.T, op string, status int, env stage6Envelope, allowedCodes ...int) {
	t.Helper()
	if !slices.Contains(allowedCodes, env.Code) || status < 400 {
		t.Fatalf("%s expected business error in %v with HTTP>=400, got http=%d code=%d msg=%q data=%s",
			op, allowedCodes, status, env.Code, env.Message, string(env.Data))
	}
	if env.Message == "" {
		t.Fatalf("%s error response missing message (code=%d)", op, env.Code)
	}
}

func (r *stage6Rig) registerAndLogin(prefix, nickname string) stage6User {
	r.t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	username := fmt.Sprintf("%s_%s", prefix, suffix)
	password := "Stage6Pass!123"
	deviceID := "dev-" + suffix

	regStatus, _, regEnv, _ := r.doRequest(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"username": username,
		"password": password,
		"nickname": nickname,
	})
	assertStage6OK(r.t, "register "+username, regStatus, regEnv, http.StatusCreated)
	var regData struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	regData = decodeStage6JSON[struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}](r.t, regEnv.Data)
	uid, err := strconv.ParseUint(regData.User.ID, 10, 64)
	if err != nil || uid == 0 {
		r.t.Fatalf("invalid registered user id %q", regData.User.ID)
	}
	r.trackUser(uid)

	loginStatus, _, loginEnv, _ := r.doRequest(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"username":    username,
		"password":    password,
		"device_id":   deviceID,
		"device_type": "web",
	})
	assertStage6OK(r.t, "login "+username, loginStatus, loginEnv, http.StatusOK)
	var loginData struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			SessionID    string `json:"session_id"`
			DeviceID     string `json:"device_id"`
		} `json:"tokens"`
	}
	loginData = decodeStage6JSON[struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			SessionID    string `json:"session_id"`
			DeviceID     string `json:"device_id"`
		} `json:"tokens"`
	}](r.t, loginEnv.Data)
	sid, _ := strconv.ParseUint(loginData.Tokens.SessionID, 10, 64)
	if loginData.Tokens.AccessToken == "" || sid == 0 {
		r.t.Fatalf("login %s returned incomplete tokens: %s", username, string(loginEnv.Data))
	}

	return stage6User{
		ID:           uid,
		IDStr:        regData.User.ID,
		Username:     username,
		Nickname:     nickname,
		Password:     password,
		DeviceID:     deviceID,
		SessionID:    sid,
		AccessToken:  loginData.Tokens.AccessToken,
		RefreshToken: loginData.Tokens.RefreshToken,
	}
}

func (r *stage6Rig) makeFriends(sender, recipient stage6User) (uint64, string, string) {
	r.t.Helper()
	st, _, env, _ := r.doRequest(http.MethodPost, "/api/v1/friend-requests", sender.AccessToken, map[string]string{
		"to_user_id": recipient.IDStr,
		"message":    "stage6 qa friend request",
	})
	assertStage6OK(r.t, "create friend request", st, env, http.StatusCreated)
	var reqOut struct {
		Request struct {
			ID string `json:"id"`
		} `json:"request"`
	}
	reqOut = decodeStage6JSON[struct {
		Request struct {
			ID string `json:"id"`
		} `json:"request"`
	}](r.t, env.Data)
	if reqOut.Request.ID == "" {
		r.t.Fatalf("missing friend request id in %s", string(env.Data))
	}

	accSt, _, accEnv, _ := r.doRequest(http.MethodPost, "/api/v1/friend-requests/"+reqOut.Request.ID+"/accept", recipient.AccessToken, nil)
	assertStage6OK(r.t, "accept friend request", accSt, accEnv, http.StatusOK)
	var accOut struct {
		ConversationID string `json:"conversation_id"`
		SystemMsgID    string `json:"system_msg_id"`
	}
	accOut = decodeStage6JSON[struct {
		ConversationID string `json:"conversation_id"`
		SystemMsgID    string `json:"system_msg_id"`
	}](r.t, accEnv.Data)
	convID, err := strconv.ParseUint(accOut.ConversationID, 10, 64)
	if err != nil || convID == 0 || accOut.SystemMsgID == "" {
		r.t.Fatalf("invalid accept output: %s", string(accEnv.Data))
	}
	r.trackConv(convID)
	return convID, accOut.ConversationID, accOut.SystemMsgID
}

func (r *stage6Rig) uploadTempFile(token, purpose, filename, mimeType string, content []byte) (int, stage6Envelope, stage6AssetDTO, []byte) {
	r.t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if purpose != "" {
		if err := mw.WriteField("purpose", purpose); err != nil {
			r.t.Fatalf("write purpose field: %v", err)
		}
	}
	if filename != "" {
		base := filepath.Base(filename)
		if base == "." || base == ".." || base == string(filepath.Separator) {
			r.t.Fatalf("invalid upload filename %q", filename)
		}
		tmpPath := filepath.Join(r.t.TempDir(), base)
		if err := os.WriteFile(tmpPath, content, 0o600); err != nil {
			r.t.Fatalf("write temp upload file %s: %v", tmpPath, err)
		}

		f, err := os.Open(tmpPath)
		if err != nil {
			r.t.Fatalf("open temp upload file: %v", err)
		}
		defer f.Close()

		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, base))
		if mimeType != "" {
			hdr.Set("Content-Type", mimeType)
		}
		part, err := mw.CreatePart(hdr)
		if err != nil {
			r.t.Fatalf("create multipart file part: %v", err)
		}
		if _, err := io.Copy(part, f); err != nil {
			r.t.Fatalf("copy temp file to multipart: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		r.t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, r.baseURL+"/api/v1/uploads", &body)
	if err != nil {
		r.t.Fatalf("new upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.t.Fatalf("POST /api/v1/uploads: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("read upload response: %v", err)
	}
	var env stage6Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		r.t.Fatalf("upload response (HTTP %d) is not JSON envelope: %v; body=%s", resp.StatusCode, err, string(raw))
	}
	if reqID := resp.Header.Get(errcode.HeaderRequestID); reqID == "" || env.RequestID != reqID {
		r.t.Fatalf("upload X-Request-ID mismatch: header=%q envelope=%q", reqID, env.RequestID)
	}
	var asset stage6AssetDTO
	if env.Code == 0 && len(env.Data) > 0 && string(env.Data) != "null" {
		asset = decodeWrappedOrFlat[stage6AssetDTO](r.t, env.Data, "asset")
		r.trackAsset(asset.AssetID)
	}
	return resp.StatusCode, env, asset, raw
}

func (r *stage6Rig) downloadAuthorized(pathOrURL, token string) (int, http.Header, []byte) {
	r.t.Helper()
	target := pathOrURL
	if strings.HasPrefix(pathOrURL, "/") {
		target = r.baseURL + pathOrURL
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		r.t.Fatalf("new download request %s: %v", target, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("read download %s: %v", target, err)
	}
	return resp.StatusCode, resp.Header, body
}

func (r *stage6Rig) assertAssetAccessible(user stage6User, assetID string, wantBytes []byte, desc string) {
	r.t.Helper()
	metaSt, _, metaEnv, metaRaw := r.doRequest(http.MethodGet, "/api/v1/uploads/"+assetID, user.AccessToken, nil)
	assertStage6OK(r.t, desc+" get asset metadata", metaSt, metaEnv, http.StatusOK)
	assertNoJSONStorageKeyLeak(r.t, desc+" get asset metadata", metaRaw)

	dlSt, _, dlBody := r.downloadAuthorized("/api/v1/uploads/"+assetID+"/download", user.AccessToken)
	if dlSt != http.StatusOK || sha256Hex(dlBody) != sha256Hex(wantBytes) {
		r.t.Fatalf("%s download failed: status=%d sha_got=%s sha_want=%s",
			desc, dlSt, sha256Hex(dlBody), sha256Hex(wantBytes))
	}
}

func (r *stage6Rig) assertAssetDenied(user stage6User, assetID, desc string) {
	r.t.Helper()
	metaSt, _, metaEnv, _ := r.doRequest(http.MethodGet, "/api/v1/uploads/"+assetID, user.AccessToken, nil)
	assertStage6BizErr(r.t, desc+" get asset metadata", metaSt, metaEnv, errcode.ErrAssetAccessDenied.Code)

	dlSt, _, dlBody := r.downloadAuthorized("/api/v1/uploads/"+assetID+"/download", user.AccessToken)
	var dlEnv stage6Envelope
	if err := json.Unmarshal(dlBody, &dlEnv); err != nil {
		r.t.Fatalf("%s download (HTTP %d) did not return JSON error envelope: %v; body=%s",
			desc, dlSt, err, string(dlBody))
	}
	assertStage6BizErr(r.t, desc+" download asset", dlSt, dlEnv, errcode.ErrAssetAccessDenied.Code)
}

func decodeWrappedOrFlat[T any](t *testing.T, data json.RawMessage, wrapperKey string) T {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err == nil {
		if inner, ok := m[wrapperKey]; ok && len(inner) > 0 && inner[0] == '{' {
			return decodeStage6JSON[T](t, inner)
		}
	}
	return decodeStage6JSON[T](t, data)
}

func extractConversationFromDetail(t *testing.T, data json.RawMessage) stage6ConversationDTO {
	t.Helper()
	return decodeWrappedOrFlat[stage6ConversationDTO](t, data, "conversation")
}

func extractGroupCreateData(t *testing.T, data json.RawMessage) (stage6GroupDTO, string) {
	t.Helper()
	grp := decodeWrappedOrFlat[stage6GroupDTO](t, data, "group")
	convID := grp.ConversationID
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err == nil {
		if rawConv, ok := m["conversation"]; ok && len(rawConv) > 0 && rawConv[0] == '{' {
			conv := decodeStage6JSON[stage6ConversationDTO](t, rawConv)
			if conv.ID != "" {
				convID = conv.ID
			}
		}
		if convID == "" {
			if rawID, ok := m["conversation_id"]; ok {
				var s string
				if json.Unmarshal(rawID, &s) == nil && s != "" {
					convID = s
				}
			}
		}
	}
	return grp, convID
}

type stage6WSClient struct {
	t       *testing.T
	conn    *websocket.Conn
	frames  chan stage6WSFrame
	done    chan struct{}
	mu      sync.Mutex
	pending []stage6WSFrame
	readErr error
	closed  bool
}

func (r *stage6Rig) dialWS(user stage6User) *stage6WSClient {
	r.t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: stage6IOTimeout}
	wsURL := r.wsBaseURL + "/ws?access_token=" + url.QueryEscape(user.AccessToken)
	conn, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		var body string
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			body = string(b)
		}
		r.t.Fatalf("dial /ws for %s: %v (body=%s)", user.Username, err, body)
	}
	if resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		r.t.Fatalf("/ws upgrade did not return 101: %v", resp)
	}
	c := &stage6WSClient{
		t:      r.t,
		conn:   conn,
		frames: make(chan stage6WSFrame, 64),
		done:   make(chan struct{}),
	}
	go c.readLoop()
	r.t.Cleanup(c.close)
	c.ping()
	return c
}

func (c *stage6WSClient) readLoop() {
	defer close(c.done)
	for {
		var frame stage6WSFrame
		if err := c.conn.ReadJSON(&frame); err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		c.frames <- frame
	}
}

func (c *stage6WSClient) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.conn.Close()
	<-c.done
}

func (c *stage6WSClient) send(kind, reqID string, data any) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(stage6IOTimeout))
	err := c.conn.WriteJSON(map[string]any{
		"type":      kind,
		"req_id":    reqID,
		"timestamp": time.Now().UnixMilli(),
		"data":      data,
	})
	if err != nil {
		c.t.Fatalf("ws send %s (%s): %v", kind, reqID, err)
	}
}

func (c *stage6WSClient) ping() {
	c.t.Helper()
	reqID := "ping-" + uuid.NewString()
	c.send("ping", reqID, map[string]string{"nonce": reqID})
	frame := c.awaitFrame("pong "+reqID, stage6IOTimeout, func(f stage6WSFrame) bool {
		return f.Type == "pong" && f.ReqID == reqID
	})
	var pong struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(frame.Data, &pong); err != nil || pong.Nonce != reqID {
		c.t.Fatalf("invalid pong frame: %s", string(frame.Data))
	}
}

func (c *stage6WSClient) awaitFrame(desc string, timeout time.Duration, match func(stage6WSFrame) bool) stage6WSFrame {
	c.t.Helper()
	c.mu.Lock()
	for i, f := range c.pending {
		if match(f) {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			c.mu.Unlock()
			return f
		}
	}
	c.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case f := <-c.frames:
			if f.Timestamp < 1_000_000_000_000 || len(f.Data) == 0 || f.Data[0] != '{' {
				c.t.Fatalf("received invalid WS envelope %+v", f)
			}
			if match(f) {
				return f
			}
			c.mu.Lock()
			c.pending = append(c.pending, f)
			c.mu.Unlock()
		case <-c.done:
			// Drain any buffered frames emitted right before close.
			for {
				select {
				case f := <-c.frames:
					if match(f) {
						return f
					}
					c.mu.Lock()
					c.pending = append(c.pending, f)
					c.mu.Unlock()
				default:
					c.mu.Lock()
					err := c.readErr
					c.mu.Unlock()
					c.t.Fatalf("ws closed while waiting for %s: %v", desc, err)
					return stage6WSFrame{}
				}
			}
		case <-timer.C:
			c.t.Fatalf("timed out waiting %s for %s", timeout, desc)
			return stage6WSFrame{}
		}
	}
}

func (c *stage6WSClient) expectNoFrame(desc string, wait time.Duration, match func(stage6WSFrame) bool) {
	c.t.Helper()
	c.mu.Lock()
	for _, f := range c.pending {
		if match(f) {
			c.mu.Unlock()
			c.t.Fatalf("unexpected buffered WS frame for %s: %s %s", desc, f.Type, string(f.Data))
		}
	}
	c.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case f := <-c.frames:
			if match(f) {
				c.t.Fatalf("unexpected WS frame for %s: %s %s", desc, f.Type, string(f.Data))
			}
			c.mu.Lock()
			c.pending = append(c.pending, f)
			c.mu.Unlock()
		case <-c.done:
			return
		case <-timer.C:
			return
		}
	}
}

func (c *stage6WSClient) sendMsgAndAwaitAck(convID, clientMsgID, msgType, content string, extra map[string]any) stage6WSFrame {
	c.t.Helper()
	if extra == nil {
		extra = map[string]any{}
	}
	reqID := "send-" + uuid.NewString()
	c.send("msg.send", reqID, map[string]any{
		"conversation_id": convID,
		"client_msg_id":   clientMsgID,
		"msg_type":        msgType,
		"content":         content,
		"extra":           extra,
	})
	return c.awaitFrame("msg.ack or error for "+reqID, stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type == "msg.ack" && f.ReqID == reqID {
			return true
		}
		if f.Type == "error" && f.ReqID == reqID {
			return true
		}
		return false
	})
}

func decodeMsgPushMessage(t *testing.T, frame stage6WSFrame) (string, stage6MessageDTO) {
	t.Helper()
	var payload struct {
		PushType       string           `json:"push_type"`
		DeliveryStatus string           `json:"delivery_status"`
		Message        stage6MessageDTO `json:"message"`
	}
	payload = decodeStage6JSON[struct {
		PushType       string           `json:"push_type"`
		DeliveryStatus string           `json:"delivery_status"`
		Message        stage6MessageDTO `json:"message"`
	}](t, frame.Data)
	return payload.PushType, payload.Message
}

func decodeWSError(t *testing.T, frame stage6WSFrame) stage6WSErrorData {
	t.Helper()
	return decodeStage6JSON[stage6WSErrorData](t, frame.Data)
}

func makeStage6SamplePNG(width, height int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x * 255) / max(1, width)),
				G: uint8((y * 255) / max(1, height)),
				B: 160,
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func makeStage6SamplePDF() []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	off1 := body.Len()
	body.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	off2 := body.Len()
	body.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	off3 := body.Len()
	body.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n")
	xrefOff := body.Len()
	body.WriteString("xref\n0 4\n0000000000 65535 f \n")
	fmt.Fprintf(&body, "%010d 00000 n \n", off1)
	fmt.Fprintf(&body, "%010d 00000 n \n", off2)
	fmt.Fprintf(&body, "%010d 00000 n \n", off3)
	fmt.Fprintf(&body, "trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOff)
	return body.Bytes()
}

func makeStage6SampleWAV(durationSeconds int) []byte {
	const (
		sampleRate    = 16000
		numChannels   = 1
		bitsPerSample = 16
	)
	numSamples := sampleRate * durationSeconds
	dataSize := numSamples * numChannels * (bitsPerSample / 8)
	buf := make([]byte, 44+dataSize)

	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataSize))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], numChannels)
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], sampleRate*numChannels*(bitsPerSample/8))
	binary.LittleEndian.PutUint16(buf[32:34], numChannels*(bitsPerSample/8))
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataSize))

	for i := 0; i < numSamples; i++ {
		sample := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(buf[44+i*2:44+(i+1)*2], uint16(sample))
	}
	return buf
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

func mustParseUint64(t *testing.T, raw string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		t.Fatalf("parse uint64 %q: %v", raw, err)
	}
	return v
}

func verifySeqStrictlyIncreasing(t *testing.T, items []stage6MessageDTO) {
	t.Helper()
	var prev uint64
	for i, item := range items {
		seq := item.seqUint64(t)
		if i > 0 && seq <= prev {
			t.Fatalf("expected strictly increasing seq, got prev=%d then items[%d].seq=%d", prev, i, seq)
		}
		prev = seq
	}
}

func countMessagesByClientID(ctx context.Context, db *gorm.DB, senderID uint64, clientMsgID string) (int64, error) {
	var count int64
	err := db.WithContext(ctx).Model(&model.Message{}).
		Where("sender_id = ? AND client_msg_id = ?", senderID, clientMsgID).
		Count(&count).Error
	return count, err
}

func findMessageInPage(items []stage6MessageDTO, msgID string) (stage6MessageDTO, bool) {
	for _, m := range items {
		if m.MsgID == msgID {
			return m, true
		}
	}
	return stage6MessageDTO{}, false
}

func expectDBAvailable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if db == nil {
		t.Fatal("expected live MySQL handle in Stage 6 test rig")
	}
}

func hasNoInternalStorageLeak(raw []byte) bool {
	s := string(raw)
	return !strings.Contains(s, "storage_key") && !strings.Contains(s, "thumbnail_key")
}

func assertNoJSONStorageKeyLeak(t *testing.T, op string, raw []byte) {
	t.Helper()
	if !hasNoInternalStorageLeak(raw) {
		t.Fatalf("%s leaked internal storage_key or thumbnail_key: %s", op, string(raw))
	}
}

func isValidRFC3339Millis(raw string) bool {
	if raw == "" {
		return false
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", raw); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339Nano, raw)
	return err == nil
}

func decodeJSONInto(raw []byte, dst any) error {
	return json.Unmarshal(raw, dst)
}

func mergeStage6Env(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		k, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replace := overrides[k]; replace {
				continue
			}
		}
		out = append(out, entry)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func TestStage6UploadTempFileOmitsFileFieldWhenFilenameEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}
		if r.FormValue("purpose") != model.AssetPurposeImage {
			t.Fatalf("expected purpose=%q, got %q", model.AssetPurposeImage, r.FormValue("purpose"))
		}
		if files := r.MultipartForm.File["file"]; len(files) != 0 {
			t.Fatalf("expected missing file field when filename is empty, got %d files", len(files))
		}
		const reqID = "r-stage6-empty-file"
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(errcode.HeaderRequestID, reqID)
		w.WriteHeader(errcode.ErrUploadInvalidFile.HTTPStatus)
		_ = json.NewEncoder(w).Encode(errcode.Envelope{
			Code:      errcode.ErrUploadInvalidFile.Code,
			Message:   errcode.ErrUploadInvalidFile.Message,
			RequestID: reqID,
		})
	}))
	defer srv.Close()

	rig := &stage6Rig{t: t, baseURL: srv.URL, httpClient: srv.Client()}
	st, env, _, _ := rig.uploadTempFile("test-token", model.AssetPurposeImage, "", "", nil)
	assertStage6BizErr(t, "empty filename omits file field", st, env, errcode.ErrUploadInvalidFile.Code)
}
