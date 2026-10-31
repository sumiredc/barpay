package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

func createCounter(t *testing.T, s *store.Store) {
	t.Helper()
	err := s.Tx(context.Background(), func(tx store.Querier) error {
		_, err := tx.ExecContext(context.Background(), "CREATE TABLE counter (n INTEGER NOT NULL); INSERT INTO counter VALUES (0)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readCounter(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Read().QueryRowContext(context.Background(), "SELECT n FROM counter").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCommittedWriteSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t)
	createCounter(t, s)
	if err := s.Tx(ctx, func(tx store.Querier) error {
		_, err := tx.ExecContext(ctx, "UPDATE counter SET n = 7")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	s = storetest.Reopen(t, s)
	if got := readCounter(t, s); got != 7 {
		t.Errorf("after restart n = %d, want 7", got)
	}
}

func TestFailedTransactionLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t)
	createCounter(t, s)
	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx store.Querier) error {
		if _, err := tx.ExecContext(ctx, "UPDATE counter SET n = 7"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx err = %v, want the error fn returned", err)
	}

	s = storetest.Reopen(t, s)
	if got := readCounter(t, s); got != 0 {
		t.Errorf("after a failed transaction n = %d, want 0", got)
	}
}

// A panic inside fn rolls the transaction back and releases the write lock, then continues.
func TestPanicRollsBack(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t)
	createCounter(t, s)
	func() {
		defer func() {
			if recover() != "boom" {
				t.Error("the panic did not continue out of Tx")
			}
		}()
		_ = s.Tx(ctx, func(tx store.Querier) error {
			if _, err := tx.ExecContext(ctx, "UPDATE counter SET n = 7"); err != nil {
				return err
			}
			panic("boom")
		})
	}()
	if got := readCounter(t, s); got != 0 {
		t.Errorf("after a panic n = %d, want 0", got)
	}
	// The write lock was released: another write goes through.
	if err := s.Tx(ctx, func(tx store.Querier) error {
		_, err := tx.ExecContext(ctx, "UPDATE counter SET n = 1")
		return err
	}); err != nil {
		t.Errorf("write after a panic: %v", err)
	}
}

// Transactions that read and then write must wait for each other, not fail and not lose an update.
func TestConcurrentReadModifyWriteTransactions(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t)
	createCounter(t, s)

	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			errs <- s.Tx(ctx, func(tx store.Querier) error {
				var n int
				if err := tx.QueryRowContext(ctx, "SELECT n FROM counter").Scan(&n); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, "UPDATE counter SET n = ?", n+1)
				return err
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Tx: %v", err)
		}
	}
	if got := readCounter(t, s); got != workers {
		t.Errorf("n = %d, want %d", got, workers)
	}
}
