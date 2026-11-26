package ws

import (
	"context"
	"time"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/service"
)

type rotationStore struct {
	service.AuthDataStore
	hub *Hub
}

func (hub *Hub) WatchAccessRotations(store service.AuthDataStore) service.AuthDataStore {
	return &rotationStore{AuthDataStore: store, hub: hub}
}

type rotationTx struct {
	service.AuthTxStore
	locked  *model.UserSession
	rotated bool
}

func (tx *rotationTx) LockSessionByID(ctx context.Context, id uint64) (*model.UserSession, error) {
	session, err := tx.AuthTxStore.LockSessionByID(ctx, id)
	tx.locked = session
	return session, err
}

func (tx *rotationTx) RotateSessionTokens(ctx context.Context, id uint64, hash, jti string, expires, active time.Time, ip, agent string) error {
	err := tx.AuthTxStore.RotateSessionTokens(ctx, id, hash, jti, expires, active, ip, agent)
	tx.rotated = err == nil && tx.locked != nil && tx.locked.ID == id
	return err
}

func (store *rotationStore) WithTx(ctx context.Context, apply func(service.AuthTxStore) error) error {
	var observed rotationTx
	err := store.AuthDataStore.WithTx(ctx, func(tx service.AuthTxStore) error {
		observed = rotationTx{AuthTxStore: tx}
		return apply(&observed)
	})
	if err == nil && observed.rotated {
		session := observed.locked
		notifyErr := store.hub.revokeSessions(ctx, session.UserID, []uint64{session.ID}, "rotated", session.AccessJTI)
		if notifyErr != nil {
			store.hub.log.WarnContext(ctx, "ws_rotation_notify_failed", "user_id", session.UserID, "session_id", session.ID, "error", notifyErr)
		}
	}
	return err
}
