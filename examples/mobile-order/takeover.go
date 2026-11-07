package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// A guest who signs up or logs in during a visit is asked whether the visit moves to their
// account, and BarPay attaches the member to the session only once they agree (PRD UC10).
//
// Which session and which member are sent is decided by this browser session alone: the session
// is the guest check-in this app made for it, and the member the one who just signed up or logged
// in here, both kept on the server in Browser.Takeover. Nothing a request carries names either,
// so a link or a form from someone else can never attach anyone to anything.

// Takeover is a guest visit of a browser session waiting for the member who just signed up or
// logged in there to agree to take it over.
type Takeover struct {
	SessionID string `json:"session_id"`
	MemberID  string `json:"member_id"`
}

// guestVisitToOffer returns the guest visit open in b that a member signing up or logging in
// there is offered, or "". A visit the app already knows has ended is not offered.
func (a *App) guestVisitToOffer(b Browser) string {
	if !b.Guest || b.SessionID == "" {
		return ""
	}
	open := b.SessionID
	a.store.view(func(d *Data) {
		if v := d.Visits[b.SessionID]; v != nil && v.Session != nil && v.Session.ended() {
			open = ""
		}
	})
	return open
}

// inOtherVisit says whether member m is in a visit other than the guest visit offered, which
// BarPay would answer with member_has_open_session. Only a visit the app holds as m's open one
// and BarPay still has open counts; one that cannot be read is left to BarPay to decide.
func (a *App) inOtherVisit(ctx context.Context, m *MemberAccount, offered string) bool {
	if m.OpenSessionID == "" || m.OpenSessionID == offered {
		return false
	}
	s, err := a.barpay.getSession(ctx, m.OpenSessionID)
	if err != nil {
		return false
	}
	a.store.update(func(d *Data) error { d.remember(s); return nil })
	return !s.ended()
}

// pendingTakeover returns the takeover waiting in b, or nil. It holds only while the member who
// started it is still the one logged in and the visit is still this browser's.
func pendingTakeover(b Browser) *Takeover {
	t := b.Takeover
	if t == nil || b.MemberID == "" || t.MemberID != b.MemberID || t.SessionID != b.SessionID {
		return nil
	}
	return t
}

// attachKeyName names the stored Idempotency-Key of attaching t's member to t's session. It is
// kept in the data file until BarPay gives a definite answer, so every try after a lost answer,
// also after a restart, sends the same key.
func attachKeyName(t *Takeover) string {
	return attachKeyPrefix(t.SessionID) + t.MemberID
}

func attachKeyPrefix(sessionID string) string {
	return "attach:" + sessionID + ":"
}

// dropAttachKeys forgets the keys of attaching anyone to a session that has ended: no attach can
// be tried on it again, so a key left by an unknown outcome would otherwise be kept forever.
func (d *Data) dropAttachKeys(sessionID string) {
	for name := range d.PendingKeys {
		if strings.HasPrefix(name, attachKeyPrefix(sessionID)) {
			delete(d.PendingKeys, name)
		}
	}
}

// takeoverPage is where sign-up and login lead a guest in a visit.
const takeoverPage = "/account/takeover"

// takeover shows the member what the visit carries over before they agree: its running total and
// orders, and whether its total will be charged to their saved PayPal at checkout.
func (a *App) takeover(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	t := pendingTakeover(b)
	if t == nil {
		redirect(w, r, "/account")
		return
	}
	ctx, cancel := a.ctx(r)
	defer cancel()
	s, err := a.barpay.getSession(ctx, t.SessionID)
	if errorCode(err) == "session_not_found" {
		a.endTakeover(w, r, id, takeoverRefused("session_not_found"))
		return
	}
	if err != nil {
		a.fail(w, id, b, couldNotLoad(r))
		return
	}
	a.store.update(func(d *Data) error { d.remember(s); return nil })
	switch {
	case s.MemberID != nil && *s.MemberID == t.MemberID:
		// An earlier agreement went through although its answer was lost.
		a.tookOver(w, r, id, s)
		return
	case s.MemberID != nil:
		a.endTakeover(w, r, id, takeoverRefused("session_has_member"))
		return
	case s.State != "active":
		a.endTakeover(w, r, id, takeoverRefused("session_not_active"))
		return
	}
	// Whether the total is charged to a saved PayPal is part of what the member agrees to, so the
	// page is not shown without it.
	m, err := a.barpay.getMember(ctx, t.MemberID)
	if err != nil {
		a.fail(w, id, b, couldNotLoad(r))
		return
	}
	var orders []Order
	a.store.view(func(d *Data) {
		if v := d.Visits[s.SessionID]; v != nil {
			orders = append(orders, v.Orders...)
		}
	})
	v := a.baseView(id, b, "Keep this visit")
	v.Data = struct {
		Session *Session
		Orders  []Order
		// ChargesPayPal says the visit's total will be charged to the member's saved PayPal at
		// checkout. A saved PayPal being unlinked does not link the visit (UC9).
		ChargesPayPal bool
	}{s, orders, paypalStatus(m) == "linked"}
	a.render(w, http.StatusOK, "takeover", v)
}

// agreeToTakeover asks BarPay to attach the member to the guest's session. The Idempotency-Key is
// stored before the call and kept until a definite answer, so trying again after a lost answer
// attaches once.
func (a *App) agreeToTakeover(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	t := pendingTakeover(b)
	if t == nil {
		redirect(w, r, "/account")
		return
	}
	keyName := attachKeyName(t)
	var key string
	a.store.update(func(d *Data) error {
		if key = d.PendingKeys[keyName]; key == "" {
			key = randomToken(24)
			d.PendingKeys[keyName] = key
		}
		return nil
	})
	ctx, cancel := a.ctx(r)
	defer cancel()
	s, err := a.barpay.attachMember(ctx, key, t.SessionID, t.MemberID)
	unknown := func() {
		a.fail(w, id, b, failure{Title: "Couldn't connect", Text: "Trying again won't move it twice", Button: "Try again", Href: takeoverPage, Post: true})
	}
	// Only BarPay's refusal (a 4xx with an error code) is a definite answer: anything else may
	// have attached, so the key stays for the next try.
	var apiErr *APIError
	refused := errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Code != "idempotency_key_in_progress"
	if err != nil && !refused {
		unknown()
		return
	}
	a.store.update(func(d *Data) error { delete(d.PendingKeys, keyName); return nil })
	if code := errorCode(err); code == "session_has_member" {
		// Attached by an earlier request whose key BarPay no longer keeps: whose it is decides
		// whether this is done or refused, and a session that cannot be read decides neither.
		read, rerr := a.barpay.getSession(ctx, t.SessionID)
		if rerr != nil {
			unknown()
			return
		}
		if read.MemberID != nil && *read.MemberID == t.MemberID {
			s, err = read, nil
		}
	}
	if err != nil {
		a.endTakeover(w, r, id, takeoverRefused(errorCode(err)))
		return
	}
	a.tookOver(w, r, id, s)
}

// declineTakeover leaves the visit a guest's without asking BarPay. The member stays logged in;
// the visit is paid at the register, and the account is used from the next check-in.
func (a *App) declineTakeover(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	t := pendingTakeover(b)
	if t == nil {
		redirect(w, r, "/account")
		return
	}
	// An agreement whose answer was lost may have gone through, which declining cannot undo, so
	// the session is read first. Declining never resends the attach: that would attach after the
	// member declined. An agreement still in flight from another tab can therefore land after this
	// decline; its session.member_attached notification then records the visit as the member's.
	keyName := attachKeyName(t)
	var sent bool
	a.store.view(func(d *Data) { _, sent = d.PendingKeys[keyName] })
	if sent {
		ctx, cancel := a.ctx(r)
		defer cancel()
		s, err := a.barpay.getSession(ctx, t.SessionID)
		if err != nil {
			a.fail(w, id, b, failure{Title: "Couldn't load", Text: "Your earlier answer may have moved this visit already", Button: "Try again", Href: takeoverPage})
			return
		}
		if s.MemberID != nil && *s.MemberID == t.MemberID {
			a.tookOver(w, r, id, s)
			return
		}
		a.store.update(func(d *Data) error { delete(d.PendingKeys, keyName); return nil })
	}
	a.updateBrowser(id, func(b *Browser) {
		b.Takeover = nil
		b.Flash = "This visit stays a guest visit. Your account is used from your next visit"
	})
	redirect(w, r, "/account")
}

// tookOver makes the browser session's visit the member's, as BarPay now has it.
func (a *App) tookOver(w http.ResponseWriter, r *http.Request, id string, s *Session) {
	a.store.update(func(d *Data) error { d.remember(s); return nil })
	a.updateBrowser(id, func(b *Browser) {
		b.Takeover, b.SessionID = nil, s.SessionID
		b.Toast = "You're a member now. This visit carries on as yours"
	})
	redirect(w, r, "/account")
}

// endTakeover drops the takeover and leads back to the account, telling in red why. The visit
// stays this browser's, as a guest visit.
func (a *App) endTakeover(w http.ResponseWriter, r *http.Request, id, alert string) {
	a.updateBrowser(id, func(b *Browser) { b.Takeover, b.Alert = nil, alert })
	redirect(w, r, "/account")
}

// takeoverRefused says why the visit did not move to the member's account, by BarPay's error
// code.
func takeoverRefused(code string) string {
	switch code {
	case "session_has_member":
		return "This visit is already on an account, so it can't move to yours. It stays a guest visit"
	case "session_not_active":
		return "This visit is already being paid or has ended, so it can't move to your account. It stays a guest visit"
	case "member_has_open_session":
		return "You're checked in on another device, so this visit can't move to your account. It stays a guest visit"
	case "session_not_found":
		return "This visit couldn't be found, so nothing moved to your account"
	}
	return "Couldn't move this visit to your account. It stays a guest visit. Please ask a staff member"
}
