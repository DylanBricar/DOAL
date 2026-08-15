package peerwire

import (
	"bytes"
	"testing"
)

func TestHandshakeAdvertisesDHTOnlyWhenNodeIsActive(t *testing.T) {
	t.Parallel()

	withoutDHT := buildHandshakeWithDHT([20]byte{}, bytes.Repeat([]byte{'a'}, 20), false)
	withDHT := buildHandshakeWithDHT([20]byte{}, bytes.Repeat([]byte{'a'}, 20), true)
	if withoutDHT[27]&0x01 != 0 {
		t.Fatal("handshake advertises DHT while no DHT node is active")
	}
	if withDHT[27]&0x01 == 0 {
		t.Fatal("handshake does not advertise the active DHT node")
	}
}

func TestDHTPortMessageContainsUDPPort(t *testing.T) {
	t.Parallel()

	got := buildDHTPortMessage(51413)
	want := []byte{0, 0, 0, 3, 9, 0xc8, 0xd5}
	if !bytes.Equal(got, want) {
		t.Fatalf("DHT port message = %x, want %x", got, want)
	}
}
