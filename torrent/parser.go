package torrent

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	maxBencodeDepth    = 128
	maxTorrentFileSize = 16 << 20
)

// Torrent holds the essential metadata extracted from a .torrent file.
type Torrent struct {
	InfoHash     [20]byte
	InfoHashHex  string
	Name         string
	Size         int64
	PieceCount   int
	PieceLength  int64
	PieceHashes  [][20]byte
	InfoBytes    []byte
	AnnounceURLs []string
	FilePath     string
}

// ParseFile reads and parses the .torrent file at the given path.
func ParseFile(path string) (*Torrent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("torrent: reading %q: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTorrentFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("torrent: reading %q: %w", path, err)
	}
	if len(data) > maxTorrentFileSize {
		return nil, fmt.Errorf("torrent: %q exceeds %d bytes", path, maxTorrentFileSize)
	}

	dict, infoRaw, infoBytes, end, err := decodeTorrentMetainfo(data)
	if err != nil {
		return nil, fmt.Errorf("torrent: decoding bencode in %q: %w", path, err)
	}
	if end != len(data) {
		return nil, fmt.Errorf("torrent: trailing data at offset %d in %q", end, path)
	}
	infoHash := sha1.Sum(infoBytes)

	infoDict, ok := infoRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("torrent: info value is not a dictionary in %q", path)
	}

	name := stringField(infoDict, "name")
	size := computeSize(infoDict)
	pieces := stringField(infoDict, "pieces")
	pieceCount := len(pieces) / 20
	pieceLength := intField(infoDict, "piece length")
	announceURLs := extractAnnounceURLs(dict)
	if name == "" {
		return nil, fmt.Errorf("torrent: info name is empty in %q", path)
	}
	if size <= 0 {
		return nil, fmt.Errorf("torrent: info size must be positive in %q", path)
	}
	if pieceLength <= 0 {
		return nil, fmt.Errorf("torrent: piece length must be positive in %q", path)
	}
	if len(pieces) == 0 || len(pieces)%sha1.Size != 0 {
		return nil, fmt.Errorf("torrent: pieces length must be a positive multiple of %d in %q", sha1.Size, path)
	}
	expectedPieceCount := int((size + pieceLength - 1) / pieceLength)
	if pieceCount != expectedPieceCount {
		return nil, fmt.Errorf("torrent: piece count %d does not match size and piece length (want %d) in %q", pieceCount, expectedPieceCount, path)
	}

	// Extract individual piece hashes (20 bytes each)
	var pieceHashes [][20]byte
	for i := 0; i+20 <= len(pieces); i += 20 {
		var h [20]byte
		copy(h[:], pieces[i:i+20])
		pieceHashes = append(pieceHashes, h)
	}

	t := &Torrent{
		InfoHash:     infoHash,
		InfoHashHex:  hex.EncodeToString(infoHash[:]),
		Name:         name,
		Size:         size,
		PieceCount:   pieceCount,
		PieceLength:  pieceLength,
		PieceHashes:  pieceHashes,
		InfoBytes:    infoBytes,
		AnnounceURLs: announceURLs,
		FilePath:     path,
	}

	return t, nil
}

// extractInfoHash locates the exact top-level "info" value span and hashes its
// original bytes, as required by BEP 3.
func extractInfoHash(data []byte) ([20]byte, any, []byte, error) {
	_, value, infoBytes, end, err := decodeTorrentMetainfo(data)
	if err != nil {
		return [20]byte{}, nil, nil, err
	}
	if end != len(data) {
		return [20]byte{}, nil, nil, fmt.Errorf("trailing data at offset %d", end)
	}
	hash := sha1.Sum(infoBytes)
	return hash, value, infoBytes, nil
}

func decodeTorrentMetainfo(data []byte) (map[string]any, any, []byte, int, error) {
	if len(data) == 0 || data[0] != 'd' {
		return nil, nil, nil, 0, fmt.Errorf("top-level bencode value is not a dictionary")
	}
	dict := make(map[string]any)
	var info any
	var infoBytes []byte
	offset := 1
	for {
		if offset >= len(data) {
			return nil, nil, nil, offset, fmt.Errorf("unterminated top-level dictionary")
		}
		if data[offset] == 'e' {
			offset++
			break
		}
		key, next, err := decodeString(data, offset)
		if err != nil {
			return nil, nil, nil, offset, fmt.Errorf("top-level dictionary key: %w", err)
		}
		if _, duplicate := dict[key]; duplicate {
			return nil, nil, nil, offset, fmt.Errorf("duplicate top-level key %q", key)
		}
		offset = next
		valueStart := offset
		value, next, err := decodeBencodeDepth(data, offset, 1)
		if err != nil {
			return nil, nil, nil, offset, fmt.Errorf("top-level value for key %q: %w", key, err)
		}
		dict[key] = value
		if key == "info" {
			info = value
			infoBytes = append([]byte(nil), data[valueStart:next]...)
		}
		offset = next
	}
	if info == nil {
		return nil, nil, nil, offset, fmt.Errorf("top-level info key not found")
	}
	return dict, info, infoBytes, offset, nil
}

// extractAnnounceURLs collects tracker URLs from "announce" and "announce-list".
func extractAnnounceURLs(dict map[string]any) []string {
	seen := map[string]bool{}
	var urls []string

	addURL := func(u string) {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}

	if v, ok := dict["announce"]; ok {
		if s, ok := v.(string); ok {
			addURL(s)
		}
	}

	if v, ok := dict["announce-list"]; ok {
		if tiers, ok := v.([]any); ok {
			for _, tier := range tiers {
				if tierList, ok := tier.([]any); ok {
					for _, item := range tierList {
						if s, ok := item.(string); ok {
							addURL(s)
						}
					}
				}
			}
		}
	}

	return urls
}

// computeSize returns total bytes across all files. Supports single-file and
// multi-file torrents.
func computeSize(info map[string]any) int64 {
	// Multi-file torrent.
	if files, ok := info["files"]; ok {
		if list, ok := files.([]any); ok {
			var total int64
			for _, f := range list {
				if fd, ok := f.(map[string]any); ok {
					total += intField(fd, "length")
				}
			}
			return total
		}
	}

	// Single-file torrent.
	return intField(info, "length")
}

// stringField safely extracts a string value from a map.
func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// intField safely extracts an int64 value from a map.
func intField(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// indexBytes returns the index of needle in haystack, or -1 if not found.
func indexBytes(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
outer:
	for i := 0; i <= len(haystack)-len(needle); i++ {
		for j, b := range needle {
			if haystack[i+j] != b {
				continue outer
			}
		}
		return i
	}
	return -1
}

// decodeBencode decodes one bencode value starting at offset and returns the
// Go value plus the index of the first byte after the value.
func decodeBencode(data []byte, offset int) (any, int, error) {
	return decodeBencodeDepth(data, offset, 0)
}

func decodeBencodeDepth(data []byte, offset, depth int) (any, int, error) {
	if depth >= maxBencodeDepth {
		return nil, offset, fmt.Errorf("bencode nesting exceeds %d levels", maxBencodeDepth)
	}
	if offset >= len(data) {
		return nil, offset, fmt.Errorf("unexpected end of data at offset %d", offset)
	}

	switch {
	case data[offset] == 'i':
		return decodeInt(data, offset)
	case data[offset] == 'l':
		return decodeList(data, offset, depth)
	case data[offset] == 'd':
		return decodeDict(data, offset, depth)
	case data[offset] >= '0' && data[offset] <= '9':
		return decodeString(data, offset)
	default:
		return nil, offset, fmt.Errorf("unknown bencode type %q at offset %d", data[offset], offset)
	}
}

// decodeInt decodes an integer: i<digits>e
func decodeInt(data []byte, offset int) (int64, int, error) {
	// skip 'i'
	offset++
	end := offset
	for end < len(data) && data[end] != 'e' {
		end++
	}
	if end >= len(data) {
		return 0, offset, fmt.Errorf("unterminated integer at offset %d", offset)
	}

	n, err := strconv.ParseInt(string(data[offset:end]), 10, 64)
	if err != nil {
		return 0, offset, fmt.Errorf("invalid integer at offset %d: %w", offset, err)
	}

	return n, end + 1, nil // +1 to skip 'e'
}

// decodeString decodes a byte string: <length>:<bytes>
func decodeString(data []byte, offset int) (string, int, error) {
	colonIdx := offset
	for colonIdx < len(data) && data[colonIdx] != ':' {
		colonIdx++
	}
	if colonIdx >= len(data) {
		return "", offset, fmt.Errorf("no colon found in string at offset %d", offset)
	}

	length, err := strconv.ParseUint(string(data[offset:colonIdx]), 10, 64)
	if err != nil {
		return "", offset, fmt.Errorf("invalid string length at offset %d: %w", offset, err)
	}

	start := colonIdx + 1
	if length > uint64(len(data)-start) {
		return "", offset, fmt.Errorf("string length %d exceeds data at offset %d", length, offset)
	}
	end := start + int(length)

	return string(data[start:end]), end, nil
}

// decodeList decodes a list: l<values>e
func decodeList(data []byte, offset, depth int) ([]any, int, error) {
	// skip 'l'
	offset++
	var list []any

	for {
		if offset >= len(data) {
			return nil, offset, fmt.Errorf("unterminated list")
		}
		if data[offset] == 'e' {
			return list, offset + 1, nil
		}

		val, next, err := decodeBencodeDepth(data, offset, depth+1)
		if err != nil {
			return nil, offset, err
		}
		list = append(list, val)
		offset = next
	}
}

// decodeDict decodes a dictionary: d<key><value>...e
// Keys are always strings in the bencode spec.
func decodeDict(data []byte, offset, depth int) (map[string]any, int, error) {
	// skip 'd'
	offset++
	dict := make(map[string]any)

	for {
		if offset >= len(data) {
			return nil, offset, fmt.Errorf("unterminated dictionary")
		}
		if data[offset] == 'e' {
			return dict, offset + 1, nil
		}

		key, next, err := decodeString(data, offset)
		if err != nil {
			return nil, offset, fmt.Errorf("dictionary key: %w", err)
		}
		offset = next

		val, next, err := decodeBencodeDepth(data, offset, depth+1)
		if err != nil {
			return nil, offset, fmt.Errorf("dictionary value for key %q: %w", key, err)
		}
		dict[key] = val
		offset = next
	}
}
