// Package signals keeps the signals a rate evaluation reads (the PRD's Pricing model, Signals):
// staff events, liveliness from the edge device, and occupancy.
package signals

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
)

// Phase is the spec's StaffEvent.phase.
type Phase string

const (
	Starts Phase = "starts"
	Ends   Phase = "ends"
)

// MaxLabel is the spec's StaffEvent.label maxLength, in characters.
const MaxLabel = 100

// StaffEvent is the spec's StaffEvent.
type StaffEvent struct {
	Label      string
	Phase      Phase
	RecordedAt time.Time
}

// ErrInvalidEvent is returned for a label or phase the spec's StaffEvent does not allow.
var ErrInvalidEvent = errors.New("signals: staff event does not fit the spec")

// RecordStaffEvent stores e, recorded at at, with the staff member by (the PRD's UC7 postcondition).
func RecordStaffEvent(ctx context.Context, st *store.Store, by staff.ID, label string, phase Phase, at time.Time) (StaffEvent, error) {
	if n := utf8.RuneCountInString(label); n < 1 || n > MaxLabel || !utf8.ValidString(label) || (phase != Starts && phase != Ends) {
		return StaffEvent{}, ErrInvalidEvent
	}
	e := StaffEvent{Label: label, Phase: phase, RecordedAt: at.UTC()}
	c := staff.Change{Staff: by, Operation: "sendStaffEvent", Subject: "staff_events", At: at}
	err := staff.Tx(ctx, st, c, func(tx store.Querier) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO staff_events (label, phase, recorded_at) VALUES (?, ?, ?)`,
			label, string(phase), at.UnixNano())
		if err != nil {
			return fmt.Errorf("signals: record staff event: %w", err)
		}
		return nil
	})
	if err != nil {
		return StaffEvent{}, err
	}
	return e, nil
}

// OngoingStaffEvents returns the labels whose last event is starts, in the order they started:
// an event stays an input to evaluations until the same label ends (the PRD's Pricing model, Signals).
func OngoingStaffEvents(ctx context.Context, q store.Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT label FROM staff_events e
		WHERE phase = 'starts' AND seq = (SELECT max(seq) FROM staff_events WHERE label = e.label)
		ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("signals: staff events: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("signals: staff events: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
