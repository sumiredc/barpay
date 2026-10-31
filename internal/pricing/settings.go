package pricing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
)

// UpdateSettings replaces the settings with s, in the store's currency, for the staff member by
// (the PRD's UC7, Pricing model, Guardrails and Settings changes). A floor or ceiling that leaves
// the published rate outside moves it to the nearer bound at once, beyond the max step, and
// records a range_change decision with by. A change that leaves the rate inside publishes
// nothing and records no decision. It returns settings.Errors when s breaks a condition; then
// nothing changes.
func UpdateSettings(ctx context.Context, st *store.Store, by staff.ID, s settings.Settings, at time.Time) (settings.Settings, error) {
	c := staff.Change{Staff: by, Operation: "updateSettings", Subject: "settings", At: at}
	err := staff.Tx(ctx, st, c, func(tx store.Querier) error {
		old, ok, err := settings.Load(ctx, tx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("pricing: no settings are saved to change")
		}
		s.Currency = old.Currency
		if err := settings.Save(ctx, tx, s, at); err != nil {
			return err
		}
		before, err := Current(ctx, tx, s.Currency)
		if err != nil {
			return err
		}
		g, err := FitRange(before.Rate, Limits{Floor: s.Floor, Ceiling: s.Ceiling})
		if err != nil || g.Change().Value() == 0 {
			return err
		}
		p, seq, err := publish(ctx, tx, g, at)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO decisions (decided_at, trigger, staff_id, rate_seq, explanation)
			VALUES (?, ?, ?, ?, ?)`, p.PublishedAt.UnixNano(), string(TriggerRangeChange), string(by), seq,
			rangeExplanation(g, s))
		if err != nil {
			return fmt.Errorf("pricing: record range change: %w", err)
		}
		return nil
	})
	if err != nil {
		return settings.Settings{}, err
	}
	return s, nil
}

// rangeExplanation states the change actually published (the PRD's Pricing model, Explanation),
// written by code: no AI takes part in a range change. A rate moved up only by a floor raised
// above it, and down only by a ceiling lowered below it.
func rangeExplanation(g Guarded, s settings.Settings) string {
	if g.Change().Value() > 0 {
		return fmt.Sprintf("Floor raised to %s/h: rate up %s to %s/h.",
			format(s.Floor), format(g.Change()), format(g.Rate()))
	}
	return fmt.Sprintf("Ceiling lowered to %s/h: rate down %s to %s/h.",
		format(s.Ceiling), format(money.New(-g.Change().Value(), g.Change().Currency())), format(g.Rate()))
}

// format writes a non-negative amount for staff, in English: "$12.50", "¥1,200".
func format(a money.Amount) string {
	v := a.Value()
	switch a.Currency() {
	case money.USD:
		return "$" + group(v/100) + fmt.Sprintf(".%02d", v%100)
	case money.JPY:
		return "¥" + group(v)
	}
	return a.String()
}

func group(v int64) string {
	s := strconv.FormatInt(v, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
