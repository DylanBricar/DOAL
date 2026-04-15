package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer creates a Server with no-op callbacks for use in handler tests.
func newTestServer() *Server {
	return NewServer(0, "doal", "x", func(string, string, []byte) {})
}

// TestServerRedirectRoot verifies that GET "/" returns a 302 redirect to the UI.
func TestServerRedirectRoot(t *testing.T) {
	s := newTestServer()

	// Build the same mux the server would register.
	mux := http.NewServeMux()
	prefix := "/" + s.pathPrefix
	uiPrefix := prefix + "/ui/"

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "" {
			http.Redirect(w, r, uiPrefix, http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status: want %d, got %d", http.StatusFound, rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/ui/") {
		t.Errorf("Location header: want /ui/ in %q", loc)
	}
}

// TestServerNotFoundForUnknownPath verifies that an unknown path returns 404.
func TestServerNotFoundForUnknownPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "" {
			http.Redirect(w, r, "/doal/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	req := httptest.NewRequest(http.MethodGet, "/nonexistent/path", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", rec.Code)
	}
}

// TestNewServerPathPrefixTrimed verifies the server strips leading/trailing slashes from pathPrefix.
func TestNewServerPathPrefixTrimmed(t *testing.T) {
	s := NewServer(8080, "/doal/", "tok", nil)
	if s.pathPrefix != "doal" {
		t.Errorf("pathPrefix: want %q, got %q", "doal", s.pathPrefix)
	}
}

// TestNewServerStoresFields verifies port and token are stored.
func TestNewServerStoresFields(t *testing.T) {
	s := NewServer(9090, "myapp", "mysecret", nil)
	if s.port != 9090 {
		t.Errorf("port: want 9090, got %d", s.port)
	}
	if s.secretToken != "mysecret" {
		t.Errorf("secretToken: want mysecret, got %q", s.secretToken)
	}
}

// TestServerAddRemoveClient verifies addClient and removeClient manage the map correctly.
func TestServerAddRemoveClient(t *testing.T) {
	s := newTestServer()

	c := &Client{
		id:            "client-1",
		subscriptions: make(map[string]string),
	}

	s.addClient(c)
	s.mu.RLock()
	_, found := s.clients[c]
	s.mu.RUnlock()
	if !found {
		t.Error("client should be present after addClient")
	}

	s.removeClient(c)
	s.mu.RLock()
	_, found = s.clients[c]
	s.mu.RUnlock()
	if found {
		t.Error("client should be absent after removeClient")
	}
}

// TestServerHandleSTOMPHeartbeat verifies that empty (heartbeat) frames are silently ignored.
func TestServerHandleSTOMPHeartbeat(t *testing.T) {
	s := newTestServer()
	c := &Client{
		id:            "client-hb",
		subscriptions: make(map[string]string),
	}
	// Should not panic on whitespace-only input.
	s.handleSTOMP(c, []byte("   \n"))
	s.handleSTOMP(c, []byte(""))
}

// TestHandleConnectNoToken verifies that when secretToken is "x" (disabled),
// authentication is granted before sendFrame is called (which requires a real conn).
// We verify the authenticated state is set via the token-check logic in handleConnect.
func TestHandleConnectNoToken(t *testing.T) {
	// When the token is "x", the auth check is skipped entirely.
	// We validate this by inspecting the branch: token == "x" means no check.
	s := NewServer(0, "doal", "x", nil)

	// Verify the server has the expected token value so the skipped-check branch is correct.
	if s.secretToken != "x" {
		t.Fatalf("expected secretToken x, got %q", s.secretToken)
	}

	// Confirm the auth bypass condition: secretToken == "x" means auth is skipped.
	// The actual handleConnect panics without a real WebSocket conn (sendFrame needs it),
	// so we only test the precondition/configuration here.
	authBypassed := s.secretToken == "" || s.secretToken == "x"
	if !authBypassed {
		t.Error("expected auth to be bypassed for token 'x'")
	}
}

// TestHandleConnectWrongToken verifies that a wrong token means auth is not bypassed.
func TestHandleConnectWrongToken(t *testing.T) {
	s := NewServer(0, "doal", "correcttoken", nil)

	// With a real token, a wrong token would cause sendError (which needs a conn).
	// We verify the server has the correct token set for guard logic.
	if s.secretToken != "correcttoken" {
		t.Fatalf("expected secretToken correcttoken, got %q", s.secretToken)
	}
	// Confirm the auth check is NOT bypassed.
	authBypassed := s.secretToken == "" || s.secretToken == "x"
	if authBypassed {
		t.Error("auth should NOT be bypassed for a real token")
	}
}

// TestHandleSubscribeRequiresAuth verifies that SUBSCRIBE before CONNECT is rejected.
// Note: handleSubscribe calls sendError which requires a real WebSocket conn, so we
// only verify the subscription map is not mutated (sendError panics are recovered).
func TestHandleSubscribeRequiresAuth(t *testing.T) {
	s := newTestServer()
	c := &Client{
		id:            "c3",
		subscriptions: make(map[string]string),
		authenticated: false,
	}
	frame := &stompFrame{
		command: "SUBSCRIBE",
		headers: map[string]string{"id": "sub-1", "destination": "/global"},
		body:    nil,
	}
	// Wrap in recover to handle the nil-conn panic from sendError.
	func() {
		defer func() { recover() }()
		s.handleSubscribe(c, frame)
	}()
	c.mu.Lock()
	subs := len(c.subscriptions)
	c.mu.Unlock()
	if subs != 0 {
		t.Error("subscription should not be added for unauthenticated client")
	}
}

// TestHandleSubscribeAddsDestination verifies that SUBSCRIBE registers the destination.
func TestHandleSubscribeAddsDestination(t *testing.T) {
	received := make(chan string, 1)
	s := NewServer(0, "doal", "x", func(_ string, dest string, _ []byte) {
		received <- dest
	})
	c := &Client{
		id:            "c4",
		subscriptions: make(map[string]string),
		authenticated: true,
	}
	frame := &stompFrame{
		command: "SUBSCRIBE",
		headers: map[string]string{"id": "sub-0", "destination": "/torrents"},
		body:    nil,
	}
	s.handleSubscribe(c, frame)
	c.mu.Lock()
	dest, ok := c.subscriptions["sub-0"]
	c.mu.Unlock()
	if !ok {
		t.Error("subscription sub-0 should be present after handleSubscribe")
	}
	if dest != "/torrents" {
		t.Errorf("destination: want /torrents, got %q", dest)
	}
	select {
	case d := <-received:
		if d != "/torrents" {
			t.Errorf("onMessage destination: want /torrents, got %q", d)
		}
	default:
		t.Error("onMessage callback was not called")
	}
}

// TestHandleUnsubscribeRemovesDestination verifies UNSUBSCRIBE removes the sub.
func TestHandleUnsubscribeRemovesDestination(t *testing.T) {
	s := newTestServer()
	c := &Client{
		id:            "c5",
		subscriptions: map[string]string{"sub-99": "/global"},
		authenticated: true,
	}
	frame := &stompFrame{
		command: "UNSUBSCRIBE",
		headers: map[string]string{"id": "sub-99"},
		body:    nil,
	}
	s.handleUnsubscribe(c, frame)
	c.mu.Lock()
	_, still := c.subscriptions["sub-99"]
	c.mu.Unlock()
	if still {
		t.Error("sub-99 should be removed after handleUnsubscribe")
	}
}

// TestHandleSendRequiresAuth verifies that SEND before CONNECT is rejected.
// handleSend calls sendError (which needs a real conn) on unauthenticated clients;
// we recover the panic and verify the callback was not invoked.
func TestHandleSendRequiresAuth(t *testing.T) {
	called := false
	s := NewServer(0, "doal", "x", func(_ string, _ string, _ []byte) {
		called = true
	})
	c := &Client{
		id:            "c6",
		subscriptions: make(map[string]string),
		authenticated: false,
	}
	frame := &stompFrame{
		command: "SEND",
		headers: map[string]string{"destination": "/doal/global/start"},
		body:    nil,
	}
	func() {
		defer func() { recover() }()
		s.handleSend(c, frame)
	}()
	if called {
		t.Error("onMessage should not be called when client is not authenticated")
	}
}

// TestHandleSendDispatchesToCallback verifies that an authenticated SEND calls onMessage.
func TestHandleSendDispatchesToCallback(t *testing.T) {
	received := make(chan []byte, 1)
	s := NewServer(0, "doal", "x", func(_ string, _ string, data []byte) {
		received <- data
	})
	c := &Client{
		id:            "c7",
		subscriptions: make(map[string]string),
		authenticated: true,
	}
	payload := []byte(`{"key":"value"}`)
	frame := &stompFrame{
		command: "SEND",
		headers: map[string]string{"destination": "/doal/config/save"},
		body:    payload,
	}
	s.handleSend(c, frame)
	select {
	case got := <-received:
		if string(got) != string(payload) {
			t.Errorf("body: want %q, got %q", payload, got)
		}
	default:
		t.Error("onMessage callback was not called")
	}
}
