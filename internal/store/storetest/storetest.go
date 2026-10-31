// Package storetest opens throwaway stores and restarts them, for tests that state must survive a restart.
package storetest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sumiredc/barpay/internal/store"
)

// Open opens a new, empty store in a temporary directory that is removed after the test.
func Open(t testing.TB) *store.Store {
	t.Helper()
	return open(t, filepath.Join(t.TempDir(), "barpay.db"))
}

// Reopen closes s and opens its file again, as a restart of BarPay does. Use only the returned
// store afterwards, and rebuild every object that held state from it: only what was stored survives.
func Reopen(t testing.TB, s *store.Store) *store.Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return open(t, s.Path())
}

func open(t testing.TB, path string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
