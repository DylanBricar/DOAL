package web

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPHandlerAddsPrivateDashboardSecurityHeaders(t *testing.T) {
	t.Parallel()

	server := NewServer(0, "doal", "x", nil)
	request := httptest.NewRequest(http.MethodGet, "/doal/ui/", nil)
	request.Host = "127.0.0.1"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	wantHeaders := map[string]string{
		"X-Robots-Tag":            "noindex",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "frame-ancestors 'none'",
	}
	for name, want := range wantHeaders {
		if got := response.Header().Get(name); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", name, got, want)
		}
	}
}

func TestLocalModeRejectsDNSRebindingOrigin(t *testing.T) {
	t.Parallel()

	server := NewServer(0, "doal", "x", nil)
	request := httptest.NewRequest(http.MethodGet, "http://evil.example/doal", nil)
	request.Host = "evil.example"
	request.Header.Set("Origin", "http://evil.example")
	if server.websocketOriginAllowed(request) {
		t.Fatal("local mode accepted a non-loopback Host and Origin")
	}
}

func TestEmbeddedUISupportsAuthenticatedRemoteMode(t *testing.T) {
	t.Parallel()

	html, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded UI: %v", err)
	}
	page := string(html)
	if !strings.Contains(page, "X-Joal-Auth-Token") {
		t.Fatal("embedded UI never sends the configured WebSocket auth token")
	}
	if !strings.Contains(page, "type=\"password\"") {
		t.Fatal("embedded UI has no secure token input for remote mode")
	}
	for _, remote := range []string{"cdn.tailwindcss.com", "unpkg.com", "cdn.jsdelivr.net", "fonts.googleapis.com", "new Chart("} {
		if strings.Contains(page, remote) {
			t.Errorf("embedded UI still depends on %q", remote)
		}
	}
	for _, unsafe := range []string{"doalAuthToken", "Notification.requestPermission", "innerHTML=savedHistory"} {
		if strings.Contains(page, unsafe) {
			t.Errorf("embedded UI retains unsafe browser behavior %q", unsafe)
		}
	}
	if !strings.Contains(page, "activeInfoHashes") {
		t.Fatal("embedded UI does not render the backend's active slot snapshot")
	}
}

func TestWebSocketOriginPolicy(t *testing.T) {
	same := httptest.NewRequest("GET", "https://dashboard.example.com/doal", nil)
	same.Host = "dashboard.example.com"
	same.Header.Set("Origin", "https://dashboard.example.com")
	if !websocketOriginAllowed(same) {
		t.Fatal("same-origin WebSocket request was rejected")
	}

	cross := httptest.NewRequest("GET", "https://dashboard.example.com/doal", nil)
	cross.Host = "dashboard.example.com"
	cross.Header.Set("Origin", "https://evil.example")
	if websocketOriginAllowed(cross) {
		t.Fatal("cross-origin WebSocket request was accepted")
	}

	downgrade := httptest.NewRequest(http.MethodGet, "https://dashboard.example.com/doal", nil)
	downgrade.Host = "dashboard.example.com"
	downgrade.Header.Set("Origin", "http://dashboard.example.com")
	if websocketOriginAllowed(downgrade) {
		t.Fatal("HTTPS WebSocket request accepted a downgraded HTTP origin")
	}

	nonBrowser := httptest.NewRequest("GET", "http://127.0.0.1/doal", nil)
	if !websocketOriginAllowed(nonBrowser) {
		t.Fatal("request without Origin should remain available to local clients")
	}
}

func TestParseFrameRejectsOversizedInput(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, int(maxWebSocketMessageBytes+1))
	if _, err := parseFrame(payload); err == nil {
		t.Fatal("oversized STOMP frame was accepted")
	}
}

func TestAuthTokenValidationRequiresExactMatchUnlessExplicitlyDisabled(t *testing.T) {
	if !authTokenValid("", "anything") {
		t.Fatal("empty embedded-test token should bypass authentication")
	}
	if !authTokenValid("x", "x") {
		t.Fatal("matching literal token x was rejected")
	}
	if !authTokenValid("x", "wrong") {
		t.Fatal("documented local-only token x did not disable authentication")
	}
	if authTokenValid("secret", "secreu") {
		t.Fatal("incorrect token was accepted")
	}
}

func TestUnauthenticatedWebServerBindsLoopbackOnly(t *testing.T) {
	if got := NewServer(5081, "doal", "x", nil).listenAddress(); got != "127.0.0.1:5081" {
		t.Fatalf("local-only listen address=%q", got)
	}
	if got := NewServer(5081, "doal", "real-secret", nil).listenAddress(); got != "127.0.0.1:5081" {
		t.Fatalf("authenticated server must remain loopback-only, got %q", got)
	}
}

func TestWebClientSlotsAndSubscriptionsAreBounded(t *testing.T) {
	server := NewServer(5081, "doal", "secret", nil)
	for i := 0; i < maxWebSocketClients; i++ {
		if !server.reserveClientSlot() {
			t.Fatalf("client slot %d was unexpectedly rejected", i)
		}
	}
	if server.reserveClientSlot() {
		t.Fatal("WebSocket client limit admitted one extra connection")
	}
	server.releaseClientSlot()
	if !server.reserveClientSlot() {
		t.Fatal("released WebSocket client slot was not reusable")
	}

	client := &Client{id: "bounded", authenticated: true, subscriptions: make(map[string]string)}
	for i := 0; i < maxSubscriptionsPerClient+10; i++ {
		server.handleSubscribe(client, &stompFrame{
			command: "SUBSCRIBE",
			headers: map[string]string{
				"id":          fmt.Sprintf("sub-%d", i),
				"destination": "/global",
			},
		})
	}
	if got := len(client.subscriptions); got != maxSubscriptionsPerClient {
		t.Fatalf("subscriptions=%d, limit=%d", got, maxSubscriptionsPerClient)
	}
}
