package peerwire

import (
	crand "crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// computePieceHashes returns the SHA-1 of each piece of content.
func computePieceHashes(content []byte, pieceLength int64) [][20]byte {
	var hashes [][20]byte
	for start := int64(0); start < int64(len(content)); start += pieceLength {
		end := start + pieceLength
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		hashes = append(hashes, sha1.Sum(content[start:end]))
	}
	return hashes
}

// startFakeSeed stands up a TCP peer that speaks enough of the peer-wire
// protocol to serve piece data for content. When corrupt is true it flips every
// byte so served pieces fail SHA-1 (modelling a fake-seeder with no real data).
func startFakeSeed(t *testing.T, infoHash [20]byte, content []byte, pieceLength int64, pieceCount int, corrupt bool) (host string, port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake seed listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeSeed(conn, infoHash, content, pieceLength, pieceCount, corrupt)
		}
	}()

	h, p, _ := net.SplitHostPort(ln.Addr().String())
	pn, _ := strconv.Atoi(p)
	return h, pn, func() { ln.Close() }
}

func serveFakeSeed(conn net.Conn, infoHash [20]byte, content []byte, pieceLength int64, pieceCount int, corrupt bool) {
	defer conn.Close()

	hs := make([]byte, handshakeLen)
	if _, err := io.ReadFull(conn, hs); err != nil {
		return
	}
	peerID := make([]byte, 20)
	crand.Read(peerID)
	if _, err := conn.Write(buildHandshake(infoHash, peerID)); err != nil {
		return
	}
	if err := sendBitfield(conn, pieceCount, true); err != nil {
		return
	}
	if err := sendUnchoke(conn); err != nil {
		return
	}

	for {
		id, payload, err := readMessage(conn)
		if err != nil {
			return
		}
		if id != msgRequest || len(payload) < 12 {
			continue
		}
		index := binary.BigEndian.Uint32(payload[0:4])
		begin := binary.BigEndian.Uint32(payload[4:8])
		length := binary.BigEndian.Uint32(payload[8:12])

		off := int64(index)*pieceLength + int64(begin)
		if off < 0 || off+int64(length) > int64(len(content)) {
			continue
		}
		block := make([]byte, length)
		copy(block, content[off:off+int64(length)])
		if corrupt {
			for i := range block {
				block[i] ^= 0xff
			}
		}

		msg := make([]byte, 8+len(block))
		binary.BigEndian.PutUint32(msg[0:4], index)
		binary.BigEndian.PutUint32(msg[4:8], begin)
		copy(msg[8:], block)
		if err := writeMessage(conn, msgPiece, msg); err != nil {
			return
		}
	}
}

func newTestContent(t *testing.T, size int) []byte {
	t.Helper()
	content := make([]byte, size)
	if _, err := crand.Read(content); err != nil {
		t.Fatalf("generating content: %v", err)
	}
	return content
}

func TestPieceProxyFetchesAndVerifies(t *testing.T) {
	pieceLength := int64(32 * 1024)
	content := newTestContent(t, int(pieceLength)*2+5000) // 2 full pieces + a short tail
	pieceHashes := computePieceHashes(content, pieceLength)
	pieceCount := len(pieceHashes)
	infoHash := sha1.Sum([]byte("test-torrent-verify"))
	hashHex := fmt.Sprintf("%x", infoHash)

	host, port, stop := startFakeSeed(t, infoHash, content, pieceLength, pieceCount, false)
	defer stop()

	proxy := NewPieceProxy()
	proxy.RegisterTorrent(hashHex, infoHash, nil, pieceHashes, pieceLength, int64(len(content)))
	proxy.SetPeers(hashHex, []Peer{{IP: host, Port: port}})

	// A 16 KiB block from the middle of piece 1 (exercises multi-block fetch).
	block, err := proxy.GetBlock(hashHex, 1, 0, 16384)
	if err != nil {
		t.Fatalf("GetBlock piece 1: %v", err)
	}
	want := content[pieceLength : pieceLength+16384]
	if string(block) != string(want) {
		t.Fatal("piece 1 block bytes do not match source content")
	}

	// The short final piece must fetch and verify against its real length.
	lastIdx := pieceCount - 1
	lastStart := int64(lastIdx) * pieceLength
	lastSize := int(int64(len(content)) - lastStart)
	tail, err := proxy.GetBlock(hashHex, lastIdx, 0, lastSize)
	if err != nil {
		t.Fatalf("GetBlock last piece: %v", err)
	}
	if string(tail) != string(content[lastStart:]) {
		t.Fatal("final piece bytes do not match source content")
	}
}

func TestPieceProxyNoPeersFails(t *testing.T) {
	pieceLength := int64(16 * 1024)
	content := newTestContent(t, int(pieceLength))
	infoHash := sha1.Sum([]byte("test-torrent-nopeers"))
	hashHex := fmt.Sprintf("%x", infoHash)

	proxy := NewPieceProxy()
	proxy.RegisterTorrent(hashHex, infoHash, nil, computePieceHashes(content, pieceLength), pieceLength, int64(len(content)))
	// No SetPeers — this models a canary/starvation probe: no seed to fetch from.

	if _, err := proxy.GetBlock(hashHex, 0, 0, 16384); err == nil {
		t.Fatal("expected failure when no seeds are known, got nil error")
	}
}

func TestPieceProxyCapsPeerPool(t *testing.T) {
	content := []byte("0123456789abcdef")
	infoHash := sha1.Sum([]byte("test-torrent-peer-cap"))
	hashHex := fmt.Sprintf("%x", infoHash)
	proxy := NewPieceProxy()
	proxy.RegisterTorrent(hashHex, infoHash, []byte("-qB5000-123456789012"), computePieceHashes(content, int64(len(content))), int64(len(content)), int64(len(content)))
	peers := make([]Peer, maxProxyPeers+100)
	for i := range peers {
		peers[i] = Peer{IP: "127.0.0.1", Port: 1000 + i}
	}
	proxy.SetPeers(hashHex, peers)
	proxy.mu.RLock()
	pt := proxy.torrents[hashHex]
	proxy.mu.RUnlock()
	pt.stateMu.RLock()
	got := len(pt.peers)
	pt.stateMu.RUnlock()
	if got != maxProxyPeers {
		t.Fatalf("proxy peers=%d, limit=%d", got, maxProxyPeers)
	}
}

func TestPieceProxyRejectsCorruptSeed(t *testing.T) {
	pieceLength := int64(16 * 1024)
	content := newTestContent(t, int(pieceLength))
	pieceHashes := computePieceHashes(content, pieceLength)
	infoHash := sha1.Sum([]byte("test-torrent-corrupt"))
	hashHex := fmt.Sprintf("%x", infoHash)

	host, port, stop := startFakeSeed(t, infoHash, content, pieceLength, len(pieceHashes), true) // corrupt
	defer stop()

	proxy := NewPieceProxy()
	proxy.RegisterTorrent(hashHex, infoHash, nil, pieceHashes, pieceLength, int64(len(content)))
	proxy.SetPeers(hashHex, []Peer{{IP: host, Port: port}})

	// The seed answers but its bytes fail SHA-1, so the proxy must refuse to
	// serve them rather than pass garbage to the probe.
	if _, err := proxy.GetBlock(hashHex, 0, 0, 16384); err == nil {
		t.Fatal("expected failure when the only seed serves corrupt data, got nil error")
	}
}

func TestPieceProxyUnknownTorrent(t *testing.T) {
	proxy := NewPieceProxy()
	if _, err := proxy.GetBlock("deadbeef", 0, 0, 16384); err == nil {
		t.Fatal("expected error for unregistered torrent")
	}
}

func TestPieceProxyRejectsNegativeBlockLength(t *testing.T) {
	content := []byte("0123456789abcdef")
	infoHash := sha1.Sum([]byte("test-torrent-negative-block"))
	hashHex := fmt.Sprintf("%x", infoHash)
	proxy := NewPieceProxy()
	proxy.RegisterTorrent(hashHex, infoHash, []byte("-qB5000-123456789012"), computePieceHashes(content, int64(len(content))), int64(len(content)), int64(len(content)))
	proxy.mu.RLock()
	pt := proxy.torrents[hashHex]
	proxy.mu.RUnlock()
	proxy.storeCachedPiece(pt, 0, content)
	if _, err := proxy.GetBlock(hashHex, 0, 0, -1); err == nil {
		t.Fatal("negative block length was accepted")
	}
}

func TestPieceProxyCacheIsGloballyBounded(t *testing.T) {
	pieceLength := int64(16 * 1024)
	content := newTestContent(t, int(pieceLength)*3)
	pieceHashes := computePieceHashes(content, pieceLength)
	infoHash := sha1.Sum([]byte("test-torrent-bounded-cache"))
	hashHex := fmt.Sprintf("%x", infoHash)

	host, port, stop := startFakeSeed(t, infoHash, content, pieceLength, len(pieceHashes), false)
	defer stop()

	proxy := newPieceProxyWithCacheLimit(pieceLength + 1024)
	proxy.RegisterTorrent(hashHex, infoHash, []byte("-qB5000-123456789012"), pieceHashes, pieceLength, int64(len(content)))
	proxy.SetPeers(hashHex, []Peer{{IP: host, Port: port}})
	if _, err := proxy.GetBlock(hashHex, 0, 0, int(pieceLength)); err != nil {
		t.Fatalf("GetBlock(piece 0): %v", err)
	}
	if _, err := proxy.GetBlock(hashHex, 1, 0, int(pieceLength)); err != nil {
		t.Fatalf("GetBlock(piece 1): %v", err)
	}

	if proxy.cachedBytes > proxy.cacheLimit {
		t.Fatalf("cached bytes=%d, limit=%d", proxy.cachedBytes, proxy.cacheLimit)
	}
	proxy.mu.RLock()
	pt := proxy.torrents[hashHex]
	proxy.mu.RUnlock()
	pt.stateMu.RLock()
	_, firstRetained := pt.fetched[0]
	pt.stateMu.RUnlock()
	if firstRetained {
		t.Fatal("oldest piece was not evicted from bounded cache")
	}
}

func TestPieceProxyCloseCancelsInFlightFetch(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		close(accepted)
		<-release
		_ = conn.Close()
	}()

	content := []byte("0123456789abcdef")
	pieceHashes := computePieceHashes(content, int64(len(content)))
	infoHash := sha1.Sum([]byte("test-torrent-cancel-fetch"))
	hashHex := fmt.Sprintf("%x", infoHash)
	address := listener.Addr().(*net.TCPAddr)
	proxy := NewPieceProxy()
	proxy.RegisterTorrent(hashHex, infoHash, []byte("-qB5000-123456789012"), pieceHashes, int64(len(content)), int64(len(content)))
	proxy.SetPeers(hashHex, []Peer{{IP: address.IP.String(), Port: address.Port}})

	result := make(chan error, 1)
	go func() {
		_, fetchErr := proxy.GetBlock(hashHex, 0, 0, len(content))
		result <- fetchErr
	}()
	<-accepted
	proxy.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("in-flight fetch succeeded after proxy close")
		}
		close(release)
	case <-time.After(500 * time.Millisecond):
		close(release)
		<-result
		t.Fatal("Close did not cancel the in-flight piece fetch")
	}
}
