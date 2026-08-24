package observability

import (
	"net/http"
	_ "net/http/pprof"
)

// StartProfiler launches an HTTP server exposing pprof endpoints.
func StartProfiler(addr string) error {
	return http.ListenAndServe(addr, nil)
}
