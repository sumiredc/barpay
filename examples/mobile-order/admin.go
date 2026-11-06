package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
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
// side menu, and the dialog that unlocks the exit gate from any page.
type staffFrame struct {
	Menu []sideItem
	Gate *gateDialog
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
	{"orders", "/admin/orders", "utensils", "Orders"},
	{"alerts", "/admin/alerts", "bell", "Alerts"},
	{"pricing", "/admin/pricing", "trending-up", "Pricing"},
	{"log", "/admin/notifications", "history", "Log"},
}

// adminView is the view of an admin page for a logged-in staff member. nav is the key of the
// side menu item the page belongs to: the pricing tabs ("Current rate" and "Settings") both
// belong to "pricing".
func (a *App) adminView(r *http.Request, id string, b Browser, title, nav string) view {
	return a.adminViewWithAlerts(r, id, b, title, nav, nil)
}

// adminViewWithAlerts is adminView for a page that has read the first page of the alerts
// itself, which the gate dialog takes its customers from instead of reading it again.
func (a *App) adminViewWithAlerts(r *http.Request, id string, b Browser, title, nav string, firstAlerts *[]Alert) view {
	v := a.baseView(id, b, title)
	v.Admin = true
	v.Here = r.URL.RequestURI()
	f := &staffFrame{Gate: a.gateDialog(r, b.StaffID, firstAlerts)}
	for _, m := range staffMenu {
		item := sideItem{Href: m.href, Icon: m.icon, Label: m.label, Current: m.key == nav}
		if m.key == "orders" {
			item.Badge, item.BadgeLabel = a.ordersBadge()
		}
		f.Menu = append(f.Menu, item)
	}
	v.Frame = f
	return v
}

// gateDialog is the choice of whom staff unlock the exit gate for.
type gateDialog struct {
	Customers []gateCustomer
	// ReadFailed says the alerts could not be read, so no customer is offered.
	ReadFailed bool
}

type gateCustomer struct {
	SessionID, Label string
}

// gateLookupTimeout bounds what every admin page spends on the customers of the gate dialog, so
// a BarPay that is down or slow holds the page up no longer than this. Within it, each read is
// sent once.
const gateLookupTimeout = 3 * time.Second

// gateDialog offers the customers the gate failed to open for: the sessions of the unlock_failed
// alerts on the first page of the alerts, newest first, each once. An unlock_failed without a
// session is an unlock by hand that failed; staff open the gate again for no particular customer.
// firstAlerts is that page when the caller has read it; otherwise it is read here. When it cannot
// be read in time, the dialog offers no customer.
func (a *App) gateDialog(r *http.Request, staffID string, firstAlerts *[]Alert) *gateDialog {
	ctx, cancel := context.WithTimeout(r.Context(), gateLookupTimeout)
	defer cancel()
	var alerts []Alert
	if firstAlerts != nil {
		alerts = *firstAlerts
	} else {
		var err error
		if alerts, _, err = a.barpay.listAlertsOnce(ctx, staffID, ""); err != nil {
			return &gateDialog{ReadFailed: true}
		}
	}
	g := &gateDialog{}
	seen := map[string]bool{}
	for _, al := range alerts {
		if al.Kind != "unlock_failed" || al.SessionID == "" || seen[al.SessionID] {
			continue
		}
		seen[al.SessionID] = true
		g.Customers = append(g.Customers, gateCustomer{SessionID: al.SessionID, Label: a.gateLabel(ctx, al)})
	}
	return g
}

// gateLabel names a customer of the gate dialog by their name and how and when they paid, such
// as "Yui Tanaka (paid with PayPal at 21:59)". The session is read from BarPay when the app has
// not stored it ended; without it, the session ID tells customers of the same name apart.
func (a *App) gateLabel(ctx context.Context, al Alert) string {
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
		if read, err := a.barpay.getSessionOnce(ctx, al.SessionID); err == nil {
			s = read
		}
	}
	if memberID == "" && s != nil && s.MemberID != nil {
		memberID = *s.MemberID
	}
	name := "Guest"
	a.store.view(func(d *Data) {
		if m := d.Members[memberID]; m != nil {
			name = m.Name
		}
	})
	if s == nil || s.Receipt == nil {
		return name + " (" + al.SessionID + ")"
	}
	return name + " (" + paidHow(s.Receipt.Settlement) + " at " + s.Receipt.EndedAt.Local().Format("15:04") + ")"
}

func paidHow(settlement string) string {
	switch settlement {
	case "paypal":
		return "paid with PayPal"
	case "register":
		return "paid at the register"
	case "outside":
		return "settled by staff"
	case "unpaid":
		return "closed unpaid"
	}
	return settlement
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

// adminUnlockGate sends the unlock chosen in the gate dialog, for a customer's session or for
// none, and goes back to the page the dialog was opened on to tell the outcome. BarPay records
// the staff member and the session. It is sent once: a resend could open the gate twice.
func (a *App) adminUnlockGate(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	back := adminPath(r.PostFormValue("back"))
	sessionID := r.PostFormValue("for")
	switch sessionID {
	case "":
		a.alert(id, "Choose who the gate is unlocked for.")
		redirect(w, r, back)
		return
	case "none":
		sessionID = ""
	}
	err := a.barpay.unlockGate(ctx, b.StaffID, sessionID)
	switch {
	case err == nil:
		a.flash(id, "The exit gate was told to unlock once. Check at the gate that it opened.")
	case errorCode(err) == "gate_unavailable":
		a.alert(id, "The exit gate device isn't connected, so the gate wasn't unlocked, and it won't unlock later. Check the device's power and connection.")
	case errors.Is(err, ErrNoAnswer):
		a.alert(id, "BarPay didn't answer, so it isn't known whether the gate was told to unlock. Check at the gate before you try again.")
	default:
		a.alert(id, "BarPay didn't unlock the gate: "+err.Error())
	}
	redirect(w, r, back)
}

// adminPath returns s when it is the path of an admin page of this app, and /admin otherwise, so
// a form cannot send the browser to another site or to a customer page. The path is judged as
// the redirect would clean it ("/admin/../account" is /account), and a path with an escaped
// character in it ("%2e", "%2f") is refused rather than judged.
func adminPath(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.RawPath != "" ||
		strings.HasPrefix(s, "//") || !strings.HasPrefix(u.Path, "/") {
		return "/admin"
	}
	p := path.Clean(u.Path)
	if p != "/admin" && !strings.HasPrefix(p, "/admin/") {
		return "/admin"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// Pricing: the rate, evaluate now and the decision log.

func (a *App) adminPricing(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	v := a.adminView(r, id, b, "Pricing", "pricing")
	data := struct {
		Rate      *CurrentRate
		Decisions []Decision
		Next      string
		Failed    bool
	}{}
	var err1, err2 error
	data.Rate, err1 = a.barpay.getRate(ctx)
	var next *string
	data.Decisions, next, err2 = a.barpay.listDecisions(ctx, b.StaffID, r.URL.Query().Get("cursor"))
	data.Failed = err1 != nil || err2 != nil
	if err1 != nil {
		data.Rate = nil
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

// adminUpdateSettings saves the form of the "Settings" tab and goes back to it.
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
		redirect(w, r, "/admin/settings")
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
	redirect(w, r, "/admin/settings")
}

func (a *App) adminAlerts(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	cursor := r.URL.Query().Get("cursor")
	alerts, next, err := a.barpay.listAlerts(ctx, b.StaffID, cursor)
	var firstAlerts *[]Alert
	if cursor == "" && err == nil {
		firstAlerts = &alerts
	}
	v := a.adminViewWithAlerts(r, id, b, "Alerts", "alerts", firstAlerts)
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
