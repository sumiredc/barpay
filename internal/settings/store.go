package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/store"
)

// Load returns the saved settings, or ok == false before the first start has saved any.
func Load(ctx context.Context, q store.Querier) (s Settings, ok bool, err error) {
	var cur string
	var floor, ceiling, cap int64
	err = q.QueryRowContext(ctx, `SELECT currency, floor, ceiling, block_minutes, pause_minutes, cap_per_visit, goal
		FROM settings WHERE id = 1`).Scan(&cur, &floor, &ceiling, &s.BlockMinutes, &s.PauseMinutes, &cap, &s.Goal)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, fmt.Errorf("settings: load: %w", err)
	}
	c := money.Currency(cur)
	s.Currency = c
	s.Floor, s.Ceiling, s.CapPerVisit = money.New(floor, c), money.New(ceiling, c), money.New(cap, c)
	return s, true, nil
}

// Save validates s and replaces the saved settings with it, adding it to the history of changes.
// A change made by an admin request is saved inside that request's staff transaction, so the
// staff member is recorded with it.
func Save(ctx context.Context, tx store.Querier, s Settings, at time.Time) error {
	if err := s.Validate(); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO settings
		(id, currency, floor, ceiling, block_minutes, pause_minutes, cap_per_visit, goal, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET currency = excluded.currency, floor = excluded.floor,
			ceiling = excluded.ceiling, block_minutes = excluded.block_minutes,
			pause_minutes = excluded.pause_minutes, cap_per_visit = excluded.cap_per_visit,
			goal = excluded.goal, updated_at = excluded.updated_at`,
		string(s.Currency), s.Floor.Value(), s.Ceiling.Value(), s.BlockMinutes, s.PauseMinutes,
		s.CapPerVisit.Value(), s.Goal, at.UnixNano())
	if err != nil {
		return fmt.Errorf("settings: save: %w", err)
	}
	// A clock set back must not put a change before an earlier one: "in effect at t" is the
	// last change at or before t, which needs the times in the order of the changes.
	_, err = tx.ExecContext(ctx, `INSERT INTO settings_changes
		(currency, floor, ceiling, block_minutes, pause_minutes, cap_per_visit, goal, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, max(?, coalesce((SELECT max(at) FROM settings_changes), 0)))`,
		string(s.Currency), s.Floor.Value(), s.Ceiling.Value(), s.BlockMinutes, s.PauseMinutes,
		s.CapPerVisit.Value(), s.Goal, at.UnixNano())
	if err != nil {
		return fmt.Errorf("settings: save change: %w", err)
	}
	return nil
}

// Change is settings as saved at At, in effect until the next change.
type Change struct {
	Settings
	At time.Time
}

// History returns the change in effect at since and every later one, oldest first. Before the
// first change it returns them all.
func History(ctx context.Context, q store.Querier, since time.Time) ([]Change, error) {
	rows, err := q.QueryContext(ctx, `SELECT currency, floor, ceiling, block_minutes, pause_minutes, cap_per_visit, goal, at
		FROM settings_changes
		WHERE seq >= coalesce((SELECT max(seq) FROM settings_changes WHERE at <= ?), 0)
		ORDER BY seq`, since.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("settings: history: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var cur string
		var floor, ceiling, cap, at int64
		if err := rows.Scan(&cur, &floor, &ceiling, &c.BlockMinutes, &c.PauseMinutes, &cap, &c.Goal, &at); err != nil {
			return nil, fmt.Errorf("settings: history: %w", err)
		}
		cc := money.Currency(cur)
		c.Currency = cc
		c.Floor, c.Ceiling, c.CapPerVisit = money.New(floor, cc), money.New(ceiling, cc), money.New(cap, cc)
		c.At = time.Unix(0, at).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}
