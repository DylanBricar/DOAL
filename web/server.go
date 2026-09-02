package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxWebSocketMessageBytes  int64 = 1 << 20
	maxWebSocketClients             = 64
	maxSubscriptionsPerClient       = 128
	webSocketAuthTimeout            = 5 * time.Second
	webSocketIdleTimeout            = 30 * time.Second
	webSocketWriteTimeout           = 5 * time.Second
)

//go:embed static/*
var staticFS embed.FS

var clientIDCounter uint64

// Server is the HTTP/WebSocket server that serves the frontend and handles
// STOMP connections.
type Server struct {
	port        int
	pathPrefix  string
	secretToken string
	clients     map[*Client]bool
	mu          sync.RWMutex
	onMessage   func(clientID string, msgType string, data []byte)
	clientSlots chan struct{}

	upgrader websocket.Upgrader
}

// NewServer constructs a Server. The onMessage callback is invoked for every
// STOMP SEND frame and SUBSCRIBE frame received.
func NewServer(port int, pathPrefix, secretToken string, onMessage func(string, string, []byte)) *Server {
	s := &Server{
		port:        port,
		pathPrefix:  strings.Trim(pathPrefix, "/"),
		secretToken: secretToken,
		clients:     make(map[*Client]bool),
		onMessage:   onMessage,
		clientSlots: make(chan struct{}, maxWebSocketClients),
	}

	s.upgrader = websocket.Upgrader{
		CheckOrigin:  s.websocketOriginAllowed,
		Subprotocols: []string{"v12.stomp", "v11.stomp"},
	}

	return s
}

// Start registers HTTP routes and begins listening. Blocks until the server
// encounters a fatal error.
func (s *Server) Start() error {
	addr := s.listenAddress()
	fmt.Printf("server: listening on %s, UI at /%s/ui/\n", addr, s.pathPrefix)
	server := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return server.ListenAndServe()
}

// Handler returns the complete private-dashboard HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	prefix := "/" + s.pathPrefix

	// Serve embedded static files under /{prefix}/ui/
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "embedded UI unavailable", http.StatusInternalServerError)
		})
	}
	uiPrefix := prefix + "/ui/"
	mux.Handle(uiPrefix, http.StripPrefix(uiPrefix, http.FileServer(http.FS(staticSub))))

	// WebSocket endpoint at /{prefix} (both with and without trailing slash)
	mux.HandleFunc(prefix, s.handleWebSocket)
	mux.HandleFunc(prefix+"/", s.handleWebSocket)

	// Redirect root to UI
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "" {
			http.Redirect(w, r, uiPrefix, http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		headers.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com https://unpkg.com https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self' ws: wss:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		headers.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		headers.Set("Referrer-Policy", "no-referrer")
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("X-Frame-Options", "DENY")
		headers.Set("X-Robots-Tag", "noindex, nofollow, noarchive, nosnippet")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) listenAddress() string {
	if s.secretToken == "" || s.secretToken == "x" {
		return fmt.Sprintf("127.0.0.1:%d", s.port)
	}
	return fmt.Sprintf(":%d", s.port)
}

func websocketOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && strings.EqualFold(parsed.Host, r.Host)
}

func (s *Server) websocketOriginAllowed(r *http.Request) bool {
	if (s.secretToken == "" || s.secretToken == "x") && !isLoopbackHost(r.Host) {
		return false
	}
	return websocketOriginAllowed(r)
}

func isLoopbackHost(hostPort string) bool {
	host := hostPort
	if parsed, _, err := net.SplitHostPort(hostPort); err == nil {
		host = parsed
	}
	host = strings.Trim(strings.TrimSuffix(host, "."), "[]")
	return strings.EqualFold(host, "localhost") || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

// handleWebSocket upgrades an HTTP connection to WebSocket and runs the STOMP
// read loop for that client.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.reserveClientSlot() {
		http.Error(w, "too many WebSocket clients", http.StatusServiceUnavailable)
		return
	}
	defer s.releaseClientSlot()
	fmt.Printf("server: WebSocket upgrade request from %s for %s\n", r.RemoteAddr, r.URL.Path)
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("server: WebSocket upgrade failed: %v\n", err)
		return
	}
	conn.SetReadLimit(maxWebSocketMessageBytes)
	if err := conn.SetReadDeadline(time.Now().Add(webSocketAuthTimeout)); err != nil {
		_ = conn.Close()
		return
	}
	fmt.Printf("server: WebSocket connected: %s\n", r.RemoteAddr)

	id := fmt.Sprintf("client-%d", atomic.AddUint64(&clientIDCounter, 1))
	c := &Client{
		id:            id,
		conn:          conn,
		subscriptions: make(map[string]string),
	}

	s.addClient(c)
	defer s.removeClient(c)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		s.handleSTOMP(c, msg)
		c.mu.Lock()
		authenticated := c.authenticated
		c.mu.Unlock()
		if authenticated {
			if err := conn.SetReadDeadline(time.Now().Add(webSocketIdleTimeout)); err != nil {
				break
			}
		}
	}
}

func (s *Server) reserveClientSlot() bool {
	select {
	case s.clientSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseClientSlot() {
	select {
	case <-s.clientSlots:
	default:
	}
}

func (s *Server) addClient(c *Client) {
	s.mu.Lock()
	s.clients[c] = true
	s.mu.Unlock()
}

func (s *Server) removeClient(c *Client) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
}
