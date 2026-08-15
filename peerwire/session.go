package peerwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"time"
)

const (
	peerMessageLimit  = 1 << 20
	pexInterval       = time.Minute
	keepAliveInterval = 2 * time.Minute
)

type peerSession struct {
	extensions    extensionState
	peerKey       string
	pexKnown      map[string]Peer
	nextPEX       time.Time
	nextKeepAlive time.Time
}

func (s *Server) servePeerMessages(conn net.Conn, info *TorrentInfo, infoHashHex string) {
	now := time.Now()
	session := peerSession{
		pexKnown:      make(map[string]Peer),
		nextPEX:       now.Add(pexInterval),
		nextKeepAlive: now.Add(keepAliveInterval),
	}
	defer func() {
		if session.peerKey != "" {
			s.removeLivePeer(infoHashHex, session.peerKey)
		}
	}()

	for {
		select {
		case <-s.stop:
			return
		default:
		}
		deadline := session.nextPEX
		if session.nextKeepAlive.Before(deadline) {
			deadline = session.nextKeepAlive
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return
		}

		body, err := readPeerMessage(conn)
		if err != nil {
			if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
				return
			}
		} else if len(body) > 0 {
			if !s.handlePeerMessage(conn, info, infoHashHex, body, &session) {
				return
			}
		}

		if !s.sendPeriodicMessages(conn, infoHashHex, &session, time.Now()) {
			return
		}
	}
}

func readPeerMessage(r io.Reader) ([]byte, error) {
	var lengthPrefix [4]byte
	if _, err := io.ReadFull(r, lengthPrefix[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(lengthPrefix[:])
	if length == 0 {
		return nil, nil
	}
	if length > peerMessageLimit {
		return nil, fmt.Errorf("peerwire: message length %d exceeds limit", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (s *Server) handlePeerMessage(conn net.Conn, info *TorrentInfo, infoHashHex string, body []byte, session *peerSession) bool {
	switch body[0] {
	case msgExtended:
		previousPort := session.extensions.listenPort
		if err := s.handleExtendedMessage(conn, info, body, &session.extensions); err != nil {
			return false
		}
		if session.extensions.listenPort > 0 && session.extensions.listenPort != previousPort {
			session.peerKey = s.registerLivePeer(infoHashHex, conn.RemoteAddr(), session.extensions.listenPort, session.peerKey)
		}
	case msgRequest:
		if s.mode != ModeFakeData || len(body) < 13 {
			return true
		}
		index := binary.BigEndian.Uint32(body[1:5])
		begin := binary.BigEndian.Uint32(body[5:9])
		length := binary.BigEndian.Uint32(body[9:13])
		if !validBlockRequest(info, index, begin, length) {
			_ = sendRejectRequest(conn, index, begin, length)
			return true
		}
		if err := sendPieceData(conn, index, begin, length, infoHashHex, s.pieceCache, s.pieceProxy); err != nil {
			if errors.Is(err, errVerifiedPieceUnavailable) {
				return sendRejectRequest(conn, index, begin, length) == nil
			}
			return false
		}
	}
	return true
}

func validBlockRequest(info *TorrentInfo, index, begin, length uint32) bool {
	if info == nil || length == 0 || length > 32*1024 || info.PieceCount <= 0 ||
		int(index) >= info.PieceCount || info.PieceLength <= 0 || info.TotalSize <= 0 {
		return false
	}
	if int64(index) > math.MaxInt64/info.PieceLength {
		return false
	}
	pieceStart := int64(index) * info.PieceLength
	if pieceStart < 0 || pieceStart >= info.TotalSize {
		return false
	}
	pieceSize := info.PieceLength
	if remaining := info.TotalSize - pieceStart; remaining < pieceSize {
		pieceSize = remaining
	}
	return uint64(begin)+uint64(length) <= uint64(pieceSize)
}

func (s *Server) sendPeriodicMessages(conn net.Conn, infoHashHex string, session *peerSession, now time.Time) bool {
	if !now.Before(session.nextKeepAlive) {
		if err := conn.SetWriteDeadline(now.Add(5 * time.Second)); err != nil {
			return false
		}
		if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
			return false
		}
		session.nextKeepAlive = now.Add(keepAliveInterval)
	}

	if !now.Before(session.nextPEX) {
		if session.extensions.remotePEXID > 0 {
			peers := s.livePeerSnapshot(infoHashHex, session.peerKey)
			added := make([]Peer, 0, len(peers))
			for key, peer := range peers {
				if _, known := session.pexKnown[key]; !known {
					added = append(added, peer)
				}
			}
			if payload := buildPEXPayload(added); len(payload) > 0 {
				message := buildExtendedMessage(session.extensions.remotePEXID, payload, nil)
				if err := conn.SetWriteDeadline(now.Add(5 * time.Second)); err != nil {
					return false
				}
				if _, err := conn.Write(message); err != nil {
					return false
				}
			}
			session.pexKnown = peers
		}
		session.nextPEX = now.Add(pexInterval)
	}
	return true
}

func (s *Server) registerLivePeer(infoHashHex string, remoteAddr net.Addr, listenPort int, previousKey string) string {
	host, _, err := net.SplitHostPort(remoteAddr.String())
	if err != nil || net.ParseIP(host) == nil {
		return previousKey
	}
	peer := Peer{IP: host, Port: listenPort}
	key := net.JoinHostPort(host, fmt.Sprintf("%d", listenPort))

	s.livePeersMu.Lock()
	defer s.livePeersMu.Unlock()
	if previousKey != "" && previousKey != key {
		delete(s.livePeers[infoHashHex], previousKey)
	}
	if s.livePeers[infoHashHex] == nil {
		s.livePeers[infoHashHex] = make(map[string]Peer)
	}
	s.livePeers[infoHashHex][key] = peer
	return key
}

func (s *Server) removeLivePeer(infoHashHex, key string) {
	s.livePeersMu.Lock()
	defer s.livePeersMu.Unlock()
	delete(s.livePeers[infoHashHex], key)
	if len(s.livePeers[infoHashHex]) == 0 {
		delete(s.livePeers, infoHashHex)
	}
}

func (s *Server) livePeerSnapshot(infoHashHex, excludeKey string) map[string]Peer {
	s.livePeersMu.RLock()
	defer s.livePeersMu.RUnlock()
	result := make(map[string]Peer, len(s.livePeers[infoHashHex]))
	for key, peer := range s.livePeers[infoHashHex] {
		if key != excludeKey {
			result[key] = peer
		}
	}
	return result
}
