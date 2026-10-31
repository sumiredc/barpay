package signals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sumiredc/barpay/internal/store"
)

// LivelinessInterval is how often the edge device sends liveliness, and LivelinessUse how long
// BarPay uses a value after receiving it (the PRD's Pricing model, Signals; edge.yaml's
// sendSignals). Several sends fit in one use, so a few lost ones do not drop the signal.
const (
	LivelinessInterval = 10 * time.Second
	LivelinessUse      = time.Minute
)

// ErrInvalidLiveliness is returned for a value outside 0 to 1.
var ErrInvalidLiveliness = errors.New("signals: liveliness must be from 0 to 1")

// RecordLiveliness keeps v, received at at, as the liveliness signal, replacing the one before.
func RecordLiveliness(ctx context.Context, st *store.Store, v float64, at time.Time) error {
	if math.IsNaN(v) || v < 0 || v > 1 {
		return ErrInvalidLiveliness
	}
	return st.Tx(ctx, func(tx store.Querier) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO liveliness (id, value, received_at) VALUES (1, ?, ?)
			ON CONFLICT (id) DO UPDATE SET value = excluded.value, received_at = excluded.received_at`, v, at.UnixNano())
		if err != nil {
			return fmt.Errorf("signals: record liveliness: %w", err)
		}
		return nil
	})
}

// LivelinessAt returns the liveliness signal at t, or ok == false when there is none: nothing was
// received, or the last value is LivelinessUse old or older at t.
func LivelinessAt(ctx context.Context, q store.Querier, t time.Time) (v float64, ok bool, err error) {
	var received int64
	err = q.QueryRowContext(ctx, `SELECT value, received_at FROM liveliness WHERE id = 1`).Scan(&v, &received)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("signals: liveliness: %w", err)
	}
	if t.Sub(time.Unix(0, received)) >= LivelinessUse {
		return 0, false, nil
	}
	return v, true, nil
}
