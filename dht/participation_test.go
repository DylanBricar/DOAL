package dht

import (
	"bytes"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPrivateNodesExchangeGetPeersAndAnnouncePeer(t *testing.T) {
	first := NewNode(0)
	if err := first.ConfigureNetwork(61001, nil); err != nil {
		t.Fatalf("configure first node: %v", err)
	}
	if err := first.Start(); err != nil {
		t.Fatalf("start first node: %v", err)
	}
	defer first.Stop()

	second := NewNode(0)
	if err := second.ConfigureNetwork(61002, []string{first.Addr().String()}); err != nil {
		t.Fatalf("configure second node: %v", err)
	}
	if err := second.Start(); err != nil {
		t.Fatalf("start second node: %v", err)
	}
	defer second.Stop()

	infoHash := [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	infoHashHex := hex.EncodeToString(infoHash[:])
	second.AddTorrent(infoHashHex)
	second.ParticipateOnce()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if peers := first.Peers(infoHashHex); len(peers) == 1 && peers[0].Port == 61002 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("first node never stored second node's announced peer: %#v", first.Peers(infoHashHex))
}

func TestFindNodeDoesNotInventBootstrapNodeIDs(t *testing.T) {
	t.Parallel()

	node := NewNode(0)
	if err := node.ConfigureNetwork(61003, []string{"127.0.0.1:65534"}); err != nil {
		t.Fatalf("configure node: %v", err)
	}
	if err := node.Start(); err != nil {
		t.Fatalf("start node: %v", err)
	}
	defer node.Stop()

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen test client: %v", err)
	}
	defer client.Close()

	var query bytes.Buffer
	query.WriteString("d1:ad2:id20:")
	query.Write(make([]byte, 20))
	query.WriteString("6:target20:")
	query.Write(make([]byte, 20))
	query.WriteString("e1:q9:find_node1:t2:aa1:y1:qe")
	if _, err := client.WriteToUDP(query.Bytes(), node.Addr()); err != nil {
		t.Fatalf("send find_node: %v", err)
	}
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buffer := make([]byte, 2048)
	nread, _, err := client.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("read find_node response: %v", err)
	}
	response := string(buffer[:nread])
	if !strings.Contains(response, "5:nodes0:") {
		t.Fatalf("find_node invented an ID for a bootstrap endpoint: %q", response)
	}
}

func TestDHTAcceptsPublicBootstrap(t *testing.T) {
	t.Parallel()

	node := NewNode(0)
	if err := node.ConfigureNetwork(6881, []string{"192.0.2.1:6881"}); err != nil {
		t.Fatalf("expected public bootstrap to be accepted: %v", err)
	}
}
