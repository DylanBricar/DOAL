package announce

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"doal/torrent"
)

const maxLabSybilPeers = 8

type matchedActor struct {
	announcer  *Announcer
	downloaded int64
	started    bool
	generation int
}

type matchedRing struct {
	mu   sync.Mutex
	opMu sync.Mutex

	torrent    *torrent.Torrent
	baseClient *ClientConfig
	httpClient *http.Client
	actors     []*matchedActor
	usedIDs    map[string]struct{}
	port       int
	announceIP string
	cursor     int

	accountedUploaded int64
	totalDownloaded   int64
	generations       int
}

type matchedRingSnapshot struct {
	AccountedUploaded int64
	TotalDownloaded   int64
	Generations       int
	PeerIDs           []string
	CurrentDownloaded []int64
}

func newMatchedRing(
	t *torrent.Torrent,
	baseClient *ClientConfig,
	httpClient *http.Client,
	peerCount int,
	port int,
	announceIP string,
	baselineUploaded int64,
) (*matchedRing, error) {
	if t == nil || t.Size <= 0 {
		return nil, fmt.Errorf("matched ring requires a non-empty torrent")
	}
	if baseClient == nil || httpClient == nil {
		return nil, fmt.Errorf("matched ring requires a client profile and HTTP client")
	}
	if peerCount < 2 || peerCount > maxLabSybilPeers {
		return nil, fmt.Errorf("matched ring peer count must be in [2, %d]", maxLabSybilPeers)
	}
	for _, trackerURL := range t.AnnounceURLs {
		if !IsSupportedTrackerURL(trackerURL) {
			return nil, fmt.Errorf("tracker %q is not a supported HTTP(S) URL", trackerURL)
		}
	}
	if baselineUploaded < 0 {
		baselineUploaded = 0
	}

	ring := &matchedRing{
		torrent:           t,
		baseClient:        baseClient,
		httpClient:        httpClient,
		usedIDs:           make(map[string]struct{}),
		port:              port,
		announceIP:        announceIP,
		accountedUploaded: baselineUploaded,
	}
	for i := 0; i < peerCount; i++ {
		actor, err := ring.freshActor()
		if err != nil {
			return nil, err
		}
		ring.actors = append(ring.actors, actor)
	}
	return ring, nil
}

func (r *matchedRing) freshActor() (*matchedActor, error) {
	for attempt := 0; attempt < 16; attempt++ {
		client, err := r.baseClient.CloneWithFreshIdentity()
		if err != nil {
			return nil, err
		}
		if _, exists := r.usedIDs[client.PeerID]; exists {
			continue
		}
		r.usedIDs[client.PeerID] = struct{}{}
		r.generations++
		return &matchedActor{
			announcer:  newAnnouncer(r.torrent, client, r.httpClient),
			generation: r.generations,
		}, nil
	}
	return nil, fmt.Errorf("could not generate a unique matched-ring identity")
}

// matchUploaded distributes the newly observed cumulative upload among the
// counterparty downloads. State advances only after successful announces.
func (r *matchedRing) matchUploaded(cumulativeUploaded int64) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	r.mu.Lock()
	if cumulativeUploaded <= r.accountedUploaded {
		r.mu.Unlock()
		return nil
	}
	remaining := cumulativeUploaded - r.accountedUploaded
	r.mu.Unlock()

	for remaining > 0 {
		progressed := false
		for step := 0; step < len(r.actors) && remaining > 0; step++ {
			r.mu.Lock()
			index := r.cursor % len(r.actors)
			actor := r.actors[index]
			downloaded := actor.downloaded
			r.mu.Unlock()
			if downloaded >= r.torrent.Size {
				if err := r.rotateActor(index); err != nil {
					return err
				}
				r.mu.Lock()
				actor = r.actors[index]
				downloaded = actor.downloaded
				r.mu.Unlock()
			}

			slots := int64(len(r.actors) - step)
			allocation := (remaining + slots - 1) / slots
			capacity := r.torrent.Size - downloaded
			if allocation > capacity {
				allocation = capacity
			}
			if allocation <= 0 {
				r.mu.Lock()
				r.cursor = (r.cursor + 1) % len(r.actors)
				r.mu.Unlock()
				continue
			}
			if err := r.applyDownload(actor, allocation); err != nil {
				return err
			}
			remaining -= allocation
			r.mu.Lock()
			r.accountedUploaded += allocation
			r.totalDownloaded += allocation
			r.cursor = (r.cursor + 1) % len(r.actors)
			r.mu.Unlock()
			progressed = true
		}
		if !progressed {
			return fmt.Errorf("matched ring made no accounting progress")
		}
	}
	return nil
}

func (r *matchedRing) applyDownload(actor *matchedActor, allocation int64) error {
	r.mu.Lock()
	started := actor.started
	downloaded := actor.downloaded
	r.mu.Unlock()
	if !started {
		if _, err := actor.announcer.Announce(r.params(actor, 0, "started")); err != nil {
			return fmt.Errorf("starting matched counterparty: %w", err)
		}
		r.mu.Lock()
		actor.started = true
		r.mu.Unlock()
	}

	nextDownloaded := downloaded + allocation
	event := ""
	if nextDownloaded == r.torrent.Size {
		event = "completed"
	}
	params := r.params(actor, nextDownloaded, event)
	if _, err := actor.announcer.Announce(params); err != nil {
		return fmt.Errorf("updating matched counterparty: %w", err)
	}
	r.mu.Lock()
	actor.downloaded = nextDownloaded
	r.mu.Unlock()
	return nil
}

func (r *matchedRing) rotateActor(index int) error {
	r.mu.Lock()
	old := r.actors[index]
	started := old.started
	downloaded := old.downloaded
	r.mu.Unlock()
	if started {
		if _, err := old.announcer.Announce(r.params(old, downloaded, "stopped")); err != nil {
			return fmt.Errorf("stopping completed matched counterparty: %w", err)
		}
		r.mu.Lock()
		old.started = false
		r.mu.Unlock()
	}
	r.mu.Lock()
	delete(r.usedIDs, old.announcer.client.PeerID)
	fresh, err := r.freshActor()
	if err != nil {
		r.usedIDs[old.announcer.client.PeerID] = struct{}{}
		r.mu.Unlock()
		return err
	}
	r.actors[index] = fresh
	r.mu.Unlock()
	return nil
}

func (r *matchedRing) params(actor *matchedActor, downloaded int64, event string) AnnounceParams {
	left := r.torrent.Size - downloaded
	if left < 0 {
		left = 0
	}
	return AnnounceParams{
		InfoHash:   r.torrent.InfoHash,
		Port:       r.port,
		Uploaded:   0,
		Downloaded: downloaded,
		Left:       left,
		Event:      event,
		IP:         r.announceIP,
	}
}

func (r *matchedRing) stop() error {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	var errs []error
	r.mu.Lock()
	actors := append([]*matchedActor(nil), r.actors...)
	r.mu.Unlock()
	for _, actor := range actors {
		r.mu.Lock()
		started := actor.started
		downloaded := actor.downloaded
		r.mu.Unlock()
		if !started {
			continue
		}
		if _, err := actor.announcer.Announce(r.params(actor, downloaded, "stopped")); err != nil {
			errs = append(errs, err)
			continue
		}
		r.mu.Lock()
		actor.started = false
		r.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (r *matchedRing) snapshot() matchedRingSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	snapshot := matchedRingSnapshot{
		AccountedUploaded: r.accountedUploaded,
		TotalDownloaded:   r.totalDownloaded,
		Generations:       r.generations,
		PeerIDs:           make([]string, 0, len(r.actors)),
		CurrentDownloaded: make([]int64, 0, len(r.actors)),
	}
	for _, actor := range r.actors {
		snapshot.PeerIDs = append(snapshot.PeerIDs, actor.announcer.client.PeerID)
		snapshot.CurrentDownloaded = append(snapshot.CurrentDownloaded, actor.downloaded)
	}
	return snapshot
}
