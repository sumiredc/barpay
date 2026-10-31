package gate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// An unlock reaches only an edge device waiting for it at that moment.
func TestHandNeedsAWaitingEdgeDevice(t *testing.T) {
	h := New()
	if h.Hand("glk_1") {
		t.Fatal("handed with no edge device waiting")
	}
	got := make(chan string)
	go func() {
		id, _ := h.Wait(context.Background(), time.Minute)
		got <- id
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !h.Hand("glk_2") {
		if time.Now().After(deadline) {
			t.Fatal("never handed to the waiting edge device")
		}
		time.Sleep(time.Millisecond)
	}
	if id := <-got; id != "glk_2" {
		t.Errorf("the edge device got %q", id)
	}
}

func TestWaitEndsWithoutAnUnlock(t *testing.T) {
	h := New()
	if _, ok := h.Wait(context.Background(), time.Millisecond); ok {
		t.Error("an unlock came from nowhere")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := h.Wait(ctx, time.Minute); ok {
		t.Error("an unlock came after the edge device went away")
	}
}

// Scans of one key run one at a time; scans of other keys do not wait for them.
func TestScansOfOneKeyRunOneAtATime(t *testing.T) {
	h := New()
	var running, most atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := h.Scan("ses_1")
			defer done()
			n := running.Add(1)
			for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
			}
			time.Sleep(time.Millisecond)
			running.Add(-1)
		}()
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Errorf("%d scans of one key ran at once", most.Load())
	}
	done := h.Scan("ses_1")
	other := make(chan struct{})
	go func() { h.Scan("ses_2")(); close(other) }()
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Error("a scan of another key waited")
	}
	done()
	if len(h.scans) != 0 {
		t.Errorf("%d keys left behind", len(h.scans))
	}
}
