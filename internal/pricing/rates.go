package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
)

// MaxHistory is the most rates getRateHistory returns (the spec's maxItems).
const MaxHistory = 1000

// Point is a published rate: the spec's RatePoint.
type Point struct {
	Rate        money.Amount // per hour
	Change      money.Amount // from the rate published before it; 0 for the first in its currency
	PublishedAt time.Time
}

// ErrNoRate is returned when no rate is published in the store's currency, which Prepare at
// start rules out.
var ErrNoRate = errors.New("pricing: no rate is published in the store's currency")

// The rates in the running currency are those after the last rate in another currency: the
// rates before a currency change belong to the old currency and do not continue in the new one.
const inCurrency = `SELECT seq, rate, change, published_at FROM rates
	WHERE seq > coalesce((SELECT max(seq) FROM rates WHERE currency <> ?1), 0)`

// Current returns the rate published now in cur.
func Current(ctx context.Context, q store.Querier, cur money.Currency) (Point, error) {
	rows, err := q.QueryContext(ctx, inCurrency+` ORDER BY seq DESC LIMIT 1`, string(cur))
	if err != nil {
		return Point{}, fmt.Errorf("pricing: current rate: %w", err)
	}
	ps, err := scan(rows, cur)
	if err != nil {
		return Point{}, fmt.Errorf("pricing: current rate: %w", err)
	}
	if len(ps) == 0 {
		return Point{}, ErrNoRate
	}
	return ps[0], nil
}

// History returns the rates in cur from since, oldest first: the one in effect at since (none
// if since is before the first) and every later one, at most limit of the most recent.
func History(ctx context.Context, q store.Querier, cur money.Currency, since time.Time, limit int) ([]Point, error) {
	rows, err := q.QueryContext(ctx, `WITH c AS (`+inCurrency+`)
		SELECT * FROM c WHERE seq >= coalesce((SELECT max(seq) FROM c WHERE published_at <= ?2), 0)
		ORDER BY seq DESC LIMIT ?3`, string(cur), since.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("pricing: rate history: %w", err)
	}
	ps, err := scan(rows, cur)
	if err != nil {
		return nil, fmt.Errorf("pricing: rate history: %w", err)
	}
	slices.Reverse(ps)
	return ps, nil
}

func scan(rows *sql.Rows, cur money.Currency) ([]Point, error) {
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var seq, rate, change, at int64
		if err := rows.Scan(&seq, &rate, &change, &at); err != nil {
			return nil, err
		}
		out = append(out, Point{Rate: money.New(rate, cur), Change: money.New(change, cur), PublishedAt: time.Unix(0, at).UTC()})
	}
	return out, rows.Err()
}

// publish stores g as the rate published at at, inside tx. It takes only a Guarded, so nothing
// that skipped the guardrails is ever published. A clock set back does not put the rate before
// the one published earlier: "in effect at t" is the last rate at or before t.
func publish(ctx context.Context, tx store.Querier, g Guarded, at time.Time) (Point, int64, error) {
	var seq, publishedAt int64
	err := tx.QueryRowContext(ctx, `INSERT INTO rates (currency, rate, change, published_at, settings_seq)
		VALUES (?, ?, ?, max(?, coalesce((SELECT max(published_at) FROM rates), 0)), (SELECT max(seq) FROM settings_changes))
		RETURNING seq, published_at`,
		string(g.rate.Currency()), g.rate.Value(), g.change.Value(), at.UnixNano()).Scan(&seq, &publishedAt)
	if err != nil {
		return Point{}, 0, fmt.Errorf("pricing: publish: %w", err)
	}
	return Point{Rate: g.rate, Change: g.change, PublishedAt: time.Unix(0, publishedAt).UTC()}, seq, nil
}

// PublishFirst publishes the starting rate, guarded by s, when no rate is published in
// s.Currency: on the first start and on the start that applies a currency change (the PRD's
// Pricing model, First rate). Any other start keeps the rate last published, because going
// back to the starting rate would move it without an evaluation and beyond the max step.
func PublishFirst(ctx context.Context, tx store.Querier, starting money.Amount, s settings.Settings, at time.Time) (p Point, published bool, err error) {
	p, err = Current(ctx, tx, s.Currency)
	if !errors.Is(err, ErrNoRate) {
		return p, false, err
	}
	g, err := First(starting, Limits{Floor: s.Floor, Ceiling: s.Ceiling})
	if err != nil {
		return Point{}, false, err
	}
	p, _, err = publish(ctx, tx, g, at)
	return p, err == nil, err
}

// PublishProposal guards proposal against the settings saved and the rate published now, and
// publishes the result. tx must be a transaction from store.Store.Tx, never store.Store.Read:
// that transaction takes the write lock at its start, so reading both and writing the rate in it
// keeps a concurrent floor or ceiling change from slipping in between, which would publish a rate
// clamped to the old range, or stepped from a rate no longer current. It takes a transaction
// rather than opening one so the caller can record its decision in the same one.
func PublishProposal(ctx context.Context, tx store.Querier, proposal, maxStep money.Amount, at time.Time) (Point, error) {
	s, ok, err := settings.Load(ctx, tx)
	if err != nil {
		return Point{}, err
	}
	if !ok {
		return Point{}, ErrNoRate
	}
	current, err := Current(ctx, tx, s.Currency)
	if err != nil {
		return Point{}, err
	}
	g, err := Guard(current.Rate, proposal, Limits{Floor: s.Floor, Ceiling: s.Ceiling, MaxStep: maxStep})
	if err != nil {
		return Point{}, err
	}
	p, _, err := publish(ctx, tx, g, at)
	return p, err
}

// SaveMaxStep keeps the max step BarPay runs with, for a later start that waits for a currency change.
func SaveMaxStep(ctx context.Context, tx store.Querier, m money.Amount) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO max_step (id, currency, value) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET currency = excluded.currency, value = excluded.value`,
		string(m.Currency()), m.Value())
	if err != nil {
		return fmt.Errorf("pricing: save max step: %w", err)
	}
	return nil
}

// LoadMaxStep returns the max step last saved, or ok == false if none was.
func LoadMaxStep(ctx context.Context, q store.Querier) (m money.Amount, ok bool, err error) {
	var cur string
	var v int64
	err = q.QueryRowContext(ctx, `SELECT currency, value FROM max_step WHERE id = 1`).Scan(&cur, &v)
	if errors.Is(err, sql.ErrNoRows) {
		return money.Amount{}, false, nil
	}
	if err != nil {
		return money.Amount{}, false, fmt.Errorf("pricing: load max step: %w", err)
	}
	return money.New(v, money.Currency(cur)), true, nil
}
