package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
)

func TestHealthServiceCheckStates(t *testing.T) {
	log, err := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	okProbe := func(context.Context) error { return nil }
	failProbe := func(context.Context) error { return errors.New("dial refused") }

	// 1. Both healthy
	svcOK := NewHealthService(okProbe, okProbe, time.Second, log)
	repOK, errOK := svcOK.Check(context.Background())
	if errOK != nil {
		t.Fatalf("expected nil err, got %v", errOK)
	}
	if repOK.Status != "ok" || repOK.MySQL != "ok" || repOK.Redis != "ok" {
		t.Fatalf("unexpected ok report: %+v", repOK)
	}
	if _, parseErr := time.Parse("2006-01-02T15:04:05.000Z", repOK.Timestamp); parseErr != nil {
		t.Fatalf("timestamp %q does not match millisecond UTC RFC3339: %v", repOK.Timestamp, parseErr)
	}

	// 2. MySQL down -> 19001
	svcMySQLDown := NewHealthService(failProbe, okProbe, time.Second, log)
	repMySQL, errMySQL := svcMySQLDown.Check(context.Background())
	if errMySQL == nil || errMySQL.Code != errcode.ErrMySQLUnavailable.Code || errMySQL.HTTPStatus != errcode.ErrMySQLUnavailable.HTTPStatus {
		t.Fatalf("expected 19001/503, got %v", errMySQL)
	}
	if repMySQL.MySQL != "unavailable" || repMySQL.Redis != "ok" || repMySQL.Status != "unavailable" {
		t.Fatalf("unexpected mysql-down report: %+v", repMySQL)
	}

	// 3. Redis down -> 19002
	svcRedisDown := NewHealthService(okProbe, failProbe, time.Second, log)
	repRedis, errRedis := svcRedisDown.Check(context.Background())
	if errRedis == nil || errRedis.Code != errcode.ErrRedisUnavailable.Code || errRedis.HTTPStatus != errcode.ErrRedisUnavailable.HTTPStatus {
		t.Fatalf("expected 19002/503, got %v", errRedis)
	}
	if repRedis.MySQL != "ok" || repRedis.Redis != "unavailable" || repRedis.Status != "unavailable" {
		t.Fatalf("unexpected redis-down report: %+v", repRedis)
	}

	// 4. Both down -> 19001 precedence with both marked unavailable
	svcBothDown := NewHealthService(failProbe, failProbe, time.Second, log)
	repBoth, errBoth := svcBothDown.Check(context.Background())
	if errBoth == nil || errBoth.Code != errcode.ErrMySQLUnavailable.Code || errBoth.HTTPStatus != errcode.ErrMySQLUnavailable.HTTPStatus {
		t.Fatalf("expected 19001/503 on dual failure, got %v", errBoth)
	}
	if repBoth.MySQL != "unavailable" || repBoth.Redis != "unavailable" || repBoth.Status != "unavailable" {
		t.Fatalf("unexpected dual-failure report: %+v", repBoth)
	}
}
