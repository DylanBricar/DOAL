package peerwire

import (
	"fmt"
	"os"
	"sync"
)

// cachedFile holds an open file handle and its piece length.
type cachedFile struct {
	f           *os.File
	pieceLength int64
}

// PieceCache maps torrent info hashes to real data files on disk,
// enabling SHA-1 verified piece serving in FAKE_DATA mode.
// Files are opened once on registration and kept open until unregistered.
type PieceCache struct {
	mu    sync.RWMutex
	files map[string]*cachedFile // infoHashHex -> open file
}

// NewPieceCache creates an empty piece cache.
func NewPieceCache() *PieceCache {
	return &PieceCache{
		files: make(map[string]*cachedFile),
	}
}

// RegisterFile associates a torrent's infoHash with a real file on disk.
// The file is opened immediately; the caller should call Unregister when done.
func (pc *PieceCache) RegisterFile(infoHashHex string, filePath string, pieceLength int64) {
	f, err := os.Open(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "piececache: opening %s: %v\n", filePath, err)
		return
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()

	// Close any previously registered file for this hash.
	if old, ok := pc.files[infoHashHex]; ok {
		old.f.Close()
	}
	pc.files[infoHashHex] = &cachedFile{f: f, pieceLength: pieceLength}
}

// GetPiece reads real piece data from disk. Returns nil if no file is registered.
func (pc *PieceCache) GetPiece(infoHashHex string, index int, begin int, length int) ([]byte, error) {
	pc.mu.RLock()
	cf, ok := pc.files[infoHashHex]
	pc.mu.RUnlock()

	if !ok {
		return nil, nil // no file registered
	}

	offset := int64(index)*cf.pieceLength + int64(begin)
	buf := make([]byte, length)
	n, err := cf.f.ReadAt(buf, offset)
	if err != nil && n == 0 {
		return nil, fmt.Errorf("piececache: reading piece %d: %w", index, err)
	}
	return buf[:n], nil
}

// Unregister closes and removes all cached data for a torrent.
func (pc *PieceCache) Unregister(infoHashHex string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if cf, ok := pc.files[infoHashHex]; ok {
		cf.f.Close()
		delete(pc.files, infoHashHex)
	}
}

// HasFile checks if a real data file is registered for this torrent.
func (pc *PieceCache) HasFile(infoHashHex string) bool {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	_, ok := pc.files[infoHashHex]
	return ok
}
