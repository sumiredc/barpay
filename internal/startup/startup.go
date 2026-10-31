// Package startup brings the stored state in line with the config file when BarPay starts (the PRD's UC8).
package startup

import (
	"context"
	"fmt"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/config"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
)

// OpenSessions tells whether any session is not yet closed, reading through q.
type OpenSessions interface {
	AnyOpen(ctx context.Context, q store.Querier) (bool, error)
}

// NoSessions is OpenSessions while sessions are not stored yet: none can be open.
// TODO(#16): replace with the session store once check-ins are stored.
type NoSessions struct{}

func (NoSessions) AnyOpen(context.Context, store.Querier) (bool, error) { return false, nil }

// State is what BarPay runs with after Prepare. Build every later part from it, not from the
// config file's Currency, StartingRate or StartingSettings, which are in the file's currency.
type State struct {
	// Currency is the running currency: the previous one while CurrencyChangePending is set.
	Currency money.Currency

	// Settings in effect, in Currency.
	Settings settings.Settings

	// CurrencyChangePending is set when the config file names another currency but a session
	// is open; a currency_change_pending alert was raised.
	CurrencyChangePending bool
}

// Prepare decides the settings and currency to run with, following UC8's postcondition and
// its currency exception, and stores the result in one transaction:
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
		saved, ok, err := settings.Load(ctx, tx)
		if err != nil {
			return err
		}
		if ok && saved.Currency == cfg.Currency {
			state = State{Currency: saved.Currency, Settings: saved}
			return nil
		}
		if ok {
			anyOpen, err := open.AnyOpen(ctx, tx)
			if err != nil {
				return fmt.Errorf("check for open sessions: %w", err)
			}
			if anyOpen {
				_, err := alert.Raise(ctx, tx, alert.Alert{
					Kind:     alert.CurrencyChangePending,
					RaisedAt: now,
					Message: fmt.Sprintf("The config file sets the currency to %s, but sessions are open. "+
						"BarPay keeps %s and applies the change at the first restart with no open session.",
						cfg.Currency, saved.Currency),
				})
				state = State{Currency: saved.Currency, Settings: saved, CurrencyChangePending: true}
				return err
			}
		}
		state = State{Currency: cfg.StartingSettings.Currency, Settings: cfg.StartingSettings}
		return settings.Save(ctx, tx, cfg.StartingSettings, now)
	})
	if err != nil {
		return State{}, fmt.Errorf("startup: %w", err)
	}
	return state, nil
}
