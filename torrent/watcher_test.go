package torrent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// bencodeString returns a bencode-encoded string: "<len>:<value>".
func bencodeString(s string) string {
	return fmt.Sprintf("%d:%s", len(s), s)
}

// bencodeInt returns a bencode-encoded integer: "i<n>e".
func bencodeInt(n int64) string {
	return fmt.Sprintf("i%de", n)
}

// buildMinimalTorrent creates a minimal valid .torrent file in dir and returns its path.
// It constructs well-formed bencode so the parser accepts it.
func buildMinimalTorrent(t *testing.T, dir, filename string) string {
	t.Helper()

	announceURL := "http://tracker.example.com/announce"
	name := strings.TrimSuffix(filename, ".torrent")

	// 20-byte piece hash placeholder.
	pieces := "xxxxxxxxxxxxxxxxxxxx"

	// Build the info dictionary in bencode key-sorted order.
	// Keys must be sorted: "length", "name", "piece length", "pieces".
	infoDict := "d" +
		bencodeString("length") + bencodeInt(1024) +
		bencodeString("name") + bencodeString(name) +
		bencodeString("piece length") + bencodeInt(262144) +
		bencodeString("pieces") + bencodeString(pieces) +
		"e"

	// Build the top-level dictionary. Keys: "announce", "info".
	content := "d" +
		bencodeString("announce") + bencodeString(announceURL) +
		bencodeString("info") + infoDict +
		"e"

	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test torrent %q: %v", path, err)
	}
	return path
}

// TestWatcherScanExisting verifies ScanExisting fires OnAdd for pre-existing .torrent files.
func TestWatcherScanExisting(t *testing.T) {
	dir := t.TempDir()
	buildMinimalTorrent(t, dir, "alpha.torrent")
	buildMinimalTorrent(t, dir, "beta.torrent")

	w, err := NewWatcher(dir)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.fsw.Close()

	var mu sync.Mutex
	var added []string
	w.OnAdd = func(tor *Torrent) {
		mu.Lock()
		added = append(added, tor.Name)
		mu.Unlock()
	}

	if err := w.ScanExisting(); err != nil {
		t.Fatalf("ScanExisting: %v", err)
	}

	mu.Lock()
	count := len(added)
	mu.Unlock()

	if count != 2 {
		t.Errorf("OnAdd called %d times, want 2", count)
	}
}

// TestWatcherScanExistingNoOnAdd verifies ScanExisting does not panic without OnAdd set.
func TestWatcherScanExistingNoOnAdd(t *testing.T) {
	dir := t.TempDir()
	buildMinimalTorrent(t, dir, "solo.torrent")

	w, err := NewWatcher(dir)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.fsw.Close()

	// OnAdd deliberately left nil — must not panic.
	if err := w.ScanExisting(); err != nil {
		t.Fatalf("ScanExisting: %v", err)
	}
}

// TestWatcherScanExistingEmptyDir verifies ScanExisting on an empty dir returns no error.
func TestWatcherScanExistingEmptyDir(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWatcher(dir)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.fsw.Close()

	called := false
	w.OnAdd = func(*Torrent) { called = true }
	if err := w.ScanExisting(); err != nil {
		t.Fatalf("ScanExisting on empty dir: %v", err)
	}
	if called {
		t.Error("OnAdd should not be called in empty directory")
	}
}

// TestWatcherAddCallback verifies that creating a .torrent file fires OnAdd.
func TestWatcherAddCallback(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWatcher(dir)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	added := make(chan *Torrent, 1)
	w.OnAdd = func(tor *Torrent) { added <- tor }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	// Give the watcher a moment to initialise its goroutine.
	time.Sleep(100 * time.Millisecond)

	buildMinimalTorrent(t, dir, "new.torrent")

	select {
	case tor := <-added:
		if tor.Name == "" {
			t.Error("torrent Name should not be empty")
		}
	case <-time.After(3 * time.Second):
		t.Error("OnAdd not called within 3 seconds after file creation")
	}
}

// TestWatcherRemoveCallback verifies that removing a .torrent file fires OnRemove.
func TestWatcherRemoveCallback(t *testing.T) {
	dir := t.TempDir()
	path := buildMinimalTorrent(t, dir, "remove-me.torrent")

	w, err := NewWatcher(dir)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	// Pre-load it so the map knows about the file.
	if err := w.ScanExisting(); err != nil {
		t.Fatalf("ScanExisting: %v", err)
	}

	removed := make(chan *Torrent, 1)
	w.OnRemove = func(tor *Torrent) { removed <- tor }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	time.Sleep(100 * time.Millisecond)

	if err := os.Remove(path); err != nil {
		t.Fatalf("os.Remove: %v", err)
	}

	select {
	case tor := <-removed:
		if tor == nil {
			t.Error("OnRemove received nil torrent")
		}
	case <-time.After(3 * time.Second):
		t.Error("OnRemove not called within 3 seconds after file removal")
	}
}

// TestWatcherGetTorrentsReflectsScan verifies GetTorrents returns scanned entries.
func TestWatcherGetTorrentsReflectsScan(t *testing.T) {
	dir := t.TempDir()
	buildMinimalTorrent(t, dir, "one.torrent")
	buildMinimalTorrent(t, dir, "two.torrent")

	w, err := NewWatcher(dir)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.fsw.Close()

	w.OnAdd = func(*Torrent) {}
	if err := w.ScanExisting(); err != nil {
		t.Fatalf("ScanExisting: %v", err)
	}

	torrents := w.GetTorrents()
	if len(torrents) != 2 {
		t.Errorf("GetTorrents: want 2, got %d", len(torrents))
	}
}

// TestWatcherNonTorrentFilesIgnored verifies non-.torrent files are ignored by ScanExisting.
func TestWatcherNonTorrentFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	// Write a plain text file — should be ignored.
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello"), 0o644)
	buildMinimalTorrent(t, dir, "only.torrent")

	w, err := NewWatcher(dir)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.fsw.Close()

	var count int
	w.OnAdd = func(*Torrent) { count++ }
	w.ScanExisting()

	if count != 1 {
		t.Errorf("OnAdd called %d times, want 1 (only .torrent file)", count)
	}
}

// TestWatcherNewWatcherNonExistentDir verifies that NewWatcher fails for a missing directory.
func TestWatcherNewWatcherNonExistentDir(t *testing.T) {
	_, err := NewWatcher("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Error("expected error for non-existent directory, got nil")
	}
}

// TestIsTorrentFile verifies the isTorrentFile helper.
func TestIsTorrentFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"alpha.torrent", true},
		{"ALPHA.TORRENT", true},
		{"alpha.torrent.bak", false},
		{"alpha.txt", false},
		{".torrent", true},
		{"", false},
	}
	for _, tc := range cases {
		got := isTorrentFile(tc.name)
		if got != tc.want {
			t.Errorf("isTorrentFile(%q): want %v, got %v", tc.name, tc.want, got)
		}
	}
}
