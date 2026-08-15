package web

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"testing"
)

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
	if got := NewServer(5081, "doal", "real-secret", nil).listenAddress(); got != ":5081" {
		t.Fatalf("authenticated listen address=%q", got)
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
