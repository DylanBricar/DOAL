package announce

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadBodyRejectsOversizedPlainAndGzipResponses(t *testing.T) {
	oversized := bytes.Repeat([]byte{'x'}, int(maxTrackerResponseBytes+1))
	plain := &http.Response{Body: ioNopCloser{Reader: bytes.NewReader(oversized)}, Header: make(http.Header)}
	if _, err := readBody(plain); err == nil {
		t.Fatal("oversized plain tracker response was accepted")
	}

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(oversized); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	gzipped := &http.Response{
		Body:   ioNopCloser{Reader: bytes.NewReader(compressed.Bytes())},
		Header: http.Header{"Content-Encoding": []string{"gzip"}},
	}
	if _, err := readBody(gzipped); err == nil {
		t.Fatal("oversized decompressed tracker response was accepted")
	}
}

func TestMatchedRingSnapshotIsNotBlockedByTrackerIO(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-requestStarted:
		default:
			close(requestStarted)
		}
		<-releaseRequest
		_, _ = w.Write([]byte("d8:intervali60ee"))
	}))
	defer tracker.Close()

	ring, err := newMatchedRing(matchedRingTorrent(tracker.URL, 100), matchedRingClient(), tracker.Client(), 2, 6881, "", 0)
	if err != nil {
		t.Fatalf("newMatchedRing: %v", err)
	}
	matchDone := make(chan error, 1)
	go func() { matchDone <- ring.matchUploaded(1) }()
	<-requestStarted

	snapshotDone := make(chan struct{})
	go func() {
		_ = ring.snapshot()
		close(snapshotDone)
	}()
	select {
	case <-snapshotDone:
	case <-time.After(200 * time.Millisecond):
		close(releaseRequest)
		<-matchDone
		t.Fatal("snapshot blocked behind tracker network I/O")
	}
	close(releaseRequest)
	if err := <-matchDone; err != nil {
		t.Fatalf("matchUploaded: %v", err)
	}
}

func TestMatchedRingRetainsOnlyActiveIdentities(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("d8:intervali60ee"))
	}))
	defer tracker.Close()

	ring, err := newMatchedRing(matchedRingTorrent(tracker.URL, 1), matchedRingClient(), tracker.Client(), 2, 6881, "", 0)
	if err != nil {
		t.Fatalf("newMatchedRing: %v", err)
	}
	if err := ring.matchUploaded(20); err != nil {
		t.Fatalf("matchUploaded: %v", err)
	}
	if got, want := len(ring.usedIDs), len(ring.actors); got > want {
		t.Fatalf("retained identities=%d, active actors=%d", got, want)
	}
}

type ioNopCloser struct{ *bytes.Reader }

func (ioNopCloser) Close() error { return nil }
