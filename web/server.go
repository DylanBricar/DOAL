package web

import (
	"embed"
	"fmt"
	"io/fs"
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
		CheckOrigin:  websocketOriginAllowed,
		Subprotocols: []string{"v12.stomp", "v11.stomp"},
	}

	return s
}

// Start registers HTTP routes and begins listening. Blocks until the server
// encounters a fatal error.
func (s *Server) Start() error {
	mux := http.NewServeMux()

	prefix := "/" + s.pathPrefix

	// Serve embedded static files under /{prefix}/ui/
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("server: creating static sub-fs: %w", err)
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

	addr := s.listenAddress()
	fmt.Printf("server: listening on %s, UI at %s/ui/\n", addr, prefix)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return server.ListenAndServe()
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
	expectedScheme := "http"
	if r.TLS != nil {
		expectedScheme = "https"
	}
	return strings.EqualFold(parsed.Scheme, expectedScheme) && strings.EqualFold(parsed.Host, r.Host)
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
	_ = conn.UnderlyingConn().SetDeadline(time.Time{})
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
