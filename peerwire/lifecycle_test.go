package peerwire

import (
	"bytes"
	"crypto/sha1"
	"math"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerStopClosesActiveConnectionsAndIsIdempotent(t *testing.T) {
	server := NewServer(0, ModeFakeData, "test")
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn, err := net.Dial("tcp", server.listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte{19}) // keep the accepted handler blocked in its handshake
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		server.Stop()
		server.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not close an active peer connection")
	}
}

func TestValidBlockRequestRejectsIntegerOverflow(t *testing.T) {
	info := &TorrentInfo{
		PieceCount:  4,
		PieceLength: math.MaxInt64,
		TotalSize:   math.MaxInt64,
	}
	if validBlockRequest(info, 3, 0, 1) {
		t.Fatal("block request with overflowing piece offset was accepted")
	}
}

func TestServerCannotStartAfterStop(t *testing.T) {
	server := NewServer(0, ModeFakeData, "test")
	server.Stop()
	if err := server.Start(); err == nil {
		t.Fatal("server restarted after its lifecycle had been stopped")
	}
}

func TestServerConnectionSlotsAreBounded(t *testing.T) {
	server := NewServer(0, ModeFakeData, "test")
	for i := 0; i < maxPeerConnections; i++ {
		if !server.reserveConnection() {
			t.Fatalf("connection slot %d was unexpectedly rejected", i)
		}
	}
	if server.reserveConnection() {
		t.Fatal("connection limit admitted one extra peer")
	}
	server.releaseConnection()
	if !server.reserveConnection() {
		t.Fatal("released connection slot was not reusable")
	}
}

func TestSendPieceDataFailsClosedWithoutVerifiedSource(t *testing.T) {
	var dst bytes.Buffer
	err := sendPieceData(&dst, 0, 0, 16, "missing", NewPieceCache(), nil)
	if err == nil {
		t.Fatal("piece request without a verified source succeeded")
	}
	if dst.Len() != 0 {
		t.Fatalf("wrote %d random bytes without a verified source", dst.Len())
	}
}

func TestPieceCacheRejectsCorruptFileAndOutOfBoundsBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.bin")
	content := []byte("0123456789abcdef")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	pieceLength := int64(8)
	hashes := [][20]byte{sha1.Sum(content[:8]), sha1.Sum(content[8:])}

	cache := NewPieceCache()
	defer cache.Close()
	if err := cache.RegisterFile("valid", path, pieceLength, int64(len(content)), hashes); err != nil {
		t.Fatalf("RegisterFile(valid): %v", err)
	}
	if _, err := cache.GetPiece("valid", 0, 7, 2); err == nil {
		t.Fatal("cross-piece read was accepted")
	}
	changedTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, changedTime, changedTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if cache.HasFile("valid") {
		t.Fatal("modified data file was still advertised as verified")
	}

	corrupt := append([]byte(nil), content...)
	corrupt[0] ^= 0xff
	corruptPath := filepath.Join(dir, "corrupt.bin")
	if err := os.WriteFile(corruptPath, corrupt, 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt): %v", err)
	}
	if err := cache.RegisterFile("corrupt", corruptPath, pieceLength, int64(len(corrupt)), hashes); err == nil {
		t.Fatal("corrupt data file was registered as verified")
	}

	cache.Close()
	if len(cache.files) != 0 {
		t.Fatalf("Close retained %d file handles", len(cache.files))
	}
}
