package announce

import (
	"reflect"
	"testing"

	utls "github.com/refraction-networking/utls"
)

func TestClientHelloSpecsOnlyAdvertiseHTTP11(t *testing.T) {
	t.Parallel()

	clients := []string{
		"qBittorrent/5.1.4",
		"Deluge/2.1.1 libtorrent/1.2.15.0",
		"uTorrent/354(111783378)(44498)",
		"Transmission/3.00",
	}
	for _, client := range clients {
		client := client
		t.Run(client, func(t *testing.T) {
			t.Parallel()
			spec, err := ClientHelloSpecForEmulatedClient(client)
			if err != nil {
				t.Fatalf("ClientHelloSpecForEmulatedClient: %v", err)
			}
			var protocols []string
			for _, extension := range spec.Extensions {
				switch typed := extension.(type) {
				case *utls.ALPNExtension:
					protocols = typed.AlpnProtocols
				case *utls.ApplicationSettingsExtension:
					t.Fatal("HTTP/2 application settings must not be advertised")
				}
			}
			if !reflect.DeepEqual(protocols, []string{"http/1.1"}) {
				t.Fatalf("ALPN protocols = %#v, want only HTTP/1.1", protocols)
			}
		})
	}
}

func TestLibtorrentSpecUsesOpenSSLStyleCipherOrder(t *testing.T) {
	t.Parallel()

	spec, err := ClientHelloSpecForEmulatedClient("qBittorrent/5.1.4")
	if err != nil {
		t.Fatalf("ClientHelloSpecForEmulatedClient: %v", err)
	}
	wantPrefix := []uint16{
		utls.TLS_AES_256_GCM_SHA384,
		utls.TLS_CHACHA20_POLY1305_SHA256,
		utls.TLS_AES_128_GCM_SHA256,
	}
	if len(spec.CipherSuites) < len(wantPrefix) || !reflect.DeepEqual(spec.CipherSuites[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("cipher prefix = %#v, want OpenSSL-style %#v", spec.CipherSuites, wantPrefix)
	}
}
