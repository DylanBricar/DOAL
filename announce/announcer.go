package announce

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"doal/torrent"
)

// AnnounceResponse holds the parsed response from a tracker announce.
type AnnounceResponse struct {
	Interval int
	Seeders  int
	Leechers int
}

// Announcer manages the announce lifecycle for a single torrent.
type Announcer struct {
	torrent          *torrent.Torrent
	client           *ClientConfig
	interval         int
	seeders          int
	leechers         int
	lastAnnounce     time.Time
	consecutiveFails int
	httpClient       *http.Client
}

// newAnnouncer creates an Announcer for the given torrent and client config.
func newAnnouncer(t *torrent.Torrent, client *ClientConfig, httpCl *http.Client) *Announcer {
	return &Announcer{
		torrent:    t,
		client:     client,
		interval:   1800, // default 30-minute interval
		httpClient: httpCl,
	}
}

// Announce sends a single announce request to the first reachable tracker
// in the torrent's announce list and returns the parsed response.
func (a *Announcer) Announce(params AnnounceParams) (*AnnounceResponse, error) {
	if len(a.torrent.AnnounceURLs) == 0 {
		return nil, fmt.Errorf("announcer: no tracker URLs for %q", a.torrent.Name)
	}

	var (
		resp *AnnounceResponse
		err  error
	)
	for _, trackerURL := range a.torrent.AnnounceURLs {
		resp, err = a.announceToTracker(trackerURL, params)
		if err == nil {
			a.consecutiveFails = 0
			a.lastAnnounce = time.Now()
			if resp.Interval > 0 {
				a.interval = resp.Interval
			}
			a.seeders = resp.Seeders
			a.leechers = resp.Leechers
			return resp, nil
		}
	}

	a.consecutiveFails++
	return nil, fmt.Errorf("announcer: all trackers failed for %q: %w", a.torrent.Name, err)
}

// announceToTracker performs a single HTTP GET to a specific tracker URL.
func (a *Announcer) announceToTracker(trackerURL string, params AnnounceParams) (*AnnounceResponse, error) {
	fullURL := a.client.BuildAnnounceURL(trackerURL, params)

	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %q: %w", trackerURL, err)
	}

	for _, h := range a.client.RequestHeaders {
		// Skip dynamic placeholder headers (e.g., Accept-Language: {locale}).
		if strings.Contains(h.Value, "{") {
			continue
		}
		req.Header.Set(h.Name, h.Value)
	}

	httpResp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %q: %w", trackerURL, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("tracker %q returned HTTP %d", trackerURL, httpResp.StatusCode)
	}

	body, err := readBody(httpResp)
	if err != nil {
		return nil, fmt.Errorf("reading response from %q: %w", trackerURL, err)
	}

	return parseTrackerResponse(body)
}

// readBody reads the HTTP response body, transparently decompressing gzip.
func readBody(resp *http.Response) ([]byte, error) {
	reader := resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("creating gzip reader: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	return io.ReadAll(reader)
}

// parseTrackerResponse decodes a bencoded tracker response and extracts
// interval, complete (seeders), and incomplete (leechers).
func parseTrackerResponse(data []byte) (*AnnounceResponse, error) {
	dict, err := decodeBencodeDict(data)
	if err != nil {
		return nil, fmt.Errorf("decoding tracker response: %w", err)
	}

	if failReason, ok := dict["failure reason"]; ok {
		return nil, fmt.Errorf("tracker failure: %s", failReason)
	}

	resp := &AnnounceResponse{
		Interval: intFromDict(dict, "interval"),
		Seeders:  intFromDict(dict, "complete"),
		Leechers: intFromDict(dict, "incomplete"),
	}

	return resp, nil
}

// decodeBencodeDict decodes the top-level bencoded dictionary from raw bytes.
func decodeBencodeDict(data []byte) (map[string]string, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty response")
	}
	if data[0] != 'd' {
		return nil, fmt.Errorf("expected bencode dict, got %q", data[0])
	}

	result := make(map[string]string)
	i := 1 // skip leading 'd'

	for i < len(data) && data[i] != 'e' {
		// Read key (always a bencode string).
		key, next, err := bencodeReadString(data, i)
		if err != nil {
			return nil, fmt.Errorf("reading key: %w", err)
		}
		i = next

		if i >= len(data) {
			return nil, fmt.Errorf("truncated bencode dict after key %q", key)
		}

		// Read value — we only care about string and integer values at the
		// top level; nested dicts/lists are consumed but ignored.
		switch data[i] {
		case 'i':
			val, next, err := bencodeReadInt(data, i)
			if err != nil {
				return nil, fmt.Errorf("reading int for key %q: %w", key, err)
			}
			result[key] = strconv.FormatInt(val, 10)
			i = next
		case 'd', 'l':
			// Skip nested structures.
			next, err := bencodeSkip(data, i)
			if err != nil {
				return nil, fmt.Errorf("skipping value for key %q: %w", key, err)
			}
			i = next
		default:
			// Assume string.
			val, next, err := bencodeReadString(data, i)
			if err != nil {
				return nil, fmt.Errorf("reading string for key %q: %w", key, err)
			}
			result[key] = val
			i = next
		}
	}

	return result, nil
}

// bencodeReadString reads a bencode byte string: "<len>:<bytes>".
func bencodeReadString(data []byte, offset int) (string, int, error) {
	colonIdx := offset
	for colonIdx < len(data) && data[colonIdx] != ':' {
		colonIdx++
	}
	if colonIdx >= len(data) {
		return "", offset, fmt.Errorf("no colon in bencode string at offset %d", offset)
	}

	lenStr := string(data[offset:colonIdx])
	length, err := strconv.Atoi(lenStr)
	if err != nil {
		return "", offset, fmt.Errorf("invalid bencode string length %q: %w", lenStr, err)
	}

	start := colonIdx + 1
	end := start + length
	if end > len(data) {
		return "", offset, fmt.Errorf("bencode string length %d exceeds data", length)
	}

	return string(data[start:end]), end, nil
}

// bencodeReadInt reads a bencode integer: "i<digits>e".
func bencodeReadInt(data []byte, offset int) (int64, int, error) {
	// skip 'i'
	offset++
	end := offset
	for end < len(data) && data[end] != 'e' {
		end++
	}
	if end >= len(data) {
		return 0, offset, fmt.Errorf("unterminated bencode integer")
	}
	n, err := strconv.ParseInt(string(data[offset:end]), 10, 64)
	if err != nil {
		return 0, offset, fmt.Errorf("parsing bencode integer %q: %w", data[offset:end], err)
	}
	return n, end + 1, nil
}

// bencodeSkip advances past a single bencode value without storing it.
func bencodeSkip(data []byte, offset int) (int, error) {
	if offset >= len(data) {
		return offset, fmt.Errorf("unexpected end of data")
	}
	switch {
	case data[offset] == 'i':
		_, next, err := bencodeReadInt(data, offset)
		return next, err
	case data[offset] == 'd':
		offset++ // skip 'd'
		for offset < len(data) && data[offset] != 'e' {
			next, err := bencodeSkip(data, offset)
			if err != nil {
				return offset, err
			}
			offset = next
			if offset < len(data) && data[offset] != 'e' {
				next, err = bencodeSkip(data, offset)
				if err != nil {
					return offset, err
				}
				offset = next
			}
		}
		return offset + 1, nil
	case data[offset] == 'l':
		offset++ // skip 'l'
		for offset < len(data) && data[offset] != 'e' {
			next, err := bencodeSkip(data, offset)
			if err != nil {
				return offset, err
			}
			offset = next
		}
		return offset + 1, nil
	case data[offset] >= '0' && data[offset] <= '9':
		_, next, err := bencodeReadString(data, offset)
		return next, err
	default:
		return offset, fmt.Errorf("unknown bencode type %q at offset %d", data[offset], offset)
	}
}

// intFromDict extracts an integer stored as a string from the parsed dict.
func intFromDict(dict map[string]string, key string) int {
	v, ok := dict[key]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}
