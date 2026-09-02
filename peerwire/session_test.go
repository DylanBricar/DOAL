package peerwire

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestHandlePeerMessageRefreshesWriteDeadline(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	if err := serverConn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	server := NewServer(0, ModeFakeData, "test")
	info := &TorrentInfo{PieceCount: 1, PieceLength: 16, TotalSize: 16}
	body := make([]byte, 13)
	body[0] = msgRequest
	binary.BigEndian.PutUint32(body[9:13], 32*1024)

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buffer := make([]byte, 17)
		_, _ = clientConn.Read(buffer)
	}()

	if !server.handlePeerMessage(serverConn, info, "hash", body, &peerSession{}) {
		t.Fatal("handlePeerMessage failed because a stale write deadline was not refreshed")
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("peer response was not written")
	}
}
