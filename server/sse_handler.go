package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send initial connection event
	fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"connected\",\"time\":\"%s\"}\n\n", time.Now().Format(time.RFC3339))
	flusher.Flush()

	// Subscribe to event broadcast (per-client channel; cleans up on exit).
	broadcast, unsub := s.emitter.Subscribe()
	// Track this client for GUI disconnect detection (Issue #4). onSSEConnect
	// cancels any pending resume-CLI grace timer; onSSEDisconnect (deferred)
	// arms it if this was the last client. Defer order is LIFO, so
	// onSSEDisconnect is declared FIRST and runs LAST — after unsub() has
	// removed this subscriber, so SubscriberCount() correctly reflects the
	// remaining clients.
	defer s.onSSEDisconnect()
	defer unsub()
	s.onSSEConnect()
	clientGone := r.Context().Done()

	// Heartbeat: write a lightweight SSE comment (": ping\n\n") every
	// ssePingInterval. Go cancels r.Context() when the client's TCP connection
	// closes, but for a streaming handler that only writes on events, a
	// half-open connection (network drop, browser killed without a clean FIN)
	// can go undetected until the next write. The ping forces a write, which
	// fails immediately on a broken socket, so the handler returns and the
	// disconnect is detected within one ping interval — well inside the GUI
	// resume grace window. It also keeps intermediate proxies from closing an
	// idle SSE connection.
	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()
	for {
		select {
		case <-clientGone:
			return
		case <-ping.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-broadcast:
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(data))
			flusher.Flush()
		}
	}
}

// handleEventHistory returns all past events.
func (s *Server) handleEventHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	history := s.emitter.History()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"events": history,
		"count":  len(history),
	})
}

// handleApprovals lists pending permission requests that are waiting for the
// user's decision (i.e. dangerous tool calls blocked in the dispatch path).
// The web UI polls this (and listens for "approval" SSE events) to show popups.
