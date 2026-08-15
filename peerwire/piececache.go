package peerwire

import (
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// cachedFile holds an open file handle and its piece length.
type cachedFile struct {
	f           *os.File
	pieceLength int64
	totalSize   int64
	pieceHashes [][20]byte
	modTime     time.Time
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
func (pc *PieceCache) RegisterFile(infoHashHex string, filePath string, pieceLength, totalSize int64, pieceHashes [][20]byte) error {
	if pieceLength <= 0 || totalSize <= 0 || len(pieceHashes) == 0 {
		return fmt.Errorf("piececache: incomplete piece metadata")
	}
	expectedPieces := int((totalSize-1)/pieceLength + 1)
	if len(pieceHashes) != expectedPieces {
		return fmt.Errorf("piececache: got %d hashes, want %d", len(pieceHashes), expectedPieces)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("piececache: opening %s: %w", filePath, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("piececache: stat %s: %w", filePath, err)
	}
	if info.Size() != totalSize {
		f.Close()
		return fmt.Errorf("piececache: file size %d does not match torrent size %d", info.Size(), totalSize)
	}
	for index, expected := range pieceHashes {
		pieceSize := pieceLength
		if remaining := totalSize - int64(index)*pieceLength; remaining < pieceSize {
			pieceSize = remaining
		}
		hasher := sha1.New()
		if _, err := io.CopyN(hasher, f, pieceSize); err != nil {
			f.Close()
			return fmt.Errorf("piececache: verifying piece %d: %w", index, err)
		}
		var actual [20]byte
		copy(actual[:], hasher.Sum(nil))
		if actual != expected {
			f.Close()
			return fmt.Errorf("piececache: piece %d failed SHA-1 verification", index)
		}
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()

	// Close any previously registered file for this hash.
	if old, ok := pc.files[infoHashHex]; ok {
		old.f.Close()
	}
	pc.files[infoHashHex] = &cachedFile{
		f:           f,
		pieceLength: pieceLength,
		totalSize:   totalSize,
		pieceHashes: append([][20]byte(nil), pieceHashes...),
		modTime:     info.ModTime(),
	}
	return nil
}

// GetPiece reads real piece data from disk. Returns nil if no file is registered.
func (pc *PieceCache) GetPiece(infoHashHex string, index int, begin int, length int) ([]byte, error) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	cf, ok := pc.files[infoHashHex]

	if !ok {
		return nil, nil // no file registered
	}
	if index < 0 || index >= len(cf.pieceHashes) || begin < 0 || length <= 0 {
		return nil, fmt.Errorf("piececache: invalid block coordinates")
	}
	pieceSize := cf.pieceLength
	if remaining := cf.totalSize - int64(index)*cf.pieceLength; remaining < pieceSize {
		pieceSize = remaining
	}
	if int64(begin) > pieceSize || int64(length) > pieceSize-int64(begin) {
		return nil, fmt.Errorf("piececache: block crosses piece boundary")
	}
	info, err := cf.f.Stat()
	if err != nil || info.Size() != cf.totalSize || !info.ModTime().Equal(cf.modTime) {
		return nil, fmt.Errorf("piececache: verified data file changed after registration")
	}

	offset := int64(index)*cf.pieceLength + int64(begin)
	buf := make([]byte, length)
	n, err := cf.f.ReadAt(buf, offset)
	if err != nil && n == 0 {
		return nil, fmt.Errorf("piececache: reading piece %d: %w", index, err)
	}
	return buf[:n], nil
}

// Close releases every open data file and empties the cache.
func (pc *PieceCache) Close() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for hash, cf := range pc.files {
		_ = cf.f.Close()
		delete(pc.files, hash)
	}
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
	cf, ok := pc.files[infoHashHex]
	if !ok {
		return false
	}
	info, err := cf.f.Stat()
	return err == nil && info.Size() == cf.totalSize && info.ModTime().Equal(cf.modTime)
}
