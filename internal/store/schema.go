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
	// Published rates, oldest first; the last row is the rate published now. published_at never
	// goes back from one row to the next. settings_seq is the settings change in effect when the
	// rate was guarded.
	`CREATE TABLE rates (
		seq          INTEGER PRIMARY KEY,
		currency     TEXT    NOT NULL,
		rate         INTEGER NOT NULL CHECK (rate > 0), -- per hour
		change       INTEGER NOT NULL,
		published_at INTEGER NOT NULL, -- Unix nanoseconds
		settings_seq INTEGER NOT NULL REFERENCES settings_changes (seq)
	);
	CREATE INDEX rates_published_at ON rates (published_at);
	-- The max step BarPay runs with, kept for the starts that wait for a currency change: the
	-- config file then holds the max step for the new currency.
	CREATE TABLE max_step (
		id       INTEGER PRIMARY KEY CHECK (id = 1),
		currency TEXT    NOT NULL,
		value    INTEGER NOT NULL CHECK (value > 0)
	)`,
	// Decision log: the spec's Decision. published is the rates row; on a fallback it is the rate
	// kept. inputs is the JSON of the spec's inputs, NULL for range_change, which has none.
	`CREATE TABLE decisions (
		seq         INTEGER PRIMARY KEY, -- decision order
		decided_at  INTEGER NOT NULL,    -- Unix nanoseconds
		trigger     TEXT    NOT NULL CHECK (trigger IN ('evaluation', 'range_change')),
		staff_id    TEXT    NOT NULL,    -- '' for evaluation
		inputs      TEXT,
		proposal    INTEGER,             -- NULL on fallback and for range_change
		reasoning   TEXT,                -- NULL on fallback and for range_change
		rate_seq    INTEGER NOT NULL REFERENCES rates (seq),
		explanation TEXT    NOT NULL CHECK (length(explanation) BETWEEN 1 AND 200),
		fallback    TEXT    CHECK (fallback IN ('ai_timeout', 'ai_unreadable')),
		CHECK ((trigger = 'range_change') = (staff_id <> '')),
		CHECK (trigger = 'evaluation' OR (inputs IS NULL AND proposal IS NULL AND reasoning IS NULL AND fallback IS NULL))
	)`,
	// The store app's Idempotency-Key (the spec's IdempotencyKey): the response kept for a key,
	// written in the transaction of the change it answers. scope is the API key the request
	// came with; request identifies the request the response answers.
	`CREATE TABLE idempotency_keys (
		scope   TEXT    NOT NULL CHECK (scope IN ('store', 'admin')),
		key     TEXT    NOT NULL,
		request BLOB    NOT NULL,
		status  INTEGER NOT NULL,
		body    BLOB    NOT NULL,
		kept_at INTEGER NOT NULL, -- Unix nanoseconds
		PRIMARY KEY (scope, key)
	);
	CREATE INDEX idempotency_keys_kept_at ON idempotency_keys (kept_at)`,
	// Sessions: the spec's Session as stored. state lists the spec's SessionState. A member has at
	// most one session that has not ended (UC1), held by the unique index rather than by a check
	// in code, so two check-ins at once cannot both start one.
	`CREATE TABLE sessions (
		seq            INTEGER PRIMARY KEY, -- check-in order
		id             TEXT    NOT NULL UNIQUE,
		member_id      TEXT,                -- NULL for a guest
		state          TEXT    NOT NULL CHECK (state IN ('active', 'checking_out', 'at_register',
			'payment_failed', 'closed', 'closed_unpaid')),
		currency       TEXT    NOT NULL,
		checked_in_at  INTEGER NOT NULL,    -- Unix nanoseconds, like every instant below
		total_fixed_at INTEGER,             -- NULL while active
		CHECK ((state = 'active') = (total_fixed_at IS NULL))
	);
	CREATE UNIQUE INDEX sessions_one_open_per_member ON sessions (member_id)
		WHERE member_id IS NOT NULL AND state NOT IN ('closed', 'closed_unpaid');
	CREATE INDEX sessions_state ON sessions (state)`,
	// Orders: the spec's Order as first recorded. order_id is unique within its session, so the
	// same order reported twice is recorded once (UC4).
	`CREATE TABLE orders (
		seq         INTEGER PRIMARY KEY, -- recording order
		session_id  TEXT    NOT NULL REFERENCES sessions (id),
		order_id    TEXT    NOT NULL,
		lines       TEXT    NOT NULL,    -- JSON of the spec's OrderLine array, as returned
		amount      INTEGER NOT NULL CHECK (amount >= 0),
		recorded_at INTEGER NOT NULL,    -- Unix nanoseconds
		UNIQUE (session_id, order_id)
	)`,
	// Receipts: how a session ended (the spec's Receipt), written in the transaction that ends it,
	// with the values the session showed then. An ended session is shown from its receipt, so
	// later rates, settings and a currency change do not change it. One per session.
	`CREATE TABLE receipts (
		session_id      TEXT    PRIMARY KEY REFERENCES sessions (id),
		settlement      TEXT    NOT NULL CHECK (settlement IN ('paypal', 'register', 'outside', 'unpaid')),
		payment_method  TEXT    NOT NULL CHECK (payment_method IN ('paypal', 'register')),
		rate_per_hour   INTEGER NOT NULL,
		order_count     INTEGER NOT NULL,
		orders_total    INTEGER NOT NULL,
		cover_charge    INTEGER NOT NULL,
		total           INTEGER NOT NULL,
		amount_recorded INTEGER,          -- for register and outside only
		unpaid_amount   INTEGER,          -- for unpaid only
		ended_at        INTEGER NOT NULL, -- Unix nanoseconds
		CHECK ((settlement IN ('register', 'outside')) = (amount_recorded IS NOT NULL)),
		CHECK ((settlement = 'unpaid') = (unpaid_amount IS NOT NULL))
	)`,
	// Staff events: the spec's StaffEvent, in recording order. A label's event is going on while
	// its last row is 'starts' (the PRD's Pricing model, Signals).
	`CREATE TABLE staff_events (
		seq         INTEGER PRIMARY KEY, -- recording order
		label       TEXT    NOT NULL CHECK (length(label) BETWEEN 1 AND 100),
		phase       TEXT    NOT NULL CHECK (phase IN ('starts', 'ends')),
		recorded_at INTEGER NOT NULL     -- Unix nanoseconds
	);
	CREATE INDEX staff_events_label ON staff_events (label, seq)`,
	// Liveliness: the last value the edge device sent, and when BarPay received it.
	`CREATE TABLE liveliness (
		id          INTEGER PRIMARY KEY CHECK (id = 1),
		value       REAL    NOT NULL CHECK (value BETWEEN 0 AND 1),
		received_at INTEGER NOT NULL -- Unix nanoseconds
	)`,
	// When the last rate evaluation started, written before the AI is asked, so the minimum
	// interval holds across a restart and counts evaluations that fell back or never finished.
	`CREATE TABLE evaluation_start (
		id         INTEGER PRIMARY KEY CHECK (id = 1),
		started_at INTEGER NOT NULL -- Unix nanoseconds
	)`,
	// When a session was linked to its member's saved PayPal (UC1, UC2); NULL while not linked.
	`ALTER TABLE sessions ADD COLUMN paypal_linked_at INTEGER -- Unix nanoseconds
		CHECK (paypal_linked_at IS NULL OR member_id IS NOT NULL)`,
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
