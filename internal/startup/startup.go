// Package startup brings the stored state in line with the config file when BarPay starts (the PRD's UC8).
package startup

import (
	"context"
	"fmt"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/config"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
)

// OpenSessions tells whether any session is not yet closed, reading through q.
type OpenSessions interface {
	AnyOpen(ctx context.Context, q store.Querier) (bool, error)
}

// State is what BarPay runs with after Prepare. Build every later part from it, not from the
// config file's Currency, StartingRate, MaxStep or StartingSettings, which are in the file's currency.
type State struct {
	// Currency is the running currency: the previous one while CurrencyChangePending is set.
	Currency money.Currency

	// Settings in effect, in Currency.
	Settings settings.Settings

	// MaxStep is the guardrails' max step, in Currency: the config file's, or while a currency
	// change is pending, the one BarPay ran with before the file named another currency.
	MaxStep money.Amount

	// CurrencyChangePending is set when the config file names another currency but a session
	// is open; a currency_change_pending alert was raised.
	CurrencyChangePending bool
}

// Prepare decides the settings, currency and max step to run with, following UC8's
// postcondition and its currency exception, publishes the first rate when none is published in
// the running currency (the PRD's Pricing model, First rate), and stores the result in one
// transaction. The settings are:
//   - first start: the config file's starting settings;
//   - same currency as stored: the stored settings, so changes through the admin API survive;
//   - another currency and no open session: the starting settings in the new currency, because
//     the old floor, ceiling and cap mean nothing in it;
//   - another currency and an open session: the stored settings, and a currency_change_pending
//     alert, raised again on every start while the change waits, so staff see it is still pending.
func Prepare(ctx context.Context, cfg *config.Config, st *store.Store, open OpenSessions, clk clock.Clock) (State, error) {
	now := clk.Now()
	var state State
	err := st.Tx(ctx, func(tx store.Querier) error {
		var err error
		if state, err = prepareSettings(ctx, cfg, tx, open, now); err != nil {
			return err
		}
		if state.MaxStep, err = prepareMaxStep(ctx, cfg, tx, state); err != nil {
			return err
		}
		if state.CurrencyChangePending {
			// A session is open, so a rate in the running currency was published before it
			// checked in. The starting rate is in the new currency and cannot stand in for it.
			_, err := pricing.Current(ctx, tx, state.Currency)
			return err
		}
		_, _, err = pricing.PublishFirst(ctx, tx, cfg.StartingRate, state.Settings, now)
		return err
	})
	if err != nil {
		return State{}, fmt.Errorf("startup: %w", err)
	}
	return state, nil
}

func prepareSettings(ctx context.Context, cfg *config.Config, tx store.Querier, open OpenSessions, now time.Time) (State, error) {
	saved, ok, err := settings.Load(ctx, tx)
	if err != nil {
		return State{}, err
	}
	if ok && saved.Currency == cfg.Currency {
		return State{Currency: saved.Currency, Settings: saved}, nil
	}
	if ok {
		anyOpen, err := open.AnyOpen(ctx, tx)
		if err != nil {
			return State{}, fmt.Errorf("check for open sessions: %w", err)
		}
		if anyOpen {
			_, err := alert.Raise(ctx, tx, alert.Alert{
				Kind:     alert.CurrencyChangePending,
				RaisedAt: now,
				Message: fmt.Sprintf("The config file sets the currency to %s, but sessions are open. "+
					"BarPay keeps %s and applies the change at the first restart with no open session.",
					cfg.Currency, saved.Currency),
			})
			return State{Currency: saved.Currency, Settings: saved, CurrencyChangePending: true}, err
		}
	}
	return State{Currency: cfg.StartingSettings.Currency, Settings: cfg.StartingSettings},
		settings.Save(ctx, tx, cfg.StartingSettings, now)
}

// prepareMaxStep keeps the config file's max step for the starts that wait for a currency
// change, when the file's value is in the new currency.
func prepareMaxStep(ctx context.Context, cfg *config.Config, tx store.Querier, state State) (money.Amount, error) {
	if !state.CurrencyChangePending {
		return cfg.MaxStep, pricing.SaveMaxStep(ctx, tx, cfg.MaxStep)
	}
	m, ok, err := pricing.LoadMaxStep(ctx, tx)
	if err != nil {
		return money.Amount{}, err
	}
	if !ok || m.Currency() != state.Currency {
		return money.Amount{}, fmt.Errorf("no max step in %s is kept to run with until the currency change", state.Currency)
	}
	return m, nil
}
