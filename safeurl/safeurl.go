// Package safeurl provides SSRF guards for outbound URL fetches.
//
// It blocks non-http(s) schemes and destinations that resolve to loopback,
// link-local (cloud metadata, e.g. 169.254.169.254), private RFC1918/ULA, or
// unspecified addresses — preventing the agent from being directed at
// internal services or cloud metadata endpoints.
package safeurl

import (
	"net"
	"net/url"
	"strings"
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
