package ws

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"enterprise-im/backend/internal/repository"
)

type presenceEvent struct {
	UserID      string `json:"user_id"`
	DeviceCount int    `json:"device_count"`
	At          int64  `json:"at"`
}

type sessionEvent struct {
	UserID    uint64   `json:"user_id,string"`
	SessionID uint64   `json:"session_id,string"`
	ConnIDs   []string `json:"conn_ids"`
	Reason    string   `json:"reason"`
	OnlyJTI   string   `json:"only_jti,omitempty"`
}

func (hub *Hub) listen(ctx context.Context, sub *redis.PubSub) {
	messages := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-messages:
			if !ok {
				return
			}
			if msg.Channel == hub.store.PresenceChannel() {
				var event presenceEvent
				if err := json.Unmarshal([]byte(msg.Payload), &event); err == nil {
					if err := hub.deliverPresence(ctx, event); err != nil && ctx.Err() == nil {
						hub.log.ErrorContext(ctx, "ws_presence_delivery_failed", "error", err)
					}
				}
			} else {
				var event sessionEvent
				if err := json.Unmarshal([]byte(msg.Payload), &event); err == nil {
					hub.closeSession(event)
				}
			}
		}
	}
}

func (hub *Hub) deliverPresence(ctx context.Context, event presenceEvent) error {
	userID, err := strconv.ParseUint(event.UserID, 10, 64)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, hub.cfg.WriteTimeout)
	defer cancel()
	friends, err := hub.store.FriendIDs(ctx, userID)
	if err != nil {
		return err
	}
	var lastOnline *time.Time
	if event.DeviceCount == 0 {
		at := time.UnixMilli(event.At).UTC()
		lastOnline = &at
	}
	msg := outgoing("presence.update", uuid.NewString(), map[string]any{"user_id": event.UserID, "online": event.DeviceCount > 0, "device_count": event.DeviceCount, "last_online_at": lastOnline})
	var clients []*Client
	for _, target := range append(friends, userID) {
		clients = append(clients, hub.peers.snapshot(target)...)
	}
	if len(clients) == 0 {
		return nil
	}
	leases := make([]repository.ConnLease, 0, len(clients))
	for _, client := range clients {
		leases = append(leases, client.lease)
	}
	live, err := hub.store.LiveConnections(ctx, leases)
	if err != nil {
		return err
	}
	for _, client := range clients {
		if live[client.lease.ConnID] {
			client.enqueue(msg)
		}
	}
	return nil
}

func revokeEvent(reason string) string {
	switch reason {
	case "account.disabled", "account_disabled", "disabled":
		return "account.disabled"
	case "account.kicked", "account_kicked", "kicked", "admin_kick", "kick":
		return "account.kicked"
	default:
		return "session.revoked"
	}
}

func (hub *Hub) closeSession(event sessionEvent) {
	hits := make(map[string]bool, len(event.ConnIDs))
	for _, id := range event.ConnIDs {
		hits[id] = true
	}
	for _, client := range hub.peers.snapshot(event.UserID) {
		if client.lease.SessionID == event.SessionID && hits[client.lease.ConnID] && (event.OnlyJTI == "" || client.auth.JTI == event.OnlyJTI) {
			client.revoke(envelope{}, revokeEvent(event.Reason))
		}
	}
}

func (hub *Hub) RevokeSessions(ctx context.Context, userID uint64, sessionIDs []uint64, reason string) error {
	return hub.revokeSessions(ctx, userID, sessionIDs, reason, "")
}

func (hub *Hub) revokeSessions(ctx context.Context, userID uint64, sessionIDs []uint64, reason, onlyJTI string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hub.cfg.WriteTimeout)
	defer cancel()
	var failures []error
	for _, session := range sessionIDs {
		routes, err := hub.store.SessionRoutes(ctx, session)
		if err != nil {
			failures = append(failures, err)
		}
		byNode := make(map[string][]string)
		for _, route := range routes {
			if node, conn, ok := repository.SplitRoute(route); ok {
				byNode[node] = append(byNode[node], conn)
			}
		}
		if err != nil {
			for _, client := range hub.peers.snapshot(userID) {
				if client.lease.SessionID == session {
					byNode[hub.node] = append(byNode[hub.node], client.lease.ConnID)
				}
			}
		}
		for node, ids := range byNode {
			event := sessionEvent{UserID: userID, SessionID: session, ConnIDs: ids, Reason: reason, OnlyJTI: onlyJTI}
			if node == hub.node {
				hub.closeSession(event)
				continue
			}
			data, err := json.Marshal(event)
			if err == nil {
				err = hub.store.SendControl(ctx, node, data)
			}
			if err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
