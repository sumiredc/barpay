package main

import "testing"

func TestRunExitsZero(t *testing.T) {
	if got := run(); got != 1 {
		t.Fatalf("run() = %d, want 1", got)
	}
}
