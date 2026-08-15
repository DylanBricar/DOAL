package announce

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"doal/torrent"
)

func TestMatchedRingConservesUploadedBytes(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	requests := 0
	started := 0
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		if r.URL.Query().Get("event") == "started" {
			started++
		}
		mu.Unlock()
		_, _ = w.Write([]byte("d8:intervali60e8:completei1e10:incompletei1ee"))
	}))
	defer tracker.Close()

	tor := matchedRingTorrent(tracker.URL, 100)
	ring, err := newMatchedRing(tor, matchedRingClient(), tracker.Client(), 3, 6881, "", 10)
	if err != nil {
		t.Fatalf("newMatchedRing: %v", err)
	}
	if err := ring.matchUploaded(100); err != nil {
		t.Fatalf("matchUploaded: %v", err)
	}

	snapshot := ring.snapshot()
	if snapshot.AccountedUploaded != 100 {
		t.Fatalf("accounted upload = %d, want 100", snapshot.AccountedUploaded)
	}
	if snapshot.TotalDownloaded != 90 {
		t.Fatalf("matched download = %d, want delta 90", snapshot.TotalDownloaded)
	}
	if len(snapshot.PeerIDs) != 3 || uniqueStrings(snapshot.PeerIDs) != 3 {
		t.Fatalf("counterparty peer IDs are not unique: %q", snapshot.PeerIDs)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 6 || started != 3 {
		t.Fatalf("tracker requests=%d started=%d, want 6 and 3", requests, started)
	}
}

func TestMatchedRingRotatesCompletedCounterparties(t *testing.T) {
	t.Parallel()

	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("d8:intervali60ee"))
	}))
	defer tracker.Close()

	ring, err := newMatchedRing(matchedRingTorrent(tracker.URL, 10), matchedRingClient(), tracker.Client(), 2, 6881, "", 0)
	if err != nil {
		t.Fatalf("newMatchedRing: %v", err)
	}
	if err := ring.matchUploaded(25); err != nil {
		t.Fatalf("matchUploaded: %v", err)
	}
	snapshot := ring.snapshot()
	if snapshot.TotalDownloaded != 25 || snapshot.AccountedUploaded != 25 {
		t.Fatalf("snapshot=%+v, want exact 25-byte conservation", snapshot)
	}
	if snapshot.Generations < 4 {
		t.Fatalf("generations=%d, want at least 4 after rollover", snapshot.Generations)
	}
	for _, downloaded := range snapshot.CurrentDownloaded {
		if downloaded > 10 {
			t.Fatalf("actor reported %d bytes for a 10-byte torrent", downloaded)
		}
	}
}

func TestMatchedRingDoesNotCommitFailedAnnounce(t *testing.T) {
	t.Parallel()

	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer tracker.Close()

	ring, err := newMatchedRing(matchedRingTorrent(tracker.URL, 100), matchedRingClient(), tracker.Client(), 2, 6881, "", 0)
	if err != nil {
		t.Fatalf("newMatchedRing: %v", err)
	}
	if err := ring.matchUploaded(25); err == nil {
		t.Fatal("matchUploaded should fail when the tracker rejects the announce")
	}
	snapshot := ring.snapshot()
	if snapshot.TotalDownloaded != 0 || snapshot.AccountedUploaded != 0 {
		t.Fatalf("failed announce changed accounting: %+v", snapshot)
	}
}

func matchedRingTorrent(announceURL string, size int64) *torrent.Torrent {
	return &torrent.Torrent{
		InfoHashHex:  "00112233445566778899aabbccddeeff00112233",
		Name:         "lab",
		Size:         size,
		AnnounceURLs: []string{announceURL},
	}
}

func matchedRingClient() *ClientConfig {
	return &ClientConfig{
		Query:          "info_hash={infohash}&peer_id={peerid}&port={port}&uploaded={uploaded}&downloaded={downloaded}&left={left}&key={key}&event={event}&numwant={numwant}&compact=1",
		Numwant:        50,
		RequestHeaders: []Header{{Name: "User-Agent", Value: "qBittorrent/5.0.0"}},
		PeerID:         "-qB5000-aaaaaaaaaaaa",
		Key:            "12345678",
		UserAgent:      "qBittorrent/5.0.0",
		keyGen: keyGeneratorConfig{Algorithm: keyAlgorithmConfig{
			Type: "HASH", Length: 8,
		}},
		peerIDGen: peerIDGeneratorConfig{Algorithm: peerIDAlgorithmConfig{
			Type: "REGEX", Pattern: "-qB5000-[A-Za-z0-9_~()!.*-]{12}",
		}},
	}
}

func uniqueStrings(values []string) int {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	return len(seen)
}
