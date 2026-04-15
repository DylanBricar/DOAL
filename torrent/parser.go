package torrent

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
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
	AnnounceURLs []string
	FilePath     string
}

// ParseFile reads and parses the .torrent file at the given path.
func ParseFile(path string) (*Torrent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("torrent: reading %q: %w", path, err)
	}

	raw, _, err := decodeBencode(data, 0)
	if err != nil {
		return nil, fmt.Errorf("torrent: decoding bencode in %q: %w", path, err)
	}

	dict, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("torrent: top-level bencode value is not a dictionary in %q", path)
	}

	infoHash, infoRaw, err := extractInfoHash(data)
	if err != nil {
		return nil, fmt.Errorf("torrent: %w", err)
	}

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
		AnnounceURLs: announceURLs,
		FilePath:     path,
	}

	return t, nil
}

// extractInfoHash finds the "info" key in the raw bencode bytes, re-encodes
// the value span, and SHA-1 hashes it.
func extractInfoHash(data []byte) ([20]byte, any, error) {
	// Locate "4:info" in the byte stream.
	marker := []byte("4:info")
	idx := indexBytes(data, marker)
	if idx < 0 {
		return [20]byte{}, nil, fmt.Errorf("info key not found in torrent data")
	}

	valueStart := idx + len(marker)
	value, end, err := decodeBencode(data, valueStart)
	if err != nil {
		return [20]byte{}, nil, fmt.Errorf("decoding info dictionary: %w", err)
	}

	infoBytes := data[valueStart:end]
	hash := sha1.Sum(infoBytes)
	return hash, value, nil
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
	if offset >= len(data) {
		return nil, offset, fmt.Errorf("unexpected end of data at offset %d", offset)
	}

	switch {
	case data[offset] == 'i':
		return decodeInt(data, offset)
	case data[offset] == 'l':
		return decodeList(data, offset)
	case data[offset] == 'd':
		return decodeDict(data, offset)
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

	var n int64
	negative := false
	start := offset
	if data[start] == '-' {
		negative = true
		start++
	}
	for i := start; i < end; i++ {
		if data[i] < '0' || data[i] > '9' {
			return 0, offset, fmt.Errorf("invalid digit %q in integer at offset %d", data[i], i)
		}
		n = n*10 + int64(data[i]-'0')
	}
	if negative {
		n = -n
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

	var length int
	for i := offset; i < colonIdx; i++ {
		if data[i] < '0' || data[i] > '9' {
			return "", offset, fmt.Errorf("invalid length digit %q at offset %d", data[i], i)
		}
		length = length*10 + int(data[i]-'0')
	}

	start := colonIdx + 1
	end := start + length
	if end > len(data) {
		return "", offset, fmt.Errorf("string length %d exceeds data at offset %d", length, offset)
	}

	return string(data[start:end]), end, nil
}

// decodeList decodes a list: l<values>e
func decodeList(data []byte, offset int) ([]any, int, error) {
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

		val, next, err := decodeBencode(data, offset)
		if err != nil {
			return nil, offset, err
		}
		list = append(list, val)
		offset = next
	}
}

// decodeDict decodes a dictionary: d<key><value>...e
// Keys are always strings in the bencode spec.
func decodeDict(data []byte, offset int) (map[string]any, int, error) {
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

		val, next, err := decodeBencode(data, offset)
		if err != nil {
			return nil, offset, fmt.Errorf("dictionary value for key %q: %w", key, err)
		}
		dict[key] = val
		offset = next
	}
}
