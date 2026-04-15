package torrent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher monitors a directory for .torrent file additions and removals.
type Watcher struct {
	dir      string
	torrents map[string]*Torrent // filepath -> torrent
	mu       sync.RWMutex
	OnAdd    func(*Torrent)
	OnRemove func(*Torrent)

	fsw  *fsnotify.Watcher
	done chan struct{}
}

// NewWatcher creates a Watcher for the given directory.
// OnAdd and OnRemove callbacks must be set before calling Start.
func NewWatcher(dir string) (*Watcher, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("watcher: resolving directory %q: %w", dir, err)
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher: creating fsnotify watcher: %w", err)
	}

	if err := fsw.Add(absDir); err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watcher: watching directory %q: %w", absDir, err)
	}

	return &Watcher{
		dir:      absDir,
		torrents: make(map[string]*Torrent),
		fsw:      fsw,
		done:     make(chan struct{}),
	}, nil
}

// ScanExisting loads all .torrent files currently present in the directory
// and fires OnAdd for each one found. Safe to call before Start.
func (w *Watcher) ScanExisting() error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return fmt.Errorf("watcher: scanning %q: %w", w.dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !isTorrentFile(entry.Name()) {
			continue
		}
		fullPath := filepath.Join(w.dir, entry.Name())
		w.handleAdd(fullPath)
	}

	return nil
}

// Start begins watching for file-system events. It blocks until ctx is done
// or Stop is called. Intended to run in its own goroutine.
func (w *Watcher) Start(ctx context.Context) {
	defer close(w.done)

	for {
		select {
		case <-ctx.Done():
			w.fsw.Close()
			return

		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(event)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// Errors from the watcher are non-fatal; log and continue.
			fmt.Fprintf(os.Stderr, "watcher: fsnotify error: %v\n", err)
		}
	}
}

// Stop signals the watcher to shut down and waits for it to finish.
func (w *Watcher) Stop() {
	w.fsw.Close()
	<-w.done
}

// GetTorrents returns a snapshot of all currently tracked torrents.
func (w *Watcher) GetTorrents() []*Torrent {
	w.mu.RLock()
	defer w.mu.RUnlock()

	result := make([]*Torrent, 0, len(w.torrents))
	for _, t := range w.torrents {
		result = append(result, t)
	}
	return result
}

// handleEvent dispatches file-system events to the appropriate handler.
func (w *Watcher) handleEvent(event fsnotify.Event) {
	if !isTorrentFile(event.Name) {
		return
	}

	switch {
	case event.Has(fsnotify.Create) || event.Has(fsnotify.Write):
		w.handleAdd(event.Name)
	case event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename):
		w.handleRemove(event.Name)
	}
}

// handleAdd parses the torrent file and fires OnAdd if successful.
// Retries up to 5 times with delay to handle Windows file locking.
func (w *Watcher) handleAdd(path string) {
	var t *Torrent
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		t, err = ParseFile(path)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "watcher: parsing %q: %v\n", path, err)
		return
	}

	w.mu.Lock()
	w.torrents[path] = t
	w.mu.Unlock()

	if w.OnAdd != nil {
		w.OnAdd(t)
	}
}

// handleRemove removes the torrent from the map and fires OnRemove.
func (w *Watcher) handleRemove(path string) {
	w.mu.Lock()
	t, exists := w.torrents[path]
	if exists {
		delete(w.torrents, path)
	}
	w.mu.Unlock()

	if exists && w.OnRemove != nil {
		w.OnRemove(t)
	}
}

// isTorrentFile returns true if the filename ends with ".torrent".
func isTorrentFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".torrent")
}
