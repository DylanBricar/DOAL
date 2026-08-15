package dht

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	participationInterval = 15 * time.Minute
	pendingQueryTTL       = 2 * time.Minute
	peerTTL               = 30 * time.Minute
	maxPendingQueries     = 4096
	maxPeersPerTorrent    = 256
	maxTrackedPeerHashes  = 512
	transactionIDBytes    = 4
)

type Peer struct {
	IP   string
	Port int
}

type pendingQuery struct {
	kind      string
	infoHash  [20]byte
	addr      *net.UDPAddr
	createdAt time.Time
}

type storedPeer struct {
	Peer
	seenAt time.Time
}

// Node is a bounded BEP 5 node for a configured tracker test network. It
// responds to KRPC queries and actively performs get_peers/announce_peer
// exchanges with explicitly configured bootstrap nodes.
type Node struct {
	id       [20]byte
	secret   [20]byte
	port     int
	peerPort int
	conn     *net.UDPConn

	bootstrapMu sync.RWMutex
	bootstrap   []*net.UDPAddr
	allowedIPs  map[string]struct{}
	torrents    sync.Map

	pendingMu sync.Mutex
	pending   map[string]pendingQuery
	peersMu   sync.RWMutex
	peers     map[string]map[string]storedPeer
	sendMu    sync.Mutex
	txCounter uint32

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewNode(port int) *Node {
	node := &Node{
		port:       port,
		pending:    make(map[string]pendingQuery),
		peers:      make(map[string]map[string]storedPeer),
		allowedIPs: make(map[string]struct{}),
		stop:       make(chan struct{}),
	}
	_, _ = rand.Read(node.id[:])
	_, _ = rand.Read(node.secret[:])
	return node
}

func (n *Node) ConfigureNetwork(peerPort int, endpoints []string) error {
	if peerPort <= 0 || peerPort > 65535 {
		return fmt.Errorf("dht: invalid peer port %d", peerPort)
	}
	resolved := make([]*net.UDPAddr, 0, len(endpoints))
	allowedIPs := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if !validEndpoint(endpoint) {
			return fmt.Errorf("dht: bootstrap %q is not a valid host:port endpoint", endpoint)
		}
		addr, err := net.ResolveUDPAddr("udp", endpoint)
		if err != nil {
			return fmt.Errorf("dht: resolving bootstrap %q: %w", endpoint, err)
		}
		resolved = append(resolved, addr)
		allowedIPs[addr.IP.String()] = struct{}{}
	}
	n.peerPort = peerPort
	n.bootstrapMu.Lock()
	n.bootstrap = resolved
	n.allowedIPs = allowedIPs
	n.bootstrapMu.Unlock()
	return nil
}

func validEndpoint(endpoint string) bool {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || strings.TrimSuffix(host, ".") == "" {
		return false
	}
	value, err := strconv.Atoi(port)
	return err == nil && value >= 1 && value <= 65535
}

func (n *Node) AddTorrent(infoHashHex string) {
	if raw, err := hex.DecodeString(infoHashHex); err == nil && len(raw) == 20 {
		n.torrents.Store(strings.ToLower(infoHashHex), true)
	}
}

func (n *Node) RemoveTorrent(infoHashHex string) {
	hashHex := strings.ToLower(infoHashHex)
	n.torrents.Delete(hashHex)
	n.peersMu.Lock()
	delete(n.peers, hashHex)
	n.peersMu.Unlock()
	n.pendingMu.Lock()
	for key, pending := range n.pending {
		if hex.EncodeToString(pending.infoHash[:]) == hashHex {
			delete(n.pending, key)
		}
	}
	n.pendingMu.Unlock()
}

func (n *Node) Start() error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("0.0.0.0:%d", n.port))
	if err != nil {
		return fmt.Errorf("dht: resolving listen address: %w", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("dht: listening on UDP port %d: %w", n.port, err)
	}
	n.conn = conn
	n.port = conn.LocalAddr().(*net.UDPAddr).Port

	n.wg.Add(2)
	go n.readLoop()
	go n.participationLoop()
	return nil
}

func (n *Node) Addr() *net.UDPAddr {
	if n.conn == nil {
		return nil
	}
	addr := *n.conn.LocalAddr().(*net.UDPAddr)
	if addr.IP == nil || addr.IP.IsUnspecified() {
		addr.IP = net.ParseIP("127.0.0.1")
	}
	return &addr
}

func (n *Node) Stop() {
	n.stopOnce.Do(func() {
		close(n.stop)
		if n.conn != nil {
			_ = n.conn.Close()
		}
	})
	n.wg.Wait()
	n.pendingMu.Lock()
	clear(n.pending)
	n.pendingMu.Unlock()
	n.peersMu.Lock()
	clear(n.peers)
	n.peersMu.Unlock()
}

func (n *Node) participationLoop() {
	defer n.wg.Done()
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-timer.C:
			n.ParticipateOnce()
			timer.Reset(participationInterval)
		}
	}
}

func (n *Node) ParticipateOnce() {
	n.pruneState(time.Now())
	if n.conn == nil {
		return
	}
	n.bootstrapMu.RLock()
	bootstraps := append([]*net.UDPAddr(nil), n.bootstrap...)
	n.bootstrapMu.RUnlock()

	var hashes [][20]byte
	n.torrents.Range(func(key, _ any) bool {
		raw, err := hex.DecodeString(key.(string))
		if err == nil && len(raw) == 20 {
			var hash [20]byte
			copy(hash[:], raw)
			hashes = append(hashes, hash)
		}
		return true
	})

	for _, addr := range bootstraps {
		n.sendPing(addr)
		for _, hash := range hashes {
			n.sendGetPeers(addr, hash)
		}
	}
}

func (n *Node) Peers(infoHashHex string) []Peer {
	n.pruneState(time.Now())
	n.peersMu.RLock()
	defer n.peersMu.RUnlock()
	stored := n.peers[strings.ToLower(infoHashHex)]
	result := make([]Peer, 0, len(stored))
	for _, storedPeer := range stored {
		result = append(result, storedPeer.Peer)
	}
	return result
}

func (n *Node) readLoop() {
	defer n.wg.Done()
	buf := make([]byte, 65536)
	for {
		nread, addr, err := n.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-n.stop:
				return
			default:
				continue
			}
		}
		packet := append([]byte(nil), buf[:nread]...)
		n.handleMessage(packet, addr)
	}
}

func (n *Node) handleMessage(data []byte, addr *net.UDPAddr) {
	if !n.isAllowedIP(addr.IP) {
		return
	}
	raw := string(data)
	switch {
	case strings.Contains(raw, "1:y1:q"):
		n.handleQuery(raw, addr)
	case strings.Contains(raw, "1:y1:r"):
		n.handleResponse(raw)
	}
}

func (n *Node) isAllowedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	n.bootstrapMu.RLock()
	_, allowed := n.allowedIPs[ip.String()]
	n.bootstrapMu.RUnlock()
	return allowed
}

func (n *Node) handleQuery(raw string, addr *net.UDPAddr) {
	txID := extractBencodeString(raw, "1:t")
	if txID == "" {
		return
	}
	switch {
	case strings.Contains(raw, "4:ping"):
		n.replyID(addr, txID)
	case strings.Contains(raw, "9:find_node"):
		n.replyFindNode(addr, txID)
	case strings.Contains(raw, "9:get_peers"):
		infoHash := extractBencodeString(raw, "9:info_hash")
		n.replyGetPeers(addr, txID, infoHash)
	case strings.Contains(raw, "13:announce_peer"):
		n.handleAnnouncePeer(addr, txID, raw)
	}
}

func (n *Node) handleResponse(raw string) {
	txID := extractBencodeString(raw, "1:t")
	n.pruneState(time.Now())
	n.pendingMu.Lock()
	pending, ok := n.pending[txID]
	delete(n.pending, txID)
	n.pendingMu.Unlock()
	if !ok || pending.kind != "get_peers" {
		return
	}
	token := extractBencodeString(raw, "5:token")
	if token != "" {
		n.sendAnnouncePeer(pending.addr, pending.infoHash, token)
	}
}

func (n *Node) handleAnnouncePeer(addr *net.UDPAddr, txID, raw string) {
	infoHash := extractBencodeString(raw, "9:info_hash")
	token := extractBencodeString(raw, "5:token")
	port, ok := extractBencodeInt(raw, "4:port")
	if len(infoHash) != 20 || !ok || port <= 0 || port > 65535 || token != n.tokenFor(addr.IP) {
		return
	}
	hashHex := hex.EncodeToString([]byte(infoHash))
	n.storePeer(hashHex, Peer{IP: addr.IP.String(), Port: port}, time.Now())
	n.replyID(addr, txID)
}

func (n *Node) sendPing(addr *net.UDPAddr) {
	tx := n.newTransaction("ping", [20]byte{}, addr)
	var packet bytes.Buffer
	packet.WriteString("d1:ad2:id20:")
	packet.Write(n.id[:])
	packet.WriteString(fmt.Sprintf("e1:q4:ping1:t%d:", len(tx)))
	packet.Write(tx)
	packet.WriteString("1:y1:qe")
	n.send(addr, packet.Bytes())
}

func (n *Node) sendGetPeers(addr *net.UDPAddr, infoHash [20]byte) {
	tx := n.newTransaction("get_peers", infoHash, addr)
	var packet bytes.Buffer
	packet.WriteString("d1:ad2:id20:")
	packet.Write(n.id[:])
	packet.WriteString("9:info_hash20:")
	packet.Write(infoHash[:])
	packet.WriteString(fmt.Sprintf("e1:q9:get_peers1:t%d:", len(tx)))
	packet.Write(tx)
	packet.WriteString("1:y1:qe")
	n.send(addr, packet.Bytes())
}

func (n *Node) sendAnnouncePeer(addr *net.UDPAddr, infoHash [20]byte, token string) {
	tx := n.newTransaction("announce_peer", infoHash, addr)
	var packet bytes.Buffer
	packet.WriteString("d1:ad2:id20:")
	packet.Write(n.id[:])
	packet.WriteString("9:info_hash20:")
	packet.Write(infoHash[:])
	packet.WriteString(fmt.Sprintf("4:porti%de5:token%d:", n.peerPort, len(token)))
	packet.WriteString(token)
	packet.WriteString(fmt.Sprintf("e1:q13:announce_peer1:t%d:", len(tx)))
	packet.Write(tx)
	packet.WriteString("1:y1:qe")
	n.send(addr, packet.Bytes())
}

func (n *Node) newTransaction(kind string, infoHash [20]byte, addr *net.UDPAddr) []byte {
	now := time.Now()
	for attempt := 0; attempt < 16; attempt++ {
		tx := make([]byte, transactionIDBytes)
		_, _ = rand.Read(tx)
		if n.reserveTransaction(tx, pendingQuery{kind: kind, infoHash: infoHash, addr: addr, createdAt: now}) {
			return tx
		}
	}
	for {
		tx := make([]byte, transactionIDBytes)
		binary.BigEndian.PutUint32(tx, atomic.AddUint32(&n.txCounter, 1))
		if n.reserveTransaction(tx, pendingQuery{kind: kind, infoHash: infoHash, addr: addr, createdAt: now}) {
			return tx
		}
	}
}

func (n *Node) reserveTransaction(tx []byte, pending pendingQuery) bool {
	key := string(tx)
	n.pendingMu.Lock()
	defer n.pendingMu.Unlock()
	n.prunePendingLocked(pending.createdAt)
	if _, exists := n.pending[key]; exists {
		return false
	}
	if len(n.pending) >= maxPendingQueries {
		n.evictOldestPendingLocked()
	}
	n.pending[key] = pending
	return true
}

func (n *Node) pruneState(now time.Time) {
	n.pendingMu.Lock()
	n.prunePendingLocked(now)
	n.pendingMu.Unlock()

	n.peersMu.Lock()
	for hashHex, peers := range n.peers {
		for key, peer := range peers {
			if now.Sub(peer.seenAt) > peerTTL {
				delete(peers, key)
			}
		}
		if len(peers) == 0 {
			delete(n.peers, hashHex)
		}
	}
	n.peersMu.Unlock()
}

func (n *Node) prunePendingLocked(now time.Time) {
	for key, pending := range n.pending {
		if pending.createdAt.IsZero() || now.Sub(pending.createdAt) > pendingQueryTTL {
			delete(n.pending, key)
		}
	}
	for len(n.pending) > maxPendingQueries {
		n.evictOldestPendingLocked()
	}
}

func (n *Node) evictOldestPendingLocked() {
	var oldestKey string
	var oldestTime time.Time
	for key, pending := range n.pending {
		if oldestKey == "" || pending.createdAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = pending.createdAt
		}
	}
	if oldestKey != "" {
		delete(n.pending, oldestKey)
	}
}

func (n *Node) storePeer(hashHex string, peer Peer, seenAt time.Time) {
	hashHex = strings.ToLower(hashHex)
	key := net.JoinHostPort(peer.IP, strconv.Itoa(peer.Port))
	n.peersMu.Lock()
	defer n.peersMu.Unlock()

	if n.peers[hashHex] == nil {
		if len(n.peers) >= maxTrackedPeerHashes {
			n.evictOldestPeerHashLocked()
		}
		n.peers[hashHex] = make(map[string]storedPeer)
	}
	peers := n.peers[hashHex]
	if _, exists := peers[key]; !exists && len(peers) >= maxPeersPerTorrent {
		oldestKey := ""
		var oldestTime time.Time
		for candidate, stored := range peers {
			if oldestKey == "" || stored.seenAt.Before(oldestTime) {
				oldestKey = candidate
				oldestTime = stored.seenAt
			}
		}
		delete(peers, oldestKey)
	}
	peers[key] = storedPeer{Peer: peer, seenAt: seenAt}
}

func (n *Node) evictOldestPeerHashLocked() {
	oldestHash := ""
	var oldestTime time.Time
	for hashHex, peers := range n.peers {
		latest := time.Time{}
		for _, stored := range peers {
			if stored.seenAt.After(latest) {
				latest = stored.seenAt
			}
		}
		if oldestHash == "" || latest.Before(oldestTime) {
			oldestHash = hashHex
			oldestTime = latest
		}
	}
	if oldestHash != "" {
		delete(n.peers, oldestHash)
	}
}

func (n *Node) replyID(addr *net.UDPAddr, txID string) {
	var inner bytes.Buffer
	inner.WriteString("d2:id20:")
	inner.Write(n.id[:])
	inner.WriteByte('e')
	n.send(addr, buildBencodeReply(inner.Bytes(), txID))
}

func (n *Node) replyFindNode(addr *net.UDPAddr, txID string) {
	var inner bytes.Buffer
	inner.WriteString("d2:id20:")
	inner.Write(n.id[:])
	// Bootstrap addresses are not routing-table entries until their real node
	// IDs are known. Returning an empty compact-node list is preferable to
	// inventing IDs that contradict the KRPC endpoint.
	inner.WriteString("5:nodes0:")
	inner.WriteByte('e')
	n.send(addr, buildBencodeReply(inner.Bytes(), txID))
}

func (n *Node) replyGetPeers(addr *net.UDPAddr, txID, infoHash string) {
	var inner bytes.Buffer
	inner.WriteString("d2:id20:")
	inner.Write(n.id[:])
	token := n.tokenFor(addr.IP)
	inner.WriteString(fmt.Sprintf("5:token%d:", len(token)))
	inner.WriteString(token)

	hashHex := hex.EncodeToString([]byte(infoHash))
	peers := n.Peers(hashHex)
	if len(peers) == 0 {
		inner.WriteString("5:nodes0:")
	} else {
		inner.WriteString("6:valuesl")
		for _, peer := range peers {
			if compact := compactPeer(peer); len(compact) == 6 {
				inner.WriteString("6:")
				inner.Write(compact)
			}
		}
		inner.WriteByte('e')
	}
	inner.WriteByte('e')
	n.send(addr, buildBencodeReply(inner.Bytes(), txID))
}

func compactPeer(peer Peer) []byte {
	ip := net.ParseIP(peer.IP).To4()
	if ip == nil || peer.Port <= 0 || peer.Port > 65535 {
		return nil
	}
	return append(ip, byte(peer.Port>>8), byte(peer.Port))
}

func (n *Node) tokenFor(ip net.IP) string {
	payload := append(append([]byte(nil), ip...), n.secret[:]...)
	hash := sha1.Sum(payload)
	return string(hash[:8])
}

func buildBencodeReply(innerDict []byte, txID string) []byte {
	var packet bytes.Buffer
	packet.WriteString("d1:r")
	packet.Write(innerDict)
	packet.WriteString(fmt.Sprintf("1:t%d:", len(txID)))
	packet.WriteString(txID)
	packet.WriteString("1:y1:re")
	return packet.Bytes()
}

func (n *Node) send(addr *net.UDPAddr, data []byte) {
	if n.conn == nil {
		return
	}
	n.sendMu.Lock()
	defer n.sendMu.Unlock()
	if err := n.conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return
	}
	if _, err := n.conn.WriteToUDP(data, addr); err != nil {
		slog.Warn("dht: send failed", "addr", addr, "err", err)
	}
}

func extractBencodeString(raw, key string) string {
	idx := strings.Index(raw, key)
	if idx < 0 {
		return ""
	}
	rest := raw[idx+len(key):]
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 {
		return ""
	}
	length, err := strconv.Atoi(rest[:colon])
	if err != nil || length < 0 || colon+1+length > len(rest) {
		return ""
	}
	return rest[colon+1 : colon+1+length]
}

func extractBencodeInt(raw, key string) (int, bool) {
	idx := strings.Index(raw, key+"i")
	if idx < 0 {
		return 0, false
	}
	rest := raw[idx+len(key)+1:]
	end := strings.IndexByte(rest, 'e')
	if end <= 0 {
		return 0, false
	}
	value, err := strconv.Atoi(rest[:end])
	return value, err == nil
}
