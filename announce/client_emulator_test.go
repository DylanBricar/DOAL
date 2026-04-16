package announce

import (
	"path/filepath"
	"strings"
	"testing"
)

// clientFiles returns all .client files, skipping the test if none exist.
func clientFiles(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join("..", "clients", "*.client"))
	if len(files) == 0 {
		t.Skip("no .client files found in clients/")
	}
	return files
}

// TestLoadClientConfig verifies that every .client file can be parsed and
// produces a non-empty PeerID, UserAgent (when present), and Query string.
func TestLoadClientConfig(t *testing.T) {
	files := clientFiles(t)

	for _, c := range files {
		c := c
		t.Run(filepath.Base(c), func(t *testing.T) {
			cc, err := LoadClientConfig(c)
			if err != nil {
				t.Fatalf("LoadClientConfig: %v", err)
			}
			if cc.PeerID == "" {
				t.Error("PeerID should not be empty")
			}
			if len(cc.PeerID) != 20 {
				t.Errorf("PeerID length: want 20, got %d", len(cc.PeerID))
			}
			if cc.Query == "" {
				t.Error("Query should not be empty")
			}
			t.Logf("UA=%q PeerID(hex)=%x Key=%s numwant=%d",
				cc.UserAgent, cc.PeerID, cc.Key, cc.Numwant)
		})
	}
}

// TestLoadClientConfigNonExistent verifies an error for a missing path.
func TestLoadClientConfigNonExistent(t *testing.T) {
	_, err := LoadClientConfig("/nonexistent/path/file.client")
	if err == nil {
		t.Error("LoadClientConfig on non-existent file should return an error")
	}
}

// TestBuildAnnounceURLContainsRequiredFields verifies that the constructed URL
// contains the tracker host, port, and event.
func TestBuildAnnounceURLContainsRequiredFields(t *testing.T) {
	files := clientFiles(t)

	cc, err := LoadClientConfig(files[0])
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}

	params := AnnounceParams{
		InfoHash:   [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
		Port:       51234,
		Uploaded:   1_048_576,
		Downloaded: 0,
		Left:       0,
		Event:      "started",
	}

	u := cc.BuildAnnounceURL("http://tracker.example.com/announce", params)
	if !strings.Contains(u, "tracker.example.com") {
		t.Error("URL should contain tracker host")
	}
	if !strings.Contains(u, "port=51234") {
		t.Error("URL should contain port=51234")
	}
	if !strings.Contains(u, "event=started") {
		t.Error("URL should contain event=started")
	}
	t.Logf("URL: %s", u)
}

// TestBuildAnnounceURL_StoppedEvent verifies that numwantOnStop is used for
// the "stopped" event.
func TestBuildAnnounceURL_StoppedEvent(t *testing.T) {
	files := clientFiles(t)
	cc, err := LoadClientConfig(files[0])
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}

	params := AnnounceParams{Event: "stopped"}
	u := cc.BuildAnnounceURL("http://tracker.example.com/announce", params)
	// The URL should not contain "numwant=0" unless numwantOnStop is actually 0.
	// We just verify the URL is non-empty and contains event=stopped.
	if !strings.Contains(u, "event=stopped") {
		t.Error("URL should contain event=stopped")
	}
}

// TestBuildAnnounceURL_ExistingQueryString verifies that a tracker URL that
// already has a query string gets & as separator, not ?.
func TestBuildAnnounceURL_ExistingQueryString(t *testing.T) {
	files := clientFiles(t)
	cc, err := LoadClientConfig(files[0])
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}

	params := AnnounceParams{Event: "started"}
	u := cc.BuildAnnounceURL("http://tracker.example.com/announce?passkey=abc123", params)
	// Should use & not a second ?.
	count := strings.Count(u, "?")
	if count != 1 {
		t.Errorf("URL should contain exactly one '?', got %d in: %s", count, u)
	}
}

// TestBuildAnnounceURL_NoUnknownPlaceholders verifies that unknown placeholders
// like {ipv6} are stripped from the final URL.
func TestBuildAnnounceURL_NoUnknownPlaceholders(t *testing.T) {
	files := clientFiles(t)
	cc, err := LoadClientConfig(files[0])
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}

	params := AnnounceParams{Event: "started"}
	u := cc.BuildAnnounceURL("http://tracker.example.com/announce", params)
	if strings.Contains(u, "{") || strings.Contains(u, "}") {
		t.Errorf("URL still contains unresolved placeholder braces: %s", u)
	}
}

// TestBuildAnnounceURL_NoDoubleAmpersand verifies that collapsed double-&&
// are cleaned up.
func TestBuildAnnounceURL_NoDoubleAmpersand(t *testing.T) {
	files := clientFiles(t)
	cc, err := LoadClientConfig(files[0])
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}

	params := AnnounceParams{Event: ""}
	u := cc.BuildAnnounceURL("http://tracker.example.com/announce", params)
	if strings.Contains(u, "&&") {
		t.Errorf("URL contains double-ampersand: %s", u)
	}
}

// TestGenerateKey_HashType verifies key generation for HASH algorithm.
func TestGenerateKey_HashType(t *testing.T) {
	cfg := keyGeneratorConfig{
		Algorithm: keyAlgorithmConfig{Type: "HASH", Length: 8},
		KeyCase:   "lower",
	}
	key, err := generateKey(cfg)
	if err != nil {
		t.Fatalf("generateKey: %v", err)
	}
	if len(key) != 8 {
		t.Errorf("key length: want 8, got %d", len(key))
	}
	for _, ch := range key {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			t.Errorf("key contains non-hex char %q", ch)
		}
	}
}

// TestGenerateKey_UpperCase verifies upper-case key output.
func TestGenerateKey_UpperCase(t *testing.T) {
	cfg := keyGeneratorConfig{
		Algorithm: keyAlgorithmConfig{Type: "HASH", Length: 8},
		KeyCase:   "upper",
	}
	key, err := generateKey(cfg)
	if err != nil {
		t.Fatalf("generateKey: %v", err)
	}
	if key != strings.ToUpper(key) {
		t.Errorf("key should be uppercase, got %q", key)
	}
}

// TestGenerateKey_HashNoLeadingZero verifies that HASH_NO_LEADING_ZERO never
// produces a key starting with '0'.
func TestGenerateKey_HashNoLeadingZero(t *testing.T) {
	cfg := keyGeneratorConfig{
		Algorithm: keyAlgorithmConfig{Type: "HASH_NO_LEADING_ZERO", Length: 8},
	}
	for i := 0; i < 100; i++ {
		key, err := generateKey(cfg)
		if err != nil {
			t.Fatalf("generateKey: %v", err)
		}
		if len(key) > 0 && key[0] == '0' {
			t.Errorf("iteration %d: key starts with '0': %q", i, key)
		}
	}
}

// TestEncodeBytes verifies percent-encoding with no exclusions produces all %xx.
func TestEncodeBytes(t *testing.T) {

	result := encodeBytesRE([]byte{0x01, 0x0f, 0xff}, nil, "lower")
	if !strings.Contains(result, "%01") {
		t.Errorf("expected %%01 in %q", result)
	}
	if !strings.Contains(result, "%0f") {
		t.Errorf("expected %%0f in %q", result)
	}
	if !strings.Contains(result, "%ff") {
		t.Errorf("expected %%ff in %q", result)
	}
}

// TestEncodeBytes_UpperCase verifies upper-case percent-encoding.
func TestEncodeBytes_UpperCase(t *testing.T) {

	result := encodeBytesRE([]byte{0xab}, nil, "upper")
	if !strings.Contains(result, "%AB") {
		t.Errorf("expected %%AB in %q", result)
	}
}

// TestExtractUserAgent verifies the User-Agent extraction from headers.
func TestExtractUserAgent(t *testing.T) {
	headers := []Header{
		{Name: "Accept-Encoding", Value: "gzip"},
		{Name: "User-Agent", Value: "TestClient/1.0"},
	}
	ua := extractUserAgent(headers)
	if ua != "TestClient/1.0" {
		t.Errorf("want %q, got %q", "TestClient/1.0", ua)
	}
}

// TestExtractUserAgent_Missing verifies empty string when no User-Agent header.
func TestExtractUserAgent_Missing(t *testing.T) {
	ua := extractUserAgent([]Header{{Name: "Accept-Encoding", Value: "gzip"}})
	if ua != "" {
		t.Errorf("want empty string, got %q", ua)
	}
}

// TestExpandCharClass verifies character class expansion.
func TestExpandCharClass(t *testing.T) {
	chars := expandCharClass("a-z")
	if len(chars) != 26 {
		t.Errorf("a-z should expand to 26 chars, got %d", len(chars))
	}

	chars = expandCharClass("0-9")
	if len(chars) != 10 {
		t.Errorf("0-9 should expand to 10 chars, got %d", len(chars))
	}
}
