package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/api"
	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type healthzEnvelope struct {
	Code      int                  `json:"code"`
	Message   string               `json:"message"`
	Data      service.HealthReport `json:"data"`
	RequestID string               `json:"request_id"`
}

func TestHealthzFailurePathsWithoutMockingRedisClient(t *testing.T) {
	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	// Real Redis client pointed at closed local port 1 -> verifies real TCP probe failure -> 503 + 19002.
	deadRedis := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
	})
	defer func() { _ = deadRedis.Close() }()

	svcRedisFail := service.NewHealthService(
		func(context.Context) error { return nil },
		func(ctx context.Context) error { return repository.PingRedis(ctx, deadRedis) },
		500*time.Millisecond,
		log,
	)
	router := api.NewRouter(cfg, log, controller.NewHealthController(svcRedisFail))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when redis unavailable, got %d body=%s", w.Code, w.Body.String())
	}
	var env healthzEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if env.Code != errcode.ErrRedisUnavailable.Code {
		t.Fatalf("expected business code 19002, got %d", env.Code)
	}
	if env.RequestID == "" || w.Header().Get(errcode.HeaderRequestID) != env.RequestID {
		t.Fatalf("request_id mismatch: envelope=%q header=%q", env.RequestID, w.Header().Get(errcode.HeaderRequestID))
	}
	if env.Data.MySQL != "ok" || env.Data.Redis != "unavailable" || env.Data.Status != "unavailable" {
		t.Fatalf("unexpected healthz data on redis failure: %+v", env.Data)
	}

	// Unknown route returns unified JSON 404 with X-Request-ID.
	w404 := httptest.NewRecorder()
	req404, _ := http.NewRequest(http.MethodGet, "/api/v1/not-registered-in-stage3", nil)
	router.ServeHTTP(w404, req404)
	if w404.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown route, got %d", w404.Code)
	}
	if w404.Header().Get(errcode.HeaderRequestID) == "" {
		t.Fatal("expected X-Request-ID header on 404 response")
	}
}

func TestHealthzLiveMySQLAndRedis(t *testing.T) {
	mysqlPass := os.Getenv("MYSQL_PASSWORD")
	redisPass := os.Getenv("REDIS_PASSWORD")
	if mysqlPass == "" && redisPass == "" {
		t.Skip("skipping live /healthz integration test: MYSQL_PASSWORD/REDIS_PASSWORD not set")
	}

	cfg, err := config.Load("../configs/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if v := os.Getenv("MYSQL_HOST"); v != "" {
		cfg.MySQL.Host = v
	}
	if v := os.Getenv("MYSQL_PORT"); v != "" {
		if port, convErr := strconv.Atoi(v); convErr == nil {
			cfg.MySQL.Port = port
		}
	}
	if v := os.Getenv("MYSQL_USER"); v != "" {
		cfg.MySQL.User = v
	}
	if v := os.Getenv("MYSQL_DATABASE"); v != "" {
		cfg.MySQL.Database = v
	}
	cfg.MySQL.Password = mysqlPass
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	cfg.Redis.Password = redisPass

	log, err := logger.New(config.LogConfig{Level: "warn", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}

	db, err := repository.NewMySQL(cfg.MySQL, log)
	if err != nil {
		t.Fatalf("live NewMySQL failed: %v", err)
	}
	defer func() { _ = repository.CloseMySQL(db) }()

	rdb, err := repository.NewRedis(cfg.Redis)
	if err != nil {
		t.Fatalf("live NewRedis failed: %v", err)
	}
	defer func() { _ = repository.CloseRedis(rdb) }()

	svc := service.NewHealthService(
		func(ctx context.Context) error { return repository.PingMySQL(ctx, db) },
		func(ctx context.Context) error { return repository.PingRedis(ctx, rdb) },
		cfg.Health.ProbeTimeout,
		log,
	)
	router := api.NewRouter(cfg, log, controller.NewHealthController(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on live /healthz, got %d body=%s", w.Code, w.Body.String())
	}
	var env healthzEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal live /healthz: %v", err)
	}
	if env.Code != 0 || env.Message != "ok" {
		t.Fatalf("unexpected envelope code/message: %+v", env)
	}
	if env.RequestID == "" || w.Header().Get(errcode.HeaderRequestID) != env.RequestID {
		t.Fatalf("request_id mismatch: envelope=%q header=%q", env.RequestID, w.Header().Get(errcode.HeaderRequestID))
	}
	if env.Data.Status != "ok" || env.Data.MySQL != "ok" || env.Data.Redis != "ok" {
		t.Fatalf("unexpected live healthz payload: %+v", env.Data)
	}
	if _, parseErr := time.Parse("2006-01-02T15:04:05.000Z", env.Data.Timestamp); parseErr != nil {
		t.Fatalf("invalid RFC3339 millisecond timestamp %q: %v", env.Data.Timestamp, parseErr)
	}

	// Real dependency failure injection against live MySQL + closed Redis pool -> 503 + 19002.
	closedRedis, err := repository.NewRedis(cfg.Redis)
	if err != nil {
		t.Fatalf("NewRedis for failure check: %v", err)
	}
	_ = closedRedis.Close()

	svcDegraded := service.NewHealthService(
		func(ctx context.Context) error { return repository.PingMySQL(ctx, db) },
		func(ctx context.Context) error { return repository.PingRedis(ctx, closedRedis) },
		cfg.Health.ProbeTimeout,
		log,
	)
	degradedRouter := api.NewRouter(cfg, log, controller.NewHealthController(svcDegraded))
	wFail := httptest.NewRecorder()
	reqFail, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	degradedRouter.ServeHTTP(wFail, reqFail)
	if wFail.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with closed redis, got %d", wFail.Code)
	}
	var failEnv healthzEnvelope
	if err := json.Unmarshal(wFail.Body.Bytes(), &failEnv); err != nil {
		t.Fatalf("unmarshal degraded /healthz: %v", err)
	}
	if failEnv.Code != errcode.ErrRedisUnavailable.Code || failEnv.Data.MySQL != "ok" || failEnv.Data.Redis != "unavailable" {
		t.Fatalf("unexpected degraded healthz payload: %+v", failEnv)
	}
}
