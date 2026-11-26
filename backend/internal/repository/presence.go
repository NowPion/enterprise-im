package repository

import (
	"context"
	_ "embed"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
)

const (
	deadlinesKey      = "im:presence:deadlines"
	offlinePendingKey = "im:presence:offline-pending"
)

//go:embed presence.lua
var leaseLua string

var leaseChange = redis.NewScript(leaseLua)
var acknowledgeOffline = redis.NewScript(`
if redis.call('ZSCORE', KEYS[1], ARGV[1]) == ARGV[2] then
    return redis.call('ZREM', KEYS[1], ARGV[1])
end
return 0`)
var ErrLeaseLost = errors.New("connection lease expired or owner changed")

type ConnLease struct {
	UserID, SessionID        uint64
	DeviceID, NodeID, ConnID string
}

type PresenceRepository struct {
	db  *gorm.DB
	rdb *redis.Client
	ttl time.Duration
	bus string
}

func NewPresenceRepository(db *gorm.DB, rdb *redis.Client, ttl time.Duration) *PresenceRepository {
	return &PresenceRepository{db: db, rdb: rdb, ttl: ttl, bus: "im:ws:" + strconv.Itoa(rdb.Options().DB) + ":"}
}

func (repo *PresenceRepository) PresenceChannel() string { return repo.bus + "presence" }

func (repo *PresenceRepository) change(ctx context.Context, op string, lease ConnLease) (int64, error) {
	user := strconv.FormatUint(lease.UserID, 10)
	session := strconv.FormatUint(lease.SessionID, 10)
	keys := []string{redisPresenceUserPrefix + user, "im:presence:conn:" + lease.ConnID, "im:route:user:" + user,
		"im:route:session:" + session, deadlinesKey, "im:presence:active", offlinePendingKey}
	member := user + "/" + session + "/" + lease.NodeID + ":" + lease.ConnID
	value, err := leaseChange.Run(ctx, repo.rdb, keys, op, user, session, lease.DeviceID, lease.NodeID, lease.ConnID,
		repo.ttl.Milliseconds(), (repo.ttl + 15*time.Second).Milliseconds(), member, repo.PresenceChannel()).Int64()
	if err == nil && value == -1 {
		return 0, ErrLeaseLost
	}
	return value, err
}

func (repo *PresenceRepository) Open(ctx context.Context, lease ConnLease) error {
	_, err := repo.change(ctx, "open", lease)
	return err
}

func (repo *PresenceRepository) Renew(ctx context.Context, lease ConnLease) error {
	_, err := repo.change(ctx, "ping", lease)
	return err
}

func (repo *PresenceRepository) Release(ctx context.Context, lease ConnLease) error {
	millis, err := repo.change(ctx, "close", lease)
	if err != nil || millis == 0 {
		return err
	}
	return repo.persistOffline(ctx, strconv.FormatUint(lease.UserID, 10), millis)
}

func (repo *PresenceRepository) persistOffline(ctx context.Context, userID string, millis int64) error {
	lastOnline := time.UnixMilli(millis).UTC()
	err := repo.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ? AND (last_online_at IS NULL OR last_online_at < ?)", userID, lastOnline).
		UpdateColumn("last_online_at", lastOnline).Error
	if err != nil {
		return err
	}
	return acknowledgeOffline.Run(ctx, repo.rdb, []string{offlinePendingKey}, userID, millis).Err()
}

func (repo *PresenceRepository) ReapExpired(ctx context.Context) error {
	now, err := repo.rdb.Time(ctx).Result()
	if err != nil {
		return err
	}
	members, err := repo.rdb.ZRangeByScore(ctx, deadlinesKey, &redis.ZRangeBy{Min: "-inf", Max: strconv.FormatInt(now.UnixMilli(), 10), Count: 128}).Result()
	if err != nil {
		return err
	}
	for _, member := range members {
		user, rest, _ := strings.Cut(member, "/")
		session, route, _ := strings.Cut(rest, "/")
		node, conn, ok := SplitRoute(route)
		userID, userErr := strconv.ParseUint(user, 10, 64)
		sessionID, sessionErr := strconv.ParseUint(session, 10, 64)
		if !ok || userErr != nil || sessionErr != nil {
			return errors.New("invalid presence deadline member")
		}
		if _, err := repo.change(ctx, "expire", ConnLease{UserID: userID, SessionID: sessionID, NodeID: node, ConnID: conn}); err != nil {
			return err
		}
	}
	pending, err := repo.rdb.ZRangeWithScores(ctx, offlinePendingKey, 0, 127).Result()
	if err != nil {
		return err
	}
	for _, entry := range pending {
		if err := repo.persistOffline(ctx, entry.Member.(string), int64(entry.Score)); err != nil {
			return err
		}
	}
	return nil
}

func SplitRoute(route string) (node, conn string, ok bool) {
	pos := strings.LastIndexByte(route, ':')
	if pos <= 0 || pos == len(route)-1 {
		return "", "", false
	}
	return route[:pos], route[pos+1:], true
}

func (repo *PresenceRepository) SessionRoutes(ctx context.Context, sessionID uint64) ([]string, error) {
	return repo.rdb.SMembers(ctx, "im:route:session:"+strconv.FormatUint(sessionID, 10)).Result()
}

func (repo *PresenceRepository) FriendIDs(ctx context.Context, userID uint64) ([]uint64, error) {
	var ids []uint64
	err := repo.db.WithContext(ctx).Model(&model.Friendship{}).
		Where("user_id = ? AND status = ?", userID, model.FriendshipStatusActive).Pluck("friend_id", &ids).Error
	return ids, err
}

func (repo *PresenceRepository) Subscribe(ctx context.Context, node string) (*redis.PubSub, error) {
	sub := repo.rdb.Subscribe(ctx, repo.PresenceChannel(), repo.bus+"control:"+node)
	for range 2 {
		if _, err := sub.ReceiveTimeout(ctx, 5*time.Second); err != nil {
			_ = sub.Close()
			return nil, err
		}
	}
	return sub, nil
}

func (repo *PresenceRepository) SendControl(ctx context.Context, node string, data []byte) error {
	listeners, err := repo.rdb.Publish(ctx, repo.bus+"control:"+node, data).Result()
	if err == nil && listeners == 0 {
		return errors.New("websocket node has no control subscriber")
	}
	return err
}

func (repo *PresenceRepository) LiveConnections(ctx context.Context, candidates []ConnLease) (map[string]bool, error) {
	pipe := repo.rdb.Pipeline()
	checks := make([]*redis.SliceCmd, 0, len(candidates))
	for _, lease := range candidates {
		checks = append(checks, pipe.HMGet(ctx, "im:presence:conn:"+lease.ConnID, "user_id", "session_id", "device_id", "node_id"))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(candidates))
	for idx, check := range checks {
		lease, fields := candidates[idx], check.Val()
		if len(fields) == 4 && fields[0] == strconv.FormatUint(lease.UserID, 10) && fields[1] == strconv.FormatUint(lease.SessionID, 10) && fields[2] == lease.DeviceID && fields[3] == lease.NodeID {
			live[lease.ConnID] = true
		}
	}
	return live, nil
}
