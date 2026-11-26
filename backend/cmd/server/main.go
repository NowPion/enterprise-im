package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/api"
	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/middleware"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
	"enterprise-im/backend/internal/ws"
)

// ShutdownHooks encapsulates ordered graceful shutdown steps across layers.
type ShutdownHooks struct {
	NotifyAndCloseWS func(ctx context.Context) error
	DrainMQ          func(ctx context.Context) error
	CloseDB          func() error
	CloseRedis       func() error
}

func main() {
	configPath := flag.String("config", "", "optional path to config.yaml")
	flag.Parse()

	if err := run(*configPath); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "enterprise-im server exited with error: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	authCfg, err := config.LoadAuthConfig(configPath)
	if err != nil {
		return fmt.Errorf("load auth config: %w", err)
	}

	log, err := logger.InitGlobal(cfg.Log)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	log.Info("configuration_loaded",
		slog.Any("config", cfg.RedactedSummary()),
		slog.Any("auth", authCfg.RedactedSummary()),
	)

	idGen, err := snowflake.NewGenerator(cfg.Snowflake.NodeID)
	if err != nil {
		return fmt.Errorf("init snowflake generator: %w", err)
	}

	jwtMgr, err := pkgjwt.NewManager(authCfg.JWT)
	if err != nil {
		return fmt.Errorf("init jwt manager: %w", err)
	}

	db, err := repository.NewMySQL(cfg.MySQL, log)
	if err != nil {
		return fmt.Errorf("init mysql: %w", err)
	}
	log.Info("mysql_connected", slog.String("host", cfg.MySQL.Host), slog.Int("port", cfg.MySQL.Port), slog.String("database", cfg.MySQL.Database))

	rdb, err := repository.NewRedis(cfg.Redis)
	if err != nil {
		_ = repository.CloseMySQL(db)
		return fmt.Errorf("init redis: %w", err)
	}
	log.Info("redis_connected", slog.String("addr", cfg.Redis.Addr), slog.Int("db", cfg.Redis.DB))

	router, err := buildRouter(cfg, authCfg, log, db, rdb, idGen, jwtMgr)
	if err != nil {
		_ = repository.CloseRedis(rdb)
		_ = repository.CloseMySQL(db)
		return fmt.Errorf("build router: %w", err)
	}

	ln, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		_ = repository.CloseRedis(rdb)
		_ = repository.CloseMySQL(db)
		return fmt.Errorf("listen %s: %w", cfg.Server.Addr, err)
	}
	if err := router.hub.Start(context.Background()); err != nil {
		_ = ln.Close()
		_ = repository.CloseRedis(rdb)
		_ = repository.CloseMySQL(db)
		return fmt.Errorf("start websocket hub: %w", err)
	}

	srv := &http.Server{
		Addr:         ln.Addr().String(),
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	hooks := ShutdownHooks{
		NotifyAndCloseWS: router.hub.Shutdown,
		DrainMQ: func(context.Context) error {
			log.Info("shutdown_mq_hook_noop", slog.String("reason", "no active mq consumers"))
			return nil
		},
		CloseDB:    func() error { return repository.CloseMySQL(db) },
		CloseRedis: func() error { return repository.CloseRedis(rdb) },
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return serveAndShutdown(sigCtx, log, srv, ln, cfg.Server.ShutdownTimeout, hooks)
}

type serverRouter struct {
	*gin.Engine
	hub *ws.Hub
}

func buildRouter(
	cfg *config.Config,
	authCfg *config.AuthConfig,
	log *slog.Logger,
	db *gorm.DB,
	rdb *redis.Client,
	idGen *snowflake.Generator,
	jwtMgr *pkgjwt.Manager,
) (*serverRouter, error) {
	if cfg == nil || authCfg == nil || db == nil || rdb == nil || idGen == nil || jwtMgr == nil {
		return nil, errors.New("server router dependencies must not be nil")
	}
	if log == nil {
		log = slog.Default()
	}

	healthSvc := service.NewHealthService(
		func(ctx context.Context) error { return repository.PingMySQL(ctx, db) },
		func(ctx context.Context) error { return repository.PingRedis(ctx, rdb) },
		cfg.Health.ProbeTimeout,
		log,
	)
	healthCtrl := controller.NewHealthController(healthSvc)

	authRepo := repository.NewAuthRepository(db)
	sessionRepo := repository.NewSessionRepository(db, rdb)
	userRepo := repository.NewUserRepository(db)
	userPresenceRepo := repository.NewUserPresenceRepository(rdb)
	friendRepo := repository.NewFriendshipRepository(db, rdb)
	friendReqRepo := repository.NewGormFriendRequestRepository(db)
	seqAllocator := repository.NewRedisSeqAllocator(rdb, 5*time.Second)
	frPresence := repository.NewRedisPresenceChecker(rdb)

	authState := middleware.NewAuthStateStore(authRepo, sessionRepo)
	jwtAuth := middleware.JWTAuth(jwtMgr, authState)
	leases := repository.NewPresenceRepository(db, rdb, cfg.WS.HeartbeatTimeout)
	hub := ws.NewHub(cfg, middleware.NewAccessAuthenticator(jwtMgr, authState), leases, log)

	authStore := hub.WatchAccessRotations(service.NewRepoAuthDataStore(authRepo, sessionRepo))
	authSvc := service.NewAuthService(authStore, jwtMgr, authCfg, log, hub.RevokeSessions)
	userSvc := service.NewUserService(userRepo, userPresenceRepo, log)
	friendReqSvc := service.NewFriendRequestService(friendReqRepo, friendRepo, seqAllocator, frPresence, idGen, log)
	friendSvc := service.NewFriendService(friendRepo, log)
	groupRepo := repository.NewGormGroupRepository(db)
	groupSvc := service.NewGroupService(groupRepo, seqAllocator, frPresence, idGen, hub, log)

	router := api.NewRouterWithStage4(cfg, log, healthCtrl, api.Stage4Dependencies{
		JWTAuth:           jwtAuth,
		AuthController:    controller.NewAuthController(authSvc),
		UserController:    controller.NewUserController(userSvc),
		FriendRequestCtrl: controller.NewFriendRequestController(friendReqSvc),
		FriendController:  controller.NewFriendController(friendSvc),
	})
	api.RegisterGroupRoutes(router, jwtAuth, controller.NewGroupController(groupSvc))
	api.RegisterWSRoute(router, cfg, hub)
	return &serverRouter{Engine: router, hub: hub}, nil
}

func newLoggedNoopSessionRevokeHook(log *slog.Logger) service.SessionRevokeHook {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, userID uint64, sessionIDs []uint64, reason string) error {
		log.InfoContext(ctx, "auth_session_revoke_hook_noop",
			slog.Uint64("user_id", userID),
			slog.Int("session_count", len(sessionIDs)),
			slog.String("reason", reason),
			slog.String("note", "no active websocket hub in stage 4"),
		)
		return service.NoopSessionRevokeHook(ctx, userID, sessionIDs, reason)
	}
}

func serveAndShutdown(
	sigCtx context.Context,
	log *slog.Logger,
	srv *http.Server,
	ln net.Listener,
	shutdownTimeout time.Duration,
	hooks ShutdownHooks,
) error {
	if shutdownTimeout <= 0 {
		shutdownTimeout = 15 * time.Second
	}
	serveErrCh := make(chan error, 1)
	go func() {
		log.Info("http_server_listening", slog.String("addr", ln.Addr().String()))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrCh <- err
			return
		}
		serveErrCh <- nil
	}()

	select {
	case err := <-serveErrCh:
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return errors.Join(err, executeOrderedShutdown(ctx, log, srv, hooks))
	case <-sigCtx.Done():
		log.Info("shutdown_signal_received")
	}

	timeoutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	shutdownErr := executeOrderedShutdown(timeoutCtx, log, srv, hooks)
	serveErr := <-serveErrCh
	return errors.Join(shutdownErr, serveErr)
}

func executeOrderedShutdown(ctx context.Context, log *slog.Logger, srv *http.Server, hooks ShutdownHooks) error {
	var errs []error

	// Step 1: Stop accepting new HTTP requests immediately.
	listenersClosed := make(chan struct{})
	srv.RegisterOnShutdown(func() {
		close(listenersClosed)
	})
	httpDone := make(chan error, 1)
	go func() {
		httpDone <- srv.Shutdown(ctx)
	}()

	select {
	case <-listenersClosed:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	log.Info("shutdown_step_1_stop_accepting_http")

	// Step 2: Notify and close WebSocket connections.
	if hooks.NotifyAndCloseWS != nil {
		if err := hooks.NotifyAndCloseWS(ctx); err != nil {
			errs = append(errs, fmt.Errorf("ws shutdown hook: %w", err))
		}
	}
	log.Info("shutdown_step_2_websocket_closed")

	// Step 3: Stop claiming new MQ tasks and drain active consumers.
	if hooks.DrainMQ != nil {
		if err := hooks.DrainMQ(ctx); err != nil {
			errs = append(errs, fmt.Errorf("mq drain hook: %w", err))
		}
	}
	log.Info("shutdown_step_3_mq_drained")

	// Step 4: Drain in-flight HTTP requests within timeout.
	if err := <-httpDone; err != nil {
		errs = append(errs, fmt.Errorf("http server shutdown: %w", err))
	}
	log.Info("shutdown_step_4_http_drained")

	// Step 5: Close MySQL and Redis pools last.
	if err := closeInfrastructure(log, hooks); err != nil {
		errs = append(errs, err)
	}
	log.Info("shutdown_step_5_db_and_redis_closed")

	return errors.Join(errs...)
}

func closeInfrastructure(log *slog.Logger, hooks ShutdownHooks) error {
	var errs []error
	if hooks.CloseDB != nil {
		if err := hooks.CloseDB(); err != nil {
			log.Error("mysql_close_failed", slog.Any("error", err))
			errs = append(errs, fmt.Errorf("close mysql: %w", err))
		}
	}
	if hooks.CloseRedis != nil {
		if err := hooks.CloseRedis(); err != nil {
			log.Error("redis_close_failed", slog.Any("error", err))
			errs = append(errs, fmt.Errorf("close redis: %w", err))
		}
	}
	return errors.Join(errs...)
}
