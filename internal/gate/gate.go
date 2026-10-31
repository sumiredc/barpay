// Package gate is BarPay's side of the edge device at the exit gate, as far as it lives in
// memory: it hands an unlock made by hand to an edge device waiting for one, and runs the scans of
// one exit token one at a time.
package gate

import (
	"context"
	"sync"
	"time"
)

// Hub is safe for concurrent use. Its zero value is not usable; use New.
type Hub struct {
	unlocks chan string

	mu    sync.Mutex
	scans map[string]*scan
}

type scan struct {
	mu      sync.Mutex
	waiting int // guarded by Hub.mu
}

// New returns a Hub with no edge device waiting.
func New() *Hub {
	return &Hub{unlocks: make(chan string), scans: map[string]*scan{}}
}

// Hand gives the unlock with id to an edge device waiting in Wait at this moment, and tells
// whether one was: an edge device is connected while it waits for its next unlock.
func (h *Hub) Hand(id string) bool {
	select {
	case h.unlocks <- id:
		return true
	default:
		return false
	}
}

// Wait waits for an unlock handed by Hand, for at most d or until ctx ends, and returns its ID,
// or false when none came.
func (h *Hub) Wait(ctx context.Context, d time.Duration) (string, bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case id := <-h.unlocks:
		return id, true
	case <-t.C:
	case <-ctx.Done():
	}
	return "", false
}

// Scan waits until no other scan of key runs, and returns the function that ends this one. Two
// scans of the same exit QR at once then run one checkout, and the second sees its result.
func (h *Hub) Scan(key string) (done func()) {
	h.mu.Lock()
	s := h.scans[key]
	if s == nil {
		s = &scan{}
		h.scans[key] = s
	}
	s.waiting++
	h.mu.Unlock()
	s.mu.Lock()
	return func() {
		s.mu.Unlock()
		h.mu.Lock()
		if s.waiting--; s.waiting == 0 {
			delete(h.scans, key)
		}
		h.mu.Unlock()
	}
}
