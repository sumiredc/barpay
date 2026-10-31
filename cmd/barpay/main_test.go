package main

import "testing"

func TestRunExitsZero(t *testing.T) {
	if got := run(); got != 0 {
		t.Fatalf("run() = %d, want 0", got)
	}
}
