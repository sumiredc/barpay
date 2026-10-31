package store

import (
	"context"
	"fmt"
)

// migrations brings an empty database to the current schema, one step per entry; step i
// leaves the database at user_version i+1. Append new steps; never edit or reorder an existing
// one, because databases already past it will not run it again.
var migrations = []string{}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this BarPay (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		err := s.Tx(ctx, func(tx Querier) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			// PRAGMA takes no bound parameters; i is an int, so this is not an injection point.
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migrate to schema version %d: %w", i+1, err)
		}
	}
	return nil
}
