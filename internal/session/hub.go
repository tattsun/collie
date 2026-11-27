package session

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Client is one connected browser (one WebSocket) of a signed-in user.
type Client struct {
	Send chan []byte
	// UserID is whose view this connection gets; SessionHash identifies the
	// login session (to disconnect it on sign-out).
	UserID      string
	SessionHash string

	mu     sync.Mutex
	subs   map[string]bool
	known  map[string]bool // "p:<id>" / "s:<id>" the client has been sent
	closed bool
	kill   func()

	// What the page last reported (SetForeground): whether it is in front
	// of the user, and its browser's Web Push registration.
	visible bool
	visAt   time.Time
	pushID  string
}

func NewClient(userID, sessionHash string, kill func()) *Client {
	return &Client{Send: make(chan []byte, 2048), UserID: userID, SessionHash: sessionHash,
		subs: map[string]bool{}, known: map[string]bool{}, kill: kill}
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

// Kill disconnects the client.
func (c *Client) Kill() { go c.kill() }

func (c *Client) know(key string) {
	c.mu.Lock()
	c.known[key] = true
	c.mu.Unlock()
}

// forget reports whether the client knew key, and forgets it.
func (c *Client) forget(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known[key] {
		delete(c.known, key)
		return true
	}
	return false
}

func (c *Client) unsubscribe(sid string) {
	c.mu.Lock()
	delete(c.subs, sid)
	c.mu.Unlock()
}

// foregroundTTL is how long a page's report that it is in front of the
// user holds: the page repeats it while visible, and one frozen in the
// background (as phones do) stops repeating it without saying so.
const foregroundTTL = 45 * time.Second

// SetForeground records whether the page is in front of the user, and the
// Web Push registration of its browser.
func (c *Client) SetForeground(visible bool, pushID string) {
	c.mu.Lock()
	c.visible, c.visAt, c.pushID = visible, time.Now(), pushID
	c.mu.Unlock()
}

// Foreground reports whether a page of userID in the browser registered as
// pushID is in front of them: that browser shows notifications itself.
func (h *Hub) Foreground(userID, pushID string) bool {
	if pushID == "" {
		return false
	}
	found := false
	h.each(func(c *Client) {
		c.mu.Lock()
		if !c.closed && c.UserID == userID && c.pushID == pushID && c.visible && time.Since(c.visAt) < foregroundTTL {
			found = true
		}
		c.mu.Unlock()
	})
	return found
}

// resetKnown records what a freshly sent state contained.
func (c *Client) resetKnown(projects []model.Project, sessions []model.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.known = map[string]bool{}
	for _, p := range projects {
		c.known["p:"+p.ID] = true
	}
	for _, s := range sessions {
		c.known["s:"+s.ID] = true
	}
}

// Init records the state sent on connect.
func (c *Client) Init(projects []model.Project, sessions []model.Session) {
	c.resetKnown(projects, sessions)
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

func (h *Hub) each(fn func(c *Client)) {
	h.mu.RLock()
	list := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		list = append(list, c)
	}
	h.mu.RUnlock()
	for _, c := range list {
		fn(c)
	}
}

// Each calls fn for every connected client.
func (h *Hub) Each(fn func(c *Client)) { h.each(fn) }

// Broadcast sends a message to everyone (only for data visible to all).
func (h *Hub) Broadcast(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.each(func(c *Client) { c.enqueue(b) })
}

// BroadcastWhere sends a message to the clients whose user matches.
func (h *Hub) BroadcastWhere(v any, match func(userID string) bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.each(func(c *Client) {
		if match(c.UserID) {
			c.enqueue(b)
		}
	})
}

// KillSession disconnects the clients of a login session (or, with
// hash "", all clients of userID).
func (h *Hub) KillSession(userID, hash string) {
	h.each(func(c *Client) {
		if (hash != "" && c.SessionHash == hash) || (hash == "" && c.UserID == userID) {
			c.Kill()
		}
	})
}

func (h *Hub) publish(sid string, b []byte) {
	h.each(func(c *Client) {
		c.mu.Lock()
		ok := c.subs[sid]
		c.mu.Unlock()
		if ok {
			c.enqueue(b)
		}
	})
}

func (h *Hub) subscribers(sid string) int {
	n := 0
	h.each(func(c *Client) {
		c.mu.Lock()
		if c.subs[sid] {
			n++
		}
		c.mu.Unlock()
	})
	return n
}
