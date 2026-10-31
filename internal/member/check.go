package member

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sumiredc/barpay/internal/paypal"
	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/store"
)

// checkAgainAfter is the PRD's UC1 exception: a check without a readable answer is made again
// this long after it ended.
const checkAgainAfter = time.Minute

// CheckDue checks with PayPal the saved PayPal of every member whose active session is not linked
// and whose check is due at the clock's time (the PRD's UC1 alternative flow and exceptions), and
// returns how many sessions it linked. The check tells only whether PayPal still has it: if it
// does, the session is linked, if it is still active; if PayPal says it does not, BarPay deletes
// its own record and the session stays unlinked; without a readable answer the saved PayPal is
// kept and checked again later. It decides from the stored times alone, so a check that fell due
// while BarPay was stopped is made after the restart.
func (a API) CheckDue(ctx context.Context) (int, error) {
	now := a.Clock.Now()
	var due []check
	err := a.Store.View(ctx, func(q store.Querier) error {
		sessions, err := session.ActiveUnlinked(ctx, q)
		if err != nil {
			return err
		}
		for _, s := range sessions {
			c, ok, err := dueCheck(ctx, q, s, now)
			if err != nil {
				return err
			}
			if ok {
				due = append(due, c)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range due {
		linked, err := a.check(ctx, c)
		if err != nil {
			return n, err
		}
		if linked {
			n++
		}
	}
	return n, nil
}

// check is a session whose member's saved PayPal is to be checked.
type check struct {
	sessionID, memberID, vaultID string
}

// dueCheck tells whether s's member has a saved PayPal, not being deleted, whose check is due at now.
func dueCheck(ctx context.Context, q store.Querier, s session.Unlinked, now time.Time) (check, bool, error) {
	sp, err := getSaved(ctx, q, s.MemberID)
	if errors.Is(err, errNoSaved) || (err == nil && sp.status != statusSaved) {
		return check{}, false, nil
	}
	if err != nil {
		return check{}, false, err
	}
	var next int64
	err = q.QueryRowContext(ctx, `SELECT next_at FROM paypal_checks WHERE session_id = ?`, s.ID).Scan(&next)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return check{}, false, fmt.Errorf("member: check due: %w", err)
	case now.Before(time.Unix(0, next)):
		return check{}, false, nil
	}
	return check{sessionID: s.ID, memberID: s.MemberID, vaultID: sp.vaultID}, true, nil
}

// check makes one check and stores what came of it, telling whether it linked the session. What
// is stored applies only if the member still has the same saved PayPal, not being deleted: a
// deletion or a new link that came meanwhile wins.
func (a API) check(ctx context.Context, c check) (bool, error) {
	p := a.PayPal.CheckPaymentToken(ctx, c.vaultID)
	linked, stored := false, false
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		ended := a.Clock.Now()
		if p == paypal.PresenceUnknown {
			_, err := tx.ExecContext(ctx, `INSERT INTO paypal_checks (session_id, next_at) VALUES (?, ?)
				ON CONFLICT (session_id) DO UPDATE SET next_at = excluded.next_at`, c.sessionID, ended.Add(checkAgainAfter).UnixNano())
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM paypal_checks WHERE session_id = ?`, c.sessionID); err != nil {
			return err
		}
		if ok, err := stillSaved(ctx, tx, c.memberID, c.vaultID); err != nil || !ok {
			return err
		}
		stored = true
		if p == paypal.Gone {
			return a.removeSaved(ctx, tx, c.memberID, c.vaultID, reasonNoLongerValid, ended)
		}
		var err error
		linked, err = session.Link(ctx, tx, c.sessionID, ended)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("member: check: %w", err)
	}
	if stored && (linked || p == paypal.Gone) {
		a.Notify.Wake()
	}
	return linked, nil
}

// pollInterval is how often Run looks for checks and deletions that fell due. It only wakes the
// loop: what is due is decided from the stored times.
const pollInterval = time.Second

// Run makes the checks and tries the deletions that are due when it starts and every second,
// until ctx ends.
func (a API) Run(ctx context.Context, log *slog.Logger) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		if _, err := a.CheckDue(ctx); err != nil && ctx.Err() == nil {
			log.Error("saved PayPals not checked", "error", err)
		}
		if _, err := a.DeleteDue(ctx); err != nil && ctx.Err() == nil {
			log.Error("saved PayPal deletions not tried", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
