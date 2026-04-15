package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// Client represents a single connected WebSocket/STOMP client.
type Client struct {
	id            string
	conn          *websocket.Conn
	subscriptions map[string]string // subID -> destination
	mu            sync.Mutex
	authenticated bool
	username      string
}

// stompFrame holds a parsed STOMP frame.
type stompFrame struct {
	command string
	headers map[string]string
	body    []byte
}

// parseFrame parses a raw STOMP frame from bytes.
// STOMP frame format: COMMAND\nheader:value\n...\n\nbody\0
func parseFrame(data []byte) (*stompFrame, error) {
	// Strip trailing null byte(s)
	raw := strings.TrimRight(string(data), "\x00")

	// Split on double newline separating headers from body.
	// STOMP uses \n or \r\n line endings.
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")

	headerBodySplit := strings.SplitN(normalized, "\n\n", 2)
	headerSection := headerBodySplit[0]
	body := ""
	if len(headerBodySplit) == 2 {
		body = headerBodySplit[1]
	}

	lines := strings.Split(headerSection, "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, fmt.Errorf("stomp: empty frame")
	}

	frame := &stompFrame{
		command: strings.TrimSpace(lines[0]),
		headers: make(map[string]string),
		body:    []byte(body),
	}

	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		frame.headers[key] = val
	}

	return frame, nil
}

// marshalFrame serializes a STOMP frame to bytes.
func marshalFrame(command string, headers map[string]string, body []byte) []byte {
	var sb strings.Builder
	sb.WriteString(command)
	sb.WriteByte('\n')
	for k, v := range headers {
		sb.WriteString(k)
		sb.WriteByte(':')
		sb.WriteString(v)
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	sb.Write(body)
	sb.WriteByte('\x00')
	return []byte(sb.String())
}

// sendFrame writes a STOMP frame to the client's WebSocket connection.
func (c *Client) sendFrame(command string, headers map[string]string, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, marshalFrame(command, headers, body))
}

// sendError sends a STOMP ERROR frame and closes the connection.
// Uses its own lock rather than sendFrame to atomically write and close.
func (c *Client) sendError(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	headers := map[string]string{"message": message}
	_ = c.conn.WriteMessage(websocket.TextMessage, marshalFrame("ERROR", headers, []byte(message)))
	c.conn.Close()
}

// SendToAll sends a STOMP MESSAGE frame to all clients subscribed to destination.
func (s *Server) SendToAll(destination string, payload interface{}) {
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("stomp: marshalling payload for %q: %v\n", destination, err)
		return
	}

	s.mu.RLock()
	targets := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		targets = append(targets, c)
	}
	s.mu.RUnlock()

	fmt.Printf("stomp: SendToAll dest=%q clients=%d bodyLen=%d\n", destination, len(targets), len(body))

	for _, c := range targets {
		c.mu.Lock()
		if !c.authenticated {
			c.mu.Unlock()
			continue
		}
		// Find the subscription ID for this destination.
		subID := ""
		for sid, dest := range c.subscriptions {
			if dest == destination {
				subID = sid
				break
			}
		}
		c.mu.Unlock()

		if subID == "" {
			continue
		}

		headers := map[string]string{
			"subscription":   subID,
			"destination":    destination,
			"content-type":   "application/json",
			"content-length": fmt.Sprintf("%d", len(body)),
		}
		if err := c.sendFrame("MESSAGE", headers, body); err != nil {
			fmt.Printf("stomp: sending to client %s: %v\n", c.id, err)
			c.conn.Close()
			s.removeClient(c)
		}
	}
}

// handleSTOMP processes incoming STOMP frames from the client read loop.
func (s *Server) handleSTOMP(c *Client, raw []byte) {
	// Ignore heartbeat frames
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return
	}
	frame, err := parseFrame(raw)
	if err != nil {
		fmt.Printf("stomp: parse error from client %s: %v\n", c.id, err)
		return
	}

	switch frame.command {
	case "CONNECT", "STOMP":
		s.handleConnect(c, frame)
	case "SUBSCRIBE":
		s.handleSubscribe(c, frame)
	case "UNSUBSCRIBE":
		s.handleUnsubscribe(c, frame)
	case "SEND":
		s.handleSend(c, frame)
	case "DISCONNECT":
		s.handleDisconnect(c, frame)
	default:
		fmt.Printf("stomp: unknown command %q from client %s\n", frame.command, c.id)
	}
}

func (s *Server) handleConnect(c *Client, frame *stompFrame) {
	// Check auth token if one is configured.
	if s.secretToken != "" && s.secretToken != "x" {
		token := frame.headers["X-Joal-Auth-Token"]
		if token != s.secretToken {
			c.sendError("Authentication failed")
			return
		}
	}

	c.mu.Lock()
	c.authenticated = true
	c.username = frame.headers["X-Joal-Username"]
	if c.username == "" {
		c.username = c.id
	}
	c.mu.Unlock()

	headers := map[string]string{
		"version":    "1.2",
		"heart-beat": "0,0",
	}
	if err := c.sendFrame("CONNECTED", headers, nil); err != nil {
		fmt.Printf("stomp: sending CONNECTED to %s: %v\n", c.id, err)
	}
}

func (s *Server) handleSubscribe(c *Client, frame *stompFrame) {
	c.mu.Lock()
	if !c.authenticated {
		c.mu.Unlock()
		c.sendError("Not authenticated")
		return
	}

	subID := frame.headers["id"]
	dest := frame.headers["destination"]
	if subID == "" || dest == "" {
		c.mu.Unlock()
		return
	}

	c.subscriptions[subID] = dest
	c.mu.Unlock()

	// Notify application layer AFTER releasing the lock to avoid deadlock.
	if s.onMessage != nil {
		s.onMessage(c.id, dest, nil)
	}
}

func (s *Server) handleUnsubscribe(c *Client, frame *stompFrame) {
	c.mu.Lock()
	defer c.mu.Unlock()

	subID := frame.headers["id"]
	delete(c.subscriptions, subID)
}

func (s *Server) handleSend(c *Client, frame *stompFrame) {
	c.mu.Lock()
	auth := c.authenticated
	c.mu.Unlock()

	if !auth {
		fmt.Printf("stomp: SEND rejected: client %s not authenticated\n", c.id)
		c.sendError("Not authenticated")
		return
	}

	dest := frame.headers["destination"]
	if dest == "" {
		fmt.Printf("stomp: SEND rejected: empty destination from %s\n", c.id)
		return
	}

	fmt.Printf("stomp: SEND from %s to %s (body=%d bytes)\n", c.id, dest, len(frame.body))
	if s.onMessage != nil {
		s.onMessage(c.id, dest, frame.body)
	}
}

func (s *Server) handleDisconnect(c *Client, frame *stompFrame) {
	receipt := frame.headers["receipt"]
	if receipt != "" {
		headers := map[string]string{"receipt-id": receipt}
		_ = c.sendFrame("RECEIPT", headers, nil)
	}
	s.removeClient(c)
	c.conn.Close()
}
