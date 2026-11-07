package main

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The log: BarPay's notification records and the notifications this app received, joined on the
// event ID into one row per notification, which staff search by customer to see whether a
// notification about them arrived.

// logPageSize is the records BarPay gives per page, and logSearchPages how many pages a search
// reads, newest first. BarPay's read has no search and only this app knows the names, so the
// app reads the pages and filters them; the bound keeps a search from holding the page up.
const (
	logPageSize    = 20
	logSearchPages = 10
)

type logRow struct {
	// ID is the dialog the row opens.
	ID                         string
	EventID                    string
	Time, What, Customer       string
	Status, StatusClass        string
	TypeDetail, CustomerDetail string
	Sent, App                  string
	sessionID                  string
}

func (a *App) adminNotifications(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	pages := 1
	if query != "" {
		pages = logSearchPages
	}
	cursor := q.Get("cursor")
	records, next, err := a.readLog(ctx, b.StaffID, cursor, pages)
	v := a.adminView(r, id, b, "Log", "log")
	data := struct {
		Query  string
		Rows   []logRow
		Next   string
		Failed bool
		// Newest is the first page, offered instead of rows when an older page could not be
		// read: what this app received is the newest, not what that page would have held.
		Newest string
	}{Query: query, Failed: err != nil}
	if err != nil && cursor != "" {
		data.Newest = "/admin/notifications"
		if query != "" {
			data.Newest += "?" + url.Values{"q": {query}}.Encode()
		}
		v.Data = data
		a.render(w, http.StatusOK, "admin_notifications", v)
		return
	}
	now := time.Now()
	a.store.view(func(d *Data) {
		names := customerNames(d)
		received := map[string]ReceivedEvent{}
		for _, e := range d.Received {
			received[e.EventID] = e
		}
		add := func(row logRow) {
			if matches(row, query) {
				row.ID = "log-" + strconv.Itoa(len(data.Rows)+1)
				data.Rows = append(data.Rows, row)
			}
		}
		if err == nil {
			for _, rec := range records {
				var got *ReceivedEvent
				if e, ok := received[rec.EventID]; ok {
					got = &e
				}
				add(sentRow(rec, got, names, now))
			}
			return
		}
		// BarPay didn't answer: what this app received is at hand, over as many as BarPay's
		// pages would have held, newest first by when each was made as BarPay lists them.
		last := d.Received[max(0, len(d.Received)-pages*logPageSize):]
		order := make([]ReceivedEvent, len(last))
		for i, e := range last {
			order[len(last)-1-i] = e
		}
		slices.SortStableFunc(order, func(x, y ReceivedEvent) int { return y.CreatedAt.Compare(x.CreatedAt) })
		for _, e := range order {
			add(receivedRow(e, names, now))
		}
	})
	if err == nil && next != nil {
		nq := url.Values{"cursor": {*next}}
		if query != "" {
			nq.Set("q", query)
		}
		data.Next = "/admin/notifications?" + nq.Encode()
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_notifications", v)
}

// readLog reads up to pages pages of BarPay's notification records from cursor, newest first,
// and the cursor after the last page read.
func (a *App) readLog(ctx context.Context, staffID, cursor string, pages int) ([]NotificationRecord, *string, error) {
	var all []NotificationRecord
	for i := 0; i < pages; i++ {
		records, next, err := a.barpay.listNotifications(ctx, staffID, cursor)
		if err != nil {
			return nil, nil, err
		}
		all = append(all, records...)
		if next == nil {
			return all, nil, nil
		}
		cursor = *next
	}
	return all, &cursor, nil
}

// matches says whether a row has query in the customer's name, the session ID, the event ID or
// the words for what happened, ignoring case.
func matches(row logRow, query string) bool {
	if query == "" {
		return true
	}
	q := strings.ToLower(query)
	for _, s := range []string{row.Customer, row.sessionID, row.EventID, row.What} {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}

// names tells the customers of notifications by the names this app holds: a member by their
// account, a session by the member it was for. A session of no member known here is a guest's.
type names struct {
	members        map[string]string
	sessionMembers map[string]string
}

func customerNames(d *Data) names {
	n := names{members: map[string]string{}, sessionMembers: map[string]string{}}
	for id, m := range d.Members {
		n.members[id] = m.Name
		for _, s := range m.SessionIDs {
			n.sessionMembers[s] = id
		}
	}
	for id, v := range d.Visits {
		switch {
		case v.MemberID != "":
			n.sessionMembers[id] = v.MemberID
		case v.Session != nil && v.Session.MemberID != nil:
			n.sessionMembers[id] = *v.Session.MemberID
		}
	}
	return n
}

func (n names) of(sessionID, memberID string) string {
	if memberID == "" {
		memberID = n.sessionMembers[sessionID]
	}
	if name, ok := n.members[memberID]; ok {
		return name
	}
	return "Guest"
}

// customerDetail is the customer's name with the session ID, or with "member" for a notification
// about the member's account.
func (n names) customerDetail(sessionID, memberID string) string {
	if sessionID != "" {
		return n.of(sessionID, "") + " (" + sessionID + ")"
	}
	return n.of("", memberID) + " (member)"
}

// sentRow is the row of a notification BarPay recorded, with what this app received of it, if
// anything.
func sentRow(rec NotificationRecord, got *ReceivedEvent, n names, now time.Time) logRow {
	row := logRow{
		EventID: rec.EventID, Time: logTime(rec.CreatedAt, now), What: eventWords(rec.Type),
		Customer: n.of(rec.SessionID, rec.MemberID), CustomerDetail: n.customerDetail(rec.SessionID, rec.MemberID),
		TypeDetail: typeDetail(rec.Type, got), sessionID: rec.SessionID,
		Sent: sentText(rec, now), App: appText(got, rec.Status, rec.MemberID != "", now),
	}
	row.Status, row.StatusClass = statusWords(rec.Status)
	return row
}

// receivedRow is the row of a notification this app received, when BarPay did not answer.
func receivedRow(e ReceivedEvent, n names, now time.Time) logRow {
	kind, subject, _ := strings.Cut(e.Subject, ":")
	sessionID, memberID := subject, ""
	if kind == "member" {
		sessionID, memberID = "", subject
	}
	row := logRow{
		EventID: e.EventID, Time: logTime(e.CreatedAt, now), What: eventWords(e.Type),
		Customer: n.of(sessionID, memberID), CustomerDetail: n.customerDetail(sessionID, memberID),
		TypeDetail: typeDetail(e.Type, &e), sessionID: sessionID,
		Sent: logTime(e.CreatedAt, now) + ". BarPay didn't answer, so its tries aren't known",
		App:  appText(&e, "delivered", memberID != "", now),
	}
	row.Status, row.StatusClass = statusWords("delivered")
	return row
}

// eventWords says what happened in a notification of type t; a type this app does not know is
// shown as it is.
func eventWords(t string) string {
	switch t {
	case "session.started":
		return "Checked in"
	case "session.member_attached":
		return "Guest visit moved to a member"
	case "session.paypal_linked":
		return "Linked saved PayPal to the visit"
	case "session.register_requested":
		return "Chose to pay at the register"
	case "session.closed":
		return "Paid"
	case "session.payment_failed":
		return "Payment failed"
	case "session.settled_outside":
		return "Settled by staff"
	case "session.closed_unpaid":
		return "Closed unpaid"
	case "gate.unlock_failed":
		return "Gate didn't open"
	case "member.paypal_linked":
		return "Linked PayPal"
	case "member.saved_paypal_deleted":
		return "Unlinked PayPal"
	}
	return t
}

// statusWords are the words and the pill of a record's status.
func statusWords(status string) (string, string) {
	switch status {
	case "delivered":
		return "Delivered", "closed"
	case "retrying":
		return "Retrying", "at_register"
	case "given_up":
		return "Not delivered", "payment_failed"
	}
	return status, ""
}

// typeDetail is the type, with the reason of a failed payment when this app received it.
func typeDetail(t string, got *ReceivedEvent) string {
	if t != "session.payment_failed" || got == nil {
		return t
	}
	switch got.Reason {
	case "refused":
		return t + " (refused)"
	case "unconfirmed":
		return t + " (outcome unknown)"
	}
	return t
}

// sentText says how BarPay sent a notification: when it was made, and how many tries it took,
// has taken so far, or took before BarPay gave up.
func sentText(rec NotificationRecord, now time.Time) string {
	made := logTime(rec.CreatedAt, now)
	switch rec.Status {
	case "delivered":
		if rec.Attempts <= 1 {
			return made + ", delivered on the first try"
		}
		s := made + ", delivered on try " + strconv.Itoa(rec.Attempts)
		if rec.LastAttemptAt != nil {
			s += " at " + logTime(*rec.LastAttemptAt, now)
		}
		return s
	case "retrying":
		s := made + ", not sent yet"
		if rec.Attempts > 0 && rec.LastAttemptAt != nil {
			s = made + ", sent " + times(rec.Attempts) + " (last at " + logTime(*rec.LastAttemptAt, now) + ")"
		}
		if rec.NextAttemptAt != nil {
			s += ". Next try at " + logTime(*rec.NextAttemptAt, now)
		}
		return s
	case "given_up":
		s := "Sent " + times(rec.Attempts) + " from " + dayTime(rec.CreatedAt)
		if rec.LastAttemptAt != nil {
			t := rec.LastAttemptAt.Local()
			s += "; gave up on " + t.Format("1/2") + " at " + t.Format("15:04")
		}
		return s
	}
	return made + ", " + rec.Status
}

func times(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return strconv.Itoa(n) + " times"
}

// appText says what this app did with a notification: when it received it and how it applied it,
// or that it has not received it. A member's notification not received changes nothing the
// customer sees, since the member's PayPal is read from BarPay each time it is shown.
func appText(got *ReceivedEvent, status string, member bool, now time.Time) string {
	if got != nil {
		return "Received at " + logTime(got.ReceivedAt, now) + ", number " + strconv.Itoa(got.Sequence) + ". " + appliedWords(got.Note)
	}
	s := "Not received"
	if status == "retrying" {
		s = "Not received yet"
	}
	if member {
		s += ". The member's PayPal is read from BarPay instead"
	}
	return s
}

// appliedWords puts the note handleNotification stored into words for staff.
func appliedWords(note string) string {
	switch note {
	case "applied in order":
		return "Applied in order"
	case "older than one already applied; state kept":
		return "Older than the state already read, so nothing changed"
	case "arrived after a gap; state read from BarPay":
		return "One was missing, so the state was read again from BarPay"
	}
	return note
}

// logTime is the time of day for a time today, and the date with it for another day.
func logTime(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return dayTime(t)
}

func dayTime(t time.Time) string { return t.Local().Format("1/2 15:04") }
