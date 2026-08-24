package server

// event_types_handler.go — serving the event-type registry to the web UI.
//
// core.EventTypes (infra/core/event_meta.go) is the one table mapping an
// event type to its icon/label/significance; the CLI reads it directly
// in-process, and this endpoint is how the browser reads the same table —
// the same "server owns it, both surfaces read it" shape as handleVerbs.

import (
	"net/http"

	"github.com/darkcode/infra/core"
)

// handleEventTypes lists every known event type's display metadata.
func (s *Server) handleEventTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_types": core.EventTypes})
}
