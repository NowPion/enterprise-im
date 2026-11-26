package ws

import "sync"

type registry struct {
	shards [64]shard
}

type shard struct {
	sync.RWMutex
	users map[uint64]map[string]*Client
}

func (peers *registry) forUser(user uint64) *shard {
	return &peers.shards[user%uint64(len(peers.shards))]
}

func (peers *registry) add(user uint64, id string, client *Client) {
	part := peers.forUser(user)
	part.Lock()
	defer part.Unlock()
	if part.users == nil {
		part.users = make(map[uint64]map[string]*Client)
	}
	if part.users[user] == nil {
		part.users[user] = make(map[string]*Client)
	}
	part.users[user][id] = client
}

func (peers *registry) remove(user uint64, id string, client *Client) {
	part := peers.forUser(user)
	part.Lock()
	defer part.Unlock()
	connections := part.users[user]
	if connections[id] == client {
		delete(connections, id)
		if len(connections) == 0 {
			delete(part.users, user)
		}
	}
}

func (peers *registry) snapshot(user uint64) []*Client {
	part := peers.forUser(user)
	part.RLock()
	defer part.RUnlock()
	clients := make([]*Client, 0, len(part.users[user]))
	for _, client := range part.users[user] {
		clients = append(clients, client)
	}
	return clients
}

func (peers *registry) all() []*Client {
	var clients []*Client
	for idx := range peers.shards {
		part := &peers.shards[idx]
		part.RLock()
		for _, connections := range part.users {
			for _, client := range connections {
				clients = append(clients, client)
			}
		}
		part.RUnlock()
	}
	return clients
}
