package announce

import (
	"crypto/tls"
	"net"
	"net/http"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// NewUTLSTransport creates an http.Transport that spoofs the TLS fingerprint
// to match a specific client. clientHello determines which fingerprint to use.
// Plain HTTP connections bypass uTLS and use the standard TCP dialer.
func NewUTLSTransport(clientHello utls.ClientHelloID) *http.Transport {
	return &http.Transport{
		DialTLS: func(network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}

			conn, err := net.Dial(network, addr)
			if err != nil {
				return nil, err
			}

			config := &utls.Config{
				ServerName: host,
			}

			tlsConn := utls.UClient(conn, config, clientHello)

			if err := tlsConn.Handshake(); err != nil {
				conn.Close()
				return nil, err
			}

			// Check if server negotiated HTTP/2 — if so, close and retry with standard TLS
			if tlsConn.ConnectionState().NegotiatedProtocol == "h2" {
				tlsConn.Close()
				// Fallback to standard crypto/tls without fingerprint spoofing
				stdConn, err := net.Dial(network, addr)
				if err != nil {
					return nil, err
				}
				stdTLS := tls.Client(stdConn, &tls.Config{
					ServerName: host,
					NextProtos: []string{"http/1.1"},
				})
				if err := stdTLS.Handshake(); err != nil {
					stdConn.Close()
					return nil, err
				}
				return stdTLS, nil
			}

			return tlsConn, nil
		},
		ForceAttemptHTTP2: false,
	}
}

// ClientHelloForEmulatedClient returns the uTLS ClientHelloID that best matches
// the TLS stack used by the named BitTorrent client.
func ClientHelloForEmulatedClient(clientName string) utls.ClientHelloID {
	lower := strings.ToLower(clientName)
	switch {
	case strings.Contains(lower, "qbittorrent"), strings.Contains(lower, "deluge"):
		// libtorrent → OpenSSL; Chrome fingerprint is the closest available match.
		return utls.HelloChrome_Auto
	case strings.Contains(lower, "utorrent"), strings.Contains(lower, "bittorrent"):
		// Windows SChannel; iOS fingerprint approximates SChannel behaviour.
		return utls.HelloIOS_Auto
	case strings.Contains(lower, "transmission"):
		// libcurl + OpenSSL on Linux/macOS; Firefox is a reasonable stand-in.
		return utls.HelloFirefox_Auto
	default:
		return utls.HelloChrome_Auto
	}
}
