package announce

import (
	"net/url"
	"strconv"
	"strings"
)

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
