package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The visits page (/admin): the visits in three tabs, a dialog with the detail of each, and a
// dialog to check a visit out in the ways its state allows.

type visitTab struct {
	key, label string
	// states is the state filter the tab lists BarPay's sessions with.
	states string
}

var visitTabs = []visitTab{
	{"needs", "Needs checkout", "payment_failed,at_register"},
	{"here", "Here now", "active,checking_out"},
	{"done", "Done", "closed,closed_unpaid"},
}

// unpaidAfter is how long after checkout began (total_fixed_at) a visit waiting at the register
// or after a failed charge is closed unpaid (PRD: UC6, UC7).
const unpaidAfter = 3 * 24 * time.Hour

// waitingCount is the number of visits waiting to check out, read from the first page of the
// "Needs checkout" tab only: More says the page was full, so there are more than N.
type waitingCount struct {
	N    int
	More bool
}

func countWaiting(sessions []Session, next *string) *waitingCount {
	return &waitingCount{N: len(sessions), More: next != nil}
}

// startWaitingCount reads the visits waiting to check out for the side menu's badge, alongside
// whatever else the page reads, and returns a function that waits for the count. Like the gate
// dialog's customers, the read is sent once and given up after gateLookupTimeout; without an
// answer there is no badge. skip leaves the reading to the page, which then marks the badge.
func (a *App) startWaitingCount(r *http.Request, staffID string, skip bool) func() *waitingCount {
	if skip {
		return func() *waitingCount { return nil }
	}
	got := make(chan *waitingCount, 1)
	go func() {
		ctx, cancel := context.WithTimeout(r.Context(), gateLookupTimeout)
		defer cancel()
		sessions, next, err := a.barpay.listSessionsOnce(ctx, staffID, visitTabs[0].states, "")
		if err != nil {
			got <- nil
			return
		}
		got <- countWaiting(sessions, next)
	}()
	return func() *waitingCount { return <-got }
}

// markWaiting puts the count on the side menu's Visits item.
func markWaiting(f *staffFrame, w *waitingCount) {
	if f == nil || w == nil {
		return
	}
	for i := range f.Menu {
		if f.Menu[i].Href == "/admin" {
			f.Menu[i].Badge, f.Menu[i].BadgeMore = w.N, w.More
			f.Menu[i].BadgeLabel = w.Label()
		}
	}
}

func (w *waitingCount) Label() string {
	if w.More {
		return fmt.Sprintf("More than %d waiting to check out", w.N)
	}
	return fmt.Sprintf("%d waiting to check out", w.N)
}

type visitTabLink struct {
	Href, Label string
	Current     bool
	Waiting     *waitingCount
}

// visitRow is a visit as the page draws it: its row, its detail and, when staff can check it out,
// the checkout dialog.
type visitRow struct {
	SessionID, ShortID, Name string
	CheckedInAt              time.Time
	Total                    Money
	State, StateLabel        string
	Ended                    bool
	Detail                   []detailLine
	Checkout                 *checkoutDialog
}

type detailLine struct{ Label, Value string }

type checkoutDialog struct {
	Title   string
	Summary []detailLine
	// Warn comes before the ways, and Note after the summary.
	Warn, Note string
	Ways       []checkoutWay
	Currency   string
	// Placeholder is a zero amount as staff type it in the currency.
	Placeholder string
}

// checkoutWay is one way to check a visit out. How is what the form sends; Amount, when set,
// asks how much was taken.
type checkoutWay struct {
	How, Label, Note string
	Amount           *amountChoice
}

// amountChoice offers the amount shown in the dialog (Value, as staff type it, such as "18.00")
// or another amount staff type.
type amountChoice struct {
	Value, Label string
}

func (a *App) adminVisits(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	q := r.URL.Query()
	tab := visitTabs[0]
	for _, t := range visitTabs {
		if t.key == q.Get("tab") {
			tab = t
		}
	}
	cursor := q.Get("cursor")
	firstOfNeeds := tab.key == "needs" && cursor == ""
	waiting := a.startWaitingCount(r, b.StaffID, firstOfNeeds)
	sessions, next, err := a.barpay.listSessions(ctx, b.StaffID, tab.states, cursor)
	count := waiting()
	if firstOfNeeds && err == nil {
		count = countWaiting(sessions, next)
	}
	v := a.adminView(r, id, b, "Visits", "visits")
	markWaiting(v.Frame, count)
	data := struct {
		Tabs       []visitTabLink
		Tab        string
		Rows       []visitRow
		Next       string
		ReadFailed bool
	}{Tab: tab.key, ReadFailed: err != nil}
	for _, t := range visitTabs {
		l := visitTabLink{Href: "/admin?" + url.Values{"tab": {t.key}}.Encode(), Label: t.label, Current: t.key == tab.key}
		if t.key == "needs" && count != nil && count.N > 0 {
			l.Waiting = count
		}
		data.Tabs = append(data.Tabs, l)
	}
	now := time.Now()
	a.store.view(func(d *Data) {
		for _, s := range sessions {
			var rec Visit
			if v := d.Visits[s.SessionID]; v != nil {
				rec = *v
			}
			memberID := rec.MemberID
			if s.MemberID != nil {
				memberID = *s.MemberID
			}
			name := ""
			if m := d.Members[memberID]; memberID != "" && m != nil {
				name = m.Name
			}
			data.Rows = append(data.Rows, visitRowOf(s, rec, name, memberID != "", now))
		}
	})
	if next != nil {
		data.Next = "/admin?" + url.Values{"tab": {tab.key}, "cursor": {*next}}.Encode()
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_sessions", v)
}

// visitRowOf draws a session as of now, with what the app keeps of its visit. name is the
// member's name, "" for a guest or a member the app does not know.
func visitRowOf(s Session, rec Visit, name string, member bool, now time.Time) visitRow {
	who := name
	if who == "" {
		who = "Guest"
	}
	row := visitRow{
		SessionID: s.SessionID, ShortID: shortID(s.SessionID), Name: who, CheckedInAt: s.CheckedInAt,
		Total: s.Total, State: s.State, StateLabel: visitStateLabel(s.State), Ended: s.ended(),
	}
	end := now
	if s.TotalFixedAt != nil {
		end = *s.TotalFixedAt
	}
	clock := func(t time.Time) string { return t.Local().Format("15:04") }
	checkedIn := detailLine{"Checked in", clock(s.CheckedInAt) + " (" + formatDuration(end.Sub(s.CheckedInAt)) + ")"}
	total := detailLine{"Total", fmt.Sprintf("%s (cover charge %s, orders %s)", formatMoney(s.Total), formatMoney(s.CoverCharge), formatMoney(s.OrdersTotal))}
	id := detailLine{"ID", s.SessionID}
	method := detailLine{"Payment", "PayPal"}
	if s.PaymentMethod != "paypal" {
		method.Value = "Register"
	}
	deadline := detailLine{"Deadline", ""}
	if s.TotalFixedAt != nil {
		at := s.TotalFixedAt.Add(unpaidAfter).Local()
		deadline.Value = "Closes unpaid on " + at.Format("1/2") + " at " + at.Format("15:04")
	}
	// staff names who sent the last accepted action, and when BarPay answered it.
	staff := func(actions ...string) string {
		for i := len(rec.StaffActions) - 1; i >= 0; i-- {
			for _, act := range actions {
				if rec.StaffActions[i].Action == act {
					return rec.StaffActions[i].StaffID
				}
			}
		}
		return ""
	}
	when := func(t time.Time, staffID string) string {
		if staffID == "" {
			return clock(t)
		}
		return clock(t) + ", " + staffID
	}
	unpaid := s.Total
	if s.UnpaidAmount != nil {
		unpaid = *s.UnpaidAmount
	}

	switch s.State {
	case "payment_failed", "at_register":
		row.Detail = []detailLine{checkedIn, total, method, {"History", visitHistory(s, rec)}, deadline, id}
	case "active":
		total.Label = "Total so far"
		row.Detail = []detailLine{checkedIn, total}
		if s.CoverChargeStatus == "paused" && s.CoverChargeResumesAt != nil {
			row.Detail = append(row.Detail, detailLine{"Cover charge", "free after an order until " + clock(*s.CoverChargeResumesAt)})
		}
		paypal := "not linked"
		switch {
		case !member:
			paypal = "not linked (guest)"
		case s.PaymentMethod == "paypal":
			paypal = "linked"
		}
		row.Detail = append(row.Detail, detailLine{"PayPal", paypal}, id)
	case "checking_out":
		since := end
		for _, act := range rec.StaffActions {
			if (act.Action == "close" || act.Action == "retry") && act.At.After(since) {
				since = act.At
			}
		}
		row.Detail = []detailLine{checkedIn, total, method, {"State", "Checking out: waiting for PayPal (since " + clock(since) + ")"}, id}
	case "closed":
		if s.Receipt == nil {
			row.Detail = []detailLine{checkedIn, total, id}
			break
		}
		rc := s.Receipt
		switch rc.Settlement {
		case "paypal":
			pay := fmt.Sprintf("%s with PayPal (%s)", formatMoney(rc.Total), when(rc.EndedAt, staff("close", "retry")))
			row.Detail = []detailLine{checkedIn, {"History", visitHistory(s, rec)}, total, {"Payment", pay}, id}
		default:
			took := rc.Total
			if rc.AmountRecorded != nil {
				took = *rc.AmountRecorded
			}
			pay := fmt.Sprintf("%s at the register (%s)", formatMoney(took), when(rc.EndedAt, staff("register-payment")))
			if rc.Settlement == "outside" {
				pay = fmt.Sprintf("%s settled outside BarPay (%s)", formatMoney(took), when(rc.EndedAt, staff("settle-outside")))
			}
			row.Detail = []detailLine{checkedIn, total, {"Payment", pay}, id}
		}
	case "closed_unpaid":
		state := "Closed unpaid"
		if s.Receipt != nil {
			state += " (" + when(s.Receipt.EndedAt, staff("mark-unpaid")) + ")"
		}
		row.Detail = []detailLine{checkedIn, total, {"State", state}, {"Unpaid", formatMoney(unpaid)}, id}
	default:
		row.Detail = []detailLine{checkedIn, total, id}
	}

	row.Checkout = checkoutOf(s, rec, name, unpaid)
	return row
}

// checkoutOf is the checkout dialog of a session, with the ways its state allows; nil when staff
// cannot check it out (checking out, or ended).
func checkoutOf(s Session, rec Visit, name string, unpaid Money) *checkoutDialog {
	title := "Check out " + name
	if name == "" {
		title = "Check out Guest (checked in " + s.CheckedInAt.Local().Format("15:04") + ")"
	}
	c := &checkoutDialog{Title: title, Currency: s.Total.Currency, Placeholder: typedAmount(Money{Currency: s.Total.Currency})}
	const pending = "The result comes shortly; until then the visit shows Checking out."
	took := func(m Money, suffix string) *amountChoice {
		return &amountChoice{Value: typedAmount(m), Label: "Took " + formatMoney(m) + suffix}
	}
	switch s.State {
	case "payment_failed":
		c.Summary = []detailLine{{"Unpaid", formatMoney(unpaid)}}
		if paymentFailedReason(rec) == "refused" {
			c.Summary = append(c.Summary, detailLine{"Last result", "PayPal declined"})
			c.Ways = []checkoutWay{
				{How: "retry", Label: "Charge PayPal again", Note: pending + " This fails if the customer has deleted their saved PayPal."},
				{How: "outside", Label: "Settled outside BarPay", Amount: took(unpaid, "")},
			}
		} else {
			c.Warn = "The last charge's outcome is unknown. First check in the PayPal dashboard whether it went through."
			c.Ways = []checkoutWay{
				{How: "outside", Label: "It went through (settled outside BarPay)", Amount: took(unpaid, "")},
				{How: "retry", Label: "It didn't (charge PayPal again)", Note: pending + " If the last charge went through, it isn't charged twice."},
			}
		}
	case "at_register":
		c.Summary = []detailLine{{"Total", formatMoney(s.Total)}}
		c.Ways = []checkoutWay{
			{How: "register", Label: "Paid at the register", Amount: took(s.Total, "")},
			{How: "unpaid", Label: "Left without paying (close unpaid)"},
		}
	case "active":
		c.Summary = []detailLine{{"Total so far", formatMoney(s.Total)}}
		c.Note = "The total is fixed when BarPay receives this. If a block ends in between and the total changes, both the fixed total and the amount you took are kept."
		c.Ways = []checkoutWay{{How: "register", Label: "Paid at the register", Amount: took(s.Total, " (total so far)")}}
		if s.PaymentMethod == "paypal" {
			c.Ways = append(c.Ways, checkoutWay{How: "close", Label: "Charge PayPal (left without using the exit gate)", Note: "Charges the customer's saved PayPal. " + pending})
		}
	default:
		return nil
	}
	return c
}

// paymentFailedReason is why the last charge failed, from the last session.payment_failed the
// app received: "refused" or "unconfirmed". Without one, or when staff retried the charge and its
// result has not arrived, the outcome is taken as unknown, so that staff check before charging
// again. A retry's result is a payment_failed of a higher sequence than any notification received
// when the retry was sent; BarPay stores it before answering the retry, so it may arrive first.
func paymentFailedReason(rec Visit) string {
	var last *VisitEvent
	for i := range rec.Events {
		if rec.Events[i].Type == "session.payment_failed" {
			last = &rec.Events[i]
		}
	}
	if last == nil {
		return "unconfirmed"
	}
	for _, act := range rec.StaffActions {
		if act.Action == "retry" && act.AfterSequence >= last.Sequence {
			return "unconfirmed"
		}
	}
	if last.Reason == "refused" {
		return "refused"
	}
	return "unconfirmed"
}

// visitHistory tells, from the notifications received and the charges staff sent, how a visit
// came to wait for checkout or to be paid with PayPal, such as "21:45 charged PayPal → 21:47
// failed, outcome unknown". An outcome's time is left out when it is the minute of its charge.
func visitHistory(s Session, rec Visit) string {
	clock := func(t time.Time) string { return t.Local().Format("15:04") }
	if s.State == "at_register" || s.PaymentMethod != "paypal" {
		if s.TotalFixedAt == nil {
			return "The customer chose the register; the total was fixed"
		}
		return clock(*s.TotalFixedAt) + " the customer chose the register; the total was fixed"
	}
	// Steps are in the order of the session's sequence: a retry comes after the notifications
	// received when it was sent, and before any later one.
	type step struct {
		at    time.Time
		order int
		kind  string // an event type, or a staff action
		ev    VisitEvent
	}
	var steps []step
	for _, e := range rec.Events {
		switch e.Type {
		case "session.payment_failed", "session.closed", "gate.unlock_failed":
			steps = append(steps, step{at: e.At, order: 2 * e.Sequence, kind: e.Type, ev: e})
		}
	}
	byStaff := false
	for _, act := range rec.StaffActions {
		switch act.Action {
		case "close":
			byStaff = true
		case "retry":
			steps = append(steps, step{at: act.At, order: 2*act.AfterSequence + 1, kind: "retry"})
		}
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].order < steps[j].order })

	var chargedAt time.Time
	var parts []string
	if s.TotalFixedAt != nil {
		chargedAt = *s.TotalFixedAt
		parts = append(parts, clock(chargedAt)+" charged PayPal")
	} else {
		parts = append(parts, "charged PayPal")
	}
	outcome := func(at time.Time, what string) string {
		if !chargedAt.IsZero() && clock(at) == clock(chargedAt) {
			return what
		}
		return clock(at) + " " + what
	}
	open := true         // the last charge has no outcome yet
	seenOutcome := false // a charge before has an outcome
	firstPaid := false   // the first charge paid
	for _, st := range steps {
		switch st.kind {
		case "retry":
			chargedAt, open = st.at, true
			parts = append(parts, clock(st.at)+" charged PayPal again")
		case "session.payment_failed":
			what := "failed, outcome unknown"
			if st.ev.Reason == "refused" {
				what = "PayPal declined"
			}
			parts = append(parts, outcome(st.at, what))
			open, seenOutcome = false, true
		case "session.closed":
			firstPaid = !seenOutcome
			parts = append(parts, outcome(st.at, "paid"))
			open, seenOutcome = false, true
		case "gate.unlock_failed":
			// It follows a paid charge; its session.closed may not have arrived.
			if open {
				firstPaid = !seenOutcome
				parts = append(parts, outcome(st.at, "paid"))
				open, seenOutcome = false, true
			}
			parts[len(parts)-1] += ". The gate didn't open"
		}
	}
	switch {
	case open && s.State == "payment_failed":
		parts = append(parts, "failed, outcome unknown")
	case open && s.State == "closed":
		firstPaid = !seenOutcome
		parts = append(parts, "paid")
	}
	// A charge that paid at once and that staff did not send was the exit gate's.
	if firstPaid && !byStaff {
		parts[0] += " at the exit gate"
	}
	return strings.Join(parts, " → ")
}

func visitStateLabel(s string) string {
	switch s {
	case "payment_failed":
		return "Payment failed"
	case "at_register":
		return "At the register"
	case "active":
		return "Here"
	case "checking_out":
		return "Checking out"
	case "closed":
		return "Paid"
	case "closed_unpaid":
		return "Closed unpaid"
	}
	return s
}

// shortID is the head and the tail of a session ID, such as "ses_01hx3b2n…9p8w", enough to tell
// visits apart in the list.
func shortID(id string) string {
	if len(id) <= 18 {
		return id
	}
	return id[:12] + "…" + id[len(id)-4:]
}

// typedAmount writes an amount as staff type it in the store's currency: whole yen, such as
// "1800", or dollars and cents, such as "18.00".
func typedAmount(m Money) string {
	if m.Currency == "JPY" {
		return fmt.Sprint(m.Value)
	}
	return dollars(m)
}

// parseTypedAmount reads an amount staff typed in currency, in its smallest unit: whole yen for
// JPY, such as "1800", and cents otherwise, such as "12.50".
func parseTypedAmount(s, currency string) (int64, bool) {
	if currency != "JPY" {
		return parseAmount(s)
	}
	s = strings.TrimPrefix(strings.TrimSpace(s), "¥")
	if s == "" || !onlyDigits(s) || len(s) > 15 {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// checkoutActions maps what the checkout dialog sends to the admin request it makes.
var checkoutActions = map[string]string{
	"register": "register-payment",
	"outside":  "settle-outside",
	"unpaid":   "mark-unpaid",
	"close":    "close",
	"retry":    "retry",
}

// adminCheckout sends the way chosen in a visit's checkout dialog, and goes back to the page the
// dialog was on to tell the outcome. A register payment is one register-payment request, whatever
// the state: BarPay fixes the total when it receives it, and its answer's total is the one that
// counts, so the notice tells it when it differs from the amount taken.
func (a *App) adminCheckout(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	sessionID := r.PathValue("session_id")
	back := adminPath(r.PostFormValue("back"))
	action, ok := checkoutActions[r.PostFormValue("how")]
	if !ok {
		a.alert(id, "Choose how the visit was checked out.")
		redirect(w, r, back)
		return
	}
	// What was received of the session when the request was sent tells which later notifications
	// are its result (paymentFailedReason).
	sentAt, after := time.Now(), 0
	a.store.view(func(d *Data) {
		if v := d.Visits[sessionID]; v != nil {
			for _, e := range v.Events {
				after = max(after, e.Sequence)
			}
		}
	})
	var s *Session
	var err error
	var msg string
	switch action {
	case "close", "retry":
		s, msg, err = a.charge(r, action, sessionID, b.StaffID)
	case "register-payment", "settle-outside":
		amount := r.PostFormValue("amount")
		if amount == "other" {
			amount = r.PostFormValue("other_amount")
		}
		currency := r.PostFormValue("currency")
		value, ok := parseTypedAmount(amount, currency)
		if !ok {
			a.alert(id, "Enter the amount taken, such as "+typedAmount(Money{Value: 1250, Currency: currency})+".")
			redirect(w, r, back)
			return
		}
		m := Money{Value: value, Currency: currency}
		if action == "register-payment" {
			s, err = a.barpay.recordRegisterPayment(ctx, b.StaffID, sessionID, m)
		} else {
			s, err = a.barpay.settleOutside(ctx, b.StaffID, sessionID, m)
		}
		switch {
		case err == nil:
			msg = settledNotice(s, m)
		case errors.Is(err, ErrNoAnswer) || errorCode(err) == "session_not_active" || errorCode(err) == "session_not_payment_failed":
			msg = "BarPay did not record it. If you sent it before, the visit may already be checked out: check its state below."
		}
	case "mark-unpaid":
		s, err = a.barpay.markUnpaid(ctx, b.StaffID, sessionID)
		if err == nil {
			msg = "Closed unpaid."
		}
	}
	if err != nil {
		if msg == "" {
			msg = "BarPay refused: " + err.Error()
		}
		a.alert(id, msg)
		redirect(w, r, back)
		return
	}
	a.store.update(func(d *Data) error {
		d.remember(s)
		d.noteStaffAction(sessionID, StaffAction{Action: action, StaffID: b.StaffID, At: sentAt, AfterSequence: after})
		return nil
	})
	a.flash(id, msg)
	redirect(w, r, back)
}

// settledNotice tells a recorded payment, with the total BarPay fixed when it differs from the
// amount taken.
func settledNotice(s *Session, took Money) string {
	total := s.Total
	if s.Receipt != nil {
		total = s.Receipt.Total
	}
	if total == took {
		return "Checked out."
	}
	return fmt.Sprintf("Checked out at a total of %s. You took %s.", formatMoney(total), formatMoney(took))
}

// chargeKeyName names the stored Idempotency-Key of a staff member's close or retry of a session.
func chargeKeyName(action, sessionID, staffID string) string {
	return action + ":" + sessionID + ":" + staffID
}

// charge sends a close or retry, which can charge the customer's PayPal. Its Idempotency-Key is
// stored before the call and kept until an answer arrives: confirming again after a lost answer
// sends the same key, which BarPay answers with the first response and never charges again for.
// Once an answer arrives the key is dropped, so a later retry is a new attempt.
func (a *App) charge(r *http.Request, action, sessionID, staffID string) (*Session, string, error) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	name := chargeKeyName(action, sessionID, staffID)
	var key string
	a.store.update(func(d *Data) error {
		if key = d.PendingKeys[name]; key == "" {
			key = randomToken(24)
			d.PendingKeys[name] = key
		}
		return nil
	})
	var s *Session
	var err error
	if action == "close" {
		s, err = a.barpay.closeSession(ctx, staffID, key, sessionID)
	} else {
		s, err = a.barpay.retryCharge(ctx, staffID, key, sessionID)
	}
	if errors.Is(err, ErrNoAnswer) {
		return nil, "BarPay didn't answer. Check the visit out the same way again: it sends the same request, which charges at most once.", err
	}
	a.store.update(func(d *Data) error { delete(d.PendingKeys, name); return nil })
	if err != nil {
		return nil, "", err
	}
	switch s.State {
	case "closed":
		return s, "Checked out.", nil
	case "checking_out":
		return s, "The result comes shortly; until then the visit shows Checking out.", nil
	default:
		return s, "The charge didn't go through: the visit is " + visitStateLabel(s.State) + ".", nil
	}
}
