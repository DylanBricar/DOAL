package peerwire

import (
	"crypto/sha1"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doal/torrent"
)

func TestRealPieceVerification(t *testing.T) {
	// Look for any .torrent + matching data file in ../torrents/
	torrentFile := ""
	dataFile := ""
	entries, _ := os.ReadDir("../torrents")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".torrent") { continue }
		base := strings.TrimSuffix(e.Name(), ".torrent")
		for _, d := range entries {
			if d.IsDir() || strings.HasSuffix(d.Name(), ".torrent") { continue }
			dBase := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
			if dBase == base {
				torrentFile = filepath.Join("../torrents", e.Name())
				dataFile = filepath.Join("../torrents", d.Name())
				break
			}
		}
		if torrentFile != "" { break }
	}

	// Skip if no matching pair found
	if torrentFile == "" || dataFile == "" {
		t.Skip("No .torrent + matching data file pair found in torrents/, skipping SHA-1 test")
	}

	// Parse the torrent
	tor, err := torrent.ParseFile(torrentFile)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	t.Logf("Torrent: %s", tor.Name)
	t.Logf("Size: %d bytes", tor.Size)
	t.Logf("Pieces: %d (length: %d)", tor.PieceCount, tor.PieceLength)
	t.Logf("Piece hashes: %d", len(tor.PieceHashes))

	if tor.PieceLength <= 0 {
		t.Fatal("PieceLength should be > 0")
	}
	if len(tor.PieceHashes) != tor.PieceCount {
		t.Fatalf("PieceHashes count %d != PieceCount %d", len(tor.PieceHashes), tor.PieceCount)
	}

	// Read the real file
	data, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if int64(len(data)) != tor.Size {
		t.Fatalf("File size %d != torrent size %d", len(data), tor.Size)
	}

	// Verify ALL piece hashes
	verified := 0
	failed := 0
	for i := 0; i < tor.PieceCount; i++ {
		start := int64(i) * tor.PieceLength
		end := start + tor.PieceLength
		if end > int64(len(data)) {
			end = int64(len(data))
		}

		piece := data[start:end]
		hash := sha1.Sum(piece)

		if hash != tor.PieceHashes[i] {
			t.Errorf("Piece %d: SHA-1 mismatch (got %x, want %x)", i, hash, tor.PieceHashes[i])
			failed++
			if failed > 3 {
				t.Fatal("Too many piece hash failures, stopping")
			}
		} else {
			verified++
		}
	}

	t.Logf("Verified %d/%d pieces (all SHA-1 hashes match!)", verified, tor.PieceCount)

	// Test the PieceCache
	cache := NewPieceCache()
	cache.RegisterFile(tor.InfoHashHex, dataFile, tor.PieceLength)

	// Read piece 0 via cache and verify
	block, err := cache.GetPiece(tor.InfoHashHex, 0, 0, int(tor.PieceLength))
	if err != nil {
		t.Fatalf("GetPiece: %v", err)
	}
	blockHash := sha1.Sum(block)
	if blockHash != tor.PieceHashes[0] {
		t.Error("PieceCache piece 0 SHA-1 mismatch")
	} else {
		t.Log("PieceCache: piece 0 SHA-1 verified via cache!")
	}

	// Read a sub-block (16KB from piece 5)
	if tor.PieceCount > 5 {
		subBlock, err := cache.GetPiece(tor.InfoHashHex, 5, 0, 16384)
		if err != nil {
			t.Fatalf("GetPiece sub-block: %v", err)
		}
		if len(subBlock) != 16384 {
			t.Errorf("Expected 16384 bytes, got %d", len(subBlock))
		}
	}
}

func TestPieceCacheMissingFile(t *testing.T) {
	cache := NewPieceCache()

	// No file registered
	data, err := cache.GetPiece("nonexistent", 0, 0, 1024)
	if err != nil {
		t.Errorf("Expected nil error for unregistered hash, got: %v", err)
	}
	if data != nil {
		t.Error("Expected nil data for unregistered hash")
	}
}
