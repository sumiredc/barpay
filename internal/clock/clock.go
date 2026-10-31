// Package clock gives code the current time as an input instead of reading the system clock.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time. Read it once per operation and pass that instant down.
type Clock interface {
	Now() time.Time
}

type system struct{}

func (system) Now() time.Time { return time.Now() }

// System returns the real clock. Only the program's entry point should call it.
func System() Clock { return system{} }

// Fake is a Clock that moves only when told to. It is safe for concurrent use.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake standing at t.
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set moves the clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}
