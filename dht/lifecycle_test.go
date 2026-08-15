package dht

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func TestPruneStateRemovesExpiredQueriesAndPeers(t *testing.T) {
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	node := NewNode(0)
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 6881}

	node.pending["old"] = pendingQuery{addr: addr, createdAt: now.Add(-pendingQueryTTL - time.Second)}
	node.pending["fresh"] = pendingQuery{addr: addr, createdAt: now.Add(-pendingQueryTTL + time.Second)}
	node.peers["hash"] = map[string]storedPeer{
		"old":   {Peer: Peer{IP: "127.0.0.1", Port: 1}, seenAt: now.Add(-peerTTL - time.Second)},
		"fresh": {Peer: Peer{IP: "127.0.0.1", Port: 2}, seenAt: now.Add(-peerTTL + time.Second)},
	}

	node.pruneState(now)

	if _, ok := node.pending["old"]; ok {
		t.Fatal("expired DHT transaction was retained")
	}
	if _, ok := node.pending["fresh"]; !ok {
		t.Fatal("fresh DHT transaction was pruned")
	}
	if _, ok := node.peers["hash"]["old"]; ok {
		t.Fatal("expired DHT peer was retained")
	}
	if _, ok := node.peers["hash"]["fresh"]; !ok {
		t.Fatal("fresh DHT peer was pruned")
	}
}

func TestNewTransactionKeepsPendingMapBounded(t *testing.T) {
	node := NewNode(0)
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 6881}

	for i := 0; i < maxPendingQueries+100; i++ {
		node.newTransaction("ping", [20]byte{}, addr)
	}

	if got := len(node.pending); got > maxPendingQueries {
		t.Fatalf("pending transactions=%d, limit=%d", got, maxPendingQueries)
	}
}

func TestStorePeerKeepsDHTPeerSetsBounded(t *testing.T) {
	node := NewNode(0)
	now := time.Now()

	for i := 0; i < maxPeersPerTorrent+100; i++ {
		node.storePeer("hash", Peer{IP: "127.0.0.1", Port: 1000 + i}, now.Add(time.Duration(i)*time.Nanosecond))
	}

	if got := len(node.peers["hash"]); got > maxPeersPerTorrent {
		t.Fatalf("stored peers=%d, limit=%d", got, maxPeersPerTorrent)
	}

	for i := 0; i < maxTrackedPeerHashes+100; i++ {
		node.storePeer(fmt.Sprintf("%040x", i), Peer{IP: "127.0.0.1", Port: 2000}, now.Add(time.Duration(i)*time.Nanosecond))
	}
	if got := len(node.peers); got > maxTrackedPeerHashes {
		t.Fatalf("stored peer hashes=%d, limit=%d", got, maxTrackedPeerHashes)
	}
}
