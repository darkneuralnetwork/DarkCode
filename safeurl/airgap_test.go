package safeurl

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Air gap is a setting the user opts into with the promise that nothing leaves
// the machine. It is enforced in the dialer's Control hook, which runs after
// DNS resolution with the address the kernel is about to connect to — so it
// covers redirects and rebinding, not just the URL that was checked.
//
// These tests never touch the network: an air-gapped dial fails at Control
// before any packet, and the allowed cases use a local listener.

func TestAirGapBlocksPublicAddresses(t *testing.T) {
	SetAirGap(true)
	t.Cleanup(func() { SetAirGap(false) })

	client := EgressClient(2 * time.Second)
	for _, addr := range []string{
		"http://8.8.8.8/",
		"http://1.1.1.1:80/",
		"https://93.184.216.34/",
	} {
		resp, err := client.Get(addr)
		if err == nil {
			resp.Body.Close()
			t.Errorf("%s connected while air-gapped", addr)
		}
	}
}

// TestAirGapStillAllowsLoopback. Local model servers must keep working — that
// is the documented point of allowing loopback and private ranges.
func TestAirGapStillAllowsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "local ok")
	}))
	defer srv.Close()

	SetAirGap(true)
	t.Cleanup(func() { SetAirGap(false) })

	resp, err := EgressClient(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("air gap blocked a loopback server: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// TestAirGapAppliesToBothClients. SafeClient and EgressClient exist for
// different reasons — SSRF protection versus user-configured endpoints — but
// air gap is a promise about the machine and holds for both.
func TestAirGapAppliesToBothClients(t *testing.T) {
	SetAirGap(true)
	t.Cleanup(func() { SetAirGap(false) })

	for name, c := range map[string]*http.Client{
		"EgressClient": EgressClient(2 * time.Second),
		"SafeClient":   SafeClient(2*time.Second, false),
	} {
		if resp, err := c.Get("http://8.8.8.8/"); err == nil {
			resp.Body.Close()
			t.Errorf("%s reached a public address while air-gapped", name)
		}
	}
}

// TestIsSafeFetchURLRefusesPublicHostsWhenAirGapped covers the pre-check that
// callers use before dialling.
func TestIsSafeFetchURLRefusesPublicHostsWhenAirGapped(t *testing.T) {
	SetAirGap(true)
	t.Cleanup(func() { SetAirGap(false) })

	if IsSafeFetchURL("http://8.8.8.8/", false) {
		t.Error("a public literal was accepted while air-gapped")
	}
	// Loopback stays reachable so a local server can still be attached.
	if !IsSafeFetchURL("http://127.0.0.1:8080/", true) {
		t.Error("loopback was refused while air-gapped")
	}
}

// TestAirGapIsOffByDefault. It is a deliberate opt-in; defaulting to on would
// break every cloud model on a fresh install.
func TestAirGapIsOffByDefault(t *testing.T) {
	if AirGapped() {
		t.Fatal("air gap is on without being set (test ordering leak, or a bad default)")
	}
}

// TestMetadataEndpointsAreAlwaysRefused, air gap or not. A cloud metadata
// service hands out credentials to anything that can reach it.
func TestMetadataEndpointsAreAlwaysRefused(t *testing.T) {
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://metadata.google.internal/",
		"http://metadata/",
	} {
		if IsSafeFetchURL(u, false) {
			t.Errorf("%s was allowed", u)
		}
		if IsSafeFetchURL(u, true) {
			t.Errorf("%s was allowed even with loopback permitted", u)
		}
	}
}

// TestSafeDialRejectsLinkLocalLiterals — the metadata address again, this time
// as a raw dial so a redirect cannot reach it either.
func TestSafeDialRejectsLinkLocalLiterals(t *testing.T) {
	if resp, err := SafeClient(2*time.Second, false).Get("http://169.254.169.254/"); err == nil {
		resp.Body.Close()
		t.Error("the metadata address was dialled")
	}
	// Sanity: the address really is link-local, so the test is checking what
	// it claims to.
	if ip := net.ParseIP("169.254.169.254"); ip == nil || !ip.IsLinkLocalUnicast() {
		t.Fatal("test premise is wrong")
	}
}
