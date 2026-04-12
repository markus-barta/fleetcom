package sse

import "sync"

// Event is a typed SSE message (event name + JSON data).
type Event struct {
	Name string
	Data []byte
}

type Hub struct {
	mu      sync.RWMutex
	clients map[chan Event]struct{}
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[chan Event]struct{}),
	}
}

func (h *Hub) Subscribe() chan Event {
	ch := make(chan Event, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *Hub) Broadcast(name string, data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	evt := Event{Name: name, Data: data}
	for ch := range h.clients {
		select {
		case ch <- evt:
		default:
			// client too slow, drop message
		}
	}
}
