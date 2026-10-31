package member

import (
	"context"
	"errors"
	"time"

	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/store"
)

// Saved is the members' saved PayPals as checkout charges them (session.SavedPaypals).
type Saved struct{}

var _ session.SavedPaypals = Saved{}

// VaultOf returns the vault ID of memberID's saved PayPal, and false when the member has none or
// its deletion is pending.
func (Saved) VaultOf(ctx context.Context, q store.Querier, memberID string) (string, bool, error) {
	s, err := getSaved(ctx, q, memberID)
	if errors.Is(err, errNoSaved) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return s.vaultID, s.status == statusSaved, nil
}

// Gone deletes BarPay's record of memberID's saved PayPal vaultID after PayPal answered a charge
// with it that it no longer has it, as at check-in (the PRD's UC1 exception, UC5), and stores
// member.saved_paypal_deleted (no_longer_valid).
func (Saved) Gone(ctx context.Context, tx store.Querier, memberID, vaultID string, now time.Time) error {
	return removeSaved(ctx, tx, memberID, vaultID, reasonNoLongerValid, now)
}
