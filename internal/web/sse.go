package web

// Hub fans state changes out to connected dashboards. Task 13 gives it the
// SSE transport; this is the shape the server depends on.
type Hub struct {
	ch chan int64
}

// NewHub returns a Hub.
func NewHub() *Hub { return &Hub{ch: make(chan int64, 64)} }

// Broadcast reports that a task changed. It never blocks.
func (h *Hub) Broadcast(taskID int64) {
	select {
	case h.ch <- taskID:
	default:
	}
}
