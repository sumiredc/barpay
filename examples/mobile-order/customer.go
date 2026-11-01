package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"rsc.io/qr"
)

// consentVersion names the consent text below; BarPay records it with the saved PayPal. Change
// both together.
const consentVersion = "walkout-v1"

const consentText = "When you link PayPal, the whole total of each visit (your orders and the cover charge) is charged to your PayPal when you leave through the exit gate, without asking you again. Linking itself charges nothing. You can delete your saved PayPal in Settings at any time when you have no unpaid visit."

type menuItem struct {
	ID    string
	Name  string
	Price int64 // cents
	Kind  string
}

var menuItems = []menuItem{
	{"beer", "Draft beer", 700, "Drinks"},
	{"highball", "Whisky highball", 800, "Drinks"},
	{"gin", "Gin and tonic", 900, "Drinks"},
	{"soda", "Ginger ale", 400, "Drinks"},
	{"fries", "French fries", 600, "Food"},
	{"edamame", "Edamame", 500, "Food"},
	{"karaage", "Fried chicken", 950, "Food"},
}

const menuCurrency = "USD"

var orderIDPattern = regexp.MustCompile(`^ord_[a-z0-9]{20}$`)

func (a *App) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), time.Minute)
}

// member returns a copy of the member logged in to b, or nil.
func (a *App) member(b Browser) *MemberAccount {
	if b.MemberID == "" {
		return nil
	}
	var m *MemberAccount
	a.store.view(func(d *Data) {
		if v := d.Members[b.MemberID]; v != nil {
			cp := *v
			m = &cp
		}
	})
	return m
}

// baseView fills what every customer page shows, and takes the flash message once.
func (a *App) baseView(id string, b Browser, title string) view {
	if b.Flash != "" {
		a.updateBrowser(id, func(b *Browser) { b.Flash = "" })
	}
	return view{Title: title, CSRF: b.CSRF, Member: a.member(b), Guest: b.Guest, StaffID: b.StaffID, Flash: b.Flash}
}

func (a *App) flash(id, msg string) {
	a.updateBrowser(id, func(b *Browser) { b.Flash = msg })
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// postForm returns the browser session of a form post, or false after answering a post whose
// CSRF token does not belong to it.
func (a *App) postForm(w http.ResponseWriter, r *http.Request) (string, Browser, bool) {
	id, b := a.browser(w, r)
	if err := r.ParseForm(); err != nil || !checkCSRF(r, b) {
		http.Error(w, "This form has expired. Go back, reload the page and try again.", http.StatusForbidden)
		return "", b, false
	}
	return id, b, true
}

func (a *App) fail(w http.ResponseWriter, id string, b Browser, msg string) {
	v := a.baseView(id, b, "Something went wrong")
	v.Data = msg
	a.render(w, http.StatusBadGateway, "error", v)
}

// Entrance landing, sign-up, login and guest join.

func (a *App) landing(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	ctx, cancel := a.ctx(r)
	defer cancel()
	data := struct {
		Rate    *RatePoint
		InVisit bool
		// EndedGuestVisit is a guest visit that ended, whose receipt is seen only until Done, also
		// by a guest who signed up during it. A member's ended visit needs nothing here: its
		// receipt stays in Receipts and Check in starts a new one.
		EndedGuestVisit string
	}{InVisit: b.SessionID != ""}
	if b.SessionID != "" {
		// A visit read as ended is over; one that could not be read is taken as still open.
		s, err := a.barpay.getSession(ctx, b.SessionID)
		switch {
		case errorCode(err) == "session_not_found":
			// BarPay has no such visit, so there is no receipt to keep: forget it as Done does.
			a.updateBrowser(id, func(b *Browser) { b.SessionID, b.Guest = "", false })
			b.SessionID, b.Guest = "", false
			data.InVisit = false
		case err == nil && s.ended():
			a.store.update(func(d *Data) error { d.remember(s); return nil })
			data.InVisit = false
			if s.MemberID == nil {
				data.EndedGuestVisit = s.SessionID
			}
		}
	}
	v := a.baseView(id, b, "Welcome")
	if rate, err := a.barpay.getRate(ctx); err == nil {
		data.Rate = rate
	}
	v.Data = data
	a.render(w, http.StatusOK, "landing", v)
}

func (a *App) signupForm(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	a.render(w, http.StatusOK, "signup", a.baseView(id, b, "Sign up"))
}

func (a *App) signup(w http.ResponseWriter, r *http.Request) {
	id, _, ok := a.postForm(w, r)
	if !ok {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	name := strings.TrimSpace(r.PostFormValue("name"))
	password := r.PostFormValue("password")
	switch {
	case func() bool { _, err := mail.ParseAddress(email); return err != nil }():
		a.flash(id, "Enter a valid email address.")
	case name == "" || len(name) > 60:
		a.flash(id, "Enter your name (up to 60 characters).")
	case len(password) < 8:
		a.flash(id, "Choose a password of at least 8 characters.")
	default:
		hash := hashPassword(password)
		m := &MemberAccount{ID: randomID("m_"), Email: email, Name: name, PasswordHash: hash, CreatedAt: time.Now()}
		taken := false
		a.store.update(func(d *Data) error {
			for _, other := range d.Members {
				if other.Email == email {
					taken = true
					return nil
				}
			}
			d.Members[m.ID] = m
			return nil
		})
		if taken {
			a.flash(id, "An account with this email already exists. Log in instead.")
			redirect(w, r, "/login")
			return
		}
		// A guest who signs up during a visit keeps it: it stays a guest visit, paid at the
		// register, and the account is used from the next check-in.
		a.renewBrowser(w, id, func(nb *Browser) {
			if !nb.Guest {
				nb.SessionID = ""
			}
			nb.MemberID, nb.Guest, nb.StaffID = m.ID, false, ""
			nb.Flash = "Welcome, " + name + ". Your account is ready."
		})
		redirect(w, r, "/")
		return
	}
	redirect(w, r, "/signup")
}

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	a.render(w, http.StatusOK, "login", a.baseView(id, b, "Log in"))
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	id, _, ok := a.postForm(w, r)
	if !ok {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	password := r.PostFormValue("password")
	var m *MemberAccount
	a.store.view(func(d *Data) {
		for _, v := range d.Members {
			if v.Email == email {
				cp := *v
				m = &cp
			}
		}
	})
	if m == nil {
		checkPassword(a.dummyHash, password)
		a.flash(id, "The email or password is wrong.")
		redirect(w, r, "/login")
		return
	}
	if !checkPassword(m.PasswordHash, password) {
		a.flash(id, "The email or password is wrong.")
		redirect(w, r, "/login")
		return
	}
	a.renewBrowser(w, id, func(nb *Browser) {
		// The member's own open visit wins; otherwise a guest visit open in this browser stays.
		if m.OpenSessionID != "" || !nb.Guest {
			nb.SessionID = m.OpenSessionID
		}
		nb.MemberID, nb.Guest, nb.StaffID = m.ID, false, ""
		nb.LinkState, nb.LinkMemberID = "", ""
	})
	redirect(w, r, "/")
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	id, _, ok := a.postForm(w, r)
	if !ok {
		return
	}
	a.renewBrowser(w, id, func(nb *Browser) {
		nb.MemberID, nb.Guest, nb.SessionID = "", false, ""
		nb.LinkState, nb.LinkMemberID, nb.PendingOrder = "", "", nil
	})
	redirect(w, r, "/")
}

func (a *App) joinAsGuest(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.MemberID != "" {
		redirect(w, r, "/")
		return
	}
	if !b.Guest {
		a.updateBrowser(id, func(b *Browser) { b.Guest = true })
		b.Guest = true
	}
	a.startVisit(w, r, id, b)
}

func (a *App) checkin(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.MemberID == "" && !b.Guest {
		redirect(w, r, "/")
		return
	}
	a.startVisit(w, r, id, b)
}

// startVisit checks the customer in, unless their visit is still open. The Idempotency-Key is
// stored before the call, so trying again after a lost answer returns the same session.
func (a *App) startVisit(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	open := b.SessionID
	if open == "" && b.MemberID != "" {
		if m := a.member(b); m != nil {
			open = m.OpenSessionID
		}
	}
	if open != "" {
		s, err := a.barpay.getSession(ctx, open)
		if err == nil && !s.ended() {
			a.store.update(func(d *Data) error { d.remember(s); return nil })
			a.updateBrowser(id, func(b *Browser) { b.SessionID = s.SessionID })
			redirect(w, r, "/menu")
			return
		}
	}

	keyName := "checkin:" + id
	var key string
	a.store.update(func(d *Data) error {
		if key = d.PendingKeys[keyName]; key == "" {
			key = randomToken(24)
			d.PendingKeys[keyName] = key
		}
		return nil
	})
	s, err := a.barpay.createCheckin(ctx, key, b.MemberID)
	if errors.Is(err, ErrNoAnswer) {
		a.fail(w, id, b, "We could not reach the payment service to check you in. Press Check in again: you will not be checked in twice.")
		return
	}
	a.store.update(func(d *Data) error { delete(d.PendingKeys, keyName); return nil })
	if err != nil {
		msg := "We could not check you in. Please see staff."
		if errorCode(err) == "member_has_open_session" {
			msg = "You already have an open visit on another device. Please see staff."
		}
		a.fail(w, id, b, msg)
		return
	}
	a.store.update(func(d *Data) error {
		d.remember(s)
		if m := d.Members[b.MemberID]; m != nil {
			m.OpenSessionID = s.SessionID
			m.SessionIDs = append(m.SessionIDs, s.SessionID)
		}
		return nil
	})
	a.updateBrowser(id, func(b *Browser) {
		b.SessionID = s.SessionID
		b.Flash = "You are checked in. Enjoy your stay."
	})
	redirect(w, r, "/menu")
}

// visitSession returns the customer's current session read from BarPay, or false after sending
// them to the entrance when they have none.
func (a *App) visitSession(w http.ResponseWriter, r *http.Request, id string, b Browser) (*Session, bool) {
	if b.SessionID == "" {
		redirect(w, r, "/")
		return nil, false
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	s, err := a.barpay.getSession(ctx, b.SessionID)
	if err != nil {
		a.fail(w, id, b, "We could not read your visit right now. Try again in a moment.")
		return nil, false
	}
	a.store.update(func(d *Data) error { d.remember(s); return nil })
	return s, true
}

// Menu and ordering.

func (a *App) menu(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	s, ok := a.visitSession(w, r, id, b)
	if !ok {
		return
	}
	v := a.baseView(id, b, "Menu")
	// The order_id is minted with the form, so submitting the same form again, or twice at once,
	// reports the same order, which BarPay records once.
	v.Data = struct {
		Items   []menuItem
		Session *Session
		Pending *PendingOrder
		OrderID string
	}{menuItems, s, b.PendingOrder, randomID("ord_")}
	a.render(w, http.StatusOK, "menu", v)
}

func (a *App) placeOrder(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.SessionID == "" {
		redirect(w, r, "/")
		return
	}
	if b.PendingOrder != nil {
		redirect(w, r, "/menu")
		return
	}
	orderID := r.PostFormValue("order_id")
	if !orderIDPattern.MatchString(orderID) {
		a.flash(id, "This form has expired. Choose your items again.")
		redirect(w, r, "/menu")
		return
	}
	var lines []OrderLine
	for _, item := range menuItems {
		q, err := strconv.Atoi(r.PostFormValue("qty_" + item.ID))
		if err != nil || q <= 0 {
			continue
		}
		lines = append(lines, OrderLine{Name: item.Name, Quantity: min(q, 99), UnitPrice: Money{item.Price, menuCurrency}})
	}
	if len(lines) == 0 {
		a.flash(id, "Choose at least one item.")
		redirect(w, r, "/menu")
		return
	}
	order := &PendingOrder{OrderID: orderID, Lines: lines}
	a.updateBrowser(id, func(b *Browser) { b.PendingOrder = order })
	a.sendOrder(w, r, id, b.SessionID, order)
}

func (a *App) resendOrder(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.SessionID == "" || b.PendingOrder == nil {
		redirect(w, r, "/menu")
		return
	}
	a.sendOrder(w, r, id, b.SessionID, b.PendingOrder)
}

// sendOrder reports an order. The order_id was stored before the call, so sending it again after
// a lost answer records it once.
func (a *App) sendOrder(w http.ResponseWriter, r *http.Request, id, sessionID string, order *PendingOrder) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	recorded, err := a.barpay.reportOrder(ctx, sessionID, order.OrderID, order.Lines)
	if errors.Is(err, ErrNoAnswer) {
		a.flash(id, "We could not confirm your order. Send it again: it will not be ordered twice.")
		redirect(w, r, "/menu")
		return
	}
	msg := "Your order is on its way."
	switch code := errorCode(err); {
	case err == nil:
		a.store.update(func(d *Data) error {
			v := d.visit(sessionID)
			for _, o := range v.Orders {
				if o.OrderID == recorded.OrderID {
					return nil
				}
			}
			v.Orders = append(v.Orders, *recorded)
			return nil
		})
	case code == "order_conflict":
		// The same form was sent again with other quantities: the order it placed stands.
		msg = "This order was already placed."
		err = nil
	case code == "session_not_active":
		msg = "This visit can take no more orders. Please see staff."
	default:
		msg = "We could not place your order. Please see staff."
	}
	a.updateBrowser(id, func(b *Browser) {
		b.PendingOrder = nil
		b.Flash = msg
	})
	if err != nil {
		redirect(w, r, "/menu")
		return
	}
	redirect(w, r, "/panel")
}

// The panel.

func (a *App) panel(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	s, ok := a.visitSession(w, r, id, b)
	if !ok {
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	end := time.Now()
	if s.TotalFixedAt != nil {
		end = *s.TotalFixedAt
	}
	history, err := a.barpay.getRateHistory(ctx, s.CheckedInAt)
	if err != nil {
		history = nil
	}
	var change *Money
	if n := len(history); n > 0 {
		change = &history[n-1].Change
	}
	var orders []Order
	a.store.view(func(d *Data) {
		if v := d.Visits[s.SessionID]; v != nil {
			orders = append(orders, v.Orders...)
		}
	})
	v := a.baseView(id, b, "Your visit")
	if !s.ended() {
		v.Refresh = 15
	}
	v.Data = struct {
		Session *Session
		Chart   chart
		Change  *Money
		Stayed  time.Duration
		Orders  []Order
	}{s, rateChart(history, s.CheckedInAt, end), change, end.Sub(s.CheckedInAt), orders}
	a.render(w, http.StatusOK, "panel", v)
}

func (a *App) payChoice(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	s, ok := a.visitSession(w, r, id, b)
	if !ok {
		return
	}
	if s.State != "active" {
		redirect(w, r, "/panel")
		return
	}
	v := a.baseView(id, b, "Pay")
	v.Data = s
	a.render(w, http.StatusOK, "pay", v)
}

func (a *App) payAtRegister(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.SessionID == "" {
		redirect(w, r, "/")
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	s, err := a.barpay.requestRegister(ctx, b.SessionID)
	if err != nil {
		a.fail(w, id, b, "We could not send you to the register. Please see staff.")
		return
	}
	a.store.update(func(d *Data) error { d.remember(s); return nil })
	a.flash(id, "Please pay "+formatMoney(s.Total)+" at the register.")
	redirect(w, r, "/panel")
}

// exitQR shows the session's exit token as a QR, only to the browser session the visit belongs
// to, and reloads when the token changes.
func (a *App) exitQR(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	s, ok := a.visitSession(w, r, id, b)
	if !ok {
		return
	}
	v := a.baseView(id, b, "Exit")
	data := struct {
		Session   *Session
		QR        template.URL
		ExpiresAt time.Time
		Message   string
	}{Session: s}
	switch {
	case s.State == "checking_out":
		data.Message = "Paying with PayPal…"
		v.Refresh = 3
	case s.State == "payment_failed":
		data.Message = "The payment did not go through. Please see staff."
	case s.State == "closed" && r.URL.Query().Get("again") == "":
		redirect(w, r, "/receipts/"+s.SessionID)
		return
	case s.State == "at_register" || s.State == "closed_unpaid":
		redirect(w, r, "/panel")
		return
	default:
		ctx, cancel := a.ctx(r)
		defer cancel()
		t, err := a.barpay.getExitToken(ctx, s.SessionID)
		if err != nil {
			if errorCode(err) == "exit_token_unavailable" {
				data.Message = "The exit QR is not available for this visit. Please pay at the register or see staff."
			} else {
				data.Message = "We could not get your exit QR. Try again in a moment."
				v.Refresh = 5
			}
			break
		}
		code, err := qr.Encode(t.Token, qr.M)
		if err != nil {
			data.Message = "We could not draw your exit QR. Please see staff."
			break
		}
		code.Scale = 8
		// A data URI the app made from its own PNG, which html/template would otherwise refuse.
		data.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
		data.ExpiresAt = t.ExpiresAt
		v.Refresh = max(1, min(30, int(time.Until(t.ExpiresAt).Seconds())+1))
	}
	v.Data = data
	a.render(w, http.StatusOK, "exit", v)
}

// Receipts.

func (a *App) ownsVisit(b Browser, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(b.SessionID), []byte(sessionID)) == 1 {
		return true
	}
	owned := false
	a.store.view(func(d *Data) {
		if v := d.Visits[sessionID]; v != nil && b.MemberID != "" && v.MemberID == b.MemberID {
			owned = true
		}
	})
	return owned
}

func (a *App) receipts(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	if b.MemberID == "" && !b.Guest {
		redirect(w, r, "/")
		return
	}
	var visits []Visit
	a.store.view(func(d *Data) {
		var ids []string
		if m := d.Members[b.MemberID]; m != nil {
			ids = m.SessionIDs
		} else if b.SessionID != "" {
			ids = []string{b.SessionID}
		}
		for i := len(ids) - 1; i >= 0; i-- {
			if v := d.Visits[ids[i]]; v != nil && v.Session != nil {
				visits = append(visits, *v)
			}
		}
	})
	v := a.baseView(id, b, "Receipts")
	v.Data = visits
	a.render(w, http.StatusOK, "receipts", v)
}

func (a *App) receipt(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	sessionID := r.PathValue("session_id")
	if !a.ownsVisit(b, sessionID) {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	s, err := a.barpay.getSession(ctx, sessionID)
	if err != nil {
		a.fail(w, id, b, "We could not read this receipt right now. Try again in a moment.")
		return
	}
	var orders []Order
	a.store.update(func(d *Data) error {
		d.remember(s)
		orders = append(orders, d.Visits[sessionID].Orders...)
		return nil
	})
	v := a.baseView(id, b, "Receipt")
	if !s.ended() {
		v.Refresh = 10
	}
	v.Data = struct {
		Session *Session
		Orders  []Order
		Current bool
	}{s, orders, b.SessionID == sessionID}
	a.render(w, http.StatusOK, "receipt", v)
}

// visitDone forgets the browser's ended visit, so the customer can check in again.
func (a *App) visitDone(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	if b.SessionID != "" {
		if s, err := a.barpay.getSession(ctx, b.SessionID); err == nil && s.ended() {
			a.store.update(func(d *Data) error { d.remember(s); return nil })
			a.updateBrowser(id, func(b *Browser) {
				b.SessionID = ""
				if b.Guest {
					b.Guest = false
				}
			})
		}
	}
	redirect(w, r, "/")
}

// Settings: consent and the PayPal link, and deleting the saved PayPal.

func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	if b.MemberID == "" && !b.Guest {
		redirect(w, r, "/login")
		return
	}
	v := a.baseView(id, b, "Settings")
	data := struct {
		Member         *Member
		ConsentText    string
		ConsentVersion string
		Notice         string
		ReadFailed     bool
	}{ConsentText: consentText, ConsentVersion: consentVersion}
	if b.MemberID != "" {
		ctx, cancel := a.ctx(r)
		defer cancel()
		m, err := a.barpay.getMember(ctx, b.MemberID)
		if err != nil {
			data.ReadFailed = true
		} else {
			data.Member = m
		}
		a.store.update(func(d *Data) error {
			data.Notice = d.MemberNotices[b.MemberID]
			delete(d.MemberNotices, b.MemberID)
			return nil
		})
	}
	v.Data = data
	a.render(w, http.StatusOK, "settings", v)
}

func (a *App) linkPaypal(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.MemberID == "" {
		a.flash(id, "Linking PayPal needs an account. Sign up or log in first.")
		redirect(w, r, "/settings")
		return
	}
	if r.PostFormValue("agree") != "yes" {
		a.flash(id, "To link PayPal, agree to the terms above.")
		redirect(w, r, "/settings")
		return
	}
	// state ties the return from PayPal to this browser session: a return to any other browser
	// carries a state it does not hold, and is not confirmed.
	state := randomToken(32)
	a.updateBrowser(id, func(b *Browser) {
		b.LinkState, b.LinkMemberID, b.LinkExpiresAt = state, b.MemberID, time.Now().Add(40*time.Minute)
	})
	ctx, cancel := a.ctx(r)
	defer cancel()
	link, err := a.barpay.createPaypalLink(ctx, b.MemberID, state, consentVersion, a.cfg.PublicURL.String()+"/paypal/return")
	if err != nil {
		a.updateBrowser(id, func(b *Browser) { b.LinkState, b.LinkMemberID = "", "" })
		switch errorCode(err) {
		case "saved_paypal_exists":
			a.flash(id, "You already have a saved PayPal, or its deletion is still in progress.")
		default:
			a.flash(id, "We could not start linking PayPal. Try again later.")
		}
		redirect(w, r, "/settings")
		return
	}
	// Opened as a full page: PayPal approval is not expected to work inside a frame.
	redirect(w, r, link.LinkURL)
}

// paypalReturn is where BarPay sends the member's browser back from PayPal. It confirms the link
// only when the return carries this browser session's state and the member logged in here is the
// one who asked, and always answers with a redirect to a clean URL, so the code leaves the
// address bar.
func (a *App) paypalReturn(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	q := r.URL.Query()
	result, state, code := q.Get("paypal_link"), q.Get("state"), q.Get("code")

	matches := b.LinkState != "" && state != "" &&
		subtle.ConstantTimeCompare([]byte(state), []byte(b.LinkState)) == 1 &&
		time.Now().Before(b.LinkExpiresAt) && b.MemberID != "" && b.MemberID == b.LinkMemberID
	if !matches {
		a.flash(id, "This PayPal link was started in another browser, or has expired. Nothing was saved. Log in here and link PayPal again from this browser.")
		redirect(w, r, "/settings")
		return
	}
	a.updateBrowser(id, func(b *Browser) { b.LinkState, b.LinkMemberID = "", "" })

	msg := ""
	switch result {
	case "approved":
		ctx, cancel := a.ctx(r)
		defer cancel()
		_, err := a.barpay.confirmPaypalLink(ctx, b.MemberID, code)
		switch errorCode(err) {
		case "":
			if err == nil {
				msg = "PayPal is linked. From now on you can pay by walking out through the exit gate."
				a.store.update(func(d *Data) error {
					d.toldByAnswer(b.MemberID, "member.paypal_linked", paypalLinkedNotice)
					return nil
				})
			} else {
				msg = "We could not confirm the link. Check below whether PayPal is linked, and link again if it is not."
			}
		case "link_code_invalid":
			msg = "This link has expired or was already used. Nothing was saved. You can link PayPal again."
		case "saved_paypal_exists":
			msg = "You already have a saved PayPal."
		case "paypal_account_already_linked":
			msg = "This PayPal account is already linked to another member. Nothing was saved. Link another PayPal account or pay at the register."
		default:
			msg = "PayPal did not save it. Nothing was saved. You can link PayPal again."
		}
	case "cancelled":
		msg = "You cancelled in PayPal. Nothing was saved. You can link later or pay at the register."
	case "expired":
		msg = "The link expired before you opened it. Nothing was saved. You can link PayPal again."
	default:
		msg = "PayPal did not approve. Nothing was saved. You can try again."
	}
	a.flash(id, msg)
	redirect(w, r, "/settings")
}

func (a *App) deletePaypal(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.MemberID == "" {
		redirect(w, r, "/settings")
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	confirmed, err := a.barpay.deleteSavedPaypal(ctx, b.MemberID)
	var msg string
	switch {
	case err == nil && confirmed:
		msg = savedPaypalDeletedNotice
		a.store.update(func(d *Data) error {
			d.toldByAnswer(b.MemberID, "member.saved_paypal_deleted", savedPaypalDeletedNotice)
			return nil
		})
	case err == nil:
		msg = "Deleting your saved PayPal is in progress. It shows here once PayPal confirms."
	case errorCode(err) == "saved_paypal_in_use":
		msg = "Your saved PayPal is in use by your current visit. You can delete it after you pay."
	case errorCode(err) == "saved_paypal_not_found":
		msg = "You have no saved PayPal."
	default:
		msg = "We could not delete your saved PayPal. Try again later."
	}
	a.flash(id, msg)
	redirect(w, r, "/settings")
}
