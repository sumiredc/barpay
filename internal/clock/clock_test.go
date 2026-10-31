package clock

import (
	"testing"
	"time"
)

func TestFakeMovesOnlyWhenTold(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)
	f := NewFake(t0)
	first, second := f.Now(), f.Now()
	if !first.Equal(t0) || !second.Equal(t0) {
		t.Fatalf("Now() = %v then %v, want %v on every read", first, second, t0)
	}
	f.Advance(10 * time.Minute)
	if want := t0.Add(10 * time.Minute); !f.Now().Equal(want) {
		t.Errorf("after Advance: Now() = %v, want %v", f.Now(), want)
	}
	f.Set(t0)
	if !f.Now().Equal(t0) {
		t.Errorf("after Set: Now() = %v, want %v", f.Now(), t0)
	}
}
