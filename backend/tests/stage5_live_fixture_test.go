package tests

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/model"
	pkgjwt "enterprise-im/backend/internal/pkg/jwt"
	"enterprise-im/backend/internal/repository"
)

func stage5UserKey(userID uint64) string { return fmt.Sprintf("im:presence:user:%d", userID) }
func stage5Routes(userID uint64) string  { return fmt.Sprintf("im:route:user:%d", userID) }
func stage5SessionKey(sessionID uint64) string {
	return fmt.Sprintf("im:route:session:%d", sessionID)
}

func (rig *stage5Rig) user(t *testing.T) uint64 {
	t.Helper()
	user := model.User{Username: "stage5_" + uuid.NewString(), PasswordHash: "!", Nickname: "Stage 5 QA"}
	stage5Check(t, rig.db.WithContext(t.Context()).Create(&user).Error)
	rig.cleanup = append(rig.cleanup, func(ctx context.Context) error {
		return errors.Join(rig.redis.Del(ctx, stage5UserKey(user.ID), stage5Routes(user.ID)).Err(),
			rig.db.WithContext(ctx).Delete(&user).Error)
	})
	return user.ID
}

type stage5Session struct {
	model.UserSession
	token string
}

func (rig *stage5Rig) session(t *testing.T, userID uint64) stage5Session {
	t.Helper()
	session := model.UserSession{UserID: userID, DeviceID: uuid.NewString(), DeviceType: model.DeviceTypeWeb,
		AccessJTI: pkgjwt.NewJTI(), RefreshTokenHash: pkgjwt.HashRefreshToken(uuid.NewString()),
		ExpiresAt: time.Now().Add(2 * time.Hour), LastActiveAt: time.Now()}
	stage5Check(t, rig.db.WithContext(t.Context()).Create(&session).Error)
	rig.cleanup = append(rig.cleanup, func(ctx context.Context) error {
		return errors.Join(rig.redis.Del(ctx, stage5SessionKey(session.ID), repository.TokenBlacklistKey(session.AccessJTI)).Err(),
			rig.db.WithContext(ctx).Delete(&session).Error)
	})
	token, _, _, err := rig.manager.IssueAccessTokenWithJTI(userID, session.ID, model.UserRoleUser, session.AccessJTI)
	stage5Check(t, err)
	return stage5Session{UserSession: session, token: token}
}

func (rig *stage5Rig) friends(t *testing.T, first, second uint64) {
	t.Helper()
	rows := []model.Friendship{{UserID: first, FriendID: second}, {UserID: second, FriendID: first}}
	stage5Check(t, rig.db.WithContext(t.Context()).Create(&rows).Error)
	rig.cleanup = append(rig.cleanup, func(ctx context.Context) error { return rig.db.WithContext(ctx).Delete(&rows).Error })
}

func (rig *stage5Rig) connect(t *testing.T, session stage5Session) *websocket.Conn {
	t.Helper()
	conn := rig.dial(t, session.token, false, nil)
	stage5Ping(t, conn)
	return conn
}

func (rig *stage5Rig) count(t *testing.T, userID uint64, want int) {
	t.Helper()
	stage5Eventually(t, fmt.Sprintf("user %d to have %d live connections", userID, want), func() bool {
		count, err := rig.redis.Get(t.Context(), stage5UserKey(userID)).Result()
		stage5Check(t, err)
		return count == strconv.Itoa(want)
	})
}

type stage5Publisher struct {
	lease  repository.ConnLease
	store  *repository.PresenceRepository
	online bool
}

func (rig *stage5Rig) publisher(t *testing.T, friend uint64) *stage5Publisher {
	t.Helper()
	session := rig.session(t, rig.user(t))
	rig.friends(t, friend, session.UserID)
	lease := repository.ConnLease{UserID: session.UserID, SessionID: session.ID, DeviceID: session.DeviceID,
		NodeID: rig.node, ConnID: uuid.NewString()}
	rig.cleanup = append(rig.cleanup, func(ctx context.Context) error { return rig.store.Release(ctx, lease) })
	return &stage5Publisher{lease: lease, store: rig.store}
}

func (publisher *stage5Publisher) toggle(t *testing.T) {
	t.Helper()
	if publisher.online {
		stage5Check(t, publisher.store.Release(t.Context(), publisher.lease))
	} else {
		stage5Check(t, publisher.store.Open(t.Context(), publisher.lease))
	}
	publisher.online = !publisher.online
}
