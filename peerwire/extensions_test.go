package peerwire

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestExtensionHandshakeAdvertisesMetadataSize(t *testing.T) {
	t.Parallel()

	msg := buildExtensionHandshake(6881, "qBittorrent/5.1.4", 32_768)
	if len(msg) < 6 {
		t.Fatalf("extension handshake too short: %d", len(msg))
	}
	if got := binary.BigEndian.Uint32(msg[:4]); got != uint32(len(msg)-4) {
		t.Fatalf("message length prefix = %d, want %d", got, len(msg)-4)
	}
	if msg[4] != msgExtended || msg[5] != 0 {
		t.Fatalf("extension header = %v, want extended handshake", msg[4:6])
	}
	if !bytes.Contains(msg[6:], []byte("11:ut_metadatai1e")) {
		t.Fatal("extension handshake does not advertise ut_metadata")
	}
	if !bytes.Contains(msg[6:], []byte("13:metadata_sizei32768e")) {
		t.Fatal("extension handshake does not advertise metadata_size")
	}
}

func TestPeriodicPEXUsesNegotiatedIDAndLivePeers(t *testing.T) {
	t.Parallel()

	server := NewServer(0, ModeBitfield, "qBittorrent/5.1.4")
	server.livePeers["hash"] = map[string]Peer{
		"self":  {IP: "192.0.2.1", Port: 6001},
		"other": {IP: "192.0.2.2", Port: 6002},
	}
	now := time.Now()
	session := &peerSession{
		extensions:    extensionState{remotePEXID: 7},
		peerKey:       "self",
		pexKnown:      make(map[string]Peer),
		nextPEX:       now.Add(-time.Second),
		nextKeepAlive: now.Add(time.Hour),
	}
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	done := make(chan bool, 1)
	go func() {
		done <- server.sendPeriodicMessages(serverConn, "hash", session, now)
	}()
	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	body, err := readPeerMessage(clientConn)
	if err != nil {
		t.Fatalf("readPeerMessage: %v", err)
	}
	if len(body) < 2 || body[0] != msgExtended || body[1] != 7 {
		t.Fatalf("PEX header = %v, want extended ID 7", body)
	}
	wantCompact := append(net.ParseIP("192.0.2.2").To4(), 0x17, 0x72)
	if !bytes.Contains(body[2:], wantCompact) {
		t.Fatalf("PEX body %x does not contain live peer %x", body[2:], wantCompact)
	}
	if ok := <-done; !ok {
		t.Fatal("sendPeriodicMessages failed")
	}
}

func TestMetadataRequestUsesNegotiatedRemoteExtensionID(t *testing.T) {
	t.Parallel()

	metadata := bytes.Repeat([]byte{0x42}, metadataBlockSize+5)
	info := &TorrentInfo{Metadata: metadata}
	state := &extensionState{remoteMetadataID: 9}
	request := append([]byte{msgExtended, utMetadataID}, []byte("d8:msg_typei0e5:piecei1ee")...)
	var response bytes.Buffer

	server := NewServer(0, ModeBitfield, "qBittorrent/5.1.4")
	if err := server.handleExtendedMessage(&response, info, request, state); err != nil {
		t.Fatalf("handleExtendedMessage: %v", err)
	}
	msg := response.Bytes()
	if len(msg) < 6 || msg[5] != 9 {
		t.Fatalf("response extension ID = %v, want negotiated ID 9", msg)
	}
	if !bytes.HasSuffix(msg, metadata[metadataBlockSize:]) {
		t.Fatal("response does not contain requested metadata block")
	}
}

func TestParseRemoteExtensionHandshake(t *testing.T) {
	t.Parallel()

	payload := []byte("d1:md11:ut_metadatai9e6:ut_pexi4ee1:pi51413ee")
	state := parseExtensionHandshake(payload)
	if state.remoteMetadataID != 9 || state.remotePEXID != 4 || state.listenPort != 51413 {
		t.Fatalf("parsed extension state = %+v", state)
	}
}

func TestMetadataResponseContainsRequestedBlock(t *testing.T) {
	t.Parallel()

	metadata := bytes.Repeat([]byte{0x5a}, metadataBlockSize+37)
	msg, err := buildMetadataResponse(7, metadata, 1)
	if err != nil {
		t.Fatalf("buildMetadataResponse: %v", err)
	}
	if msg[4] != msgExtended || msg[5] != 7 {
		t.Fatalf("extension header = %v, want ID 7", msg[4:6])
	}
	if !bytes.Contains(msg[6:], []byte("8:msg_typei1e")) || !bytes.Contains(msg[6:], []byte("5:piecei1e")) {
		t.Fatalf("metadata response header missing fields: %q", msg[6:])
	}
	if !bytes.HasSuffix(msg, metadata[metadataBlockSize:]) {
		t.Fatal("metadata response does not end with requested metadata block")
	}
}

func TestPEXPayloadContainsLiveIPv4Peers(t *testing.T) {
	t.Parallel()

	peers := []Peer{
		{IP: "192.0.2.10", Port: 6881},
		{IP: "2001:db8::1", Port: 6882},
		{IP: "invalid", Port: 1},
	}
	payload := buildPEXPayload(peers)
	wantCompact := append(net.ParseIP("192.0.2.10").To4(), 0x1a, 0xe1)
	if !bytes.Contains(payload, wantCompact) {
		t.Fatalf("PEX payload %x does not contain compact peer %x", payload, wantCompact)
	}
	if !bytes.Contains(payload, []byte("7:added.f1:")) {
		t.Fatalf("PEX payload %q does not contain one flags byte", payload)
	}
}
