package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleSSE streams job state changes as Server-Sent Events.
//
// Protocol: each event is JSON JobEvent as data: line, event type "job".
// Client disconnect unsubscribes immediately (no leaked goroutine/channel).
func (a *API) handleSSE(w http.ResponseWriter, r *http.Request) {
	if a.Events == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "events hub not configured")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ch, unsub := a.Events.Subscribe()
	defer unsub()

	// Heartbeat keeps proxies from closing idle connections.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: job\ndata: %s\n\n", b)
			flusher.Flush()
		}
	}
}
