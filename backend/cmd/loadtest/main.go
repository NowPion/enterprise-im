package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type Config struct {
	BaseURL            string
	WSURL              string
	Clients            int
	GroupSize          int
	SingleMsgsPerUser  int
	GroupMsgsPerUser   int
	SendInterval       time.Duration
	SendStagger        time.Duration
	HeartbeatInterval  time.Duration
	HoldDuration       time.Duration
	SetupConcurrency   int
	DialConcurrency    int
	RequestTimeout     time.Duration
	AckTimeout         time.Duration
	SettleTimeout      time.Duration
	IdempotencySamples int
	Prefix             string
	AdminUsername      string
	AdminPassword      string
	UserPassword       string
	ReportOut          string
}

func loadConfig() Config {
	cfg := Config{
		BaseURL:            envOrDefault("LOADTEST_BASE_URL", "http://127.0.0.1:8081"),
		WSURL:              strings.TrimSpace(os.Getenv("LOADTEST_WS_URL")),
		Clients:            envInt("LOADTEST_CLIENTS", 1000),
		GroupSize:          envInt("LOADTEST_GROUP_SIZE", 20),
		SingleMsgsPerUser:  envInt("LOADTEST_SINGLE_MSGS", 2),
		GroupMsgsPerUser:   envInt("LOADTEST_GROUP_MSGS", 2),
		SendInterval:       envDuration("LOADTEST_SEND_INTERVAL", 100*time.Millisecond),
		SendStagger:        envDuration("LOADTEST_SEND_STAGGER", 2*time.Second),
		HeartbeatInterval:  envDuration("LOADTEST_HEARTBEAT_INTERVAL", 10*time.Second),
		HoldDuration:       envDuration("LOADTEST_HOLD_DURATION", 11*time.Second),
		SetupConcurrency:   envInt("LOADTEST_SETUP_CONCURRENCY", 24),
		DialConcurrency:    envInt("LOADTEST_DIAL_CONCURRENCY", 64),
		RequestTimeout:     envDuration("LOADTEST_REQUEST_TIMEOUT", 15*time.Second),
		AckTimeout:         envDuration("LOADTEST_ACK_TIMEOUT", 15*time.Second),
		SettleTimeout:      envDuration("LOADTEST_SETTLE_TIMEOUT", 45*time.Second),
		IdempotencySamples: envInt("LOADTEST_IDEMPOTENCY_SAMPLES", 50),
		Prefix:             strings.TrimSpace(os.Getenv("LOADTEST_PREFIX")),
		AdminUsername:      envOrDefault("LOADTEST_ADMIN_USERNAME", "admin"),
		AdminPassword:      envOrDefault("LOADTEST_ADMIN_PASSWORD", "Admin@123456"),
		UserPassword:       envOrDefault("LOADTEST_USER_PASSWORD", "LoadTest@123456"),
		ReportOut:          strings.TrimSpace(os.Getenv("LOADTEST_REPORT_OUT")),
	}

	flag.StringVar(&cfg.BaseURL, "base-url", cfg.BaseURL, "Target backend HTTP base URL")
	flag.StringVar(&cfg.WSURL, "ws-url", cfg.WSURL, "Optional explicit WS URL (defaults to base-url + /ws)")
	flag.IntVar(&cfg.Clients, "clients", cfg.Clients, "Number of concurrent WebSocket clients")
	flag.IntVar(&cfg.GroupSize, "group-size", cfg.GroupSize, "Members per group chat")
	flag.IntVar(&cfg.SingleMsgsPerUser, "single-msgs", cfg.SingleMsgsPerUser, "Single-chat messages sent per user")
	flag.IntVar(&cfg.GroupMsgsPerUser, "group-msgs", cfg.GroupMsgsPerUser, "Group-chat messages sent per user")
	flag.DurationVar(&cfg.SendInterval, "send-interval", cfg.SendInterval, "Delay between messages sent by the same client")
	flag.DurationVar(&cfg.SendStagger, "send-stagger", cfg.SendStagger, "Random initial stagger window before each client starts sending")
	flag.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", cfg.HeartbeatInterval, "WebSocket ping interval")
	flag.DurationVar(&cfg.HoldDuration, "hold-duration", cfg.HoldDuration, "Minimum connected duration to exercise heartbeat keepalive")
	flag.IntVar(&cfg.SetupConcurrency, "setup-concurrency", cfg.SetupConcurrency, "Worker concurrency for HTTP registration/login/setup")
	flag.IntVar(&cfg.DialConcurrency, "dial-concurrency", cfg.DialConcurrency, "Worker concurrency for WebSocket dialing")
	flag.DurationVar(&cfg.RequestTimeout, "request-timeout", cfg.RequestTimeout, "HTTP request timeout")
	flag.DurationVar(&cfg.AckTimeout, "ack-timeout", cfg.AckTimeout, "Per-message msg.ack timeout")
	flag.DurationVar(&cfg.SettleTimeout, "settle-timeout", cfg.SettleTimeout, "Max wait for msg.push delivery and disconnect zeroing")
	flag.IntVar(&cfg.IdempotencySamples, "idempotency-samples", cfg.IdempotencySamples, "Number of already-acked messages to resend for idempotency verification")
	flag.StringVar(&cfg.Prefix, "prefix", cfg.Prefix, "Username prefix for created test accounts (auto-generated if empty)")
	flag.StringVar(&cfg.AdminUsername, "admin-user", cfg.AdminUsername, "Admin username for /api/v1/admin/metrics/*")
	flag.StringVar(&cfg.AdminPassword, "admin-pass", cfg.AdminPassword, "Admin password (prefer LOADTEST_ADMIN_PASSWORD env)")
	flag.StringVar(&cfg.UserPassword, "user-pass", cfg.UserPassword, "Test user password (prefer LOADTEST_USER_PASSWORD env)")
	flag.StringVar(&cfg.ReportOut, "report-out", cfg.ReportOut, "Optional path to write markdown metrics summary")
	flag.Parse()

	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.WSURL == "" {
		cfg.WSURL = deriveWSURL(cfg.BaseURL)
	}
	if cfg.Prefix == "" {
		cfg.Prefix = fmt.Sprintf("lt_%d_%s", time.Now().Unix(), shortRandHex(3))
	}
	return cfg
}

func validateConfig(cfg Config) error {
	if cfg.BaseURL == "" || cfg.WSURL == "" {
		return fmt.Errorf("base-url and ws-url must not be empty")
	}
	if cfg.Clients < 2 {
		return fmt.Errorf("clients must be >= 2 (got %d)", cfg.Clients)
	}
	if cfg.GroupSize < 2 {
		return fmt.Errorf("group-size must be >= 2 (got %d)", cfg.GroupSize)
	}
	if cfg.SingleMsgsPerUser < 0 || cfg.GroupMsgsPerUser < 0 || (cfg.SingleMsgsPerUser == 0 && cfg.GroupMsgsPerUser == 0) {
		return fmt.Errorf("single-msgs and group-msgs must be >= 0 and at least one > 0")
	}
	if cfg.SetupConcurrency < 1 {
		cfg.SetupConcurrency = 1
	}
	if cfg.DialConcurrency < 1 {
		cfg.DialConcurrency = 1
	}
	return nil
}

type TestUser struct {
	Index       int
	Username    string
	Nickname    string
	DeviceID    string
	UserID      uint64
	UserIDStr   string
	SessionID   string
	AccessToken string
	SingleConv  *ConversationInfo
	GroupConv   *ConversationInfo
	WS          *WSConn
}

type ConversationInfo struct {
	ConvID    uint64
	ConvIDStr string
	Type      string
	Members   []uint64
}

type SentMessageRecord struct {
	SenderIndex int
	SenderID    uint64
	ConvID      uint64
	ConvIDStr   string
	ConvType    string
	ClientMsgID string
	MsgID       uint64
	Seq         uint64
	Content     string
	Members     []uint64
}

type LoadTestReport struct {
	StartedAt   time.Time
	FinishedAt  time.Time
	Config      Config
	HealthOK    bool
	Baseline    AdminMetricsOverview
	PeakMetrics AdminMetricsOverview
	PostSend    AdminMetricsOverview
	FinalMetric AdminMetricsOverview
	StreamRows  []AdminStreamRow

	SetupRegisterDur    time.Duration
	SetupLoginDur       time.Duration
	SetupTopologyDur    time.Duration
	WSDialDur           time.Duration
	MessagingDur        time.Duration
	DeliveryDrainDur    time.Duration
	SettleDur           time.Duration
	DisconnectDur       time.Duration
	DisconnectConverge  time.Duration
	TotalDur            time.Duration

	RegisteredUsers     int
	LoggedInUsers       int
	SetupHTTP19004Count int64
	SingleConversations int
	GroupConversations  int
	WSConnectedCount    int
	WSDisconnectedCount int
	WSUnexpectedCloses  int64

	Summary             TrackerSummary
	IdempotencyChecked  int
	IdempotencyPassed   int
	IdempotencyFailed   int
	Assertions          []AssertionResult
	AllPassed           bool
}

type AssertionResult struct {
	Name   string
	Passed bool
	Detail string
}

func main() {
	cfg := loadConfig()
	if err := validateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid loadtest config: %v\n", err)
		os.Exit(2)
	}

	ctx := context.Background()
	report, err := runLoadTest(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadtest execution error: %v\n", err)
		os.Exit(1)
	}

	md := formatReportMarkdown(report)
	fmt.Print(md)

	if cfg.ReportOut != "" {
		if writeErr := os.WriteFile(cfg.ReportOut, []byte(md), 0o644); writeErr != nil {
			fmt.Fprintf(os.Stderr, "failed to write report to %s: %v\n", cfg.ReportOut, writeErr)
			os.Exit(1)
		}
	}

	if !report.AllPassed {
		os.Exit(1)
	}
}

func runLoadTest(ctx context.Context, cfg Config) (*LoadTestReport, error) {
	rep := &LoadTestReport{
		StartedAt: time.Now().UTC(),
		Config:    cfg,
	}

	api := NewAPIClient(cfg.BaseURL, cfg.RequestTimeout)

	fmt.Printf("[loadtest] target=%s ws=%s clients=%d group_size=%d single_msgs=%d group_msgs=%d prefix=%s\n",
		cfg.BaseURL, cfg.WSURL, cfg.Clients, cfg.GroupSize, cfg.SingleMsgsPerUser, cfg.GroupMsgsPerUser, cfg.Prefix)

	// Phase 0: Health check and admin authentication.
	if err := api.Healthz(ctx); err != nil {
		return nil, fmt.Errorf("healthz check failed: %w", err)
	}
	rep.HealthOK = true

	adminLogin, err := api.Login(ctx, cfg.AdminUsername, cfg.AdminPassword, "lt-admin-"+cfg.Prefix, "web")
	if err != nil {
		return nil, fmt.Errorf("admin login failed: %w", err)
	}
	adminToken := adminLogin.Tokens.AccessToken

	baseline, err := api.AdminMetricsOverview(ctx, adminToken)
	if err != nil {
		return nil, fmt.Errorf("baseline admin metrics overview failed: %w", err)
	}
	rep.Baseline = baseline
	fmt.Printf("[loadtest] phase 0 ok: healthz=ok baseline online_users=%d ws_connections=%d\n",
		baseline.OnlineUsers, baseline.WSConnections)

	// Phase 1: Register and login N distinct users via real HTTP APIs.
	users := make([]*TestUser, cfg.Clients)
	for i := 0; i < cfg.Clients; i++ {
		users[i] = &TestUser{
			Index:    i,
			Username: fmt.Sprintf("%s_u%04d", cfg.Prefix, i),
			Nickname: fmt.Sprintf("LTUser%04d", i),
			DeviceID: fmt.Sprintf("lt-dev-%s-%04d", cfg.Prefix, i),
		}
	}

	tReg := time.Now()
	if err := runPool(ctx, cfg.Clients, cfg.SetupConcurrency, func(idx int) error {
		u := users[idx]
		regOut, regErr := api.Register(ctx, u.Username, cfg.UserPassword, u.Nickname)
		if regErr != nil {
			return fmt.Errorf("register user %s: %w", u.Username, regErr)
		}
		uid, parseErr := strconv.ParseUint(regOut.User.ID, 10, 64)
		if parseErr != nil || uid == 0 {
			return fmt.Errorf("invalid registered user id %q", regOut.User.ID)
		}
		u.UserID = uid
		u.UserIDStr = regOut.User.ID
		return nil
	}); err != nil {
		return nil, err
	}
	rep.RegisteredUsers = cfg.Clients
	rep.SetupRegisterDur = time.Since(tReg)

	tLogin := time.Now()
	var setup19004Count atomic.Int64
	if err := runPool(ctx, cfg.Clients, cfg.SetupConcurrency, func(idx int) error {
		u := users[idx]
		var loginOut LoginData
		err := retryOn19004(&setup19004Count, func() error {
			var e error
			loginOut, e = api.Login(ctx, u.Username, cfg.UserPassword, u.DeviceID, "web")
			return e
		})
		if err != nil {
			return fmt.Errorf("login user %s: %w", u.Username, err)
		}
		u.AccessToken = loginOut.Tokens.AccessToken
		u.SessionID = loginOut.Tokens.SessionID
		return nil
	}); err != nil {
		return nil, err
	}
	rep.LoggedInUsers = cfg.Clients
	rep.SetupLoginDur = time.Since(tLogin)
	fmt.Printf("[loadtest] phase 1 ok: registered=%d (%s) logged_in=%d (%s) setup_19004_retries=%d\n",
		rep.RegisteredUsers, rep.SetupRegisterDur.Round(time.Millisecond),
		rep.LoggedInUsers, rep.SetupLoginDur.Round(time.Millisecond), setup19004Count.Load())

	// Phase 2: Create single-chat pairs (friendships) and group chats via real HTTP APIs.
	tTopo := time.Now()
	pairCount := cfg.Clients / 2
	singleConvs := make([]*ConversationInfo, pairCount)
	if err := runPool(ctx, pairCount, cfg.SetupConcurrency, func(pairIdx int) error {
		uA := users[pairIdx*2]
		uB := users[pairIdx*2+1]
		var reqID string
		if reqErr := retryOn19004(&setup19004Count, func() error {
			var e error
			reqID, e = api.SendFriendRequest(ctx, uA.AccessToken, uB.UserIDStr, "loadtest-pair")
			return e
		}); reqErr != nil {
			return fmt.Errorf("friend request %s->%s: %w", uA.Username, uB.Username, reqErr)
		}
		var convIDStr string
		if accErr := retryOn19004(&setup19004Count, func() error {
			var e error
			convIDStr, e = api.AcceptFriendRequest(ctx, uB.AccessToken, reqID)
			return e
		}); accErr != nil {
			return fmt.Errorf("accept friend request %s->%s: %w", uB.Username, uA.Username, accErr)
		}
		convID, parseErr := strconv.ParseUint(convIDStr, 10, 64)
		if parseErr != nil || convID == 0 {
			return fmt.Errorf("invalid single conversation id %q", convIDStr)
		}
		conv := &ConversationInfo{
			ConvID:    convID,
			ConvIDStr: convIDStr,
			Type:      "single",
			Members:   []uint64{uA.UserID, uB.UserID},
		}
		singleConvs[pairIdx] = conv
		uA.SingleConv = conv
		uB.SingleConv = conv
		return nil
	}); err != nil {
		return nil, err
	}
	rep.SingleConversations = pairCount

	// Partition users into groups of cfg.GroupSize (folding a remainder < 2 into the previous group).
	type groupSpec struct {
		start int
		end   int
	}
	var groupSpecs []groupSpec
	gSize := cfg.GroupSize
	if gSize > cfg.Clients {
		gSize = cfg.Clients
	}
	for start := 0; start < cfg.Clients; start += gSize {
		end := start + gSize
		if end > cfg.Clients {
			end = cfg.Clients
		}
		if end-start < 2 && len(groupSpecs) > 0 {
			groupSpecs[len(groupSpecs)-1].end = end
			break
		}
		groupSpecs = append(groupSpecs, groupSpec{start: start, end: end})
	}
	groupConvs := make([]*ConversationInfo, len(groupSpecs))
	if err := runPool(ctx, len(groupSpecs), cfg.SetupConcurrency, func(gIdx int) error {
		spec := groupSpecs[gIdx]
		owner := users[spec.start]
		memberIDStrs := make([]string, 0, spec.end-spec.start-1)
		memberIDs := make([]uint64, 0, spec.end-spec.start)
		memberIDs = append(memberIDs, owner.UserID)
		for idx := spec.start + 1; idx < spec.end; idx++ {
			memberIDStrs = append(memberIDStrs, users[idx].UserIDStr)
			memberIDs = append(memberIDs, users[idx].UserID)
		}
		groupName := fmt.Sprintf("LTGroup_%s_%03d", cfg.Prefix, gIdx)
		var convIDStr string
		if grpErr := retryOn19004(&setup19004Count, func() error {
			var e error
			convIDStr, e = api.CreateGroup(ctx, owner.AccessToken, groupName, memberIDStrs)
			return e
		}); grpErr != nil {
			return fmt.Errorf("create group %s: %w", groupName, grpErr)
		}
		convID, parseErr := strconv.ParseUint(convIDStr, 10, 64)
		if parseErr != nil || convID == 0 {
			return fmt.Errorf("invalid group conversation id %q", convIDStr)
		}
		conv := &ConversationInfo{
			ConvID:    convID,
			ConvIDStr: convIDStr,
			Type:      "group",
			Members:   memberIDs,
		}
		groupConvs[gIdx] = conv
		for idx := spec.start; idx < spec.end; idx++ {
			users[idx].GroupConv = conv
		}
		return nil
	}); err != nil {
		return nil, err
	}
	rep.GroupConversations = len(groupConvs)
	rep.SetupHTTP19004Count = setup19004Count.Load()
	rep.SetupTopologyDur = time.Since(tTopo)
	fmt.Printf("[loadtest] phase 2 ok: single_convs=%d group_convs=%d (%s) setup_19004_retries=%d\n",
		rep.SingleConversations, rep.GroupConversations, rep.SetupTopologyDur.Round(time.Millisecond), rep.SetupHTTP19004Count)

	// Phase 3: Dial all N WS clients simultaneously (bounded dial semaphore) and start heartbeats.
	tracker := NewTracker()
	tDial := time.Now()
	var unexpectedCloses atomic.Int64
	if err := runPool(ctx, cfg.Clients, cfg.DialConcurrency, func(idx int) error {
		u := users[idx]
		wsConn, dialDur, dialErr := DialWS(ctx, cfg.WSURL, u.UserID, u.AccessToken, tracker, func() {
			unexpectedCloses.Add(1)
		})
		if dialErr != nil {
			return fmt.Errorf("dial ws user %s: %w", u.Username, dialErr)
		}
		tracker.RecordConnectLatency(dialDur)
		// Perform initial ping/pong auth probe aligned with frontend WSClient.
		if pingErr := wsConn.Ping(ctx, cfg.AckTimeout); pingErr != nil {
			_ = wsConn.CloseGracefully(2 * time.Second)
			return fmt.Errorf("initial ping user %s: %w", u.Username, pingErr)
		}
		u.WS = wsConn
		return nil
	}); err != nil {
		closeAllWS(users)
		return nil, err
	}
	rep.WSDialDur = time.Since(tDial)
	rep.WSConnectedCount = cfg.Clients

	// Start periodic heartbeat loops for all connected clients.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	var hbWG sync.WaitGroup
	for _, u := range users {
		user := u
		hbWG.Add(1)
		go func() {
			defer hbWG.Done()
			// Stagger initial heartbeat tick across clients to avoid synchronized burst.
			jitter := randDuration(cfg.HeartbeatInterval / 2)
			select {
			case <-hbCtx.Done():
				return
			case <-time.After(jitter):
			}
			ticker := time.NewTicker(cfg.HeartbeatInterval)
			defer ticker.Stop()
			for {
				if err := user.WS.Ping(hbCtx, cfg.AckTimeout); err != nil && hbCtx.Err() == nil {
					tracker.RecordHeartbeatError()
				}
				select {
				case <-hbCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	// Verify peak online metrics via admin endpoint.
	peakMetrics, err := pollMetrics(ctx, api, adminToken, 5*time.Second, func(m AdminMetricsOverview) bool {
		return m.OnlineUsers == int64(cfg.Clients) && m.WSConnections == int64(cfg.Clients)
	})
	if err != nil {
		// Still capture latest snapshot even if target wasn't reached.
		if latest, mErr := api.AdminMetricsOverview(ctx, adminToken); mErr == nil {
			peakMetrics = latest
		}
	}
	rep.PeakMetrics = peakMetrics
	fmt.Printf("[loadtest] phase 3 ok: ws_connected=%d (%s) peak online_users=%d ws_connections=%d\n",
		rep.WSConnectedCount, rep.WSDialDur.Round(time.Millisecond),
		peakMetrics.OnlineUsers, peakMetrics.WSConnections)

	// Phase 4: Multi-user concurrent messaging across single and group chats.
	holdStarted := time.Now()
	tMsg := time.Now()
	var sentMu sync.Mutex
	sentRecords := make([]SentMessageRecord, 0, cfg.Clients*(cfg.SingleMsgsPerUser+cfg.GroupMsgsPerUser))

	sendErr := runPool(ctx, cfg.Clients, cfg.Clients, func(idx int) error {
		u := users[idx]
		if cfg.SendStagger > 0 {
			time.Sleep(randDuration(cfg.SendStagger))
		}

		type sendTask struct {
			conv *ConversationInfo
			seqN int
		}
		tasks := make([]sendTask, 0, cfg.SingleMsgsPerUser+cfg.GroupMsgsPerUser)
		maxRounds := max(cfg.SingleMsgsPerUser, cfg.GroupMsgsPerUser)
		for r := 0; r < maxRounds; r++ {
			if r < cfg.SingleMsgsPerUser && u.SingleConv != nil {
				tasks = append(tasks, sendTask{conv: u.SingleConv, seqN: r + 1})
			}
			if r < cfg.GroupMsgsPerUser && u.GroupConv != nil {
				tasks = append(tasks, sendTask{conv: u.GroupConv, seqN: r + 1})
			}
		}

		localSent := make([]SentMessageRecord, 0, len(tasks))
		for tIdx, task := range tasks {
			if tIdx > 0 && cfg.SendInterval > 0 {
				time.Sleep(cfg.SendInterval)
			}
			clientMsgID := uuid.NewString()
			content := fmt.Sprintf("lt|%s|%s|%s|%d|%s", cfg.Prefix, task.conv.Type, u.Username, tIdx+1, clientMsgID[:8])
			ack, retries, err := sendWithRetry(ctx, u.WS, tracker, task.conv.ConvIDStr, clientMsgID, content, cfg.AckTimeout)
			if retries > 0 {
				tracker.AddAckRetries(retries)
			}
			if err != nil {
				tracker.RecordAckFailure(fmt.Sprintf("user=%s conv=%s client_msg_id=%s err=%v", u.Username, task.conv.ConvIDStr, clientMsgID, err))
				continue
			}
			msgID, err1 := strconv.ParseUint(ack.MsgID, 10, 64)
			seq, err2 := strconv.ParseUint(ack.Seq, 10, 64)
			if err1 != nil || err2 != nil || msgID == 0 || seq == 0 {
				tracker.RecordAckFailure(fmt.Sprintf("invalid ack ids msg_id=%q seq=%q", ack.MsgID, ack.Seq))
				continue
			}
			tracker.RegisterExpectedMessage(ExpectedMessage{
				MsgID:       msgID,
				ConvID:      task.conv.ConvID,
				Seq:         seq,
				SenderID:    u.UserID,
				ClientMsgID: clientMsgID,
				Recipients:  task.conv.Members,
				ConvType:    task.conv.Type,
			})
			localSent = append(localSent, SentMessageRecord{
				SenderIndex: u.Index,
				SenderID:    u.UserID,
				ConvID:      task.conv.ConvID,
				ConvIDStr:   task.conv.ConvIDStr,
				ConvType:    task.conv.Type,
				ClientMsgID: clientMsgID,
				MsgID:       msgID,
				Seq:         seq,
				Content:     content,
				Members:     task.conv.Members,
			})
		}

		if len(localSent) > 0 {
			sentMu.Lock()
			sentRecords = append(sentRecords, localSent...)
			sentMu.Unlock()
		}
		return nil
	})
	if sendErr != nil {
		hbCancel()
		hbWG.Wait()
		closeAllWS(users)
		return nil, sendErr
	}
	rep.MessagingDur = time.Since(tMsg)

	// Wait for all expected msg.push deliveries to arrive across all clients.
	tSettle := time.Now()
	settled := tracker.WaitForExpectedDeliveries(ctx, cfg.SettleTimeout)
	rep.DeliveryDrainDur = time.Since(tMsg)
	// Also keep connections alive until HoldDuration has elapsed so every client exercises >=1 periodic heartbeat.
	if elapsed := time.Since(holdStarted); elapsed < cfg.HoldDuration {
		time.Sleep(cfg.HoldDuration - elapsed)
	}
	// Extra quiet window to catch any late/duplicate msg.push frames.
	time.Sleep(750 * time.Millisecond)
	rep.SettleDur = time.Since(tSettle)
	fmt.Printf("[loadtest] phase 4 ok: sent_records=%d settled=%v messaging_dur=%s delivery_drain=%s settle_dur=%s\n",
		len(sentRecords), settled, rep.MessagingDur.Round(time.Millisecond), rep.DeliveryDrainDur.Round(time.Millisecond), rep.SettleDur.Round(time.Millisecond))

	// Phase 4b: Idempotent retry verification (resend sample of already-acked client_msg_id).
	sampleCount := min(cfg.IdempotencySamples, len(sentRecords))
	if sampleCount > 0 {
		rep.IdempotencyChecked = sampleCount
		beforeDup := tracker.DuplicateCount()
		var idemPass, idemFail atomic.Int64
		_ = runPool(ctx, sampleCount, min(32, sampleCount), func(i int) error {
			rec := sentRecords[(i*len(sentRecords))/sampleCount]
			u := users[rec.SenderIndex]
			ack, _, err := sendWithRetry(ctx, u.WS, tracker, rec.ConvIDStr, rec.ClientMsgID, rec.Content, cfg.AckTimeout)
			if err != nil {
				idemFail.Add(1)
				return nil
			}
			if ack.MsgID == strconv.FormatUint(rec.MsgID, 10) && ack.Seq == strconv.FormatUint(rec.Seq, 10) {
				idemPass.Add(1)
			} else {
				idemFail.Add(1)
			}
			return nil
		})
		time.Sleep(500 * time.Millisecond)
		afterDup := tracker.DuplicateCount()
		rep.IdempotencyFailed = int(idemFail.Load()) + (afterDup - beforeDup)
		rep.IdempotencyPassed = int(idemPass.Load())
		fmt.Printf("[loadtest] idempotency probes: checked=%d passed=%d failed=%d\n",
			rep.IdempotencyChecked, rep.IdempotencyPassed, rep.IdempotencyFailed)
	}

	// Capture post-send metrics snapshot before disconnecting.
	if m, mErr := api.AdminMetricsOverview(ctx, adminToken); mErr == nil {
		rep.PostSend = m
	}
	if sRows, sErr := api.AdminMetricsStreams(ctx, adminToken); sErr == nil {
		rep.StreamRows = sRows
	}

	// Phase 5: Stop heartbeats and gracefully disconnect all N WS clients.
	hbCancel()
	hbWG.Wait()
	// Mark expected disconnect before sending Close frames so clean closes aren't counted as unexpected.
	for _, u := range users {
		if u.WS != nil {
			u.WS.MarkExpectedClose()
		}
	}
	tDisc := time.Now()
	var discCount atomic.Int64
	_ = runPool(ctx, cfg.Clients, cfg.DialConcurrency, func(idx int) error {
		u := users[idx]
		if u.WS != nil {
			_ = u.WS.CloseGracefully(3 * time.Second)
			discCount.Add(1)
		}
		return nil
	})
	rep.WSDisconnectedCount = int(discCount.Load())
	rep.DisconnectDur = time.Since(tDisc)
	rep.WSUnexpectedCloses = unexpectedCloses.Load()

	// Wait for server Hub + PresenceRepository cleanup to converge online_users & ws_connections to 0.
	tConv := time.Now()
	finalMetric, convErr := pollMetrics(ctx, api, adminToken, cfg.SettleTimeout, func(m AdminMetricsOverview) bool {
		return m.OnlineUsers == 0 && m.WSConnections == 0
	})
	rep.DisconnectConverge = time.Since(tConv)
	if convErr != nil {
		if latest, mErr := api.AdminMetricsOverview(ctx, adminToken); mErr == nil {
			finalMetric = latest
		}
	} else {
		// Re-verify stability after 500ms.
		time.Sleep(500 * time.Millisecond)
		if latest, mErr := api.AdminMetricsOverview(ctx, adminToken); mErr == nil {
			finalMetric = latest
		}
	}
	rep.FinalMetric = finalMetric

	// Refresh stream metrics after settle/disconnect.
	if sRows, sErr := api.AdminMetricsStreams(ctx, adminToken); sErr == nil {
		rep.StreamRows = sRows
	}

	rep.Summary = tracker.Summarize()
	rep.FinishedAt = time.Now().UTC()
	rep.TotalDur = rep.FinishedAt.Sub(rep.StartedAt)

	rep.Assertions = evaluateAssertions(rep)
	rep.AllPassed = true
	for _, a := range rep.Assertions {
		if !a.Passed {
			rep.AllPassed = false
		}
	}

	return rep, nil
}

func retryOn19004(counter *atomic.Int64, fn func() error) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == 19004 {
			if counter != nil {
				counter.Add(1)
			}
			time.Sleep(time.Duration(10+attempt*10)*time.Millisecond + randDuration(30*time.Millisecond))
			continue
		}
		return err
	}
	return err
}

func sendWithRetry(
	ctx context.Context,
	ws *WSConn,
	tracker *Tracker,
	convIDStr, clientMsgID, content string,
	ackTimeout time.Duration,
) (MsgAckData, int, error) {
	const maxAttempts = 6
	retries := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ack, wsErr, err := ws.SendTextMessage(ctx, convIDStr, clientMsgID, content, ackTimeout)
		if err == nil && wsErr == nil {
			return ack, retries, nil
		}
		if wsErr != nil {
			if wsErr.Retryable && attempt+1 < maxAttempts {
				retries++
				backoff := time.Duration(25*(1<<attempt)) * time.Millisecond
				if backoff > 500*time.Millisecond {
					backoff = 500 * time.Millisecond
				}
				time.Sleep(backoff + randDuration(30*time.Millisecond))
				continue
			}
			return MsgAckData{}, retries, fmt.Errorf("ws error code=%d retryable=%v msg=%s", wsErr.Code, wsErr.Retryable, wsErr.Message)
		}
		return MsgAckData{}, retries, err
	}
	return MsgAckData{}, retries, fmt.Errorf("exhausted retries for client_msg_id=%s", clientMsgID)
}

func evaluateAssertions(rep *LoadTestReport) []AssertionResult {
	s := rep.Summary
	expectedSent := rep.Config.Clients*(rep.Config.SingleMsgsPerUser+rep.Config.GroupMsgsPerUser)
	// Note: if Clients is odd, 1 user has no pair so SingleMsgsPerUser applies to Clients-1 users.
	if rep.Config.Clients%2 == 1 {
		expectedSent = (rep.Config.Clients-1)*rep.Config.SingleMsgsPerUser + rep.Config.Clients*rep.Config.GroupMsgsPerUser
	}

	connOK := rep.WSConnectedCount == rep.Config.Clients &&
		rep.PeakMetrics.OnlineUsers == int64(rep.Config.Clients) &&
		rep.PeakMetrics.WSConnections == int64(rep.Config.Clients) &&
		rep.WSUnexpectedCloses == 0 &&
		s.HeartbeatPongs >= int64(rep.Config.Clients)

	lossPassed := s.TotalSentAttempts == expectedSent &&
		s.AckFailures == 0 &&
		s.MissingDeliveries == 0 &&
		s.LossRate == 0

	dupPassed := s.DuplicateDeliveries == 0 &&
		s.AckSeqCollisions == 0 &&
		s.DuplicateRate == 0 &&
		rep.IdempotencyFailed == 0

	orderPassed := s.OutOfOrderCount == 0

	zeroPassed := rep.WSDisconnectedCount == rep.Config.Clients &&
		rep.FinalMetric.OnlineUsers == 0 &&
		rep.FinalMetric.WSConnections == 0

	return []AssertionResult{
		{
			Name:   fmt.Sprintf("%d concurrent WS connections & heartbeat", rep.Config.Clients),
			Passed: connOK,
			Detail: fmt.Sprintf("connected=%d/%d peak_online=%d peak_ws_conns=%d unexpected_closes=%d pings=%d pongs=%d",
				rep.WSConnectedCount, rep.Config.Clients, rep.PeakMetrics.OnlineUsers, rep.PeakMetrics.WSConnections,
				rep.WSUnexpectedCloses, s.HeartbeatPings, s.HeartbeatPongs),
		},
		{
			Name:   "Message loss rate == 0",
			Passed: lossPassed,
			Detail: fmt.Sprintf("sent=%d/%d ack_failures=%d expected_deliveries=%d actual_unique_deliveries=%d missing=%d loss_rate=%.6f",
				s.AckedMessages, expectedSent, s.AckFailures, s.ExpectedDeliveries, s.ActualUniqueDeliveries, s.MissingDeliveries, s.LossRate),
		},
		{
			Name:   "Message duplicate rate == 0 (incl. idempotency probes)",
			Passed: dupPassed,
			Detail: fmt.Sprintf("duplicate_deliveries=%d ack_seq_collisions=%d duplicate_rate=%.6f idem_checked=%d idem_passed=%d idem_failed=%d",
				s.DuplicateDeliveries, s.AckSeqCollisions, s.DuplicateRate, rep.IdempotencyChecked, rep.IdempotencyPassed, rep.IdempotencyFailed),
		},
		{
			Name:   "Per-conversation seq strictly increasing (no out-of-order)",
			Passed: orderPassed,
			Detail: fmt.Sprintf("out_of_order_count=%d checked_streams=%d", s.OutOfOrderCount, s.CheckedClientConvStreams),
		},
		{
			Name:   "Post-disconnect online users & WS connections == 0",
			Passed: zeroPassed,
			Detail: fmt.Sprintf("disconnected=%d/%d final_online_users=%d final_ws_connections=%d converge_ms=%d",
				rep.WSDisconnectedCount, rep.Config.Clients, rep.FinalMetric.OnlineUsers, rep.FinalMetric.WSConnections,
				rep.DisconnectConverge.Milliseconds()),
		},
	}
}

func formatReportMarkdown(rep *LoadTestReport) string {
	var b strings.Builder
	s := rep.Summary
	status := "PASS"
	if !rep.AllPassed {
		status = "FAIL"
	}

	b.WriteString(fmt.Sprintf("# Enterprise IM 千级 WebSocket 压测报告 (%s)\n\n", status))
	b.WriteString(fmt.Sprintf("- **开始时间 (UTC)**：%s\n", rep.StartedAt.Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("- **结束时间 (UTC)**：%s\n", rep.FinishedAt.Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("- **总耗时**：%s\n", rep.TotalDur.Round(time.Millisecond)))
	b.WriteString(fmt.Sprintf("- **目标服务**：`%s` (`%s`)\n", rep.Config.BaseURL, rep.Config.WSURL))
	b.WriteString(fmt.Sprintf("- **测试规模**：`clients=%d`，`single_convs=%d`，`group_convs=%d`（每群约 `%d` 人），`single_msgs/user=%d`，`group_msgs/user=%d`\n\n",
		rep.Config.Clients, rep.SingleConversations, rep.GroupConversations, rep.Config.GroupSize,
		rep.Config.SingleMsgsPerUser, rep.Config.GroupMsgsPerUser))

	b.WriteString("## 1. 验收断言结果\n\n")
	b.WriteString("| 断言项 | 结果 | 实测详情 |\n")
	b.WriteString("| :--- | :--- | :--- |\n")
	for _, a := range rep.Assertions {
		res := "PASS"
		if !a.Passed {
			res = "FAIL"
		}
		b.WriteString(fmt.Sprintf("| %s | **%s** | `%s` |\n", a.Name, res, a.Detail))
	}
	b.WriteString("\n")

	b.WriteString("## 2. 核心实测指标\n\n")
	b.WriteString("| 指标 | 实测值 |\n")
	b.WriteString("| :--- | :--- |\n")
	b.WriteString(fmt.Sprintf("| 注册 / 登录用户数 | %d / %d（耗时 %s / %s；初始化 19004 死锁重试 %d 次） |\n",
		rep.RegisteredUsers, rep.LoggedInUsers, rep.SetupRegisterDur.Round(time.Millisecond), rep.SetupLoginDur.Round(time.Millisecond), rep.SetupHTTP19004Count))
	b.WriteString(fmt.Sprintf("| 单聊会话数 / 群聊会话数 | %d / %d（建群加好友耗时 %s） |\n",
		rep.SingleConversations, rep.GroupConversations, rep.SetupTopologyDur.Round(time.Millisecond)))
	b.WriteString(fmt.Sprintf("| 并发 WS 建连数 / 耗时 | %d / %s |\n", rep.WSConnectedCount, rep.WSDialDur.Round(time.Millisecond)))
	b.WriteString(fmt.Sprintf("| 峰值在线用户数 / WS 连接数（服务端统计） | %d / %d |\n",
		rep.PeakMetrics.OnlineUsers, rep.PeakMetrics.WSConnections))
	b.WriteString(fmt.Sprintf("| 心跳 Ping 发送 / Pong 收到 / 异常 | %d / %d / %d |\n",
		s.HeartbeatPings, s.HeartbeatPongs, s.HeartbeatErrors))
	b.WriteString(fmt.Sprintf("| 发送消息数（单聊 / 群聊 / 总计） | %d / %d / %d |\n",
		s.SingleMessagesAcked, s.GroupMessagesAcked, s.AckedMessages))
	b.WriteString(fmt.Sprintf("| 发送阶段耗时 / 吞吐 (msg/s) | %s / %.2f msg/s |\n",
		rep.MessagingDur.Round(time.Millisecond), safeRate(float64(s.AckedMessages), rep.MessagingDur.Seconds())))
	b.WriteString(fmt.Sprintf("| 预期推送总数 (`msg.push`) / 实际唯一收到 | %d / %d（端到端收齐耗时 %s，吞吐 %.2f push/s） |\n",
		s.ExpectedDeliveries, s.ActualUniqueDeliveries, rep.DeliveryDrainDur.Round(time.Millisecond), safeRate(float64(s.ActualUniqueDeliveries), rep.DeliveryDrainDur.Seconds())))
	b.WriteString(fmt.Sprintf("| 消息丢失数 / 丢失率 | %d / %.6f |\n", s.MissingDeliveries, s.LossRate))
	b.WriteString(fmt.Sprintf("| 消息重复数 / 重复率 | %d / %.6f |\n", s.DuplicateDeliveries, s.DuplicateRate))
	b.WriteString(fmt.Sprintf("| 同会话 `seq` 乱序次数 | %d |\n", s.OutOfOrderCount))
	b.WriteString(fmt.Sprintf("| 发送重试次数 / ACK 失败数 / WS 错误帧 | %d / %d / %d |\n",
		s.AckRetries, s.AckFailures, s.WSErrorFrames))
	b.WriteString(fmt.Sprintf("| 幂等重发校验（样本 / 通过 / 失败） | %d / %d / %d |\n",
		rep.IdempotencyChecked, rep.IdempotencyPassed, rep.IdempotencyFailed))
	b.WriteString(fmt.Sprintf("| 断开连接数 / 异常断线数 | %d / %d |\n", rep.WSDisconnectedCount, rep.WSUnexpectedCloses))
	b.WriteString(fmt.Sprintf("| 断开后最终在线用户数 / WS 连接数 | %d / %d（收敛耗时 %s） |\n",
		rep.FinalMetric.OnlineUsers, rep.FinalMetric.WSConnections, rep.DisconnectConverge.Round(time.Millisecond)))
	b.WriteString(fmt.Sprintf("| 最终 Stream Pending / 死信数量 | %d / %d |\n",
		rep.FinalMetric.Streams.PendingTotal, rep.FinalMetric.Streams.DeadLetterCount))
	b.WriteString(fmt.Sprintf("| 服务端峰值 Goroutines / HeapAlloc | %d / %.2f MiB |\n\n",
		rep.PostSend.Goroutines, float64(rep.PostSend.HeapBytes)/(1024*1024)))

	b.WriteString("## 3. 延迟分位数统计\n\n")
	b.WriteString("| 阶段 | 样本数 | Avg | P50 | P95 | P99 | Max |\n")
	b.WriteString("| :--- | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	writeLatencyRow(&b, "WS 建连 (Handshake)", s.ConnectLatency)
	writeLatencyRow(&b, "心跳 Ping -> Pong", s.PingLatency)
	writeLatencyRow(&b, "发送 msg.send -> msg.ack", s.AckLatency)
	writeLatencyRow(&b, "端到端 msg.send -> msg.push", s.DeliveryLatency)
	b.WriteString("\n")

	if len(rep.StreamRows) > 0 {
		b.WriteString("## 4. Redis Streams 消费组最终状态\n\n")
		b.WriteString("| Stream | Group | Consumer | LastDeliveredID | Pending | OldestPendingAge(ms) |\n")
		b.WriteString("| :--- | :--- | :--- | :--- | ---: | ---: |\n")
		for _, row := range rep.StreamRows {
			b.WriteString(fmt.Sprintf("| `%s` | `%s` | `%s` | `%s` | %d | %d |\n",
				row.Stream, row.Group, row.Consumer, row.LastDeliveredID, row.PendingCount, row.OldestPendingAge))
		}
		b.WriteString("\n")
	}

	if len(s.SampleFailures) > 0 {
		b.WriteString("## 5. 失败样本诊断（最多前 20 条）\n\n")
		for _, sample := range s.SampleFailures {
			b.WriteString(fmt.Sprintf("- `%s`\n", sample))
		}
		b.WriteString("\n")
	}

	return b.String()
}

func writeLatencyRow(b *strings.Builder, name string, st LatencyStats) {
	b.WriteString(fmt.Sprintf("| %s | %d | %s | %s | %s | %s | %s |\n",
		name, st.Count, fmtMs(st.Avg), fmtMs(st.P50), fmtMs(st.P95), fmtMs(st.P99), fmtMs(st.Max)))
}

func fmtMs(d time.Duration) string {
	return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000.0)
}

func safeRate(num, secs float64) float64 {
	if secs <= 0 {
		return 0
	}
	return num / secs
}

func runPool(ctx context.Context, total, concurrency int, fn func(idx int) error) error {
	if total <= 0 {
		return nil
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > total {
		concurrency = total
	}
	jobs := make(chan int, total)
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if runCtx.Err() != nil {
					return
				}
				if err := fn(idx); err != nil {
					errOnce.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func pollMetrics(
	ctx context.Context,
	api *APIClient,
	adminToken string,
	timeout time.Duration,
	pred func(AdminMetricsOverview) bool,
) (AdminMetricsOverview, error) {
	deadline := time.Now().Add(timeout)
	var last AdminMetricsOverview
	var lastErr error
	for {
		m, err := api.AdminMetricsOverview(ctx, adminToken)
		if err == nil {
			last = m
			if pred(m) {
				return m, nil
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return last, lastErr
			}
			return last, fmt.Errorf("metrics condition not satisfied before %s (last online_users=%d ws_connections=%d)",
				timeout, last.OnlineUsers, last.WSConnections)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func closeAllWS(users []*TestUser) {
	for _, u := range users {
		if u != nil && u.WS != nil {
			u.WS.MarkExpectedClose()
			_ = u.WS.CloseGracefully(time.Second)
		}
	}
}

func deriveWSURL(baseURL string) string {
	switch {
	case strings.HasPrefix(baseURL, "https://"):
		return "wss://" + strings.TrimPrefix(baseURL, "https://") + "/ws"
	case strings.HasPrefix(baseURL, "http://"):
		return "ws://" + strings.TrimPrefix(baseURL, "http://") + "/ws"
	default:
		return "ws://" + baseURL + "/ws"
	}
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

func envDuration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def
	}
	return d
}

func shortRandHex(nBytes int) string {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

func randDuration(maxD time.Duration) time.Duration {
	if maxD <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxD)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func computeLatencyStats(samples []time.Duration) LatencyStats {
	if len(samples) == 0 {
		return LatencyStats{}
	}
	cp := make([]time.Duration, len(samples))
	copy(cp, samples)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	var sum int64
	for _, d := range cp {
		sum += int64(d)
	}
	return LatencyStats{
		Count: len(cp),
		Avg:   time.Duration(sum / int64(len(cp))),
		P50:   percentile(cp, 0.50),
		P95:   percentile(cp, 0.95),
		P99:   percentile(cp, 0.99),
		Max:   cp[len(cp)-1],
	}
}

func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
