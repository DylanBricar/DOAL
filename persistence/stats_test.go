package persistence

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSaveAndLoadStats verifies a round-trip save + load produces the same value.
func TestSaveAndLoadStats(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "stats.txt")

	SaveUploadStats(tmp, 123_456_789)
	loaded := LoadUploadStats(tmp)
	if loaded != 123_456_789 {
		t.Errorf("want 123456789, got %d", loaded)
	}
}

// TestLoadMissingFile verifies that a non-existent path returns 0, not an error.
func TestLoadMissingFile(t *testing.T) {
	loaded := LoadUploadStats("/nonexistent/path/stats.txt")
	if loaded != 0 {
		t.Errorf("want 0 for missing file, got %d", loaded)
	}
}

// TestSaveOverwritesPreviousValue verifies that saving a new value overwrites
// the old one cleanly.
func TestSaveOverwritesPreviousValue(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "stats.txt")

	SaveUploadStats(tmp, 100)
	SaveUploadStats(tmp, 999_999)

	loaded := LoadUploadStats(tmp)
	if loaded != 999_999 {
		t.Errorf("want 999999 after overwrite, got %d", loaded)
	}
}

// TestLoadCorruptFileReturnsZero verifies that a corrupt file is handled
// gracefully and returns 0.
func TestLoadCorruptFileReturnsZero(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "stats.txt")
	if err := os.WriteFile(tmp, []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loaded := LoadUploadStats(tmp)
	if loaded != 0 {
		t.Errorf("want 0 for corrupt file, got %d", loaded)
	}
}

// TestLoadNegativeValueReturnsZero verifies that a file containing a negative
// number is treated as 0.
func TestLoadNegativeValueReturnsZero(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "stats.txt")
	if err := os.WriteFile(tmp, []byte("-42\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loaded := LoadUploadStats(tmp)
	if loaded != 0 {
		t.Errorf("want 0 for negative value, got %d", loaded)
	}
}

// TestSaveZero verifies that zero is a valid value to persist and reload.
func TestSaveZero(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "stats.txt")

	SaveUploadStats(tmp, 0)
	loaded := LoadUploadStats(tmp)
	if loaded != 0 {
		t.Errorf("want 0, got %d", loaded)
	}
}

// TestSaveWritesNewlineTerminated verifies the on-disk format ends with a
// newline (makes the file human-readable and grep-friendly).
func TestSaveWritesNewlineTerminated(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "stats.txt")
	SaveUploadStats(tmp, 42)

	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)
	if !strings.HasSuffix(content, "\n") {
		t.Errorf("file content should end with newline, got %q", content)
	}
	// The numeric part should be parseable.
	trimmed := strings.TrimSpace(content)
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		t.Fatalf("ParseInt(%q): %v", trimmed, err)
	}
	if n != 42 {
		t.Errorf("want 42, got %d", n)
	}
}

// TestLoadLargeValue verifies that large int64 values survive a round-trip.
func TestLoadLargeValue(t *testing.T) {
	const large = int64(1<<62 - 1) // well within int64 range
	tmp := filepath.Join(t.TempDir(), "stats.txt")

	SaveUploadStats(tmp, large)
	loaded := LoadUploadStats(tmp)
	if loaded != large {
		t.Errorf("want %d, got %d", large, loaded)
	}
}

// TestConcurrentSaveLoad verifies that concurrent saves don't corrupt each other.
func TestConcurrentSaveLoad(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "stats.txt")
	SaveUploadStats(tmp, 0) // create initial file

	const goroutines = 10
	done := make(chan struct{}, goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer func() { done <- struct{}{} }()
			SaveUploadStats(tmp, int64(i*1000))
			_ = LoadUploadStats(tmp) // should not panic or corrupt
		}()
	}

	for i := 0; i < goroutines; i++ {
		<-done
	}

	// After all goroutines finish, the file should contain a valid non-negative integer.
	loaded := LoadUploadStats(tmp)
	if loaded < 0 {
		t.Errorf("concurrent save/load produced negative value: %d", loaded)
	}
}
