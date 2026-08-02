// Package httpx is the only place in DarkCode that constructs an HTTP client.
//
// safeurl already had the right controls — an SSRF guard and air-gap
// enforcement in a dialer Control hook, which correctly defeats DNS rebinding
// because it sees the address the kernel is about to connect to. The problem
// was that using them was opt-in: twenty call sites reached for
// http.DefaultClient or a bare &http.Client{} instead, so the guarantees held
// wherever someone had remembered and nowhere else.
//
// Most of those were not sloppy, they were reasonable-looking. A model
// downloader "just fetching a release from GitHub" has no obvious reason to
// think about SSRF. But air_gap: true is documented as refusing every
// connection that leaves the machine, and it did not refuse that one.
//
// So the client is centralised and the exception is made loud: a CI grep fails
// the build on http.DefaultClient or &http.Client{ outside this package. The
// point is not that the helper is better code — it is that "did this call site
// remember?" stops being a question anyone has to ask.
package httpx

import (
	"net/http"
	"time"

	"github.com/darkcode/safeurl"
)

// Intent says what a caller is reaching for, which decides how much of the
// address space it may reach. Three intents, because there are exactly three
// kinds of outbound call in this system and they need different rules.
type Intent int

const (
	// Fetch is for URLs a model, a web page or a tool definition chose. Full
	// SSRF rules: loopback, link-local (cloud metadata) and private ranges are
	// all refused, at dial time and on every redirect hop. This is the default
	// when in doubt.
	Fetch Intent = iota

	// Egress is for endpoints the USER configured — an LLM provider, an MCP
	// server, a self-hosted vLLM box. Private addresses are legitimate here, so
	// the SSRF rules do not apply, but air-gap still does: the user choosing an
	// endpoint does not override the user saying "nothing leaves this machine".
	Egress

	// Local is for talking to something on this machine: a local model server,
	// a health endpoint. Loopback and private are permitted; the wider internet
	// is not, so a misconfiguration that points a "local" client at a public
	// host fails instead of quietly working.
	Local
)

// Default timeouts per intent. A model download needs minutes; a liveness probe
// against localhost should give up in seconds.
const (
	DefaultFetchTimeout  = 60 * time.Second
	DefaultEgressTimeout = 5 * time.Minute
	DefaultLocalTimeout  = 10 * time.Second
)

// Client returns the client for an intent, with that intent's default timeout.
func Client(i Intent) *http.Client {
	switch i {
	case Egress:
		return safeurl.EgressClient(DefaultEgressTimeout)
	case Local:
		return safeurl.SafeClient(DefaultLocalTimeout, true)
	default:
		return safeurl.SafeClient(DefaultFetchTimeout, false)
	}
}

// ClientTimeout is Client with an explicit timeout, for callers that know their
// own bound — a 500ms hardware probe, or a multi-gigabyte model download.
func ClientTimeout(i Intent, timeout time.Duration) *http.Client {
	switch i {
	case Egress:
		return safeurl.EgressClient(timeout)
	case Local:
		return safeurl.SafeClient(timeout, true)
	default:
		return safeurl.SafeClient(timeout, false)
	}
}

// NoTimeout is for streaming transfers whose duration cannot be predicted —
// a model download over a slow link, an SSE stream. The dial-time guards still
// apply; only the overall deadline is lifted, so callers must bound the work
// themselves with a context.
func NoTimeout(i Intent) *http.Client {
	return ClientTimeout(i, 0)
}
