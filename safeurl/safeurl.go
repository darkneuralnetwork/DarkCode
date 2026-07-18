// Package safeurl provides SSRF guards for outbound URL fetches.
//
// It blocks non-http(s) schemes and destinations that resolve to loopback,
// link-local (cloud metadata, e.g. 169.254.169.254), private RFC1918/ULA, or
// unspecified addresses — preventing the agent from being directed at
// internal services or cloud metadata endpoints.
package safeurl

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// IsSafeFetchURL reports whether a URL may be fetched on behalf of a user.
// allowLoopback lifts the loopback/private restriction for explicitly local
// use cases.
func IsSafeFetchURL(rawURL string, allowLoopback bool) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	lh := strings.ToLower(host)
	if lh == "metadata.google.internal" || lh == "169.254.169.254" || lh == "metadata" {
		return false
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ipSafe(ip, allowLoopback) {
			return false
		}
	}
	return true
}

// safeDialControl returns a net.Dialer Control hook that rejects connections
// whose resolved destination IP is unsafe. This closes the TOCTOU / DNS-
// rebinding gap that IsSafeFetchURL alone cannot: IsSafeFetchURL resolves the
// host once, but the HTTP client re-resolves at dial time, so a hostname whose
// DNS flips to 127.0.0.1 / 169.254.169.254 between the two lookups (or across
// a redirect) would otherwise slip through. The Control hook runs with the
// actual address the kernel is about to connect to, after resolution, so the
// check is authoritative.
func safeDialControl(allowLoopback bool) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("safeurl: cannot parse dial address %q: %w", address, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			// Control always receives a resolved IP literal; a non-IP here is
			// unexpected, so fail closed.
			return fmt.Errorf("safeurl: dial address %q is not an IP", host)
		}
		if !ipSafe(ip, allowLoopback) {
			return fmt.Errorf("safeurl: blocked connection to disallowed address %s (SSRF guard)", ip)
		}
		return nil
	}
}

// SafeTransport returns an *http.Transport whose dials are validated at connect
// time against the SSRF rules, defeating DNS-rebinding. Use it for any HTTP
// client that fetches user- or model-supplied URLs. Pair it with
// IsSafeFetchURL for an early, friendlier rejection before the request is made.
func SafeTransport(allowLoopback bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   safeDialControl(allowLoopback),
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// SafeClient returns an *http.Client using SafeTransport with the given
// per-request timeout. Redirects are followed but each redirect hop is
// re-validated by the dial-time Control hook, so a redirect to an internal
// address is blocked just like a direct request.
func SafeClient(timeout time.Duration, allowLoopback bool) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: SafeTransport(allowLoopback),
	}
}

func ipSafe(ip net.IP, allowLoopback bool) bool {
	if ip.IsLoopback() {
		return allowLoopback
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if ip.IsUnspecified() {
		return false
	}
	if ip.IsPrivate() {
		return allowLoopback
	}
	return true
}
