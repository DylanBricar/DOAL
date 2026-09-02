package announce

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"doal/networkpolicy"
)

type netIPResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// IsSupportedTrackerURL reports whether raw is a usable HTTP(S) tracker URL.
// Tracker ownership is deliberately not inferred from or restricted by its
// hostname: deployments may use any public, private or local domain.
func IsSupportedTrackerURL(raw string) bool {
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return false
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return false
		}
	}
	return true
}

// resolveDialTargets resolves address once and returns only addresses that are
// safe to dial under the configured network policy. Returning resolved IPs
// also closes the DNS-rebinding window between validation and connection.
func resolveDialTargets(ctx context.Context, resolver netIPResolver, address string, allowPrivateNetworks bool) ([]string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.TrimSuffix(host, ".") == "" {
		return nil, fmt.Errorf("invalid network target")
	}
	if value, err := strconv.Atoi(port); err != nil || value < 1 || value > 65535 {
		return nil, fmt.Errorf("invalid network target port")
	}
	if allowPrivateNetworks {
		return []string{address}, nil
	}

	var addresses []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{literal}
	} else {
		addresses, err = resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolving network target: %w", err)
		}
	}

	targets := make([]string, 0, len(addresses))
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if !networkpolicy.IsPublicAddress(address) {
			continue
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		targets = append(targets, net.JoinHostPort(address.String(), port))
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("network target resolves only to private or special-use addresses")
	}
	return targets, nil
}

func validateTrackerNetworkTarget(ctx context.Context, raw string, allowPrivateNetworks bool) error {
	if allowPrivateNetworks {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parsing tracker target: %w", err)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	_, err = resolveDialTargets(ctx, net.DefaultResolver, net.JoinHostPort(u.Hostname(), port), false)
	return err
}
