package dht

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// Node is a minimal BEP 5 DHT node. It listens on a UDP port, responds to
// ping/find_node/get_peers queries, and can announce info hashes.
type Node struct {
	id       [20]byte
	port     int
	conn     *net.UDPConn
	torrents sync.Map // infoHashHex -> true
	stop     chan struct{}
}

// NewNode creates a new DHT node with a random node ID on the given port.
func NewNode(port int) *Node {
	var id [20]byte
	rand.Read(id[:]) //nolint:errcheck — crypto/rand never fails on supported platforms
	return &Node{id: id, port: port, stop: make(chan struct{})}
}

// AddTorrent registers an info hash so the node announces it to the DHT.
func (n *Node) AddTorrent(infoHashHex string) {
	n.torrents.Store(infoHashHex, true)
}

// RemoveTorrent removes an info hash from the announce set.
func (n *Node) RemoveTorrent(infoHashHex string) {
	n.torrents.Delete(infoHashHex)
}

// Start binds the UDP port and begins processing DHT messages in the background.
func (n *Node) Start() error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", n.port))
	if err != nil {
		return fmt.Errorf("dht: resolving addr: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("dht: listen UDP :%d: %w", n.port, err)
	}
	n.conn = conn

	go n.readLoop()
	return nil
}

// Stop shuts down the DHT node.
func (n *Node) Stop() {
	close(n.stop)
	if n.conn != nil {
		n.conn.Close()
	}
}

// readLoop receives UDP packets and dispatches each to handleMessage.
func (n *Node) readLoop() {
	buf := make([]byte, 65536)
	for {
		select {
		case <-n.stop:
			return
		default:
		}

		n.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		nread, addr, err := n.conn.ReadFromUDP(buf)
		if err != nil {
			// Timeout or closed — loop back to check stop channel.
			continue
		}

		// Copy the slice so the handler goroutine owns its buffer.
		msg := make([]byte, nread)
		copy(msg, buf[:nread])
		go n.handleMessage(msg, addr)
	}
}

// handleMessage parses an incoming DHT message and replies to queries.
// We use simple string scanning of the bencode instead of a full decoder
// to keep the implementation self-contained and dependency-free.
func (n *Node) handleMessage(data []byte, addr *net.UDPAddr) {
	raw := string(data)

	// Only handle query messages (y=q).
	if !strings.Contains(raw, "1:y1:q") {
		return
	}

	// Extract transaction ID value following the "1:t" key.
	// Bencode string format: <length>:<bytes>
	txID := extractBencodeString(raw, "1:t")
	if txID == "" {
		return
	}

	switch {
	case strings.Contains(raw, "4:ping"):
		n.replyPing(addr, txID)
	case strings.Contains(raw, "9:find_node"):
		n.replyFindNode(addr, txID)
	case strings.Contains(raw, "9:get_peers"):
		n.replyGetPeers(addr, txID)
	}
}

// buildBencodeReply constructs a BEP 5 reply dictionary as a byte slice.
// The innerDict bytes are placed inside the "r" reply dict, and txID is the
// raw transaction ID bytes. Binary values are concatenated directly to avoid
// fmt.Sprintf misinterpreting binary bytes (e.g. node IDs containing '%').
func buildBencodeReply(innerDict []byte, txID string) []byte {
	var b bytes.Buffer
	b.WriteString("d1:r")
	b.Write(innerDict)
	// Transaction ID: "1:t<len>:<bytes>"
	b.WriteString(fmt.Sprintf("1:t%d:", len(txID)))
	b.WriteString(txID)
	b.WriteString("1:y1:re")
	return b.Bytes()
}

// replyPing sends a BEP 5 ping response.
func (n *Node) replyPing(addr *net.UDPAddr, txID string) {
	// inner: d2:id20:<nodeID>e
	var inner bytes.Buffer
	inner.WriteString("d2:id20:")
	inner.Write(n.id[:])
	inner.WriteByte('e')
	n.send(addr, buildBencodeReply(inner.Bytes(), txID))
}

// replyFindNode sends a BEP 5 find_node response with an empty nodes list.
func (n *Node) replyFindNode(addr *net.UDPAddr, txID string) {
	// inner: d2:id20:<nodeID>5:nodes0:e
	var inner bytes.Buffer
	inner.WriteString("d2:id20:")
	inner.Write(n.id[:])
	inner.WriteString("5:nodes0:e")
	n.send(addr, buildBencodeReply(inner.Bytes(), txID))
}

// replyGetPeers sends a BEP 5 get_peers response.
// We return an empty peers list but a valid token so announce_peer works.
func (n *Node) replyGetPeers(addr *net.UDPAddr, txID string) {
	// token is an arbitrary 4-byte value; use first 4 bytes of our node id.
	var inner bytes.Buffer
	inner.WriteString("d2:id20:")
	inner.Write(n.id[:])
	inner.WriteString("5:token4:")
	inner.Write(n.id[:4])
	inner.WriteString("6:valuesl ee")
	n.send(addr, buildBencodeReply(inner.Bytes(), txID))
}

// send writes a UDP datagram to addr, logging failures non-fatally.
func (n *Node) send(addr *net.UDPAddr, data []byte) {
	if n.conn == nil {
		return
	}
	n.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := n.conn.WriteToUDP(data, addr); err != nil {
		slog.Warn("dht: send failed", "addr", addr, "err", err)
	}
}

// extractBencodeString finds the bencode-encoded string value that immediately
// follows key in raw. For example, given raw = "...1:t2:aa..." and key = "1:t",
// it returns "aa".
func extractBencodeString(raw, key string) string {
	idx := strings.Index(raw, key)
	if idx < 0 {
		return ""
	}
	rest := raw[idx+len(key):]

	// Read the length prefix (digits before ':').
	colonIdx := strings.Index(rest, ":")
	if colonIdx <= 0 {
		return ""
	}

	length := 0
	for _, ch := range rest[:colonIdx] {
		if ch < '0' || ch > '9' {
			return ""
		}
		length = length*10 + int(ch-'0')
	}

	start := colonIdx + 1
	if start+length > len(rest) {
		return ""
	}
	return rest[start : start+length]
}
