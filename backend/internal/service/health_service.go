package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"enterprise-im/backend/internal/pkg/errcode"
)

const (
	HealthStateOK          = "ok"
	HealthStateUnavailable = "unavailable"
)

// ProbeFunc checks the live availability of an infrastructure dependency.
type ProbeFunc func(ctx context.Context) error

// HealthReport matches the frozen GET /healthz data payload in docs/api.md.
type HealthReport struct {
	Status    string `json:"status"`
	MySQL     string `json:"mysql"`
	Redis     string `json:"redis"`
	Timestamp string `json:"timestamp"`
}

// HealthService performs real dependency probes against MySQL and Redis.
type HealthService struct {
	pingMySQL    ProbeFunc
	pingRedis    ProbeFunc
	probeTimeout time.Duration
	log          *slog.Logger
}

// NewHealthService constructs a HealthService with real dependency probes.
func NewHealthService(pingMySQL, pingRedis ProbeFunc, probeTimeout time.Duration, log *slog.Logger) *HealthService {
	if probeTimeout <= 0 {
		probeTimeout = 2 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &HealthService{
		pingMySQL:    pingMySQL,
		pingRedis:    pingRedis,
		probeTimeout: probeTimeout,
		log:          log,
	}
}

// Check executes real concurrent health probes against MySQL and Redis.
// Precedence rule when both dependencies fail: HTTP 503 is returned with business code 19001
// (MySQL primary source of truth), while the data payload reports both mysql="unavailable" and redis="unavailable".
func (s *HealthService) Check(ctx context.Context) (HealthReport, *errcode.Error) {
	probeCtx, cancel := context.WithTimeout(ctx, s.probeTimeout)
	defer cancel()

	var (
		mysqlErr error
		redisErr error
		wg       sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		if s.pingMySQL == nil {
			mysqlErr = context.DeadlineExceeded
			return
		}
		mysqlErr = s.pingMySQL(probeCtx)
	}()
	go func() {
		defer wg.Done()
		if s.pingRedis == nil {
			redisErr = context.DeadlineExceeded
			return
		}
		redisErr = s.pingRedis(probeCtx)
	}()
	wg.Wait()

	report := HealthReport{
		Status:    HealthStateOK,
		MySQL:     HealthStateOK,
		Redis:     HealthStateOK,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	if mysqlErr != nil {
		report.Status = HealthStateUnavailable
		report.MySQL = HealthStateUnavailable
		s.log.ErrorContext(ctx, "healthz_mysql_probe_failed", slog.Any("error", mysqlErr))
	}
	if redisErr != nil {
		report.Status = HealthStateUnavailable
		report.Redis = HealthStateUnavailable
		s.log.ErrorContext(ctx, "healthz_redis_probe_failed", slog.Any("error", redisErr))
	}

	if mysqlErr != nil {
		return report, errcode.ErrMySQLUnavailable
	}
	if redisErr != nil {
		return report, errcode.ErrRedisUnavailable
	}
	return report, nil
}
