package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"rsc.io/qr"
)

// consentVersion names the consent text below; BarPay records it with the saved PayPal. Change
// both together.
const consentVersion = "walkout-v1"

const consentText = "When you link PayPal, the whole total of each visit (your orders and the cover charge) is charged to your PayPal when you leave through the exit gate, without asking you again. Linking itself charges nothing. You can delete your saved PayPal in Settings at any time when you have no unpaid visit."

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

// baseView fills what every customer page shows, and takes the flash, alert and toast messages
// once.
func (a *App) baseView(id string, b Browser, title string) view {
	if b.Flash != "" || b.Alert != "" || b.Toast != "" {
		a.updateBrowser(id, func(b *Browser) { b.Flash, b.Alert, b.Toast = "", "", "" })
	}
	return view{Title: title, CSRF: b.CSRF, Member: a.member(b), Guest: b.Guest, StaffID: b.StaffID, Flash: b.Flash, Alert: b.Alert, Toast: b.Toast}
}

func (a *App) flash(id, msg string) {
	a.updateBrowser(id, func(b *Browser) { b.Flash = msg })
}

// alert keeps an error for the next page, customer or admin, which shows it in red at the top.
func (a *App) alert(id, msg string) {
	a.updateBrowser(id, func(b *Browser) { b.Alert = msg })
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

// failure is what the error page shows: what went wrong, what to do now, and the one button at
// the bottom that leads on.
type failure struct {
	Title, Text string
	Button      string
	// Href is where Button goes. With Post, Button sends the form there instead, as trying again.
	Href string
	Post bool
}

func (a *App) fail(w http.ResponseWriter, id string, b Browser, f failure) {
	v := a.baseView(id, b, f.Title)
	v.Data = f
	a.render(w, http.StatusBadGateway, "error", v)
}

// couldNotLoad is the failure of a page that could not read BarPay: Try again opens it again.
func couldNotLoad(r *http.Request) failure {
	return failure{Title: "Couldn't load", Button: "Try again", Href: r.URL.RequestURI()}
}

// Entrance landing, sign-up, login and guest join.

func (a *App) landing(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	ctx, cancel := a.ctx(r)
	defer cancel()
	data := landingData{InVisit: b.SessionID != ""}
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
	// Before login and before a member's check-in the entrance is the start, so it has no tabs.
	v.Tabbar = data.InVisit || data.EndedGuestVisit != ""
	if !v.Tabbar {
		// The rate is shown before entering only. One that cannot be read is left out, and the
		// customer can still enter: the bill shows it later.
		if rate, err := a.barpay.getRate(ctx); err == nil {
			data.Rate = rate
		}
		if v.Member != nil {
			// A saved PayPal that cannot be read is not said to be missing.
			m, err := a.barpay.getMember(ctx, v.Member.ID)
			data.PaypalUnlinked = err == nil && m.SavedPaypal == nil
		}
	}
	v.Data = data
	a.render(w, http.StatusOK, "landing", v)
}

type landingData struct {
	// Rate is nil when BarPay's rate could not be read.
	Rate    *CurrentRate
	InVisit bool
	// EndedGuestVisit is a guest visit that ended, whose receipt is seen only until Done, also
	// by a guest who signed up during it. A member's ended visit needs nothing here: its
	// receipt stays in Receipts and Check in starts a new one.
	EndedGuestVisit string
	PaypalUnlinked  bool
}

// signupInput is what the sign-up page shows: what was entered, but the password, and the error
// under each field.
type signupInput struct {
	Name, Email                          string
	NameError, EmailError, PasswordError string
	// KeepsVisit says the guest visit open in this browser will be offered to the new account.
	KeepsVisit bool
}

func (a *App) signupForm(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	v := a.baseView(id, b, "Sign up")
	v.Data = signupInput{KeepsVisit: a.guestVisitToOffer(b) != ""}
	a.render(w, http.StatusOK, "signup", v)
}

func (a *App) signup(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	in := signupInput{Name: strings.TrimSpace(r.PostFormValue("name")), Email: strings.TrimSpace(r.PostFormValue("email")),
		KeepsVisit: a.guestVisitToOffer(b) != ""}
	email := strings.ToLower(in.Email)
	password := r.PostFormValue("password")
	if !validName(in.Name) {
		in.NameError = "Enter your name (up to 60 characters)"
	}
	if _, err := mail.ParseAddress(email); err != nil {
		in.EmailError = "Enter a valid email address"
	}
	if !strongPassword(password) {
		in.PasswordError = "Use 8+ characters with upper and lower case letters, a number and a symbol"
	}
	if in.NameError != "" || in.EmailError != "" || in.PasswordError != "" {
		// Drawn again here, so the customer corrects the field instead of typing it all again.
		v := a.baseView(id, b, "Sign up")
		v.Data = in
		a.render(w, http.StatusOK, "signup", v)
		return
	}
	hash := hashPassword(password)
	m := &MemberAccount{ID: randomID("m_"), Email: email, Name: in.Name, PasswordHash: hash, CreatedAt: time.Now()}
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
		a.updateBrowser(id, func(b *Browser) {
			b.Flash = "This email is already registered. Please log in"
			b.LoginEmail = in.Email
		})
		redirect(w, r, "/login")
		return
	}
	// A guest who signs up during a visit keeps it in this browser, and is asked whether it moves
	// to the new account.
	offered := a.guestVisitToOffer(b)
	a.renewBrowser(w, id, func(nb *Browser) {
		if !nb.Guest {
			nb.SessionID = ""
		}
		nb.MemberID, nb.Guest, nb.StaffID = m.ID, false, ""
		nb.Takeover = takeoverOf(offered, m.ID)
		nb.Toast = "Welcome, " + in.Name + ". Your account is ready"
	})
	redirect(w, r, afterLogin(offered))
}

// takeoverOf is the takeover a sign-up or login of memberID starts for the guest visit offered,
// or nil when none was.
func takeoverOf(offered, memberID string) *Takeover {
	if offered == "" {
		return nil
	}
	return &Takeover{SessionID: offered, MemberID: memberID}
}

// afterLogin is where a sign-up or login leads: the takeover when it offered a guest visit.
func afterLogin(offered string) string {
	if offered != "" {
		return takeoverPage
	}
	return "/"
}

// strongPassword tells a password of 8 or more characters with a lower case letter, an upper
// case letter, a digit and a symbol (any other character), as the sign-up page asks.
func strongPassword(p string) bool {
	var lower, upper, digit, symbol bool
	for _, c := range p {
		switch {
		case 'a' <= c && c <= 'z':
			lower = true
		case 'A' <= c && c <= 'Z':
			upper = true
		case '0' <= c && c <= '9':
			digit = true
		default:
			symbol = true
		}
	}
	return utf8.RuneCountInString(p) >= 8 && lower && upper && digit && symbol
}

// loginInput is the email the login page fills in: the one entered, or the one a sign-up found
// already registered.
type loginInput struct {
	Email string
	// KeepsVisit says the guest visit open in this browser will be offered to the member.
	KeepsVisit bool
}

// validName says whether a member's name, already trimmed, can be kept: not empty and at most 60
// characters, counted as characters rather than bytes, so a name in Japanese gets as many. A
// control or format character (a newline, a zero-width space, a bidi override) is refused, as it
// would change how the name and the text around it are drawn wherever the name is shown.
func validName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 60 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	if b.LoginEmail != "" {
		a.updateBrowser(id, func(b *Browser) { b.LoginEmail = "" })
	}
	v := a.baseView(id, b, "Log in")
	v.Data = loginInput{Email: b.LoginEmail, KeepsVisit: a.guestVisitToOffer(b) != ""}
	a.render(w, http.StatusOK, "login", v)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	entered := strings.TrimSpace(r.PostFormValue("email"))
	email := strings.ToLower(entered)
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
	// An unknown email is checked against a dummy hash too, so it takes as long as a wrong
	// password.
	hash := a.dummyHash
	if m != nil {
		hash = m.PasswordHash
	}
	if !checkPassword(hash, password) || m == nil {
		// Drawn again here with the email kept, so only the password is typed again.
		v := a.baseView(id, b, "Log in")
		v.Alert = "Wrong email or password"
		v.Data = loginInput{Email: entered, KeepsVisit: a.guestVisitToOffer(b) != ""}
		a.render(w, http.StatusOK, "login", v)
		return
	}
	// A guest visit open in this browser stays, to be paid here, and is offered to the member,
	// unless the member is in another visit, which BarPay would refuse to attach them beside.
	// Otherwise the member's own open visit wins over a guest visit that ended here.
	offered := a.guestVisitToOffer(b)
	refused := ""
	if offered != "" {
		ctx, cancel := a.ctx(r)
		defer cancel()
		if a.inOtherVisit(ctx, m, offered) {
			offered, refused = "", takeoverRefused("member_has_open_session")
		}
	}
	a.renewBrowser(w, id, func(nb *Browser) {
		if offered == "" && refused == "" && (m.OpenSessionID != "" || !nb.Guest) {
			nb.SessionID = m.OpenSessionID
		}
		nb.MemberID, nb.Guest, nb.StaffID = m.ID, false, ""
		nb.LinkState, nb.LinkMemberID = "", ""
		nb.Takeover = takeoverOf(offered, m.ID)
		if refused != "" {
			nb.Alert = refused
		}
	})
	if refused != "" {
		redirect(w, r, "/account")
		return
	}
	redirect(w, r, afterLogin(offered))
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	id, _, ok := a.postForm(w, r)
	if !ok {
		return
	}
	a.renewBrowser(w, id, func(nb *Browser) {
		nb.MemberID, nb.Guest, nb.SessionID = "", false, ""
		nb.LinkState, nb.LinkMemberID, nb.PendingOrder, nb.Takeover = "", "", nil, nil
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
		a.fail(w, id, b, failure{Title: "Couldn't connect", Text: "Trying again won't check you in twice", Button: "Check in again", Href: "/checkin", Post: true})
		return
	}
	a.store.update(func(d *Data) error { delete(d.PendingKeys, keyName); return nil })
	if err != nil {
		f := failure{Title: "Couldn't check in", Text: "Please ask a staff member", Button: "Back", Href: "/"}
		if errorCode(err) == "member_has_open_session" {
			f.Title = "You're checked in on another device"
		}
		a.fail(w, id, b, f)
		return
	}
	a.store.update(func(d *Data) error { d.remember(s); return nil })
	a.updateBrowser(id, func(b *Browser) {
		b.SessionID = s.SessionID
		b.Toast = "You're checked in"
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
		a.fail(w, id, b, couldNotLoad(r))
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
	items := map[string]menuItem{}
	for _, item := range menuItems {
		// The script reads the options of every item as a list, also of one without options.
		if item.Options == nil {
			item.Options = []menuOption{}
		}
		items[item.ID] = item
	}
	// The cart is kept by the page's script, which draws the item sheet from the menu.
	itemsJSON, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	// The order_id is minted with the form, so submitting the same form again, or twice at once,
	// reports the same order, which BarPay records once.
	v.Data = struct {
		Sections   []menuSection
		ItemsJSON  string
		Session    *Session
		StateLabel string
		Pending    *PendingOrder
		OrderID    string
	}{menuSections(), string(itemsJSON), s, customerStateLabel(s.State), b.PendingOrder, randomID("ord_")}
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
	// A form from before the menu changed is chosen again, as an expired one is.
	lines, known := orderLines(r.PostForm["line"])
	if !orderIDPattern.MatchString(orderID) || !known {
		a.flash(id, "Please choose again")
		redirect(w, r, "/menu")
		return
	}
	if len(lines) == 0 {
		a.flash(id, "Choose at least one item")
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
		// The menu shows the order that is still pending, to be sent again.
		redirect(w, r, "/menu")
		return
	}
	// A recorded order is told by a toast; one that was not stays on the menu in red.
	msg := "Order received"
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
		msg = "This order was already received"
		err = nil
	case code == "session_not_active":
		msg = "This visit can't take more orders. Please ask a staff member"
	default:
		msg = "Couldn't place the order. Please ask a staff member"
	}
	a.updateBrowser(id, func(b *Browser) {
		b.PendingOrder = nil
		if err == nil {
			b.Toast = msg
		} else {
			b.Alert = msg
		}
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
	now := time.Now()
	end := now
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
	v := a.baseView(id, b, "Bill")
	if !s.ended() {
		v.Refresh = 15
	}
	v.Data = billData{s, rateChart(history, s.CheckedInAt, end), change, end.Sub(s.CheckedInAt), orders, pauseLeft(s, now)}
	a.render(w, http.StatusOK, "panel", v)
}

// billData is what the bill (panel.html) draws.
type billData struct {
	Session *Session
	Chart   chart
	Change  *Money
	Stayed  time.Duration
	Orders  []Order
	// Pause is the time left in the pause of the cover charge, or nil when there is no pause or
	// BarPay does not tell when it ends.
	Pause *countdown
}

// countdown is a time left, as the bill shows it and counts it down in the browser.
type countdown struct {
	Seconds int
	Clock   string
}

// pauseLeft is the time until the pause of the cover charge ends at now, rounded up to the
// second. It is 0 from the instant the pause ends, at which BarPay no longer counts it as paused.
func pauseLeft(s *Session, now time.Time) *countdown {
	if s.CoverChargeStatus != "paused" || s.CoverChargeResumesAt == nil {
		return nil
	}
	left := max(0, int((s.CoverChargeResumesAt.Sub(now)+time.Second-1)/time.Second))
	return &countdown{left, fmt.Sprintf("%d:%02d", left/60, left%60)}
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
		a.fail(w, id, b, failure{Title: "Couldn't send you to the register", Text: "Please ask a staff member", Button: "Back", Href: "/panel"})
		return
	}
	// The bill tells it by its state at the register, so there is no message on top of it.
	a.store.update(func(d *Data) error { d.remember(s); return nil })
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
	// State is the one the page shows: "qr", "checking_out", "payment_failed", "unavailable" or
	// "read_failed".
	data := struct {
		Session *Session
		State   string
		QR      template.URL
		// Lifetime is the time from receiving the token to its expires_at, over which the bar
		// under the QR empties.
		Lifetime float64
		// Again is this page, which "Try again" opens.
		Again string
	}{Session: s, Again: "/exit"}
	if s.State == "closed" {
		data.Again = "/exit?again=1"
	}
	switch {
	case s.State == "checking_out":
		data.State = "checking_out"
		v.Refresh = 3
	case s.State == "payment_failed":
		data.State = "payment_failed"
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
		received := time.Now()
		if err != nil {
			data.State = "read_failed"
			if errorCode(err) == "exit_token_unavailable" {
				data.State = "unavailable"
			}
			break
		}
		code, err := qr.Encode(t.Token, qr.M)
		if err != nil {
			data.State = "read_failed"
			break
		}
		code.Scale = 8
		// A data URI the app made from its own PNG, which html/template would otherwise refuse.
		data.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
		data.State = "qr"
		left := t.ExpiresAt.Sub(received)
		data.Lifetime = max(1, left.Seconds())
		// Reload when the QR changes, not before: a reload on the same token would start the bar
		// full again.
		v.Refresh = max(1, int(left.Seconds())+1)
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

// receipts is the member's visit history: the visits that ended, newest first. The visit going on
// is on the bill, and a guest keeps no history.
func (a *App) receipts(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	switch {
	case b.Guest:
		redirect(w, r, "/account")
		return
	case b.MemberID == "":
		redirect(w, r, "/")
		return
	}
	var visits []Visit
	a.store.view(func(d *Data) {
		var ids []string
		if m := d.Members[b.MemberID]; m != nil {
			ids = m.SessionIDs
		}
		for i := len(ids) - 1; i >= 0; i-- {
			if v := d.Visits[ids[i]]; v != nil && v.Session != nil && v.Session.ended() && v.Session.Receipt != nil {
				visits = append(visits, *v)
			}
		}
	})
	v := a.baseView(id, b, "Visit history")
	v.Data = visits
	a.render(w, http.StatusOK, "receipts", v)
}

// receiptLine is one item of a receipt, the same item of every order of the visit added up.
type receiptLine struct {
	Name     string
	Quantity int
	Amount   Money
}

func receiptLines(orders []Order) []receiptLine {
	var lines []receiptLine
	at := map[OrderLine]int{}
	for _, o := range orders {
		for _, l := range o.Lines {
			key := OrderLine{Name: l.Name, UnitPrice: l.UnitPrice}
			i, ok := at[key]
			if !ok {
				i = len(lines)
				at[key] = i
				lines = append(lines, receiptLine{Name: l.Name, Amount: Money{Currency: l.UnitPrice.Currency}})
			}
			lines[i].Quantity += l.Quantity
			lines[i].Amount.Value += int64(l.Quantity) * l.UnitPrice.Value
		}
	}
	return lines
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
	if err != nil || (s.ended() && s.Receipt == nil) {
		a.fail(w, id, b, couldNotLoad(r))
		return
	}
	var orders []Order
	a.store.update(func(d *Data) error {
		d.remember(s)
		orders = append(orders, d.Visits[sessionID].Orders...)
		return nil
	})
	if !s.ended() {
		// A visit going on has no receipt yet: its bill shows where it stands.
		redirect(w, r, "/panel")
		return
	}
	v := a.baseView(id, b, "Receipt")
	current := b.SessionID == sessionID
	v.Data = struct {
		Session *Session
		Lines   []receiptLine
		Stayed  time.Duration
		// Current is the visit of this browser that just ended, which the customer finishes with
		// Done. It is thanked for unless it ended unpaid.
		Current bool
		Thanks  bool
		// GateAgain offers the exit QR again after a PayPal checkout, for a gate that did not open.
		GateAgain bool
	}{s, receiptLines(orders), s.Receipt.EndedAt.Sub(s.CheckedInAt), current,
		current && s.Receipt.Settlement != "unpaid", current && s.Receipt.Settlement == "paypal"}
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

// The PayPal page, under the account: consent and the PayPal link, and unlinking the saved
// PayPal. The page says "unlink"; BarPay's operation deletes the saved PayPal.

// paypalPage is where every PayPal operation and the return from PayPal end.
const paypalPage = "/account/paypal"

func (a *App) paypal(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	switch {
	case b.Guest:
		// The account shows a guest the PayPal row as members only.
		redirect(w, r, "/account")
		return
	case b.MemberID == "":
		redirect(w, r, "/login")
		return
	}
	v := a.baseView(id, b, "PayPal")
	data := struct {
		Member         *Member
		ConsentText    string
		ConsentVersion string
		ReadFailed     bool
	}{ConsentText: consentText, ConsentVersion: consentVersion}
	ctx, cancel := a.ctx(r)
	defer cancel()
	m, err := a.barpay.getMember(ctx, b.MemberID)
	if err != nil {
		data.ReadFailed = true
	} else {
		data.Member = m
		if paypalStatus(m) == "unlinking" {
			// The page changes once PayPal confirms; nothing else tells the customer.
			v.Refresh = 10
		}
	}
	var notice string
	a.store.update(func(d *Data) error {
		notice = d.MemberNotices[b.MemberID]
		delete(d.MemberNotices, b.MemberID)
		return nil
	})
	switch notice {
	case "":
	case paypalNoLongerValidNotice:
		v.Alert = notice
	default:
		v.Toast = notice
	}
	v.Data = data
	a.render(w, http.StatusOK, "paypal", v)
}

// paypalStatus says how the member's saved PayPal stands: "linked", "unlinking" or "not_linked".
func paypalStatus(m *Member) string {
	switch {
	case m.SavedPaypal == nil:
		return "not_linked"
	case m.SavedPaypal.Status == "deletion_pending":
		return "unlinking"
	default:
		return "linked"
	}
}

// toast tells the next customer page that an operation went through.
func (a *App) toast(id, msg string) {
	a.updateBrowser(id, func(b *Browser) { b.Toast = msg })
}

func (a *App) linkPaypal(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.MemberID == "" {
		redirect(w, r, paypalPage)
		return
	}
	if r.PostFormValue("agree") != "yes" {
		a.alert(id, "Please agree to the terms")
		redirect(w, r, paypalPage)
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
	link, err := a.barpay.createPaypalLink(ctx, b.MemberID, state, consentVersion, a.cfg.PublicURL.String()+paypalPage+"/return")
	if err != nil {
		a.updateBrowser(id, func(b *Browser) { b.LinkState, b.LinkMemberID = "", "" })
		switch errorCode(err) {
		case "saved_paypal_exists":
			// While it is being unlinked, the page says so itself.
			if m, err := a.barpay.getMember(ctx, b.MemberID); err != nil || paypalStatus(m) != "unlinking" {
				a.flash(id, "PayPal is already linked")
			}
		default:
			a.alert(id, "Couldn't start linking. Please try again later")
		}
		redirect(w, r, paypalPage)
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
		a.alert(id, "This link was started in another browser or has expired. Nothing was saved. Please link again here")
		redirect(w, r, paypalPage)
		return
	}
	a.updateBrowser(id, func(b *Browser) { b.LinkState, b.LinkMemberID = "", "" })

	// tell is how the message is shown: a toast when PayPal got linked, a red notice when the
	// customer has to do something again, a yellow one otherwise.
	tell, msg := a.alert, ""
	switch result {
	case "approved":
		ctx, cancel := a.ctx(r)
		defer cancel()
		_, err := a.barpay.confirmPaypalLink(ctx, b.MemberID, code)
		switch errorCode(err) {
		case "":
			if err == nil {
				tell, msg = a.toast, paypalLinkedNotice
				a.store.update(func(d *Data) error {
					d.toldByAnswer(b.MemberID, "member.paypal_linked", paypalLinkedNotice)
					return nil
				})
			} else {
				tell, msg = a.flash, "Couldn't confirm the link. If PayPal isn't linked below, link it again"
			}
		case "link_code_invalid":
			msg = "The link expired. Nothing was saved. Please link again"
		case "saved_paypal_exists":
			tell, msg = a.flash, "PayPal is already linked"
		case "paypal_account_already_linked":
			msg = "This PayPal account is linked to another member. Nothing was saved"
		default:
			msg = "PayPal didn't link it. Nothing was saved. Please try again"
		}
	case "cancelled":
		tell, msg = a.flash, "You cancelled in PayPal. Nothing was saved"
	case "expired":
		msg = "The link expired. Nothing was saved. Please link again"
	default:
		msg = "PayPal didn't link it. Nothing was saved. Please try again"
	}
	tell(id, msg)
	redirect(w, r, paypalPage)
}

func (a *App) deletePaypal(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.MemberID == "" {
		redirect(w, r, paypalPage)
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	confirmed, err := a.barpay.deleteSavedPaypal(ctx, b.MemberID)
	switch {
	case err == nil && confirmed:
		a.toast(id, savedPaypalDeletedNotice)
		a.store.update(func(d *Data) error {
			d.toldByAnswer(b.MemberID, "member.saved_paypal_deleted", savedPaypalDeletedNotice)
			return nil
		})
	case err == nil, errorCode(err) == "saved_paypal_in_use":
		// The page shows that it is being unlinked, or that the current visit uses it.
	case errorCode(err) == "saved_paypal_not_found":
		a.flash(id, "PayPal isn't linked")
	default:
		a.alert(id, "Couldn't unlink. Please try again later")
	}
	redirect(w, r, paypalPage)
}
