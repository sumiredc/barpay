package main

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// The alerts page lists only the exit gate's failed unlocks (unlock_failed). A failed or unknown
// charge and a call to the register are seen and dealt with on the visits page; a failed unlock
// is not, as the visit has already ended paid. Staff mark a failed unlock as done once they dealt
// with it. BarPay's alerts have no such state, so the app keeps it, by alert ID; "Open" are the
// failed unlocks not marked done.

const (
	// alertsPerScreen is how many failed unlocks fill one screen of the alerts page.
	alertsPerScreen = 20
	// alertPagesPerScreen bounds the pages of BarPay's alerts read for one screen. The other
	// kinds can fill page after page; past this, staff go on with Older alerts.
	alertPagesPerScreen = 10
)

// alertScan is a screen of failed unlocks read from BarPay's alerts.
type alertScan struct {
	// Alerts are the alerts kept, newest first.
	Alerts []Alert
	// Next is the cursor to go on reading from, and "" once BarPay's list has ended.
	Next string
	// sessions holds the sessions read out for the alerts, so a page reads each once.
	sessions map[string]*Session
}

// scanAlerts reads BarPay's alerts from cursor, a page at a time, and keeps the unlock_failed
// alerts that keep says to, until alertsPerScreen are kept, the list ends or alertPagesPerScreen
// pages are read. BarPay cannot be asked for one kind only. Every alert of a page read is looked
// at, so a screen can hold a few more than alertsPerScreen, and Older alerts goes on from the next
// page without skipping any. resend says whether a read that got no answer is sent again.
// Only a first page that cannot be read is an error: a later one ends the screen there, with what
// was read so far, and Older alerts goes on from the page that could not be read.
func (a *App) scanAlerts(ctx context.Context, staffID, cursor string, keep func(Alert) bool, resend bool) (*alertScan, error) {
	sc := &alertScan{sessions: map[string]*Session{}}
	for page := range alertPagesPerScreen {
		alerts, next, err := a.barpay.readAlerts(ctx, staffID, cursor, resend)
		if err != nil && page == 0 {
			return nil, err
		}
		if err != nil {
			break
		}
		for _, al := range alerts {
			if al.Kind == "unlock_failed" && keep(al) {
				sc.Alerts = append(sc.Alerts, al)
			}
		}
		if next == nil || *next == "" {
			return sc, nil
		}
		cursor = *next
		if len(sc.Alerts) >= alertsPerScreen {
			break
		}
	}
	sc.Next = cursor
	return sc, nil
}

func everyAlert(Alert) bool { return true }

// notDone keeps the alerts not marked done when it is made.
func (a *App) notDone() func(Alert) bool {
	done := a.doneAlerts()
	return func(al Alert) bool { _, ok := done[al.ID]; return !ok }
}

func (a *App) doneAlerts() map[string]AlertDone {
	done := map[string]AlertDone{}
	a.store.view(func(d *Data) {
		for id, dn := range d.AlertsDone {
			done[id] = dn
		}
	})
	return done
}

// count is the count of the open failed unlocks beside Alerts and on the Open tab, with a "+"
// when the screen stopped short of the end of the list.
func (sc *alertScan) count() (n int, more bool, label string) {
	n, more = len(sc.Alerts), sc.Next != ""
	label = strconv.Itoa(n)
	if more {
		label += "+"
	}
	return n, more, label + " open"
}

// customer names the customer of an alert's session and returns the session with its receipt,
// or nil when it is not known to have ended. The session is read from BarPay when the app has not
// stored it ended, once a page once it is read: a read that failed, such as one cut short by the
// gate dialog's time limit, is tried again for the alert's detail. A member's name comes from the app's accounts; anyone else is a Guest.
func (a *App) customer(ctx context.Context, sc *alertScan, al Alert) (string, *Session) {
	var s *Session
	memberID := al.MemberID
	a.store.view(func(d *Data) {
		if v := d.Visits[al.SessionID]; v != nil {
			if v.Session != nil && v.Session.Receipt != nil {
				cp := *v.Session
				s = &cp
			}
			if memberID == "" {
				memberID = v.MemberID
			}
		}
	})
	if s == nil {
		read, ok := sc.sessions[al.SessionID]
		if !ok {
			var err error
			if read, err = a.barpay.getSessionOnce(ctx, al.SessionID); err == nil {
				sc.sessions[al.SessionID] = read
			}
		}
		if read != nil && read.Receipt != nil {
			s = read
		}
		if memberID == "" && read != nil && read.MemberID != nil {
			memberID = *read.MemberID
		}
	}
	name := "Guest"
	a.store.view(func(d *Data) {
		if m := d.Members[memberID]; m != nil {
			name = m.Name
		}
	})
	return name, s
}

// alertRow is a failed unlock as the alerts page draws it, in its row and its detail dialog.
type alertRow struct {
	ID, Raised, What, SessionID string
	// Customer is "—" for an unlock by hand for no particular customer.
	Customer string
	// Paid says how the customer paid, from the session's receipt, when it is known.
	Paid string
	// Done is who marked it done and when, if anyone did.
	Done           bool
	DoneBy, DoneAt string
	// Markable says the ID has the form of an alert's, the only form that can be marked done; an
	// ID BarPay sent in another form is not put into a form's path.
	Markable bool
}

func (a *App) alertRow(ctx context.Context, sc *alertScan, al Alert, done map[string]AlertDone, now time.Time) alertRow {
	row := alertRow{ID: al.ID, Raised: when(al.RaisedAt, now), SessionID: al.SessionID, Customer: "—",
		What: "Unlocked by hand, but the exit gate didn't open"}
	row.Markable = alertIDPattern.MatchString(al.ID)
	if dn, ok := done[al.ID]; ok {
		row.Done, row.DoneBy, row.DoneAt = true, dn.StaffID, when(dn.At, now)
	}
	if al.SessionID == "" {
		return row
	}
	name, s := a.customer(ctx, sc, al)
	row.Customer = name
	row.What = "Paid, but the exit gate didn't open"
	if s != nil {
		row.Paid = paidSentence(s.Receipt, now)
	}
	return row
}

// paidSentence tells how and when a customer paid, such as "The customer paid $33.00 with
// PayPal at 21:59.", with the day as well before today.
func paidSentence(r *Receipt, now time.Time) string {
	at := when(r.EndedAt, now)
	recorded := r.Total
	if r.AmountRecorded != nil {
		recorded = *r.AmountRecorded
	}
	switch r.Settlement {
	case "paypal":
		return "The customer paid " + formatMoney(r.Total) + " with PayPal at " + at + "."
	case "register":
		return "The customer paid " + formatMoney(recorded) + " at the register at " + at + "."
	case "outside":
		return "Staff settled " + formatMoney(recorded) + " for the customer at " + at + "."
	case "unpaid":
		return "The visit was closed unpaid at " + at + "."
	}
	return ""
}

// when is a time of the alerts page: the clock for today, and the day as well before then.
func when(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// adminAlerts draws the Open tab, or the All tab with ?tab=all, from ?cursor= on.
func (a *App) adminAlerts(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	q := r.URL.Query()
	all, cursor := q.Get("tab") == "all", q.Get("cursor")
	done := a.doneAlerts()
	keep := everyAlert
	if !all {
		keep = func(al Alert) bool { _, ok := done[al.ID]; return !ok }
	}
	sc, err := a.scanAlerts(ctx, b.StaffID, cursor, keep, true)
	var open *alertsRead
	if !all && cursor == "" {
		open = &alertsRead{sc, err}
	}
	v := a.adminViewWithAlerts(r, id, b, "Alerts", "alerts", open)
	data := struct {
		All    bool
		Rows   []alertRow
		Next   string
		Failed bool
	}{All: all, Failed: err != nil}
	if err == nil {
		now := time.Now()
		for _, al := range sc.Alerts {
			data.Rows = append(data.Rows, a.alertRow(ctx, sc, al, done, now))
		}
		if sc.Next != "" {
			nq := url.Values{"cursor": {sc.Next}}
			if all {
				nq.Set("tab", "all")
			}
			data.Next = "/admin/alerts?" + nq.Encode()
		}
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_alerts", v)
}

// alertIDPattern is the spec's pattern of an alert ID. BarPay cannot be asked for one alert, so
// an ID is taken on its form: one that is not an alert's is never listed, and only staff can
// send it.
var alertIDPattern = regexp.MustCompile(`^alt_[A-Za-z0-9]{16,32}$`)

// adminAlertDone marks a failed unlock as done by the staff member, and goes back to the page it
// was sent from. The first mark stands: marking it again keeps who did it first and when.
func (a *App) adminAlertDone(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	alertID := r.PathValue("alert_id")
	if !alertIDPattern.MatchString(alertID) {
		http.NotFound(w, r)
		return
	}
	err := a.store.update(func(d *Data) error {
		if _, ok := d.AlertsDone[alertID]; !ok {
			d.AlertsDone[alertID] = AlertDone{StaffID: b.StaffID, At: time.Now()}
		}
		return nil
	})
	if err != nil {
		a.alert(id, "The alert wasn't marked as done: "+err.Error())
	}
	redirect(w, r, adminPath(r.PostFormValue("back")))
}
