package safeurl

import (
	"testing"
	"time"
)

// These cases are hermetic: IsSafeFetchURL only reaches net.LookupIP for
// hostnames, and Go resolves IP-literal hosts without touching the network,
// so every case below is decided from a literal IP or an early scheme/host
// check — no DNS required.
func TestIsSafeFetchURL(t *testing.T) {
	cases := []struct {
		name          string
		url           string
		allowLoopback bool
		want          bool
	}{
		{"unparseable", "://no-scheme", false, false},
		{"non-http scheme", "ftp://example.com/x", false, false},
		{"file scheme", "file:///etc/passwd", false, false},
		{"empty host", "http://", false, false},
		{"gce metadata ip", "http://169.254.169.254/latest/meta-data/", false, false},
		{"gce metadata host", "http://metadata.google.internal/", false, false},
		{"bare metadata host", "http://metadata/", false, false},
		{"loopback denied", "http://127.0.0.1:8080/", false, false},
		{"loopback allowed", "http://127.0.0.1:8080/", true, true},
		{"ipv6 loopback denied", "http://[::1]/", false, false},
		{"private rfc1918 denied", "http://10.0.0.1/", false, false},
		{"private rfc1918 allowed", "http://10.0.0.1/", true, true},
		{"private 192.168 denied", "http://192.168.1.1/", false, false},
		{"link-local denied", "http://169.254.10.10/", false, false},
		{"unspecified denied", "http://0.0.0.0/", false, false},
		{"public ip allowed", "http://8.8.8.8/", false, true},
		{"public https allowed", "https://1.1.1.1/", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSafeFetchURL(tc.url, tc.allowLoopback); got != tc.want {
				t.Errorf("IsSafeFetchURL(%q, %v) = %v, want %v", tc.url, tc.allowLoopback, got, tc.want)
			}
		})
	}
}

// TestSafeDialControl is the DNS-rebinding guard: the dial-time hook must
// reject a connection to an internal IP even though IsSafeFetchURL is bypassed
// here (simulating a hostname that resolved to a safe IP but re-resolved to an
// internal one at connect time).
func TestSafeDialControl(t *testing.T) {
	cases := []struct {
		addr          string
		allowLoopback bool
		wantErr       bool
	}{
		{"127.0.0.1:80", false, true},
		{"127.0.0.1:80", true, false},
		{"169.254.169.254:80", false, true}, // cloud metadata
		{"10.0.0.5:443", false, true},       // rfc1918
		{"192.168.1.10:443", false, true},
		{"0.0.0.0:80", false, true},
		{"8.8.8.8:443", false, false}, // public
		{"[::1]:80", false, true},     // ipv6 loopback
		{"not-an-addr", false, true},
	}
	for _, tc := range cases {
		ctrl := safeDialControl(tc.allowLoopback)
		err := ctrl("tcp", tc.addr, nil)
		if (err != nil) != tc.wantErr {
			t.Errorf("safeDialControl(%v)(%q) err=%v, wantErr=%v", tc.allowLoopback, tc.addr, err, tc.wantErr)
		}
	}
}

func TestSafeClientConstruction(t *testing.T) {
	c := SafeClient(5*time.Second, false)
	if c.Timeout != 5*time.Second {
		t.Errorf("SafeClient timeout = %v, want 5s", c.Timeout)
	}
	if c.Transport == nil {
		t.Fatal("SafeClient transport is nil")
	}
}
