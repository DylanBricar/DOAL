package torrent

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestParseRealTorrents exercises ParseFile against every .torrent in the
// torrents/ directory and validates the essential fields.
func TestParseRealTorrents(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "torrents", "*.torrent"))
	if len(files) == 0 {
		t.Skip("no .torrent files found in torrents/")
	}

	for _, f := range files {
		f := f // capture
		t.Run(filepath.Base(f), func(t *testing.T) {
			tor, err := ParseFile(f)
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			if tor.Name == "" {
				t.Error("Name should not be empty")
			}
			if tor.Size <= 0 {
				t.Errorf("Size should be > 0, got %d", tor.Size)
			}
			if tor.PieceCount <= 0 {
				t.Errorf("PieceCount should be > 0, got %d", tor.PieceCount)
			}
			if len(tor.AnnounceURLs) == 0 {
				t.Error("AnnounceURLs should have at least one entry")
			}
			if tor.InfoHashHex == "" {
				t.Error("InfoHashHex should not be empty")
			}
			if len(tor.InfoHash) != 20 {
				t.Errorf("InfoHash should be 20 bytes, got %d", len(tor.InfoHash))
			}
			if tor.FilePath == "" {
				t.Error("FilePath should not be empty")
			}
			// InfoHashHex must be valid lower-case hex of length 40.
			if len(tor.InfoHashHex) != 40 {
				t.Errorf("InfoHashHex length: want 40, got %d (%q)", len(tor.InfoHashHex), tor.InfoHashHex)
			}
			for _, ch := range tor.InfoHashHex {
				if !strings.ContainsRune("0123456789abcdef", ch) {
					t.Errorf("InfoHashHex contains non-hex char %q", ch)
					break
				}
			}
			// Each announce URL should look like a URL.
			for _, u := range tor.AnnounceURLs {
				if !strings.HasPrefix(u, "http") && !strings.HasPrefix(u, "udp") {
					t.Errorf("announce URL %q has unexpected scheme", u)
				}
			}
			t.Logf("name=%s size=%d pieces=%d hash=%s urls=%d",
				tor.Name, tor.Size, tor.PieceCount, tor.InfoHashHex, len(tor.AnnounceURLs))
		})
	}
}

// TestParseNonExistentFile verifies an error is returned for a missing path.
func TestParseNonExistentFile(t *testing.T) {
	_, err := ParseFile("/nonexistent/path/file.torrent")
	if err == nil {
		t.Error("ParseFile on non-existent path should return an error")
	}
}

// TestDecodeBencode_Integer validates integer decoding.
func TestDecodeBencode_Integer(t *testing.T) {
	cases := []struct {
		input string
		want  int64
	}{
		{"i42e", 42},
		{"i0e", 0},
		{"i-7e", -7},
		{"i9999999e", 9999999},
	}
	for _, tc := range cases {
		val, _, err := decodeBencode([]byte(tc.input), 0)
		if err != nil {
			t.Errorf("decodeBencode(%q): %v", tc.input, err)
			continue
		}
		n, ok := val.(int64)
		if !ok {
			t.Errorf("decodeBencode(%q): expected int64, got %T", tc.input, val)
			continue
		}
		if n != tc.want {
			t.Errorf("decodeBencode(%q): want %d, got %d", tc.input, tc.want, n)
		}
	}
}

// TestDecodeBencode_String validates string decoding.
func TestDecodeBencode_String(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"4:spam", "spam"},
		{"0:", ""},
		{"5:hello", "hello"},
	}
	for _, tc := range cases {
		val, _, err := decodeBencode([]byte(tc.input), 0)
		if err != nil {
			t.Errorf("decodeBencode(%q): %v", tc.input, err)
			continue
		}
		s, ok := val.(string)
		if !ok {
			t.Errorf("decodeBencode(%q): expected string, got %T", tc.input, val)
			continue
		}
		if s != tc.want {
			t.Errorf("decodeBencode(%q): want %q, got %q", tc.input, tc.want, s)
		}
	}
}

// TestDecodeBencode_List validates list decoding.
func TestDecodeBencode_List(t *testing.T) {
	val, _, err := decodeBencode([]byte("l4:spami42ee"), 0)
	if err != nil {
		t.Fatalf("decodeBencode list: %v", err)
	}
	list, ok := val.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", val)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(list))
	}
	if list[0].(string) != "spam" {
		t.Errorf("list[0]: want %q, got %q", "spam", list[0])
	}
	if list[1].(int64) != 42 {
		t.Errorf("list[1]: want 42, got %v", list[1])
	}
}

// TestDecodeBencode_Dict validates dictionary decoding.
func TestDecodeBencode_Dict(t *testing.T) {
	val, _, err := decodeBencode([]byte("d3:cow3:moo4:spami42ee"), 0)
	if err != nil {
		t.Fatalf("decodeBencode dict: %v", err)
	}
	dict, ok := val.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", val)
	}
	if dict["cow"].(string) != "moo" {
		t.Errorf("dict[cow]: want %q, got %v", "moo", dict["cow"])
	}
	if dict["spam"].(int64) != 42 {
		t.Errorf("dict[spam]: want 42, got %v", dict["spam"])
	}
}

// TestIndexBytes validates the internal byte-search helper.
func TestIndexBytes(t *testing.T) {
	cases := []struct {
		haystack string
		needle   string
		want     int
	}{
		{"hello world", "world", 6},
		{"hello world", "hello", 0},
		{"hello world", "xyz", -1},
		{"aaa", "aaa", 0},
		{"", "x", -1},
		{"abc", "", 0},
	}
	for _, tc := range cases {
		got := indexBytes([]byte(tc.haystack), []byte(tc.needle))
		if got != tc.want {
			t.Errorf("indexBytes(%q, %q): want %d, got %d", tc.haystack, tc.needle, tc.want, got)
		}
	}
}

// TestAnnounceURLDeduplication ensures duplicate tracker URLs are collapsed.
func TestAnnounceURLDeduplication(t *testing.T) {
	dict := map[string]any{
		"announce": "http://tracker.example.com/announce",
		"announce-list": []any{
			[]any{"http://tracker.example.com/announce"}, // duplicate
			[]any{"udp://tracker2.example.com:1337"},
		},
	}
	urls := extractAnnounceURLs(dict)
	if len(urls) != 2 {
		t.Errorf("expected 2 unique URLs, got %d: %v", len(urls), urls)
	}
}
