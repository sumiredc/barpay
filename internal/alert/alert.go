// Package alert records alerts for staff in the shape of the API spec's Alert (the PRD's Guarantees, Observability).
// Each alert is raised by the code that detects its cause; listing them is the admin API's job.
package alert

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/sumiredc/barpay/internal/store"
)

// Kind is the spec's Alert.kind.
type Kind string

const (
	PaymentFailed         Kind = "payment_failed"
	UnlockFailed          Kind = "unlock_failed"
	RegisterRequested     Kind = "register_requested"
	LateOrderRejected     Kind = "late_order_rejected"
	MaxStayReached        Kind = "max_stay_reached"
	NotificationGivenUp   Kind = "notification_given_up"
	DeletionFailing       Kind = "deletion_failing"
	CurrencyChangePending Kind = "currency_change_pending"
)

// Kinds is the spec's enum. The alerts table's CHECK constraint lists the same values.
var Kinds = []Kind{
	PaymentFailed, UnlockFailed, RegisterRequested, LateOrderRejected,
	MaxStayReached, NotificationGivenUp, DeletionFailing, CurrencyChangePending,
}

const maxMessageLen = 500

var (
	sessionPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]{16,32}$`)
	memberPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// ErrInvalid is returned for an alert that does not fit the spec's Alert.
var ErrInvalid = errors.New("alert: invalid")

// ErrNotFound is returned by Get for an unknown ID.
var ErrNotFound = errors.New("alert: not found")

// Alert is one alert. SessionID and MemberID are empty when the alert is not about one.
type Alert struct {
	ID        string
	Kind      Kind
	RaisedAt  time.Time
	SessionID string
	MemberID  string
	Message   string // for staff, in English
}

// Raise stores a new alert with a fresh ID, inside tx so it is kept only if the change that
// caused it is. a.ID is ignored. It returns the stored alert.
func Raise(ctx context.Context, tx store.Querier, a Alert) (Alert, error) {
	if err := a.validate(); err != nil {
		return Alert{}, err
	}
	a.ID = newID()
	a.RaisedAt = a.RaisedAt.UTC()
	_, err := tx.ExecContext(ctx, `INSERT INTO alerts (id, kind, raised_at, session_id, member_id, message)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, string(a.Kind), a.RaisedAt.UnixNano(), a.SessionID, a.MemberID, a.Message)
	if err != nil {
		return Alert{}, fmt.Errorf("alert: raise: %w", err)
	}
	return a, nil
}

// Get returns the alert with id, or ErrNotFound.
func Get(ctx context.Context, q store.Querier, id string) (Alert, error) {
	var a Alert
	var kind string
	var at int64
	err := q.QueryRowContext(ctx, `SELECT id, kind, raised_at, session_id, member_id, message FROM alerts WHERE id = ?`, id).
		Scan(&a.ID, &kind, &at, &a.SessionID, &a.MemberID, &a.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, ErrNotFound
	}
	if err != nil {
		return Alert{}, fmt.Errorf("alert: get: %w", err)
	}
	a.Kind = Kind(kind)
	a.RaisedAt = time.Unix(0, at).UTC()
	return a, nil
}

func (a Alert) validate() error {
	switch {
	case !slices.Contains(Kinds, a.Kind):
		return fmt.Errorf("%w: unknown kind %q", ErrInvalid, a.Kind)
	case a.RaisedAt.IsZero():
		return fmt.Errorf("%w: no time raised", ErrInvalid)
	case a.Message == "" || utf8.RuneCountInString(a.Message) > maxMessageLen:
		return fmt.Errorf("%w: message must be 1 to %d characters", ErrInvalid, maxMessageLen)
	case a.SessionID != "" && !sessionPattern.MatchString(a.SessionID):
		return fmt.Errorf("%w: malformed session ID", ErrInvalid)
	case a.MemberID != "" && !memberPattern.MatchString(a.MemberID):
		return fmt.Errorf("%w: malformed member ID", ErrInvalid)
	}
	return nil
}

// newID returns "alt_" and 26 random base32 characters (130 bits), which fit the spec's pattern.
func newID() string { return "alt_" + rand.Text() }
