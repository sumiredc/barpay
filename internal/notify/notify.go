// Package notify sends the signed notifications of the PRD's Responsibilities, Notifications to
// the store app, and keeps their log. Each event is stored by the code that causes it, inside the
// transaction of that change (Enqueue); a Dispatcher sends what was stored after it commits.
package notify

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sumiredc/barpay/internal/store"
)

// Type is the spec's EventBase.type: the name of one of the spec's webhooks.
type Type string

const (
	SessionStarted           Type = "session.started"
	SessionMemberAttached    Type = "session.member_attached"
	SessionPaypalLinked      Type = "session.paypal_linked"
	SessionRegisterRequested Type = "session.register_requested"
	SessionClosed            Type = "session.closed"
	SessionPaymentFailed     Type = "session.payment_failed"
	SessionSettledOutside    Type = "session.settled_outside"
	SessionClosedUnpaid      Type = "session.closed_unpaid"
	GateUnlockFailed         Type = "gate.unlock_failed"
	MemberPaypalLinked       Type = "member.paypal_linked"
	MemberSavedPaypalDeleted Type = "member.saved_paypal_deleted"
)

// types are the spec's webhooks.
var types = []Type{
	SessionStarted, SessionMemberAttached, SessionPaypalLinked, SessionRegisterRequested, SessionClosed,
	SessionPaymentFailed, SessionSettledOutside, SessionClosedUnpaid, GateUnlockFailed, MemberPaypalLinked,
	MemberSavedPaypalDeleted,
}

// subjectKind is what a sequence number counts per: the session for session.* and gate.*, the
// member for member.* (the spec's EventBase).
type subjectKind string

const (
	sessionSubject subjectKind = "session"
	memberSubject  subjectKind = "member"
)

func (t Type) subject() subjectKind {
	if strings.HasPrefix(string(t), "member.") {
		return memberSubject
	}
	return sessionSubject
}

// The spec's SessionId and MemberId.
var (
	sessionPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]{16,32}$`)
	memberPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// ErrInvalid is returned for an event that does not fit the spec.
var ErrInvalid = errors.New("notify: invalid event")

// Event is one notification to store.
type Event struct {
	Type Type
	// SubjectID is the session ID for session.* and gate.*, the member ID for member.*.
	SubjectID string
	// Data becomes the event's data: a value that encodes as a JSON object, such as the
	// struct of the webhook's schema.
	Data any
}

// Stored is an event as Enqueue stored it.
type Stored struct {
	ID       string
	Sequence int64
}

// body is the spec's EventBase with data, in the order the spec lists the fields.
type body struct {
	ID        string          `json:"id"`
	Type      Type            `json:"type"`
	CreatedAt string          `json:"created_at"`
	Sequence  int64           `json:"sequence"`
	Data      json.RawMessage `json:"data"`
}

// Enqueue stores ev inside tx, the transaction of the change it reports, so it is sent exactly
// when that change commits; at is the instant of that change. It gives the event its ID and the
// next sequence number of its subject. Call Dispatcher.Wake after the commit to send it at once.
func Enqueue(ctx context.Context, tx store.Querier, at time.Time, ev Event) (Stored, error) {
	if err := ev.validate(); err != nil {
		return Stored{}, err
	}
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return Stored{}, fmt.Errorf("%w: data: %v", ErrInvalid, err)
	}
	if len(data) == 0 || data[0] != '{' {
		return Stored{}, fmt.Errorf("%w: data must be a JSON object", ErrInvalid)
	}
	kind := ev.Type.subject()
	// Transactions take the write lock when they begin (store.Open), so no other one can store
	// a number for this subject between this read and the insert; the unique constraint on
	// (subject_kind, subject_id, sequence) refuses a duplicate if that ever changes.
	var last int64
	err = tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence), 0) FROM notifications
		WHERE subject_kind = ? AND subject_id = ?`, string(kind), ev.SubjectID).Scan(&last)
	if err != nil {
		return Stored{}, fmt.Errorf("notify: next sequence: %w", err)
	}
	at = at.UTC()
	s := Stored{ID: newID(), Sequence: last + 1}
	b, err := json.Marshal(body{ID: s.ID, Type: ev.Type, CreatedAt: formatTime(at), Sequence: s.Sequence, Data: data})
	if err != nil {
		return Stored{}, fmt.Errorf("notify: encode: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notifications
		(id, type, subject_kind, subject_id, sequence, body, created_at, status, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'retrying', ?)`,
		s.ID, string(ev.Type), string(kind), ev.SubjectID, s.Sequence, b, at.UnixNano(), at.UnixNano())
	if err != nil {
		return Stored{}, fmt.Errorf("notify: store: %w", err)
	}
	return s, nil
}

func (ev Event) validate() error {
	if !slices.Contains(types, ev.Type) {
		return fmt.Errorf("%w: unknown type %q", ErrInvalid, ev.Type)
	}
	pattern := sessionPattern
	if ev.Type.subject() == memberSubject {
		pattern = memberPattern
	}
	if !pattern.MatchString(ev.SubjectID) {
		return fmt.Errorf("%w: malformed %s ID", ErrInvalid, ev.Type.subject())
	}
	return nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// newID returns "evt_" and 26 random base32 characters (130 bits), which fit the spec's pattern.
func newID() string { return "evt_" + rand.Text() }
