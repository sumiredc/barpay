package store

import (
	"context"
	"fmt"
)

// migrations brings an empty database to the current schema, one step per entry; step i
// leaves the database at user_version i+1. Append new steps; never edit or reorder an existing
// one, because databases already past it will not run it again.
var migrations = []string{
	// Settings: one row. Its currency is the store's currency in effect.
	`CREATE TABLE settings (
		id            INTEGER PRIMARY KEY CHECK (id = 1),
		currency      TEXT    NOT NULL,
		floor         INTEGER NOT NULL,
		ceiling       INTEGER NOT NULL,
		block_minutes INTEGER NOT NULL,
		pause_minutes INTEGER NOT NULL,
		cap_per_visit INTEGER NOT NULL,
		goal          INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL -- Unix nanoseconds
	)`,
	// Alerts: the spec's Alert. kind lists the spec's enum, as alert.Kinds does.
	`CREATE TABLE alerts (
		seq        INTEGER PRIMARY KEY, -- raising order
		id         TEXT    NOT NULL UNIQUE,
		kind       TEXT    NOT NULL CHECK (kind IN ('payment_failed', 'unlock_failed',
			'register_requested', 'late_order_rejected', 'max_stay_reached',
			'notification_given_up', 'deletion_failing', 'currency_change_pending')),
		raised_at  INTEGER NOT NULL, -- Unix nanoseconds
		session_id TEXT    NOT NULL, -- '' when not about a session
		member_id  TEXT    NOT NULL, -- '' when not about a member
		message    TEXT    NOT NULL CHECK (length(message) BETWEEN 1 AND 500)
	)`,
	// Staff actions: who made each change through the admin API, written with the change.
	`CREATE TABLE staff_actions (
		seq       INTEGER PRIMARY KEY,
		staff_id  TEXT    NOT NULL,
		operation TEXT    NOT NULL, -- the spec's operationId
		subject   TEXT    NOT NULL, -- a session ID, or a name such as 'settings'
		at        INTEGER NOT NULL  -- Unix nanoseconds
	);
	CREATE INDEX staff_actions_subject ON staff_actions (subject, seq)`,
}

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
