package server

import (
	"fmt"
	"sync"
)

// agentConn is one connected agent. Writes go through the send channel so a
// single goroutine owns the socket.
type agentConn struct {
	hostID   string
	remoteIP string
	send     chan []byte
	done     chan struct{}
}

type Hub struct {
	mu    sync.RWMutex
	conns map[string]*agentConn
}

func newHub() *Hub { return &Hub{conns: map[string]*agentConn{}} }

func (h *Hub) get(hostID string) *agentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[hostID]
}

// set registers a connection and returns the previous one, if any, so the
// caller can close it.
func (h *Hub) set(c *agentConn) *agentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.conns[c.hostID]
	h.conns[c.hostID] = c
	return old
}

func (h *Hub) remove(c *agentConn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.hostID] == c {
		delete(h.conns, c.hostID)
		return true
	}
	return false
}

// Bus fans out change notifications to dashboard SSE clients. Events only say
// what changed; the browser refetches.
type Bus struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func newBus() *Bus { return &Bus{subs: map[chan string]struct{}{}} }

func (b *Bus) subscribe() chan string {
	ch := make(chan string, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Bus) unsubscribe(ch chan string) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

func (b *Bus) publish(kind, id string) {
	b.publishRaw(fmt.Sprintf(`{"kind":%q,"id":%q}`, kind, id))
}

// publishRaw sends a prebuilt JSON message to every dashboard client.
func (b *Bus) publishRaw(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- msg:
		default: // slow client, it will resync on the next event
		}
	}
}
