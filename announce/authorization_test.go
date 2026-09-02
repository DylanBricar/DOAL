package announce

import (
	"context"
	"net"
	"testing"
)

func TestSupportedTrackerURLAcceptsAnyHTTPDomain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "first HTTPS domain", raw: "https://tracker.example.com/announce", want: true},
		{name: "different HTTPS domain", raw: "https://tracker.example.net:8443/announce", want: true},
		{name: "HTTP domain", raw: "http://tracker.example.org/announce", want: true},
		{name: "localhost", raw: "http://localhost:8080/announce", want: true},
		{name: "public IP", raw: "https://192.0.2.1/announce", want: true},
		{name: "userinfo confusion", raw: "https://tracker.example.com@evil.example/announce", want: false},
		{name: "unsupported scheme", raw: "udp://tracker.example.com:6969/announce", want: false},
		{name: "missing host", raw: "https:///announce", want: false},
		{name: "invalid port", raw: "https://tracker.example.com:99999/announce", want: false},
		{name: "malformed URL", raw: "://tracker.example.com", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSupportedTrackerURL(tc.raw); got != tc.want {
				t.Fatalf("IsSupportedTrackerURL(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestResolveDialTargetsRejectsPrivateNetworksByDefault(t *testing.T) {
	t.Parallel()

	if _, err := resolveDialTargets(context.Background(), net.DefaultResolver, "127.0.0.1:8080", false); err == nil {
		t.Fatal("private loopback target was accepted without explicit opt-in")
	}
	targets, err := resolveDialTargets(context.Background(), net.DefaultResolver, "127.0.0.1:8080", true)
	if err != nil {
		t.Fatalf("explicit private-network opt-in was rejected: %v", err)
	}
	if len(targets) != 1 || targets[0] != "127.0.0.1:8080" {
		t.Fatalf("opt-in targets = %q, want original loopback address", targets)
	}
}

func TestPublicNetworkPolicyRejectsSpecialUseRanges(t *testing.T) {
	t.Parallel()

	for _, address := range []string{"100.64.0.1:80", "198.18.0.1:80", "203.0.113.9:80", "[2001:db8::1]:443"} {
		if _, err := resolveDialTargets(context.Background(), net.DefaultResolver, address, false); err == nil {
			t.Errorf("special-use address %s was accepted", address)
		}
	}
}
