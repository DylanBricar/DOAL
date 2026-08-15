package announce

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

var helloLibtorrentOpenSSL = utls.ClientHelloID{Client: "Libtorrent", Version: "OpenSSL"}

// NewUTLSTransport creates an http.Transport that spoofs the TLS fingerprint
// to match a specific client. clientHello determines which fingerprint to use.
// Plain HTTP connections bypass uTLS and use the standard TCP dialer.
func NewUTLSTransport(clientHello utls.ClientHelloID) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}

			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}

			config := &utls.Config{
				ServerName: host,
				NextProtos: []string{"http/1.1"},
			}

			spec, err := clientHelloSpec(clientHello)
			if err != nil {
				conn.Close()
				return nil, err
			}
			tlsConn := utls.UClient(conn, config, utls.HelloCustom)
			if err := tlsConn.ApplyPreset(spec); err != nil {
				conn.Close()
				return nil, fmt.Errorf("applying TLS fingerprint: %w", err)
			}

			if err := tlsConn.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}

			return tlsConn, nil
		},
		ForceAttemptHTTP2:   false,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// ClientHelloSpecForEmulatedClient returns a fresh HTTP/1.1-only TLS spec.
// Libtorrent-based clients use an OpenSSL-style ordering instead of a browser
// parrot; other profiles retain their closest uTLS family without h2/ALPS.
func ClientHelloSpecForEmulatedClient(clientName string) (*utls.ClientHelloSpec, error) {
	return clientHelloSpec(ClientHelloForEmulatedClient(clientName))
}

func clientHelloSpec(clientHello utls.ClientHelloID) (*utls.ClientHelloSpec, error) {
	if clientHello.Client == helloLibtorrentOpenSSL.Client {
		return libtorrentOpenSSLSpec(), nil
	}
	spec, err := utls.UTLSIdToSpec(clientHello)
	if err != nil {
		return nil, fmt.Errorf("building TLS fingerprint %s: %w", clientHello.Str(), err)
	}
	forceHTTP11(&spec)
	return &spec, nil
}

func forceHTTP11(spec *utls.ClientHelloSpec) {
	extensions := make([]utls.TLSExtension, 0, len(spec.Extensions)+1)
	foundALPN := false
	for _, extension := range spec.Extensions {
		switch typed := extension.(type) {
		case *utls.ALPNExtension:
			typed.AlpnProtocols = []string{"http/1.1"}
			foundALPN = true
			extensions = append(extensions, typed)
		case *utls.ApplicationSettingsExtension:
			continue
		default:
			extensions = append(extensions, extension)
		}
	}
	if !foundALPN {
		extensions = append(extensions, &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}})
	}
	spec.Extensions = extensions
}

func libtorrentOpenSSLSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         utls.VersionTLS12,
		TLSVersMax:         utls.VersionTLS13,
		CompressionMethods: []uint8{0},
		CipherSuites: []uint16{
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				utls.PSSWithSHA384,
				utls.PKCS1WithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA512,
			}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
		},
	}
}

// ClientHelloForEmulatedClient returns the uTLS ClientHelloID that best matches
// the TLS stack used by the named BitTorrent client.
func ClientHelloForEmulatedClient(clientName string) utls.ClientHelloID {
	lower := strings.ToLower(clientName)
	switch {
	case strings.Contains(lower, "qbittorrent"), strings.Contains(lower, "deluge"):
		return helloLibtorrentOpenSSL
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
