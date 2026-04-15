package peerwire

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"
)

const (
	// ModeNone drops incoming connections immediately after the OS accepts them.
	ModeNone = "NONE"
	// ModeHandshakeOnly completes the BitTorrent handshake and then closes.
	ModeHandshakeOnly = "HANDSHAKE_ONLY"
	// ModeBitfield completes the handshake, then sends a bitfield + unchoke.
	ModeBitfield = "BITFIELD"
	// ModeFakeData completes handshake, sends bitfield + unchoke, and serves
	// random data for any piece requests received.
	ModeFakeData = "FAKE_DATA"

	// btProtocol is the fixed 19-byte protocol identifier in BitTorrent handshakes.
	btProtocol = "BitTorrent protocol"

	// handshakeLen is the total size of a BitTorrent handshake in bytes:
	//   1  (pstrlen) + 19 (pstr) + 8 (reserved) + 20 (info_hash) + 20 (peer_id) = 68
	handshakeLen = 68

	// msgBitfield is the BitTorrent message ID for bitfield messages.
	msgBitfield = byte(5)
	// msgUnchoke is the BitTorrent message ID for unchoke messages.
	msgUnchoke = byte(1)
	// msgPiece is the BitTorrent message ID for piece messages.
	msgPiece = byte(7)
	// msgRequest is the BitTorrent message ID for request messages.
	msgRequest = byte(6)
)

// TorrentInfo holds the metadata the peerwire server needs to respond to peers.
type TorrentInfo struct {
	InfoHash   [20]byte
	PieceCount int
	PeerID     []byte
}

// Server listens for incoming BitTorrent peer connections and responds
// according to the configured peerResponseMode.
type Server struct {
	port           int
	mode           string
	clientName     string
	activeTorrents sync.Map // infoHashHex -> *TorrentInfo
	pieceCache     *PieceCache
	listener       net.Listener
	stop           chan struct{}
	wg             sync.WaitGroup
}

// NewServer creates a Server that will listen on the given port using the
// specified mode ("NONE", "HANDSHAKE_ONLY", "BITFIELD", or "FAKE_DATA").
// clientName is used in the BEP 10 extension handshake (e.g. "qBittorrent 5.0.0").
func NewServer(port int, mode string, clientName string) *Server {
	return &Server{
		port:       port,
		mode:       mode,
		clientName: clientName,
		pieceCache: NewPieceCache(),
		stop:       make(chan struct{}),
	}
}

// RegisterDataFile associates a torrent with a real file for SHA-1 verified piece serving.
func (s *Server) RegisterDataFile(infoHashHex string, filePath string, pieceLength int64) {
	s.pieceCache.RegisterFile(infoHashHex, filePath, pieceLength)
}

// RegisterTorrent makes a torrent eligible for peer connections.
func (s *Server) RegisterTorrent(info TorrentInfo) {
	s.activeTorrents.Store(fmt.Sprintf("%x", info.InfoHash), &info)
}

// UnregisterTorrent removes a torrent from the active set.
func (s *Server) UnregisterTorrent(infoHashHex string) {
	s.activeTorrents.Delete(infoHashHex)
	if s.pieceCache != nil {
		s.pieceCache.Unregister(infoHashHex)
	}
}

// Start binds the listener and begins accepting peer connections in the
// background. It returns an error if the port cannot be bound.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("peerwire: listening on port %d: %w", s.port, err)
	}
	s.listener = ln

	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// Stop closes the listener and waits for all active connection handlers to
// finish.
func (s *Server) Stop() {
	close(s.stop)
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
}

// acceptLoop runs in a background goroutine, accepting new TCP connections and
// spawning a handler goroutine for each one.
func (s *Server) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.stop:
				return // normal shutdown
			default:
				fmt.Printf("peerwire: accept error: %v\n", err)
				continue
			}
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close()
			s.handleConnection(c)
		}(conn)
	}
}

// handleConnection dispatches an incoming peer connection through the full
// handshake and any subsequent message handling required by the mode.
func (s *Server) handleConnection(conn net.Conn) {
	if s.mode == ModeNone {
		return
	}

	// Set initial deadline for handshake (30s). Extended later for keep-alive/pieces.
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// 1. Read the 68-byte incoming handshake.
	buf := make([]byte, handshakeLen)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}

	// 2. Validate the protocol string (bytes 1..19).
	if buf[0] != 19 || string(buf[1:20]) != btProtocol {
		return
	}

	// 3. Extract info_hash (bytes 28..47).
	var infoHash [20]byte
	copy(infoHash[:], buf[28:48])

	infoHashHex := fmt.Sprintf("%x", infoHash)
	val, ok := s.activeTorrents.Load(infoHashHex)
	if !ok {
		return
	}
	info := val.(*TorrentInfo)

	// 4. Send response handshake.
	peerID := info.PeerID
	if len(peerID) != 20 {
		peerID = make([]byte, 20)
		if _, err := rand.Read(peerID); err != nil {
			return
		}
	}

	handshake := buildHandshake(infoHash, peerID)
	if _, err := conn.Write(handshake); err != nil {
		return
	}

	if s.mode == ModeHandshakeOnly {
		return
	}

	// 4b. Send BEP 10 extension handshake (we advertised extension protocol support).
	if err := s.sendExtensionHandshake(conn, s.clientName); err != nil {
		return
	}

	// 5. Send bitfield (all pieces marked as available).
	if err := sendBitfield(conn, info.PieceCount); err != nil {
		return
	}

	// 6. Send unchoke.
	if err := sendUnchoke(conn); err != nil {
		return
	}

	if s.mode == ModeBitfield {
		s.keepAliveLoop(conn)
		return
	}

	// FAKE_DATA mode: handle piece requests (real data if available, random otherwise).
	if s.mode == ModeFakeData {
		s.servePiecesWithCache(conn, info.PieceCount, infoHashHex)
	}
}

// reservedBytes matches typical libtorrent-based clients (qBittorrent, Deluge):
// DHT (bit 0x01 of byte 7), Fast Extension (bit 0x04 of byte 7),
// Extension Protocol (bit 0x10 of byte 5).
var reservedBytes = [8]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x05}

// sendExtensionHandshake sends a BEP 10 extension protocol handshake.
// This is required because we advertise extension protocol support in our
// reserved bytes (bit 0x10 of byte 5).
func (s *Server) sendExtensionHandshake(conn net.Conn, clientName string) error {
	// Bencode dictionary: {m: {ut_metadata: 1, ut_pex: 2}, p: <port>, v: <client>, reqq: 250}
	payload := fmt.Sprintf("d1:md11:ut_metadatai1e6:ut_pexi2ee1:pi%de1:v%d:%s4:reqqi250ee",
		s.port, len(clientName), clientName)

	msgLen := 2 + len(payload) // 1 byte msg_id + 1 byte ext_id + payload
	buf := make([]byte, 4+msgLen)
	binary.BigEndian.PutUint32(buf[0:4], uint32(msgLen))
	buf[4] = 20 // extended message
	buf[5] = 0  // handshake (ext msg id 0)
	copy(buf[6:], []byte(payload))

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Write(buf)
	return err
}

// buildHandshake constructs the 68-byte BitTorrent handshake response.
func buildHandshake(infoHash [20]byte, peerID []byte) []byte {
	var h [handshakeLen]byte
	h[0] = 19
	copy(h[1:20], btProtocol)
	copy(h[20:28], reservedBytes[:])
	copy(h[28:48], infoHash[:])
	copy(h[48:68], peerID[:20])
	return h[:]
}

// sendBitfield sends a BitTorrent bitfield message with all pieces set to 1.
func sendBitfield(w io.Writer, pieceCount int) error {
	if pieceCount <= 0 {
		return nil
	}

	byteCount := int(math.Ceil(float64(pieceCount) / 8.0))
	bitfield := make([]byte, byteCount)

	// Set all bits for present pieces.
	for i := 0; i < pieceCount; i++ {
		bitfield[i/8] |= 1 << uint(7-i%8)
	}

	// Message format: 4-byte length prefix + 1-byte message ID + payload.
	msgLen := uint32(1 + byteCount)
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, msgLen)

	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write([]byte{msgBitfield}); err != nil {
		return err
	}
	_, err := w.Write(bitfield)
	return err
}

// sendUnchoke sends a 5-byte BitTorrent unchoke message.
func sendUnchoke(w io.Writer) error {
	// unchoke: length=1, id=1
	msg := []byte{0, 0, 0, 1, msgUnchoke}
	_, err := w.Write(msg)
	return err
}

// drainConnection reads and discards incoming data until the connection closes
// or an error occurs.
// keepAliveLoop sends a BitTorrent keep-alive (4 zero bytes) every 120 seconds
// and discards any incoming data, for up to 10 minutes of inactivity.
// Every other keep-alive tick it also sends a PEX message (BEP 11).
func (s *Server) keepAliveLoop(conn net.Conn) {
	defer conn.Close()
	ticker := time.NewTicker(120 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(10 * time.Minute)
	tick := 0

	// Read incoming data in background so we don't block on the ticker.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()

	for time.Now().Before(deadline) {
		select {
		case <-s.stop:
			return
		case <-readDone:
			return
		case <-ticker.C:
			tick++
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			// Keep-alive: 4-byte length prefix of zero.
			if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
				return
			}
			// Send PEX every other tick.
			if tick%2 == 0 {
				if err := s.sendPEXMessage(conn); err != nil {
					return
				}
			}
		}
	}
}

// sendPEXMessage sends a BEP 11 peer exchange message with no peers.
// Extension message ID 2 matches the ut_pex value advertised in the
// BEP 10 extension handshake.
func (s *Server) sendPEXMessage(conn net.Conn) error {
	// Minimal PEX payload: empty added, added.f and dropped lists.
	payload := "d5:added0:7:added.f0:7:dropped0:e"
	msgLen := 2 + len(payload) // 1 byte msg_id(20) + 1 byte ext_id(2) + payload
	buf := make([]byte, 4+msgLen)
	binary.BigEndian.PutUint32(buf[0:4], uint32(msgLen))
	buf[4] = 20 // extended message
	buf[5] = 2  // ut_pex extension ID as advertised in handshake
	copy(buf[6:], []byte(payload))
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Write(buf)
	return err
}

// servePiecesWithCache reads incoming BT request messages and responds with
// real piece data from the cache when available, random data otherwise.
func (s *Server) servePiecesWithCache(conn net.Conn, pieceCount int, infoHashHex string) {
	servePiecesInternal(conn, pieceCount, infoHashHex, s.pieceCache)
}

func servePiecesInternal(conn net.Conn, pieceCount int, infoHashHex string, cache *PieceCache) {
	lenBuf := make([]byte, 4)
	for {
		// Read the 4-byte length prefix.
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return
		}
		msgLen := binary.BigEndian.Uint32(lenBuf)

		if msgLen == 0 {
			// Keep-alive message.
			continue
		}

		if msgLen > 1<<20 {
			// Refuse unreasonably large messages.
			return
		}

		// Read the message body.
		body := make([]byte, msgLen)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}

		msgID := body[0]
		if msgID != msgRequest {
			// Ignore non-request messages (interest, have, etc.).
			continue
		}

		if len(body) < 13 {
			continue
		}

		// request payload: index(4) + begin(4) + length(4)
		index := binary.BigEndian.Uint32(body[1:5])
		begin := binary.BigEndian.Uint32(body[5:9])
		length := binary.BigEndian.Uint32(body[9:13])

		if pieceCount > 0 && int(index) >= pieceCount {
			continue
		}

		if err := sendPieceData(conn, index, begin, length, infoHashHex, cache); err != nil {
			return
		}
	}
}

// sendPieceData writes a BitTorrent piece message, using real data from cache if available.
func sendPieceData(w io.Writer, index, begin, length uint32, infoHashHex string, cache *PieceCache) error {
	const maxBlock = 32 * 1024
	if length > maxBlock {
		length = maxBlock
	}

	var block []byte

	// Try real data from cache first
	if cache != nil && infoHashHex != "" {
		data, err := cache.GetPiece(infoHashHex, int(index), int(begin), int(length))
		if err == nil && data != nil && len(data) == int(length) {
			block = data
		}
	}

	// Fallback to random data
	if block == nil {
		block = make([]byte, length)
		if _, err := rand.Read(block); err != nil {
			return err
		}
	}

	// piece message: 4-byte len + id(1) + index(4) + begin(4) + data
	payloadLen := uint32(1 + 4 + 4 + length)
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, payloadLen)

	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write([]byte{msgPiece}); err != nil {
		return err
	}

	indexBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(indexBuf, index)
	if _, err := w.Write(indexBuf); err != nil {
		return err
	}

	beginBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(beginBuf, begin)
	if _, err := w.Write(beginBuf); err != nil {
		return err
	}

	_, err := w.Write(block)
	return err
}
