package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The admin screen. Staff log in with an ID and password from the config file; the ID they
// logged in with is the BarPay-Staff-Id of every admin request they make, so BarPay records who
// acted.

type staffHandler func(w http.ResponseWriter, r *http.Request, id string, b Browser)

// staffOnly sends a browser without a logged-in staff member to the staff login, and refuses a
// form post that this app did not draw for the browser session.
func (a *App) staffOnly(h staffHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, b := a.browser(w, r)
		if b.StaffID == "" {
			redirect(w, r, "/admin/login")
			return
		}
		if _, ok := a.cfg.Staff[b.StaffID]; !ok {
			// The account was removed from the config file since the login.
			a.updateBrowser(id, func(b *Browser) { b.StaffID = "" })
			redirect(w, r, "/admin/login")
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil || !checkCSRF(r, b) {
				http.Error(w, "This form has expired. Go back, reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		h(w, r, id, b)
	}
}

// staffFrame is what layout.html draws around an admin page for a logged-in staff member: the
// side menu.
type staffFrame struct {
	Menu []sideItem
}

type sideItem struct {
	Href, Icon, Label string
	Current           bool
	// Badge is a count beside the label, left out at 0. What it counts is up to the page it opens.
	Badge      int
	BadgeLabel string
}

// staffMenu lists the side menu from the top. key is what an admin page names as its own item.
var staffMenu = []struct{ key, href, icon, label string }{
	{"visits", "/admin", "users", "Visits"},
	{"alerts", "/admin/alerts", "bell", "Alerts"},
	{"pricing", "/admin/pricing", "trending-up", "Pricing"},
	{"log", "/admin/notifications", "history", "Log"},
}

// adminView is the view of an admin page for a logged-in staff member. nav is the key of the
// side menu item the page belongs to: the pricing tabs ("Current rate" and "Settings") both
// belong to "pricing".
func (a *App) adminView(r *http.Request, id string, b Browser, title, nav string) view {
	v := a.baseView(id, b, title)
	v.Admin = true
	v.Here = r.URL.RequestURI()
	f := &staffFrame{}
	for _, m := range staffMenu {
		f.Menu = append(f.Menu, sideItem{Href: m.href, Icon: m.icon, Label: m.label, Current: m.key == nav})
	}
	v.Frame = f
	return v
}

func (a *App) staffLoginForm(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	if b.StaffID != "" {
		redirect(w, r, "/admin")
		return
	}
	v := a.baseView(id, b, "Staff login")
	v.Admin = true
	a.render(w, http.StatusOK, "staff_login", v)
}

func (a *App) staffLoginSubmit(w http.ResponseWriter, r *http.Request) {
	id, _, ok := a.postForm(w, r)
	if !ok {
		return
	}
	staffID := strings.TrimSpace(r.PostFormValue("staff_id"))
	if !a.staffLogin(staffID, r.PostFormValue("password")) {
		a.alert(id, "The staff ID or password is wrong.")
		redirect(w, r, "/admin/login")
		return
	}
	a.renewBrowser(w, id, func(nb *Browser) { nb.StaffID = staffID })
	redirect(w, r, "/admin")
}

func (a *App) staffLogout(w http.ResponseWriter, r *http.Request) {
	id, _, ok := a.postForm(w, r)
	if !ok {
		return
	}
	a.renewBrowser(w, id, func(nb *Browser) { nb.StaffID = "" })
	redirect(w, r, "/admin/login")
}

var sessionStates = []string{"active", "checking_out", "at_register", "payment_failed", "closed", "closed_unpaid"}

type sessionRow struct {
	Session
	Stay       time.Duration
	PendingKey bool
}

func (a *App) adminSessions(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	q := r.URL.Query()
	var states []string
	for _, s := range q["state"] {
		if slices.Contains(sessionStates, s) {
			states = append(states, s)
		}
	}
	v := a.adminView(r, id, b, "Visits", "visits")
	sessions, next, err := a.barpay.listSessions(ctx, b.StaffID, strings.Join(states, ","), q.Get("cursor"))
	data := struct {
		Rows       []sessionRow
		Next       string
		States     []string
		Selected   []string
		ReadFailed bool
	}{States: sessionStates, Selected: states, ReadFailed: err != nil}
	now := time.Now()
	a.store.view(func(d *Data) {
		for _, s := range sessions {
			end := now
			if s.TotalFixedAt != nil {
				end = *s.TotalFixedAt
			}
			_, pending := d.PendingKeys[chargeKeyName(chargeAction(s.State), s.SessionID, b.StaffID)]
			data.Rows = append(data.Rows, sessionRow{Session: s, Stay: end.Sub(s.CheckedInAt), PendingKey: pending})
		}
	})
	if next != nil {
		nq := url.Values{"cursor": {*next}}
		if len(states) > 0 {
			nq["state"] = states
		}
		data.Next = "/admin?" + nq.Encode()
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_sessions", v)
}

// chargeAction is the admin request that would charge a session in this state.
func chargeAction(state string) string {
	if state == "payment_failed" {
		return "retry"
	}
	return "close"
}

// chargeKeyName names the stored Idempotency-Key of a staff member's close or retry of a session.
func chargeKeyName(action, sessionID, staffID string) string {
	return action + ":" + sessionID + ":" + staffID
}

// parseAmount reads an amount staff typed, such as "12.50", in cents.
func parseAmount(s string) (int64, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > 2 {
		return 0, false
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 || w > 90_000_000_000_000 {
		return 0, false
	}
	var f int64
	if frac != "" {
		frac += strings.Repeat("0", 2-len(frac))
		if f, err = strconv.ParseInt(frac, 10, 64); err != nil || f < 0 {
			return 0, false
		}
	}
	return w*100 + f, true
}

func (a *App) adminSessionAction(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	sessionID, action := r.PathValue("session_id"), r.PathValue("action")
	amount := func() (Money, bool) {
		cents, ok := parseAmount(r.PostFormValue("amount"))
		return Money{Value: cents, Currency: r.PostFormValue("currency")}, ok
	}
	var s *Session
	var err error
	msg := ""
	switch action {
	case "close", "retry":
		s, msg, err = a.charge(r, action, sessionID, b.StaffID)
	case "register-payment", "settle-outside":
		m, ok := amount()
		if !ok {
			a.flash(id, "Enter the amount taken, such as 12.50.")
			redirect(w, r, "/admin")
			return
		}
		if action == "register-payment" {
			s, err = a.barpay.recordRegisterPayment(ctx, b.StaffID, sessionID, m)
		} else {
			s, err = a.barpay.settleOutside(ctx, b.StaffID, sessionID, m)
		}
		if err == nil {
			msg = fmt.Sprintf("Recorded %s for %s. The session is closed.", formatMoney(m), sessionID)
		} else if errors.Is(err, ErrNoAnswer) || errorCode(err) == "session_not_active" || errorCode(err) == "session_not_payment_failed" {
			msg = "BarPay did not record it. If you sent it before, the session may already be closed: check its state below."
		}
	case "mark-unpaid":
		s, err = a.barpay.markUnpaid(ctx, b.StaffID, sessionID)
		if err == nil {
			msg = "Marked " + sessionID + " as unpaid."
		}
	case "unlock":
		err = a.barpay.unlockGate(ctx, b.StaffID, sessionID)
		if err == nil {
			msg = "The gate is opening."
		} else if errorCode(err) == "gate_unavailable" {
			msg = "No exit gate is connected."
		}
	default:
		http.NotFound(w, r)
		return
	}
	if s != nil && err == nil {
		a.store.update(func(d *Data) error { d.remember(s); return nil })
	}
	if msg == "" && err != nil {
		msg = "BarPay refused: " + err.Error()
	}
	a.flash(id, msg)
	redirect(w, r, "/admin")
}

// charge sends a close or retry, which can charge the customer's PayPal. Its Idempotency-Key is
// stored before the call and kept until an answer arrives: pressing the button again after a lost
// answer sends the same key, which BarPay answers with the first response and never charges again
// for. Once an answer arrives the key is dropped, so a later retry is a new attempt.
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
		return nil, "BarPay did not answer. Press the button again: it sends the same request, which charges at most once.", err
	}
	a.store.update(func(d *Data) error { delete(d.PendingKeys, name); return nil })
	if err != nil {
		return nil, "", err
	}
	switch s.State {
	case "closed":
		return s, fmt.Sprintf("Charged %s to PayPal. %s is closed.", formatMoney(s.Total), sessionID), nil
	case "checking_out":
		return s, "The charge's outcome is not known yet. BarPay keeps trying, and the result arrives by notification.", nil
	default:
		return s, fmt.Sprintf("The charge did not go through: %s is %s.", sessionID, stateLabel(s.State)), nil
	}
}

func (a *App) adminUnlockGate(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	err := a.barpay.unlockGate(ctx, b.StaffID, "")
	switch {
	case err == nil:
		a.flash(id, "The gate is opening.")
	case errorCode(err) == "gate_unavailable":
		a.flash(id, "No exit gate is connected.")
	default:
		a.flash(id, "BarPay did not open the gate: "+err.Error())
	}
	redirect(w, r, "/admin")
}

// Pricing: the rate, evaluate now, settings and the decision log.

func (a *App) adminPricing(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	v := a.adminView(r, id, b, "Pricing", "pricing")
	data := struct {
		Rate      *CurrentRate
		Settings  *Settings
		Decisions []Decision
		Next      string
		Failed    bool
	}{}
	var err1, err2, err3 error
	data.Rate, err1 = a.barpay.getRate(ctx)
	data.Settings, err2 = a.barpay.getSettings(ctx, b.StaffID)
	var next *string
	data.Decisions, next, err3 = a.barpay.listDecisions(ctx, b.StaffID, r.URL.Query().Get("cursor"))
	data.Failed = err1 != nil || err2 != nil || err3 != nil
	if err1 != nil {
		data.Rate = nil
	}
	if err2 != nil {
		data.Settings = nil
	}
	if next != nil {
		data.Next = "/admin/pricing?" + url.Values{"cursor": {*next}}.Encode()
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_pricing", v)
}

func (a *App) adminEvaluate(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	e, err := a.barpay.requestRateEvaluation(ctx)
	switch {
	case err != nil:
		a.flash(id, "BarPay did not evaluate the rate: "+err.Error())
	case e.Fallback != nil:
		a.flash(id, "The AI gave no usable answer ("+*e.Fallback+"); the rate stays at "+formatMoney(e.Rate.RatePerHour)+"/h.")
	case !e.Evaluated:
		a.flash(id, "Too soon after the last evaluation; the rate stays at "+formatMoney(e.Rate.RatePerHour)+"/h.")
	default:
		a.flash(id, "Evaluated: "+formatMoney(e.Rate.RatePerHour)+"/h ("+formatSignedMoney(e.Rate.Change)+").")
	}
	redirect(w, r, "/admin/pricing")
}

func (a *App) adminUpdateSettings(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	currency := r.PostFormValue("currency")
	money := func(field string) (Money, bool) {
		c, ok := parseAmount(r.PostFormValue(field))
		return Money{Value: c, Currency: currency}, ok
	}
	integer := func(field string) (int, bool) {
		n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(field)))
		return n, err == nil
	}
	floor, ok1 := money("floor")
	ceiling, ok2 := money("ceiling")
	capPerVisit, ok3 := money("cap_per_visit")
	block, ok4 := integer("block_minutes")
	pause, ok5 := integer("pause_minutes")
	goal, ok6 := integer("goal")
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) {
		a.flash(id, "Every field needs a value: amounts such as 12.50, minutes and the goal as whole numbers.")
		redirect(w, r, "/admin/pricing")
		return
	}
	_, err := a.barpay.updateSettings(ctx, b.StaffID, Settings{
		Floor: floor, Ceiling: ceiling, BlockMinutes: block, PauseMinutes: pause, CapPerVisit: capPerVisit, Goal: goal,
	})
	if err != nil {
		a.flash(id, "The settings were not changed: "+err.Error())
	} else {
		a.flash(id, "The settings were saved.")
	}
	redirect(w, r, "/admin/pricing")
}

func (a *App) adminAlerts(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	alerts, next, err := a.barpay.listAlerts(ctx, b.StaffID, r.URL.Query().Get("cursor"))
	v := a.adminView(r, id, b, "Alerts", "alerts")
	data := struct {
		Alerts []Alert
		Next   string
		Failed bool
	}{Alerts: alerts, Failed: err != nil}
	if next != nil {
		data.Next = "/admin/alerts?" + url.Values{"cursor": {*next}}.Encode()
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_alerts", v)
}

func (a *App) adminNotifications(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	records, next, err := a.barpay.listNotifications(ctx, b.StaffID, r.URL.Query().Get("cursor"))
	v := a.adminView(r, id, b, "Log", "log")
	data := struct {
		Records  []NotificationRecord
		Next     string
		Failed   bool
		Received []ReceivedEvent
	}{Records: records, Failed: err != nil}
	if next != nil {
		data.Next = "/admin/notifications?" + url.Values{"cursor": {*next}}.Encode()
	}
	a.store.view(func(d *Data) {
		for i := len(d.Received) - 1; i >= 0 && len(data.Received) < 100; i-- {
			data.Received = append(data.Received, d.Received[i])
		}
	})
	v.Data = data
	a.render(w, http.StatusOK, "admin_notifications", v)
}
