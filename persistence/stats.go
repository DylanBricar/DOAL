package persistence

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

var mu sync.Mutex

// LoadUploadStats reads the cumulative upload total from path.
// Returns 0 if the file does not exist or cannot be parsed — never fatal.
func LoadUploadStats(path string) int64 {
	mu.Lock()
	defer mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		// Missing file is not an error; the counter starts at zero.
		return 0
	}

	raw := strings.TrimSpace(string(data))
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "persistence: parsing upload stats from %q: %v — starting at 0\n", path, err)
		return 0
	}

	if n < 0 {
		fmt.Fprintf(os.Stderr, "persistence: negative upload stats in %q — starting at 0\n", path)
		return 0
	}

	return n
}

// SaveUploadStats atomically writes total to path using a temp-file rename
// so a crash mid-write never leaves a corrupt file.
func SaveUploadStats(path string, total int64) {
	mu.Lock()
	defer mu.Unlock()

	tmp := path + ".tmp"
	content := strconv.FormatInt(total, 10) + "\n"

	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "persistence: writing temp file %q: %v\n", tmp, err)
		return
	}

	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "persistence: renaming %q -> %q: %v\n", tmp, path, err)
		// Best-effort cleanup.
		os.Remove(tmp)
	}
}
