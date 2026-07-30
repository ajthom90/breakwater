package scheduler

import (
	"sync"
)

// JobEvent is a live job state change published for the web SSE stream (M2-S5).
// Emitted after successful catalog transitions (create, dispatch, progress,
// terminal). Subscribers must not block; the hub drops to full channels.
type JobEvent struct {
	JobID        string `json:"job_id"`
	MachineID    string `json:"machine_id"`
	Type         string `json:"type"`
	State        string `json:"state"`
	BytesRead    int64  `json:"bytes_read,omitempty"`
	BytesStored  int64  `json:"bytes_stored,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// EventHub fans out JobEvents to SSE / API subscribers.
// Safe for concurrent Publish and Subscribe. Client disconnect must call the
// returned unsubscribe func — tested in server/internal/web.
type EventHub struct {
	mu   sync.Mutex
	subs map[chan JobEvent]struct{}
}

// NewEventHub constructs an empty hub.
func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[chan JobEvent]struct{})}
}

// Subscribe registers a buffered subscriber. Call the returned function on
// client disconnect to free the channel and avoid goroutine/channel leaks.
func (h *EventHub) Subscribe() (<-chan JobEvent, func()) {
	if h == nil {
		ch := make(chan JobEvent)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan JobEvent, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	unsub := func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
	return ch, unsub
}

// Publish non-blockingly sends ev to all subscribers. Full buffers drop the
// event for that subscriber (prefer liveness over perfect delivery).
func (h *EventHub) Publish(ev JobEvent) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// slow consumer; drop
		}
	}
}

// SubscriberCount returns the number of live subscribers (tests).
func (h *EventHub) SubscriberCount() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
