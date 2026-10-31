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
	// Notifications: the outbox and the notification log. A row is written in the transaction of
	// the change it reports and sent after that commits. sequence is per subject (the spec's EventBase).
	`CREATE TABLE notifications (
		seq              INTEGER PRIMARY KEY, -- storing order; the log lists by it
		id               TEXT    NOT NULL UNIQUE,
		type             TEXT    NOT NULL,
		subject_kind     TEXT    NOT NULL CHECK (subject_kind IN ('session', 'member')),
		subject_id       TEXT    NOT NULL,
		sequence         INTEGER NOT NULL CHECK (sequence >= 1),
		body             BLOB    NOT NULL, -- sent unchanged on every attempt
		created_at       INTEGER NOT NULL, -- Unix nanoseconds, like every instant below
		status           TEXT    NOT NULL CHECK (status IN ('retrying', 'delivered', 'given_up')),
		attempts         INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
		first_attempt_at INTEGER,          -- NULL before the first attempt
		last_attempt_at  INTEGER,          -- NULL before the first attempt
		next_attempt_at  INTEGER,          -- NULL once delivered or given up
		UNIQUE (subject_kind, subject_id, sequence),
		CHECK ((status = 'retrying') = (next_attempt_at IS NOT NULL))
	);
	CREATE INDEX notifications_due ON notifications (next_attempt_at) WHERE status = 'retrying'`,
	// Settings changes: every saved settings, kept so a session's cover charge can be computed
	// with the settings in effect at each block (the PRD's Pricing model, Settings changes).
	// at never goes back from one row to the next. The settings saved before this step are copied in.
	`CREATE TABLE settings_changes (
		seq           INTEGER PRIMARY KEY,
		currency      TEXT    NOT NULL,
		floor         INTEGER NOT NULL,
		ceiling       INTEGER NOT NULL,
		block_minutes INTEGER NOT NULL,
		pause_minutes INTEGER NOT NULL,
		cap_per_visit INTEGER NOT NULL,
		goal          INTEGER NOT NULL,
		at            INTEGER NOT NULL -- Unix nanoseconds
	);
	INSERT INTO settings_changes (currency, floor, ceiling, block_minutes, pause_minutes, cap_per_visit, goal, at)
		SELECT currency, floor, ceiling, block_minutes, pause_minutes, cap_per_visit, goal, updated_at FROM settings`,
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
