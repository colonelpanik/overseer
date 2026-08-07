package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// subscriberBuffer is how many pending events a single dashboard may lag by
// before events start being dropped for it. A dropped event is harmless: the
// page reloads on the next one, and reloading twice looks the same as once.
const subscriberBuffer = 8

// Hub fans task-change notifications out to connected dashboards.
type Hub struct {
	mu     sync.Mutex
	next   int
	subs   map[int]chan int64
	closed bool
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: map[int]chan int64{}}
}

// Subscribe registers a listener and returns its channel plus an
// unsubscribe function. The returned function is safe to call twice.
func (h *Hub) Subscribe() (<-chan int64, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	id := h.next
	h.next++
	ch := make(chan int64, subscriberBuffer)
	h.subs[id] = ch

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if c, ok := h.subs[id]; ok {
				delete(h.subs, id)
				close(c)
			}
		})
	}
}

// Broadcast reports that a task changed. It never blocks: a subscriber that
// is not keeping up simply misses events.
func (h *Hub) Broadcast(taskID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- taskID:
		default:
		}
	}
}

// handleEvents streams task changes as server-sent events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	events, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// A periodic comment keeps proxies and browsers from closing an idle
	// stream.
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case taskID, open := <-events:
			if !open {
				return
			}
			fmt.Fprintf(w, "event: change\ndata: %d\n\n", taskID)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
