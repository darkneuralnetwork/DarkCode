package server

// config_schema.go — serving the settings surface rather than hard-coding it.
//
// The Settings tab, the console and this API each used to decide independently
// which settings existed, and each decided differently: plan_depth reached the
// browser but not the console, air_gap and the cost limits reached neither.
// Nothing was broken by that, which is why it lasted — the cost was that every
// new setting needed three decisions and usually got one.
//
// The descriptors live on the config type. This endpoint hands them to the
// browser, so a new setting is one decision again.

import (
	"net/http"

	"github.com/darkcode/infra/config"
)

// handleConfigSchema serves the field descriptors so an interface can render
// the settings without hard-coding which ones exist.
//
// The Settings tab, the console and the API each used to decide that
// separately, and each decided differently. Serving the descriptors means a new
// setting is one decision rather than three, and config.TestEveryConfigFieldIsDescribed
// fails if a field is ever added without one.
func (s *Server) handleConfigSchema(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	fields := config.Fields()
	groups := make([]string, 0, 6)
	seen := map[string]bool{}
	for _, f := range fields {
		if !seen[f.Group] {
			seen[f.Group] = true
			groups = append(groups, f.Group)
		}
	}
	s.cfgMu.RLock()
	values := config.Values(s.cfg)
	s.cfgMu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"fields": fields,
		"groups": groups,
		// Values ride along so an interface renders the surface in one call.
		// Secrets are redacted by config.Values, next to the field, rather
		// than by each caller remembering to.
		"values": values,
		// Interfaces render primary by default and put the rest behind a
		// disclosure, so they need the count without walking the list.
		"primary_count": len(config.FieldsInTier(config.TierPrimary)),
	})
}
