package server

// verbs_handler.go — serving the strategy verbs to the web UI.
//
// The composer has always shown a "/ for Commands" hint with nothing behind it,
// and the verbs themselves were reachable only from the console. Both surfaces
// now read package verb, and this endpoint is how the browser reads it: the
// list cannot drift from the one the handler actually parses, because there is
// only one list.

import (
	"net/http"

	"github.com/darkcode/kernel/verb"
)

type verbInfo struct {
	Verb string `json:"verb"` // with the leading slash, ready to insert
	Name string `json:"name"`
	Help string `json:"help"`
}

// handleVerbs lists the strategy verbs, cheapest first.
func (s *Server) handleVerbs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	names := verb.Names()
	out := make([]verbInfo, 0, len(names))
	for _, n := range names {
		st, ok := verb.Lookup(n)
		if !ok {
			continue
		}
		out = append(out, verbInfo{Verb: "/" + st.Name, Name: st.Name, Help: st.Help})
	}
	writeJSON(w, http.StatusOK, map[string]any{"verbs": out})
}
