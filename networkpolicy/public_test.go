package networkpolicy

import (
	"net/netip"
	"testing"
)

func TestIsPublicAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		address string
		want    bool
	}{
		{address: "8.8.8.8", want: true},
		{address: "2606:4700:4700::1111", want: true},
		{address: "127.0.0.1", want: false},
		{address: "10.0.0.1", want: false},
		{address: "100.64.0.1", want: false},
		{address: "198.18.0.1", want: false},
		{address: "203.0.113.1", want: false},
		{address: "2001:db8::1", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.address, func(t *testing.T) {
			t.Parallel()
			if got := IsPublicAddress(netip.MustParseAddr(tc.address)); got != tc.want {
				t.Fatalf("IsPublicAddress(%s) = %v, want %v", tc.address, got, tc.want)
			}
		})
	}
}
