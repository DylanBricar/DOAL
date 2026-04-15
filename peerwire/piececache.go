package peerwire

import (
	"fmt"
	"os"
	"sync"
)

// PieceCache maps torrent info hashes to real data files on disk,
// enabling SHA-1 verified piece serving in FAKE_DATA mode.
type PieceCache struct {
	mu          sync.RWMutex
	files       map[string]string // infoHashHex -> file path
	pieceLength map[string]int64  // infoHashHex -> piece length
}

// NewPieceCache creates an empty piece cache.
func NewPieceCache() *PieceCache {
	return &PieceCache{
		files:       make(map[string]string),
		pieceLength: make(map[string]int64),
	}
}

// RegisterFile associates a torrent's infoHash with a real file on disk.
func (pc *PieceCache) RegisterFile(infoHashHex string, filePath string, pieceLength int64) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.files[infoHashHex] = filePath
	pc.pieceLength[infoHashHex] = pieceLength
}

// GetPiece reads real piece data from disk. Returns nil if no file is registered.
func (pc *PieceCache) GetPiece(infoHashHex string, index int, begin int, length int) ([]byte, error) {
	pc.mu.RLock()
	filePath, ok := pc.files[infoHashHex]
	pl := pc.pieceLength[infoHashHex]
	pc.mu.RUnlock()

	if !ok || filePath == "" {
		return nil, nil // no file registered
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("piececache: opening %s: %w", filePath, err)
	}
	defer f.Close()

	offset := int64(index)*pl + int64(begin)
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && n == 0 {
		return nil, fmt.Errorf("piececache: reading piece %d: %w", index, err)
	}
	return buf[:n], nil
}

// Unregister removes all cached data for a torrent.
func (pc *PieceCache) Unregister(infoHashHex string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	delete(pc.files, infoHashHex)
	delete(pc.pieceLength, infoHashHex)
}

// HasFile checks if a real data file is registered for this torrent.
func (pc *PieceCache) HasFile(infoHashHex string) bool {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	_, ok := pc.files[infoHashHex]
	return ok
}
