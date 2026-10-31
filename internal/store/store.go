// Package store keeps all of BarPay's state in one SQLite file, so that it survives a restart.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Querier runs SQL on the database or inside a transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is the database. It is safe for concurrent use.
type Store struct {
	db   *sql.DB
	path string

	closeOnce sync.Once
	closeErr  error
}

// Open opens or creates the database file at path and brings its schema up to date.
func Open(ctx context.Context, path string) (*Store, error) {
	// WAL lets reads run while one write is in progress. synchronous(FULL) makes a committed
	// transaction survive a power loss, not only a crash of the process. _txlock=immediate takes
	// the write lock at BEGIN, so two transactions that read then write wait for each other
	// instead of failing when the second one tries to write.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: %s: %w", path, err)
	}
	return s, nil
}

// Path is the database file.
func (s *Store) Path() string { return s.path }

// Close closes the database. Calling it again returns the first result.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

// Read runs reads outside a transaction. Anything that writes uses Tx.
func (s *Store) Read() Querier { return s.db }

// Tx runs fn in one transaction: it commits if fn returns nil, and rolls back otherwise,
// including when fn panics (the panic then continues).
func (s *Store) Tx(ctx context.Context, fn func(Querier) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			err = errors.Join(err, ignoreDone(tx.Rollback()))
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

func ignoreDone(err error) error {
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}
