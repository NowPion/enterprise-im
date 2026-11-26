package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/config"
)

// NewRedis creates a Redis 7 client and verifies connectivity with a real PING on startup.
func NewRedis(cfg config.RedisConfig) (*redis.Client, error) {
	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
		DialTimeout:  dialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	})

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	if err := PingRedis(ctx, client); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("verify redis startup connectivity: %w", err)
	}

	return client, nil
}

// PingRedis runs a real PING command against Redis.
func PingRedis(ctx context.Context, client *redis.Client) error {
	if client == nil {
		return errors.New("redis client is nil")
	}
	res, err := client.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	if res != "PONG" {
		return fmt.Errorf("redis ping returned unexpected response %q", res)
	}
	return nil
}

// CloseRedis closes the Redis client pool.
func CloseRedis(client *redis.Client) error {
	if client == nil {
		return nil
	}
	return client.Close()
}
