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

// Save validates s and replaces the saved settings with it. A change made by an admin request
// is saved inside that request's staff transaction, so the staff member is recorded with it.
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
	return nil
}
