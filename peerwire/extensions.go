package peerwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
)

const (
	msgExtended       = byte(20)
	metadataBlockSize = 16 * 1024
	utMetadataID      = byte(1)
	utPEXID           = byte(2)
)

type extensionState struct {
	remoteMetadataID byte
	remotePEXID      byte
	listenPort       int
}

func buildExtensionHandshake(port int, clientName string, metadataSize int) []byte {
	var payload string
	if metadataSize > 0 {
		payload = fmt.Sprintf(
			"d1:md11:ut_metadatai%de6:ut_pexi%dee13:metadata_sizei%de1:pi%de4:reqqi250e1:v%d:%se",
			utMetadataID, utPEXID, metadataSize, port, len(clientName), clientName,
		)
	} else {
		payload = fmt.Sprintf(
			"d1:md6:ut_pexi%dee1:pi%de4:reqqi250e1:v%d:%se",
			utPEXID, port, len(clientName), clientName,
		)
	}
	return buildExtendedMessage(0, []byte(payload), nil)
}

func buildMetadataResponse(extensionID byte, metadata []byte, piece int) ([]byte, error) {
	if extensionID == 0 {
		return nil, fmt.Errorf("metadata extension ID must be positive")
	}
	if piece < 0 {
		return nil, fmt.Errorf("metadata piece must be non-negative")
	}
	start := piece * metadataBlockSize
	if start >= len(metadata) {
		return nil, fmt.Errorf("metadata piece %d is outside %d bytes", piece, len(metadata))
	}
	end := start + metadataBlockSize
	if end > len(metadata) {
		end = len(metadata)
	}
	header := []byte(fmt.Sprintf("d8:msg_typei1e5:piecei%de10:total_sizei%dee", piece, len(metadata)))
	return buildExtendedMessage(extensionID, header, metadata[start:end]), nil
}

func buildMetadataReject(extensionID byte, piece int) []byte {
	payload := []byte(fmt.Sprintf("d8:msg_typei2e5:piecei%dee", piece))
	return buildExtendedMessage(extensionID, payload, nil)
}

func buildPEXPayload(peers []Peer) []byte {
	compact := make([]byte, 0, len(peers)*6)
	flags := make([]byte, 0, len(peers))
	seen := make(map[string]struct{}, len(peers))
	for _, peer := range peers {
		ip := net.ParseIP(peer.IP).To4()
		if ip == nil || peer.Port <= 0 || peer.Port > 65535 {
			continue
		}
		key := fmt.Sprintf("%s:%d", ip.String(), peer.Port)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		compact = append(compact, ip...)
		compact = append(compact, byte(peer.Port>>8), byte(peer.Port))
		flags = append(flags, 0x02) // upload_only / seed
	}
	if len(compact) == 0 {
		return nil
	}

	var payload bytes.Buffer
	payload.WriteString("d5:added")
	payload.WriteString(fmt.Sprintf("%d:", len(compact)))
	payload.Write(compact)
	payload.WriteString("7:added.f")
	payload.WriteString(fmt.Sprintf("%d:", len(flags)))
	payload.Write(flags)
	payload.WriteByte('e')
	return payload.Bytes()
}

func buildExtendedMessage(extensionID byte, header, data []byte) []byte {
	messageLength := 2 + len(header) + len(data)
	message := make([]byte, 4+messageLength)
	binary.BigEndian.PutUint32(message[:4], uint32(messageLength))
	message[4] = msgExtended
	message[5] = extensionID
	copy(message[6:], header)
	copy(message[6+len(header):], data)
	return message
}

func parseExtensionHandshake(payload []byte) extensionState {
	state := extensionState{}
	if value, ok := bencodedInt(payload, "ut_metadata"); ok && value > 0 && value <= 255 {
		state.remoteMetadataID = byte(value)
	}
	if value, ok := bencodedInt(payload, "ut_pex"); ok && value > 0 && value <= 255 {
		state.remotePEXID = byte(value)
	}
	if value, ok := bencodedInt(payload, "p"); ok && value > 0 && value <= 65535 {
		state.listenPort = value
	}
	return state
}

func bencodedInt(payload []byte, key string) (int, bool) {
	marker := []byte(fmt.Sprintf("%d:%si", len(key), key))
	start := bytes.Index(payload, marker)
	if start < 0 {
		return 0, false
	}
	start += len(marker)
	end := start
	for end < len(payload) && payload[end] != 'e' {
		end++
	}
	if end == start || end >= len(payload) {
		return 0, false
	}
	value, err := strconv.Atoi(string(payload[start:end]))
	return value, err == nil
}

func (s *Server) handleExtendedMessage(w io.Writer, info *TorrentInfo, body []byte, state *extensionState) error {
	if len(body) < 2 || body[0] != msgExtended {
		return nil
	}
	extensionID := body[1]
	payload := body[2:]
	if extensionID == 0 {
		parsed := parseExtensionHandshake(payload)
		*state = parsed
		return nil
	}
	if extensionID != utMetadataID {
		return nil
	}

	messageType, ok := bencodedInt(payload, "msg_type")
	if !ok || messageType != 0 {
		return nil
	}
	piece, ok := bencodedInt(payload, "piece")
	if !ok || piece < 0 || state.remoteMetadataID == 0 {
		return nil
	}

	response, err := buildMetadataResponse(state.remoteMetadataID, info.Metadata, piece)
	if err != nil {
		response = buildMetadataReject(state.remoteMetadataID, piece)
	}
	if _, err := w.Write(response); err != nil {
		return fmt.Errorf("writing metadata response: %w", err)
	}
	return nil
}
