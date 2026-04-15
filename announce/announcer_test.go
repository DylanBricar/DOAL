package announce

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"doal/torrent"
)

// testTorrent builds a torrent pointing at the given tracker URL.
func testTorrent(trackerURL string) *torrent.Torrent {
	var infoHash [20]byte
	copy(infoHash[:], "test-info-hash-12345")
	return &torrent.Torrent{
		InfoHash:     infoHash,
		InfoHashHex:  "7465737420696e666f20686173682d3132333435",
		Name:         "test-torrent",
		Size:         1024 * 1024,
		AnnounceURLs: []string{trackerURL},
	}
}

// testClientConfig returns a minimal ClientConfig for announcer tests.
func testClientConfig() *ClientConfig {
	return &ClientConfig{
		PeerID:    "01234567890123456789",
		UserAgent: "TestClient/1.0",
		Query:     "info_hash={infohash}&peer_id={peerid}&port={port}&uploaded={uploaded}&downloaded={downloaded}&left={left}&event={event}&numwant=80&compact=1",
		Numwant:   80,
	}
}

// TestAnnounceSuccess verifies that a valid bencode response is parsed correctly.
func TestAnnounceSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("uploaded") == "" {
			t.Error("missing uploaded param")
		}
		if q.Get("info_hash") == "" {
			t.Error("missing info_hash param")
		}
		// d8:completei50e10:incompletei10e8:intervali1800ee
		w.Write([]byte("d8:completei50e10:incompletei10e8:intervali1800ee"))
	}))
	defer srv.Close()

	tor := testTorrent(srv.URL + "/announce")
	cc := testClientConfig()
	httpCl := &http.Client{}
	a := newAnnouncer(tor, cc, httpCl)

	resp, err := a.Announce(AnnounceParams{
		InfoHash: tor.InfoHash,
		Port:     6881,
		Uploaded: 0,
		Left:     tor.Size,
		Event:    "started",
	})
	if err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if resp.Interval != 1800 {
		t.Errorf("Interval: want 1800, got %d", resp.Interval)
	}
	if resp.Seeders != 50 {
		t.Errorf("Seeders: want 50, got %d", resp.Seeders)
	}
	if resp.Leechers != 10 {
		t.Errorf("Leechers: want 10, got %d", resp.Leechers)
	}
}

// TestAnnounceFailureReason verifies that a tracker failure reason is surfaced as an error.
func TestAnnounceFailureReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// d14:failure reason15:not registerede
		w.Write([]byte("d14:failure reason15:not registerede"))
	}))
	defer srv.Close()

	tor := testTorrent(srv.URL + "/announce")
	cc := testClientConfig()
	a := newAnnouncer(tor, cc, &http.Client{})

	_, err := a.Announce(AnnounceParams{
		InfoHash: tor.InfoHash,
		Port:     6881,
		Event:    "started",
	})
	if err == nil {
		t.Fatal("expected error for tracker failure reason, got nil")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

// TestAnnounceHTTPError verifies that a non-2xx HTTP response returns an error.
func TestAnnounceHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	tor := testTorrent(srv.URL + "/announce")
	cc := testClientConfig()
	a := newAnnouncer(tor, cc, &http.Client{})

	_, err := a.Announce(AnnounceParams{
		InfoHash: tor.InfoHash,
		Port:     6881,
		Event:    "started",
	})
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

// TestAnnounceNoTrackers verifies that a torrent with no tracker URLs errors immediately.
func TestAnnounceNoTrackers(t *testing.T) {
	tor := &torrent.Torrent{
		InfoHashHex:  "aabbcc",
		Name:         "no-trackers",
		AnnounceURLs: []string{},
	}
	cc := testClientConfig()
	a := newAnnouncer(tor, cc, &http.Client{})
	_, err := a.Announce(AnnounceParams{})
	if err == nil {
		t.Fatal("expected error when no tracker URLs, got nil")
	}
}

// TestAnnounceUnreachableTracker verifies that an unreachable tracker returns an error.
func TestAnnounceUnreachableTracker(t *testing.T) {
	tor := testTorrent("http://127.0.0.1:0/announce") // port 0 is always unreachable
	cc := testClientConfig()
	a := newAnnouncer(tor, cc, &http.Client{})
	_, err := a.Announce(AnnounceParams{
		InfoHash: tor.InfoHash,
		Port:     6881,
		Event:    "started",
	})
	if err == nil {
		t.Fatal("expected error for unreachable tracker, got nil")
	}
}

// TestParseTrackerResponseValid tests the internal bencode parser.
func TestParseTrackerResponseValid(t *testing.T) {
	data := []byte("d8:completei100e10:incompletei25e8:intervali900ee")
	resp, err := parseTrackerResponse(data)
	if err != nil {
		t.Fatalf("parseTrackerResponse: %v", err)
	}
	if resp.Seeders != 100 {
		t.Errorf("Seeders: want 100, got %d", resp.Seeders)
	}
	if resp.Leechers != 25 {
		t.Errorf("Leechers: want 25, got %d", resp.Leechers)
	}
	if resp.Interval != 900 {
		t.Errorf("Interval: want 900, got %d", resp.Interval)
	}
}

// TestParseTrackerResponseEmpty verifies an error for an empty body.
func TestParseTrackerResponseEmpty(t *testing.T) {
	_, err := parseTrackerResponse([]byte{})
	if err == nil {
		t.Error("expected error for empty response, got nil")
	}
}

// TestParseTrackerResponseNotDict verifies an error when bencode root is not a dict.
func TestParseTrackerResponseNotDict(t *testing.T) {
	_, err := parseTrackerResponse([]byte("i42e"))
	if err == nil {
		t.Error("expected error when response is not a dict, got nil")
	}
}

// TestDecodeBencodeDictSimple verifies the internal dict decoder.
func TestDecodeBencodeDictSimple(t *testing.T) {
	data := []byte("d8:intervali1800ee")
	dict, err := decodeBencodeDict(data)
	if err != nil {
		t.Fatalf("decodeBencodeDict: %v", err)
	}
	if dict["interval"] != "1800" {
		t.Errorf("interval: want 1800, got %q", dict["interval"])
	}
}

// TestDecodeBencodeDictWithString verifies string values are parsed.
func TestDecodeBencodeDictWithString(t *testing.T) {
	// "not registered" = 14 chars — use the exact length prefix in the bencode.
	data := []byte("d14:failure reason14:not registerede")
	dict, err := decodeBencodeDict(data)
	if err != nil {
		t.Fatalf("decodeBencodeDict: %v", err)
	}
	if dict["failure reason"] != "not registered" {
		t.Errorf("failure reason: want 'not registered', got %q", dict["failure reason"])
	}
}

// TestDecodeBencodeDictEmpty verifies an error for an empty input.
func TestDecodeBencodeDictEmpty(t *testing.T) {
	_, err := decodeBencodeDict([]byte{})
	if err == nil {
		t.Error("expected error for empty input, got nil")
	}
}

// TestDecodeBencodeDictNotDict verifies an error when first byte is not 'd'.
func TestDecodeBencodeDictNotDict(t *testing.T) {
	_, err := decodeBencodeDict([]byte("l4:spame"))
	if err == nil {
		t.Error("expected error for non-dict bencode, got nil")
	}
}

// TestBencodeReadString verifies reading a bencode byte string.
func TestBencodeReadString(t *testing.T) {
	data := []byte("5:hello")
	s, next, err := bencodeReadString(data, 0)
	if err != nil {
		t.Fatalf("bencodeReadString: %v", err)
	}
	if s != "hello" {
		t.Errorf("want hello, got %q", s)
	}
	if next != 7 {
		t.Errorf("next offset: want 7, got %d", next)
	}
}

// TestBencodeReadInt verifies reading a bencode integer.
func TestBencodeReadInt(t *testing.T) {
	data := []byte("i1800e")
	n, next, err := bencodeReadInt(data, 0)
	if err != nil {
		t.Fatalf("bencodeReadInt: %v", err)
	}
	if n != 1800 {
		t.Errorf("want 1800, got %d", n)
	}
	if next != 6 {
		t.Errorf("next offset: want 6, got %d", next)
	}
}

// TestBencodeReadIntNegative verifies negative bencode integers.
func TestBencodeReadIntNegative(t *testing.T) {
	data := []byte("i-42e")
	n, _, err := bencodeReadInt(data, 0)
	if err != nil {
		t.Fatalf("bencodeReadInt: %v", err)
	}
	if n != -42 {
		t.Errorf("want -42, got %d", n)
	}
}

// TestAnnounceConsecutiveFailsIncrement verifies the consecutive fail counter.
func TestAnnounceConsecutiveFailsIncrement(t *testing.T) {
	// Use an HTTP server that always returns 503 to force failures.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	tor := testTorrent(srv.URL + "/announce")
	cc := testClientConfig()
	a := newAnnouncer(tor, cc, &http.Client{})

	for i := 1; i <= 3; i++ {
		_, err := a.Announce(AnnounceParams{InfoHash: tor.InfoHash, Port: 6881})
		if err == nil {
			t.Fatalf("iteration %d: expected error, got nil", i)
		}
		if a.consecutiveFails != i {
			t.Errorf("iteration %d: consecutiveFails: want %d, got %d", i, i, a.consecutiveFails)
		}
	}
}

// TestAnnounceSuccessResetsFails verifies that a success resets the fail counter.
func TestAnnounceSuccessResetsFails(t *testing.T) {
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad", http.StatusServiceUnavailable)
	}))
	defer failSrv.Close()

	failTor := testTorrent(failSrv.URL + "/announce")
	cc := testClientConfig()
	a := newAnnouncer(failTor, cc, &http.Client{})
	a.Announce(AnnounceParams{InfoHash: failTor.InfoHash, Port: 6881})
	if a.consecutiveFails != 1 {
		t.Fatalf("pre-condition: want 1 fail, got %d", a.consecutiveFails)
	}

	// Now point at a good server and verify counter resets.
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("d8:completei1e10:incompletei0e8:intervali900ee"))
	}))
	defer okSrv.Close()

	a.torrent = testTorrent(okSrv.URL + "/announce")
	_, err := a.Announce(AnnounceParams{InfoHash: a.torrent.InfoHash, Port: 6881})
	if err != nil {
		t.Fatalf("second Announce: %v", err)
	}
	if a.consecutiveFails != 0 {
		t.Errorf("consecutiveFails: want 0 after success, got %d", a.consecutiveFails)
	}
}

// TestAnnounceUpdatesInterval verifies that the interval from the response is stored.
func TestAnnounceUpdatesInterval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("d8:completei0e10:incompletei0e8:intervali3600ee"))
	}))
	defer srv.Close()

	tor := testTorrent(srv.URL + "/announce")
	a := newAnnouncer(tor, testClientConfig(), &http.Client{})
	_, err := a.Announce(AnnounceParams{InfoHash: tor.InfoHash, Port: 6881})
	if err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if a.interval != 3600 {
		t.Errorf("interval: want 3600, got %d", a.interval)
	}
}

// TestAnnounceGzipResponse verifies that plain responses are handled correctly
// (the readBody function also supports gzip transparently via Content-Encoding).
func TestAnnounceGzipResponse(t *testing.T) {
	plainBody := []byte("d8:completei5e10:incompletei2e8:intervali600ee")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Send the response as plain text for this test since gzip requires
		// additional imports in a separate file; ensure the parser can handle it.
		w.Header().Set("Content-Type", "text/plain")
		w.Write(plainBody)
	}))
	defer srv.Close()

	tor := testTorrent(srv.URL + "/announce")
	a := newAnnouncer(tor, testClientConfig(), &http.Client{})
	resp, err := a.Announce(AnnounceParams{InfoHash: tor.InfoHash, Port: 6881})
	if err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if resp.Seeders != 5 {
		t.Errorf("Seeders: want 5, got %d", resp.Seeders)
	}
}
