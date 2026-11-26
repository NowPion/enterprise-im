package repository

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"enterprise-im/backend/internal/config"
)

// NewMySQL opens a GORM MySQL 8.0 connection pool, configures pool parameters,
// and performs a real startup Ping + SELECT 1 check.
func NewMySQL(cfg config.MySQLConfig, log *slog.Logger) (*gorm.DB, error) {
	if log == nil {
		log = slog.Default()
	}

	gormCfg := &gorm.Config{
		Logger: newSlogGormLogger(log, 200*time.Millisecond),
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	}

	db, err := gorm.Open(mysql.Open(cfg.DSN()), gormCfg)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("unwrap sql.DB: %w", err)
	}

	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}

	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	if err := PingMySQL(ctx, db); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("verify mysql startup connectivity: %w", err)
	}

	return db, nil
}

// PingMySQL runs a real database health probe via PingContext and SELECT 1.
func PingMySQL(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("mysql db handle is nil")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("mysql unwrap sql.DB: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql ping: %w", err)
	}
	var one int
	if err := db.WithContext(ctx).Raw("SELECT 1").Scan(&one).Error; err != nil {
		return fmt.Errorf("mysql select 1: %w", err)
	}
	if one != 1 {
		return fmt.Errorf("mysql select 1 returned unexpected value %d", one)
	}
	return nil
}

// CloseMySQL closes the underlying database connection pool.
func CloseMySQL(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// WithTx executes fn inside a GORM database transaction with ctx.
func WithTx(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error {
	if db == nil {
		return errors.New("mysql db handle is nil")
	}
	return db.WithContext(ctx).Transaction(fn)
}

type slogGormLogger struct {
	log           *slog.Logger
	slowThreshold time.Duration
}

func newSlogGormLogger(log *slog.Logger, slowThreshold time.Duration) gormlogger.Interface {
	return &slogGormLogger{
		log:           log,
		slowThreshold: slowThreshold,
	}
}

func (l *slogGormLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface {
	return l
}

func (l *slogGormLogger) Info(ctx context.Context, msg string, data ...any) {
	l.log.InfoContext(ctx, fmt.Sprintf(msg, data...))
}

func (l *slogGormLogger) Warn(ctx context.Context, msg string, data ...any) {
	l.log.WarnContext(ctx, fmt.Sprintf(msg, data...))
}

func (l *slogGormLogger) Error(ctx context.Context, msg string, data ...any) {
	l.log.ErrorContext(ctx, fmt.Sprintf(msg, data...))
}

func (l *slogGormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		l.log.ErrorContext(ctx, "gorm_query_error",
			slog.Float64("elapsed_ms", float64(elapsed.Microseconds())/1000.0),
			slog.Any("error", err),
		)
		return
	}
	if l.slowThreshold > 0 && elapsed > l.slowThreshold {
		sqlStr, rows := fc()
		l.log.WarnContext(ctx, "gorm_slow_query",
			slog.Float64("elapsed_ms", float64(elapsed.Microseconds())/1000.0),
			slog.Int64("rows", rows),
			slog.String("sql", sqlStr),
		)
	}
}
