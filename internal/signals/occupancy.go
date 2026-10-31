package signals

import (
	"context"
	"fmt"

	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/store"
)

// Occupancy is the active sessions divided by capacity, at most 1 (the PRD's Pricing model,
// Signals). Only active sessions count: a session at the register or whose payment failed can stay
// so for days after the customer left, and one checking out is a customer on the way out.
func Occupancy(ctx context.Context, q store.Querier, capacity int) (float64, error) {
	if capacity < 1 {
		return 0, fmt.Errorf("signals: capacity %d is below 1", capacity)
	}
	counts, err := session.CountByState(ctx, q)
	if err != nil {
		return 0, err
	}
	return min(1, float64(counts[session.Active])/float64(capacity)), nil
}
