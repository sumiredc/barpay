package main

import (
	"sort"
	"strings"
	"time"
)

// What the app keeps of a visit for the staff's visits page, which BarPay does not give: the
// notifications of its session, to tell the history and why a charge failed, and the staff
// member who checked it out, whom BarPay's receipt does not name.

// VisitEvent is a notification of a visit's session that the app received: session.* or
// gate.unlock_failed.
type VisitEvent struct {
	Type     string    `json:"type"`
	Sequence int       `json:"sequence"`
	At       time.Time `json:"at"`
	// Reason is data.reason of a session.payment_failed: "refused" or "unconfirmed".
	Reason string `json:"reason,omitempty"`
}

// StaffAction is a checkout of a visit that staff sent from this app and BarPay accepted: close,
// retry, register-payment, settle-outside or mark-unpaid. At is when it was sent, and
// AfterSequence the highest sequence of the session's notifications received then: one of a
// higher sequence came after the request, whichever arrived first.
type StaffAction struct {
	Action        string    `json:"action"`
	StaffID       string    `json:"staff_id"`
	At            time.Time `json:"at"`
	AfterSequence int       `json:"after_sequence"`
}

// visitEventsKept bounds the notifications kept per visit; a visit has a handful.
const visitEventsKept = 50

// noteVisitEvent keeps a session notification on its visit, also one older than a notification
// already applied: it still says what happened and when.
func (d *Data) noteVisitEvent(e *event) {
	s := e.Data.Session
	if s == nil || !(strings.HasPrefix(e.Type, "session.") || e.Type == "gate.unlock_failed") {
		return
	}
	v := d.visit(s.SessionID)
	v.Events = append(v.Events, VisitEvent{Type: e.Type, Sequence: e.Sequence, At: e.CreatedAt, Reason: e.Data.Reason})
	sort.SliceStable(v.Events, func(i, j int) bool { return v.Events[i].Sequence < v.Events[j].Sequence })
	if n := len(v.Events); n > visitEventsKept {
		v.Events = append([]VisitEvent(nil), v.Events[n-visitEventsKept:]...)
	}
}

// noteStaffAction keeps an accepted checkout of a visit.
func (d *Data) noteStaffAction(sessionID string, act StaffAction) {
	v := d.visit(sessionID)
	v.StaffActions = append(v.StaffActions, act)
}
