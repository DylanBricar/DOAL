package peerwire

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// The optional piece provider is restricted to the isolated tracker lab. It
// fetches complete pieces, verifies their torrent SHA-1, and retains them in a
// bounded cache. Requests without a verifiable source fail closed.

const (
	proxyBlockSize            = 16 * 1024       // 16 KiB standard request block
	proxyDialTimeout          = 6 * time.Second // per-peer TCP connect timeout
	proxyPieceTimeout         = 25 * time.Second
	proxyMaxPeerTries         = 8       // seeds attempted before giving up on a piece
	proxyMaxOutstanding       = 64      // request-pipeline window
	proxyMaxMessageLen        = 1 << 20 // reject peer-wire messages larger than 1 MiB
	defaultProxyCacheLimit    = int64(64 << 20)
	maxProxyConcurrentFetches = 8
	maxProxyPeers             = 512

	msgChoke      byte = 0
	msgInterested byte = 2
	msgKeepAlive  byte = 0xff // sentinel id for a 0-length keep-alive
)

var (
	errProxyNoPeers   = errors.New("proxy: no seeds known for torrent")
	errProxyNotFound  = errors.New("proxy: torrent not registered")
	errProxyBadPiece  = errors.New("proxy: seed served data that failed SHA-1")
	errProxyEmptyMeta = errors.New("proxy: torrent has no piece metadata")
)

// proxyTorrent holds the leeching state for one torrent.
type proxyTorrent struct {
	infoHash    [20]byte
	peerID      []byte
	pieceHashes [][20]byte
	pieceLength int64
	totalSize   int64

	stateMu sync.RWMutex
	peers   []Peer
	fetched map[int][]byte // index -> verified piece bytes

	fetchMu  sync.Mutex
	inflight map[int]*pieceFetch
	closed   bool
}

type pieceFetch struct {
	done chan struct{}
	data []byte
	err  error
}

type proxyCacheKey struct {
	torrent *proxyTorrent
	index   int
}

type proxyCacheEntry struct {
	key  proxyCacheKey
	size int64
}

// Peer is a real seed address the proxy can leech verified pieces from.
type Peer struct {
	IP   string
	Port int
}

// PieceProxy fetches and SHA-1 verifies pieces from real swarm seeds on demand.
type PieceProxy struct {
	mu       sync.RWMutex
	torrents map[string]*proxyTorrent // infoHashHex -> state

	cacheMu      sync.Mutex
	cacheLimit   int64
	cachedBytes  int64
	cacheLRU     *list.List
	cacheEntries map[proxyCacheKey]*list.Element
	fetchSlots   chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	closeOnce    sync.Once
}

// NewPieceProxy creates an empty proxy.
func NewPieceProxy() *PieceProxy {
	return newPieceProxyWithCacheLimit(defaultProxyCacheLimit)
}

func newPieceProxyWithCacheLimit(limit int64) *PieceProxy {
	if limit < 0 {
		limit = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PieceProxy{
		torrents:     make(map[string]*proxyTorrent),
		cacheLimit:   limit,
		cacheLRU:     list.New(),
		cacheEntries: make(map[proxyCacheKey]*list.Element),
		fetchSlots:   make(chan struct{}, maxProxyConcurrentFetches),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// RegisterTorrent adds the piece metadata the proxy needs to fetch and verify.
func (p *PieceProxy) RegisterTorrent(infoHashHex string, infoHash [20]byte, peerID []byte, pieceHashes [][20]byte, pieceLength, totalSize int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.torrents[infoHashHex]; ok {
		return
	}
	p.torrents[infoHashHex] = &proxyTorrent{
		infoHash:    infoHash,
		peerID:      append([]byte(nil), peerID...),
		pieceHashes: append([][20]byte(nil), pieceHashes...),
		pieceLength: pieceLength,
		totalSize:   totalSize,
		fetched:     make(map[int][]byte),
		inflight:    make(map[int]*pieceFetch),
	}
}

// SetPeers updates the seed pool for a torrent from the latest tracker announce.
func (p *PieceProxy) SetPeers(infoHashHex string, peers []Peer) {
	p.mu.RLock()
	pt := p.torrents[infoHashHex]
	p.mu.RUnlock()
	if pt == nil {
		return
	}
	if len(peers) > maxProxyPeers {
		peers = peers[:maxProxyPeers]
	}
	pt.stateMu.Lock()
	pt.peers = append([]Peer(nil), peers...)
	pt.stateMu.Unlock()
}

// Unregister drops all proxy state for a torrent.
func (p *PieceProxy) Unregister(infoHashHex string) {
	p.mu.Lock()
	pt := p.torrents[infoHashHex]
	delete(p.torrents, infoHashHex)
	p.mu.Unlock()
	if pt == nil {
		return
	}
	pt.fetchMu.Lock()
	pt.closed = true
	pt.fetchMu.Unlock()
	p.removeTorrentCache(pt)
}

// Close drops all torrent and cache state. In-flight fetches are allowed to
// finish, but their results are discarded.
func (p *PieceProxy) Close() {
	p.closeOnce.Do(func() {
		p.cancel()
		p.mu.Lock()
		torrents := make([]*proxyTorrent, 0, len(p.torrents))
		for _, pt := range p.torrents {
			torrents = append(torrents, pt)
		}
		clear(p.torrents)
		p.mu.Unlock()
		for _, pt := range torrents {
			pt.fetchMu.Lock()
			pt.closed = true
			pt.fetchMu.Unlock()
		}
		p.cacheMu.Lock()
		for _, pt := range torrents {
			pt.stateMu.Lock()
			clear(pt.fetched)
			pt.stateMu.Unlock()
		}
		p.cacheLRU.Init()
		clear(p.cacheEntries)
		p.cachedBytes = 0
		p.cacheMu.Unlock()
	})
}

// GetBlock returns [begin, begin+length) of piece index, fetching and verifying
// the whole piece from a seed on first access and serving from cache afterward.
func (p *PieceProxy) GetBlock(infoHashHex string, index, begin, length int) ([]byte, error) {
	p.mu.RLock()
	pt := p.torrents[infoHashHex]
	p.mu.RUnlock()
	if pt == nil {
		return nil, errProxyNotFound
	}

	piece, err := p.piece(pt, index)
	if err != nil {
		return nil, err
	}
	if begin < 0 || begin >= len(piece) || length <= 0 {
		return nil, fmt.Errorf("proxy: begin %d out of range for piece %d (len %d)", begin, index, len(piece))
	}
	if length > len(piece)-begin {
		return nil, fmt.Errorf("proxy: block length %d crosses piece %d boundary", length, index)
	}
	end := begin + length
	return piece[begin:end], nil
}

// piece returns a verified piece from cache, fetching it if necessary. The fetch
// is serialized per torrent so a burst of block requests triggers one download.
func (p *PieceProxy) piece(pt *proxyTorrent, index int) ([]byte, error) {
	select {
	case <-p.ctx.Done():
		return nil, p.ctx.Err()
	default:
	}
	cached, ok := p.cachedPiece(pt, index)
	if ok {
		return cached, nil
	}

	pt.fetchMu.Lock()
	if pt.closed {
		pt.fetchMu.Unlock()
		return nil, errProxyNotFound
	}
	if pending := pt.inflight[index]; pending != nil {
		pt.fetchMu.Unlock()
		<-pending.done
		return pending.data, pending.err
	}
	pending := &pieceFetch{done: make(chan struct{})}
	pt.inflight[index] = pending
	pt.fetchMu.Unlock()
	pt.stateMu.RLock()
	peers := append([]Peer(nil), pt.peers...)
	pt.stateMu.RUnlock()

	var data []byte
	var err error
	select {
	case p.fetchSlots <- struct{}{}:
		data, err = fetchPiece(p.ctx, pt, peers, index)
		<-p.fetchSlots
	case <-p.ctx.Done():
		err = p.ctx.Err()
	}
	pt.fetchMu.Lock()
	if err == nil {
		if !pt.closed {
			p.storeCachedPiece(pt, index, data)
		} else {
			err = errProxyNotFound
			data = nil
		}
	}
	pending.data = data
	pending.err = err
	delete(pt.inflight, index)
	close(pending.done)
	pt.fetchMu.Unlock()
	return data, err
}

func (p *PieceProxy) cachedPiece(pt *proxyTorrent, index int) ([]byte, bool) {
	key := proxyCacheKey{torrent: pt, index: index}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	pt.stateMu.RLock()
	data, ok := pt.fetched[index]
	pt.stateMu.RUnlock()
	if ok {
		if element := p.cacheEntries[key]; element != nil {
			p.cacheLRU.MoveToFront(element)
		}
	}
	return data, ok
}

func (p *PieceProxy) storeCachedPiece(pt *proxyTorrent, index int, data []byte) {
	if int64(len(data)) > p.cacheLimit || p.cacheLimit == 0 {
		return
	}
	key := proxyCacheKey{torrent: pt, index: index}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if existing := p.cacheEntries[key]; existing != nil {
		p.cacheLRU.MoveToFront(existing)
		return
	}
	pt.stateMu.Lock()
	pt.fetched[index] = data
	pt.stateMu.Unlock()
	element := p.cacheLRU.PushFront(proxyCacheEntry{key: key, size: int64(len(data))})
	p.cacheEntries[key] = element
	p.cachedBytes += int64(len(data))
	for p.cachedBytes > p.cacheLimit {
		p.evictOldestCachedPieceLocked()
	}
}

func (p *PieceProxy) evictOldestCachedPieceLocked() {
	element := p.cacheLRU.Back()
	if element == nil {
		return
	}
	entry := element.Value.(proxyCacheEntry)
	entry.key.torrent.stateMu.Lock()
	delete(entry.key.torrent.fetched, entry.key.index)
	entry.key.torrent.stateMu.Unlock()
	delete(p.cacheEntries, entry.key)
	p.cachedBytes -= entry.size
	p.cacheLRU.Remove(element)
}

func (p *PieceProxy) removeTorrentCache(pt *proxyTorrent) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	for key, element := range p.cacheEntries {
		if key.torrent != pt {
			continue
		}
		entry := element.Value.(proxyCacheEntry)
		p.cachedBytes -= entry.size
		p.cacheLRU.Remove(element)
		delete(p.cacheEntries, key)
	}
	pt.stateMu.Lock()
	clear(pt.fetched)
	pt.stateMu.Unlock()
}

// fetchPiece tries seeds in turn until one serves a SHA-1-valid piece.
func fetchPiece(ctx context.Context, pt *proxyTorrent, peers []Peer, index int) ([]byte, error) {
	if index < 0 || index >= len(pt.pieceHashes) || pt.pieceLength <= 0 || pt.totalSize <= 0 {
		return nil, errProxyEmptyMeta
	}
	pieceSize := pieceSizeAt(pt, index)
	if pieceSize <= 0 {
		return nil, fmt.Errorf("proxy: piece %d has non-positive size", index)
	}
	if len(peers) == 0 {
		return nil, errProxyNoPeers
	}

	var lastErr error = errProxyBadPiece
	tries := 0
	for k := 0; k < len(peers) && tries < proxyMaxPeerTries; k++ {
		// Rotate the starting seed by piece index to spread load across the pool.
		peer := peers[(index+k)%len(peers)]
		tries++

		data, err := leechPieceFromPeer(ctx, peer, pt, index, pieceSize)
		if err != nil {
			lastErr = err
			continue
		}
		if sha1.Sum(data) != pt.pieceHashes[index] {
			lastErr = errProxyBadPiece
			continue
		}
		return data, nil
	}
	return nil, fmt.Errorf("proxy: no seed served valid piece %d after %d tries: %w", index, tries, lastErr)
}

// pieceSizeAt returns the byte length of a piece, accounting for a short final piece.
func pieceSizeAt(pt *proxyTorrent, index int) int {
	start := int64(index) * pt.pieceLength
	if start >= pt.totalSize {
		return 0
	}
	if remaining := pt.totalSize - start; remaining < pt.pieceLength {
		return int(remaining)
	}
	return int(pt.pieceLength)
}

// leechPieceFromPeer downloads one whole piece from a single seed using a
// standard interested/unchoke/request/piece exchange with a pipelined request
// window. The returned bytes are NOT yet SHA-1 checked — the caller verifies.
func leechPieceFromPeer(ctx context.Context, peer Peer, pt *proxyTorrent, index, pieceSize int) ([]byte, error) {
	addr := net.JoinHostPort(peer.IP, strconv.Itoa(peer.Port))
	dialer := net.Dialer{Timeout: proxyDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)
	conn.SetDeadline(time.Now().Add(proxyPieceTimeout))

	if err := proxyHandshake(conn, pt); err != nil {
		return nil, err
	}
	// Express interest immediately; well-behaved seeds unchoke in response.
	if err := writeMessage(conn, msgInterested, nil); err != nil {
		return nil, err
	}

	numBlocks := (pieceSize + proxyBlockSize - 1) / proxyBlockSize
	piece := make([]byte, pieceSize)
	received := make([]bool, numBlocks)
	remaining := numBlocks

	unchoked := false
	nextReq := 0
	outstanding := 0

	pump := func() error {
		if !unchoked {
			return nil
		}
		for outstanding < proxyMaxOutstanding && nextReq < numBlocks {
			begin := nextReq * proxyBlockSize
			length := proxyBlockSize
			if begin+length > pieceSize {
				length = pieceSize - begin
			}
			if err := writeMessage(conn, msgRequest, requestPayload(index, begin, length)); err != nil {
				return err
			}
			nextReq++
			outstanding++
		}
		return nil
	}

	for remaining > 0 {
		id, payload, err := readMessage(conn)
		if err != nil {
			return nil, err
		}
		switch id {
		case msgKeepAlive:
			// nothing
		case msgChoke:
			unchoked = false
		case msgUnchoke:
			unchoked = true
			if err := pump(); err != nil {
				return nil, err
			}
		case msgPiece:
			if len(payload) < 8 {
				continue
			}
			pieceIdx := binary.BigEndian.Uint32(payload[0:4])
			begin := int(binary.BigEndian.Uint32(payload[4:8]))
			block := payload[8:]
			if int(pieceIdx) != index || begin < 0 || begin+len(block) > pieceSize {
				continue
			}
			bi := begin / proxyBlockSize
			if bi < 0 || bi >= numBlocks || received[bi] {
				continue
			}
			copy(piece[begin:], block)
			received[bi] = true
			remaining--
			if outstanding > 0 {
				outstanding--
			}
			if err := pump(); err != nil {
				return nil, err
			}
		default:
			// bitfield / have / extended / cancel — ignore.
		}
	}
	return piece, nil
}

// proxyHandshake performs the outbound BitTorrent handshake and validates that
// the peer answered for the expected info hash.
func proxyHandshake(conn net.Conn, pt *proxyTorrent) error {
	peerID := pt.peerID
	if len(peerID) != 20 {
		peerID = randomPeerID()
	}
	if _, err := conn.Write(buildHandshake(pt.infoHash, peerID)); err != nil {
		return err
	}

	resp := make([]byte, handshakeLen)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if resp[0] != 19 || string(resp[1:20]) != btProtocol {
		return fmt.Errorf("proxy: peer sent invalid handshake protocol")
	}
	var gotHash [20]byte
	copy(gotHash[:], resp[28:48])
	if gotHash != pt.infoHash {
		return fmt.Errorf("proxy: peer answered for a different info hash")
	}
	return nil
}

// readMessage reads one length-prefixed peer-wire message. A zero-length prefix
// is a keep-alive, reported with the msgKeepAlive sentinel id.
func readMessage(conn net.Conn) (byte, []byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 {
		return msgKeepAlive, nil, nil
	}
	if n > proxyMaxMessageLen {
		return 0, nil, fmt.Errorf("proxy: peer message too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

// writeMessage writes one length-prefixed peer-wire message.
func writeMessage(conn net.Conn, id byte, payload []byte) error {
	buf := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(1+len(payload)))
	buf[4] = id
	copy(buf[5:], payload)
	_, err := conn.Write(buf)
	return err
}

// requestPayload builds the 12-byte body of a request message.
func requestPayload(index, begin, length int) []byte {
	p := make([]byte, 12)
	binary.BigEndian.PutUint32(p[0:4], uint32(index))
	binary.BigEndian.PutUint32(p[4:8], uint32(begin))
	binary.BigEndian.PutUint32(p[8:12], uint32(length))
	return p
}

// randomPeerID returns a random 20-byte peer id for handshakes when no client
// peer id is configured.
func randomPeerID() []byte {
	id := make([]byte, 20)
	if _, err := rand.Read(id); err != nil {
		// crypto/rand should not fail; fall back to a fixed marker.
		copy(id, []byte("-DO0000-proxyfallback"))
	}
	return id
}
