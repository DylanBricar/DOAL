package bandwidth

import (
	"testing"
	"time"

	"doal/config"
)

func TestDispatcherStopAndWaitJoinsRunLoop(t *testing.T) {
	d := NewDispatcher(&config.Config{}, NewRandomSpeedProvider(1, 1), nil)
	go d.Run()
	d.Stop()
	done := make(chan struct{})
	go func() {
		d.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher Run loop did not stop")
	}
}
