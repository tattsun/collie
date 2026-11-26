package session

import (
	"encoding/json"
	"sync"
)

// Client is one connected browser (one WebSocket).
type Client struct {
	Send chan []byte

	mu     sync.Mutex
	subs   map[string]bool
	closed bool
	kill   func()
}

func NewClient(kill func()) *Client {
	return &Client{Send: make(chan []byte, 2048), subs: map[string]bool{}, kill: kill}
}

// enqueue never blocks: a client that cannot keep up is disconnected and
// will resynchronise from a fresh snapshot when it reconnects.
func (c *Client) enqueue(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.Send <- b:
	default:
		c.closed = true
		go c.kill()
	}
}

// Enqueue queues a raw message for this client.
func (c *Client) Enqueue(b []byte) { c.enqueue(b) }

func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
}

func NewHub() *Hub { return &Hub{clients: map[*Client]struct{}{}} }

func (h *Hub) Add(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) Remove(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.Close()
}

func (h *Hub) Broadcast(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.enqueue(b)
	}
}

func (h *Hub) publish(sid string, b []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.mu.Lock()
		ok := c.subs[sid]
		c.mu.Unlock()
		if ok {
			c.enqueue(b)
		}
	}
}

func (h *Hub) subscribers(sid string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for c := range h.clients {
		c.mu.Lock()
		if c.subs[sid] {
			n++
		}
		c.mu.Unlock()
	}
	return n
}
