package peerwire

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"
)

const (
	maxPeerConnections = 256
	// ModeNone drops incoming connections immediately after the OS accepts them.
	ModeNone = "NONE"
	// ModeHandshakeOnly completes the BitTorrent handshake and then closes.
	ModeHandshakeOnly = "HANDSHAKE_ONLY"
	// ModeBitfield completes the handshake, then sends a bitfield + unchoke.
	ModeBitfield = "BITFIELD"
	// ModeFakeData completes the peer session and serves only data that can be
	// verified from a registered file or the optional verified-piece provider.
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
	// msgRejectRequest is the Fast Extension response for an unavailable block.
	msgRejectRequest = byte(16)
)

var errVerifiedPieceUnavailable = errors.New("peerwire: verified piece unavailable")

// TorrentInfo holds the metadata the peerwire server needs to respond to peers.
type TorrentInfo struct {
	InfoHash   [20]byte
	PieceCount int
	PeerID     []byte

	// Piece metadata used only by the on-demand piece proxy. Leave zero when the
	// proxy is disabled.
	PieceHashes [][20]byte
	PieceLength int64
	TotalSize   int64
	Metadata    []byte
}

// Server listens for incoming BitTorrent peer connections and responds
// according to the configured peerResponseMode.
type Server struct {
	port            int
	mode            string
	clientName      string
	activeTorrents  sync.Map // infoHashHex -> *TorrentInfo
	pieceCache      *PieceCache
	pieceProxy      *PieceProxy // nil unless the piece proxy is enabled
	dhtPort         int
	livePeers       map[string]map[string]Peer
	livePeersMu     sync.RWMutex
	listener        net.Listener
	lifecycleMu     sync.Mutex
	stop            chan struct{}
	stopOnce        sync.Once
	connections     map[net.Conn]struct{}
	connectionsMu   sync.Mutex
	connectionSlots chan struct{}
	wg              sync.WaitGroup
}

// EnableDHT links the PeerWire capability bits and PORT message to an active
// local DHT UDP listener. Call before Start.
func (s *Server) EnableDHT(port int) {
	if port > 0 && port <= 65535 {
		s.dhtPort = port
	}
}

// NewServer creates a Server that will listen on the given port using the
// specified mode ("NONE", "HANDSHAKE_ONLY", "BITFIELD", or "FAKE_DATA").
// clientName is used in the BEP 10 extension handshake (e.g. "qBittorrent 5.0.0").
func NewServer(port int, mode string, clientName string) *Server {
	return &Server{
		port:            port,
		mode:            mode,
		clientName:      clientName,
		pieceCache:      NewPieceCache(),
		livePeers:       make(map[string]map[string]Peer),
		stop:            make(chan struct{}),
		connections:     make(map[net.Conn]struct{}),
		connectionSlots: make(chan struct{}, maxPeerConnections),
	}
}

// RegisterDataFile associates a torrent with a real file for SHA-1 verified piece serving.
func (s *Server) RegisterDataFile(infoHashHex string, filePath string, pieceLength, totalSize int64, pieceHashes [][20]byte) error {
	return s.pieceCache.RegisterFile(infoHashHex, filePath, pieceLength, totalSize, pieceHashes)
}

// EnablePieceProxy activates on-demand piece leeching so cache misses are served
// with verified data fetched from real seeds. Call before RegisterTorrent.
func (s *Server) EnablePieceProxy() {
	if s.pieceProxy == nil {
		s.pieceProxy = NewPieceProxy()
	}
}

// UpdatePeers feeds the latest tracker-returned seeds to the piece proxy so it
// knows where to leech pieces from. No-op when the proxy is disabled.
func (s *Server) UpdatePeers(infoHashHex string, peers []Peer) {
	if s.pieceProxy != nil {
		s.pieceProxy.SetPeers(infoHashHex, peers)
	}
}

// RegisterTorrent makes a torrent eligible for peer connections.
func (s *Server) RegisterTorrent(info TorrentInfo) {
	hashHex := fmt.Sprintf("%x", info.InfoHash)
	s.activeTorrents.Store(hashHex, &info)
	if s.pieceProxy != nil && len(info.PieceHashes) > 0 {
		s.pieceProxy.RegisterTorrent(hashHex, info.InfoHash, info.PeerID, info.PieceHashes, info.PieceLength, info.TotalSize)
	}
}

// UnregisterTorrent removes a torrent from the active set.
func (s *Server) UnregisterTorrent(infoHashHex string) {
	s.activeTorrents.Delete(infoHashHex)
	if s.pieceCache != nil {
		s.pieceCache.Unregister(infoHashHex)
	}
	if s.pieceProxy != nil {
		s.pieceProxy.Unregister(infoHashHex)
	}
}

// Start binds the listener and begins accepting peer connections in the
// background. It returns an error if the port cannot be bound.
func (s *Server) Start() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	select {
	case <-s.stop:
		return fmt.Errorf("peerwire: server has been stopped")
	default:
	}
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
	s.stopOnce.Do(func() {
		close(s.stop)
		s.lifecycleMu.Lock()
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.lifecycleMu.Unlock()
		s.connectionsMu.Lock()
		for conn := range s.connections {
			_ = conn.Close()
		}
		s.connectionsMu.Unlock()
		if s.pieceProxy != nil {
			s.pieceProxy.Close()
		}
	})
	s.wg.Wait()
	s.pieceCache.Close()
	s.activeTorrents.Range(func(key, _ any) bool {
		s.activeTorrents.Delete(key)
		return true
	})
	s.livePeersMu.Lock()
	clear(s.livePeers)
	s.livePeersMu.Unlock()
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

		select {
		case <-s.stop:
			_ = conn.Close()
			return
		default:
		}
		if !s.reserveConnection() {
			_ = conn.Close()
			continue
		}
		s.connectionsMu.Lock()
		s.connections[conn] = struct{}{}
		s.connectionsMu.Unlock()
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer s.releaseConnection()
			defer func() {
				s.connectionsMu.Lock()
				delete(s.connections, c)
				s.connectionsMu.Unlock()
			}()
			defer c.Close()
			s.handleConnection(c)
		}(conn)
	}
}

func (s *Server) reserveConnection() bool {
	select {
	case s.connectionSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseConnection() {
	select {
	case <-s.connectionSlots:
	default:
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
	remoteSupportsDHT := buf[27]&0x01 != 0

	// 4. Send response handshake.
	peerID := info.PeerID
	if len(peerID) != 20 {
		peerID = make([]byte, 20)
		if _, err := rand.Read(peerID); err != nil {
			return
		}
	}

	handshake := buildHandshakeWithDHT(infoHash, peerID, s.dhtPort > 0)
	if _, err := conn.Write(handshake); err != nil {
		return
	}
	if remoteSupportsDHT && s.dhtPort > 0 {
		if _, err := conn.Write(buildDHTPortMessage(s.dhtPort)); err != nil {
			return
		}
	}

	if s.mode == ModeHandshakeOnly {
		return
	}

	// 4b. Send BEP 10 extension handshake (we advertised extension protocol support).
	if err := s.sendExtensionHandshake(conn, s.clientName, len(info.Metadata)); err != nil {
		return
	}

	// 5. Advertise pieces only when a fully verified local file is registered.
	if err := sendBitfield(conn, info.PieceCount, s.pieceCache.HasFile(infoHashHex)); err != nil {
		return
	}

	// 6. Send unchoke.
	if err := sendUnchoke(conn); err != nil {
		return
	}

	s.servePeerMessages(conn, info, infoHashHex)
}

// baseReservedBytes advertises Fast Extension (bit 0x04 of byte 7) and
// Extension Protocol (bit 0x10 of byte 5). DHT is added only for a live node.
var baseReservedBytes = [8]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x04}

// sendExtensionHandshake sends a BEP 10 extension protocol handshake.
// This is required because we advertise extension protocol support in our
// reserved bytes (bit 0x10 of byte 5).
func (s *Server) sendExtensionHandshake(conn net.Conn, clientName string, metadataSize int) error {
	buf := buildExtensionHandshake(s.port, clientName, metadataSize)
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Write(buf)
	return err
}

// buildHandshake constructs the 68-byte BitTorrent handshake response.
func buildHandshake(infoHash [20]byte, peerID []byte) []byte {
	return buildHandshakeWithDHT(infoHash, peerID, false)
}

func buildHandshakeWithDHT(infoHash [20]byte, peerID []byte, dhtEnabled bool) []byte {
	var h [handshakeLen]byte
	h[0] = 19
	copy(h[1:20], btProtocol)
	reserved := baseReservedBytes
	if dhtEnabled {
		reserved[7] |= 0x01
	}
	copy(h[20:28], reserved[:])
	copy(h[28:48], infoHash[:])
	copy(h[48:68], peerID[:20])
	return h[:]
}

func buildDHTPortMessage(port int) []byte {
	message := []byte{0, 0, 0, 3, 9, 0, 0}
	binary.BigEndian.PutUint16(message[5:], uint16(port))
	return message
}

// sendBitfield sends a BitTorrent bitfield. Bits are set only when all pieces
// are backed by a locally verified file.
func sendBitfield(w io.Writer, pieceCount int, allAvailable bool) error {
	if pieceCount <= 0 {
		return nil
	}

	byteCount := int(math.Ceil(float64(pieceCount) / 8.0))
	bitfield := make([]byte, byteCount)

	if allAvailable {
		for i := 0; i < pieceCount; i++ {
			bitfield[i/8] |= 1 << uint(7-i%8)
		}
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

// sendPieceData writes a BitTorrent piece message. It serves, in order of
// preference: real bytes from a verified local data file, then SHA-1-verified
// bytes from the optional provider. It fails closed when neither is available.
func sendPieceData(w io.Writer, index, begin, length uint32, infoHashHex string, cache *PieceCache, proxy *PieceProxy) error {
	const maxBlock = 32 * 1024
	if length == 0 || length > maxBlock {
		return fmt.Errorf("peerwire: invalid block length %d", length)
	}

	var block []byte

	// 1. Real data from a local file.
	if cache != nil && infoHashHex != "" {
		data, err := cache.GetPiece(infoHashHex, int(index), int(begin), int(length))
		if err == nil && data != nil && len(data) == int(length) {
			block = data
		}
	}

	// 2. On-demand piece proxy: leech + SHA-1 verify from a real seed.
	if block == nil && proxy != nil && infoHashHex != "" {
		data, err := proxy.GetBlock(infoHashHex, int(index), int(begin), int(length))
		if err == nil && len(data) == int(length) {
			block = data
		}
	}

	// Never manufacture bytes for a piece we cannot verify.
	if block == nil {
		return errVerifiedPieceUnavailable
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

func sendRejectRequest(w io.Writer, index, begin, length uint32) error {
	msg := make([]byte, 17)
	binary.BigEndian.PutUint32(msg[0:4], 13)
	msg[4] = msgRejectRequest
	binary.BigEndian.PutUint32(msg[5:9], index)
	binary.BigEndian.PutUint32(msg[9:13], begin)
	binary.BigEndian.PutUint32(msg[13:17], length)
	_, err := w.Write(msg)
	return err
}
