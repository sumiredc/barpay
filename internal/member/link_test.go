package member

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/spectest"
)

// UC2 main flow and postcondition, Guarantees, Consent: link URL → PayPal → return → confirm saves
// PayPal for the member with the consent version, links the active session, and sends
// member.paypal_linked and session.paypal_linked, each fitting the spec.
func TestLinkSavesPayPalAndLinksTheActiveSession(t *testing.T) {
	f := newFixture(t)
	s, _ := f.checkin(t, "m_1")
	w := f.createLink("m_1", linkBody("consent-v1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("createPaypalLink: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "PaypalLink", w.Body.Bytes())
	if !strings.Contains(w.Body.String(), `"expires_at":"2026-10-31T18:30:00Z"`) {
		t.Errorf("link %s; want it to expire 30 minutes after it was made", w.Body)
	}
	if n := len(f.pp.Calls("")); n != 0 {
		t.Errorf("asking for a link URL made %d PayPal calls, want none", n)
	}
	linkURL := f.linkURL(t, "m_1")
	token := f.toPayPal(t, linkURL)
	urls := f.pp.URLs(token)
	if urls.Return != linkURL+"/return" || urls.Cancel != linkURL+"/cancel" {
		t.Errorf("PayPal returns to %+v; want BarPay's URLs of this link", urls)
	}
	f.pp.Approve(token, "payer-1")
	code := returned(t, f.browse(t, urls.Return)).Get("code")
	f.clk.Advance(2 * time.Minute)

	w = f.confirm("m_1", code)
	if w.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Member", w.Body.Bytes())
	v := decodeMember(t, w.Body.Bytes())
	if v.SavedPaypal == nil || v.SavedPaypal.Status != "saved" || v.SavedPaypal.ConsentVersion != "consent-v1" ||
		v.SavedPaypal.SavedAt != "2026-10-31T18:02:00Z" || v.SavedPaypal.InUseBySessionID == nil || *v.SavedPaypal.InUseBySessionID != s.SessionID {
		t.Errorf("member = %+v", v.SavedPaypal)
	}
	if got := f.getMember(t, "m_1"); got.SavedPaypal == nil || got.SavedPaypal.Status != "saved" {
		t.Errorf("getMember = %+v", got)
	}
	if got := f.lookup(t, s.SessionID); got.PaymentMethod != "paypal" {
		t.Errorf("the session pays by %s, want paypal", got.PaymentMethod)
	}
	if n := len(f.pp.Vaults()); n != 1 {
		t.Errorf("PayPal holds %d saved PayPals, want 1", n)
	}
	m := f.events(t, "m_1", "member.paypal_linked")
	ss := f.events(t, s.SessionID, "session.paypal_linked")
	if len(m) != 1 || len(ss) != 1 {
		t.Fatalf("%d member.paypal_linked and %d session.paypal_linked events, want 1 each", len(m), len(ss))
	}
	spectest.Valid(t, "MemberPaypalLinkedEvent", m[0])
	spectest.Valid(t, "SessionPaypalLinkedEvent", ss[0])
	if f.wake.n.Load() == 0 {
		t.Error("the dispatcher was not woken")
	}
}

// UC2 exception: a member whose session has left active keeps the saved PayPal for the next visit,
// and that session is not linked. A member with no open session just keeps it.
func TestConfirmWithoutAnActiveSession(t *testing.T) {
	for _, st := range []string{"at_register", "checking_out", "payment_failed", "closed", "none"} {
		t.Run(st, func(t *testing.T) {
			f := newFixture(t)
			var id string
			if st != "none" {
				s, _ := f.checkin(t, "m_1")
				id = s.SessionID
				if st == "at_register" {
					f.do(call{method: http.MethodPost, path: "/v1/sessions/" + id + "/register-request"})
				} else {
					f.setState(t, id, st)
				}
			}
			v := f.linked(t, "m_1", "payer-1")
			if v.SavedPaypal == nil || v.SavedPaypal.InUseBySessionID != nil {
				t.Errorf("member = %+v; want saved and used by no session", v.SavedPaypal)
			}
			if id == "" {
				return
			}
			if n := len(f.events(t, id, "session.paypal_linked")); n != 0 {
				t.Errorf("%d session.paypal_linked events, want none", n)
			}
			if st == "at_register" {
				if got := f.lookup(t, id); got.PaymentMethod != "register" {
					t.Errorf("payment method %s, want register", got.PaymentMethod)
				}
			}
			if n := f.count(t, "SELECT count(*) FROM sessions WHERE paypal_linked_at IS NOT NULL"); n != 0 {
				t.Error("the session was linked")
			}
		})
	}
}

// UC2 exception: a member who has a saved PayPal, or one being deleted, cannot ask for a link URL
// nor confirm a code obtained before; the code is used up all the same.
func TestLinkRefusedWhileASavedPayPalExists(t *testing.T) {
	f := newFixture(t)
	early := f.approvedCode(t, "m_1", "payer-1")
	pending := f.approvedCode(t, "m_1", "payer-1")
	f.linked(t, "m_1", "payer-1")

	assertError(t, f.createLink("m_1", linkBody("consent-v1")), http.StatusConflict, "saved_paypal_exists")
	assertError(t, f.confirm("m_1", early), http.StatusConflict, "saved_paypal_exists")
	assertError(t, f.confirm("m_1", early), http.StatusUnprocessableEntity, "link_code_invalid")

	f.pp.Next(paypaltest.DeletePaymentToken, paypaltest.NoAnswer, paypaltest.NoAnswer, paypaltest.NoAnswer)
	if w := f.deleteSaved("m_1"); w.Code != http.StatusAccepted {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	assertError(t, f.createLink("m_1", linkBody("consent-v1")), http.StatusConflict, "saved_paypal_exists")
	assertError(t, f.confirm("m_1", pending), http.StatusConflict, "saved_paypal_exists")
	if n := len(f.pp.Calls(paypaltest.CreatePaymentToken)); n != 1 {
		t.Errorf("PayPal was asked to save %d times, want only the first link", n)
	}
}

// Guarantees, Consent: a link request without the consent version shown, or with a malformed one,
// is refused and no link URL is made.
func TestLinkNeedsTheConsentVersion(t *testing.T) {
	for name, body := range map[string]string{
		"missing":   `{"state":"` + state + `","return_url":"` + returnURL + `"}`,
		"empty":     linkBody(""),
		"malformed": linkBody("v 1"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			w := f.createLink("m_1", body)
			assertError(t, w, http.StatusBadRequest, "invalid_request")
			if _, field, _ := errorOf(t, w); field != "consent_version" {
				t.Errorf("field = %q, want consent_version", field)
			}
			if n := f.count(t, "SELECT count(*) FROM paypal_links"); n != 0 {
				t.Errorf("%d links stored", n)
			}
		})
	}
}

// UC2 exception (guest) and the spec's PaypalLinkRequest: a member ID that is not one, or a
// malformed body, is refused.
func TestLinkRequestMustBeWellFormed(t *testing.T) {
	f := newFixture(t)
	for _, m := range []string{"m!1", "m%201", strings.Repeat("a", 65)} {
		w := f.createLink(m, linkBody("consent-v1"))
		assertError(t, w, http.StatusBadRequest, "invalid_request")
		if _, field, _ := errorOf(t, w); field != "member_id" {
			t.Errorf("%s: field = %q, want member_id", m, field)
		}
	}
	for name, body := range map[string]string{
		"short state":   `{"state":"short","consent_version":"v1","return_url":"` + returnURL + `"}`,
		"no return URL": `{"state":"` + state + `","consent_version":"v1"}`,
		"relative URL":  `{"state":"` + state + `","consent_version":"v1","return_url":"/settings"}`,
		"extra field":   `{"state":"` + state + `","consent_version":"v1","return_url":"` + returnURL + `","member_id":"m_2"}`,
		"not JSON":      `state=x`,
	} {
		assertError(t, f.createLink("m_1", body), http.StatusBadRequest, "invalid_request")
		_ = name
	}
	if n := f.count(t, "SELECT count(*) FROM paypal_links"); n != 0 {
		t.Errorf("%d links stored", n)
	}
}

// UC2 exception: a return URL outside the store app's origin is refused, and no link URL is made.
func TestReturnURLOutsideTheOrigin(t *testing.T) {
	f := newFixture(t)
	for _, u := range []string{"https://evil.test/settings", "http://store.test/settings", "https://store.test.evil/x",
		"https://store.test:8443/x", "https://user@store.test/x"} {
		body := `{"state":"` + state + `","consent_version":"v1","return_url":"` + u + `"}`
		w := f.createLink("m_1", body)
		assertError(t, w, http.StatusUnprocessableEntity, "return_url_not_allowed")
	}
	if n := f.count(t, "SELECT count(*) FROM paypal_links"); n != 0 {
		t.Errorf("%d links stored", n)
	}
	// The origin is compared without regard to case.
	if w := f.createLink("m_1", `{"state":"`+state+`","consent_version":"v1","return_url":"HTTPS://Store.Test/x"}`); w.Code != http.StatusCreated {
		t.Errorf("same origin in upper case: %d %s", w.Code, w.Body)
	}
}

// UC2 exception, Responsibilities, PayPal link: a link URL works until just before 30 minutes after
// it was made; from then on, and once replaced by a newer one, opening it sends the member back as
// expired, calls nothing and saves nothing.
func TestLinkURLStopsWorking(t *testing.T) {
	f := newFixture(t)
	u := f.linkURL(t, "m_1")
	f.clk.Set(t0.Add(30*time.Minute - time.Nanosecond))
	f.toPayPal(t, u)

	f.clk.Set(t0)
	late := f.linkURL(t, "m_2")
	f.clk.Set(t0.Add(30 * time.Minute))
	if q := returned(t, f.browse(t, late)); q.Get("paypal_link") != "expired" || q.Get("state") != state || q.Has("code") {
		t.Errorf("at 30 minutes: %v; want expired", q)
	}

	f.clk.Set(t0.Add(time.Hour))
	old := f.linkURL(t, "m_3")
	newer := f.linkURL(t, "m_3")
	if q := returned(t, f.browse(t, old)); q.Get("paypal_link") != "expired" {
		t.Errorf("replaced: %v; want expired", q)
	}
	f.toPayPal(t, newer)
	if n := len(f.pp.Calls(paypaltest.CreateSetupToken)); n != 2 {
		t.Errorf("%d setup tokens made, want 2 (none for the expired and replaced URLs)", n)
	}
	if n := f.count(t, "SELECT count(*) FROM saved_paypals"); n != 0 {
		t.Errorf("%d saved PayPals", n)
	}
}

// The return and cancel count against the same 30 minutes.
func TestReturnAfterTheLinkStoppedWorking(t *testing.T) {
	f := newFixture(t)
	token := f.toPayPal(t, f.linkURL(t, "m_1"))
	f.pp.Approve(token, "payer-1")
	f.clk.Set(t0.Add(30 * time.Minute))
	if q := returned(t, f.browse(t, f.pp.URLs(token).Return)); q.Get("paypal_link") != "expired" || q.Has("code") {
		t.Errorf("return at 30 minutes: %v; want expired", q)
	}
	if q := returned(t, f.browse(t, f.pp.URLs(token).Cancel)); q.Get("paypal_link") != "expired" {
		t.Errorf("cancel at 30 minutes: %v; want expired", q)
	}
}

// UC2 exceptions: cancelling in PayPal returns cancelled, and a setup token that was not approved
// returns failed; neither has a code, and nothing is saved.
func TestCancelledOrNotApproved(t *testing.T) {
	f := newFixture(t)
	token := f.toPayPal(t, f.linkURL(t, "m_1"))
	if q := returned(t, f.browse(t, f.pp.URLs(token).Cancel)); q.Get("paypal_link") != "cancelled" || q.Has("code") {
		t.Errorf("cancel: %v", q)
	}
	token = f.toPayPal(t, f.linkURL(t, "m_2"))
	if q := returned(t, f.browse(t, f.pp.URLs(token).Return)); q.Get("paypal_link") != "failed" || q.Has("code") {
		t.Errorf("not approved: %v", q)
	}
	if n := f.count(t, "SELECT count(*) FROM saved_paypals") + f.count(t, "SELECT count(*) FROM paypal_links WHERE code_hash IS NOT NULL"); n != 0 {
		t.Errorf("something was saved or a code issued")
	}
	if n := len(f.pp.Calls(paypaltest.CreatePaymentToken)); n != 0 {
		t.Errorf("PayPal was asked to save %d times", n)
	}
}

// Fixed constraints, Verify before trusting: a return is accepted once per link, a token ID added
// to the URL is ignored, and only the setup token stored with the link is checked.
func TestReturnTrustsOnlyTheStoredSetupToken(t *testing.T) {
	f := newFixture(t)
	mine := f.toPayPal(t, f.linkURL(t, "m_1"))
	other := f.toPayPal(t, f.linkURL(t, "m_2"))
	f.pp.Approve(other, "payer-2")
	ret := f.pp.URLs(mine).Return + "?token=" + other + "&approval_token_id=" + other + "&code=plc_x"
	if q := returned(t, f.browse(t, ret)); q.Get("paypal_link") != "failed" || q.Get("code") != "" {
		t.Errorf("return with another link's token: %v; want failed", q)
	}
	checked := f.pp.Calls(paypaltest.SetupTokenApproved)
	if len(checked) != 1 || checked[0].ID != mine {
		t.Errorf("PayPal was asked about %+v; want only %s", checked, mine)
	}
	f.pp.Approve(mine, "payer-1")
	if q := returned(t, f.browse(t, f.pp.URLs(mine).Return)); q.Get("paypal_link") != "expired" {
		t.Errorf("second return: %v; want expired", q)
	}
	if q := returned(t, f.browse(t, f.pp.URLs(mine).Cancel)); q.Get("paypal_link") != "expired" {
		t.Errorf("cancel after the return: %v; want expired", q)
	}
}

// Responsibilities, PayPal link: the redirects carry only paypal_link, state and code, never a
// PayPal token, and send no referrer.
func TestRedirectsCarryOnlyTheResult(t *testing.T) {
	f := newFixture(t)
	w := f.browse(t, f.linkURL(t, "m_1"))
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("redirect to PayPal: Referrer-Policy = %q", got)
	}
	token := paypaltest.SetupTokenOf(w.Header().Get("Location"))
	f.pp.Approve(token, "payer-1")
	w = f.browse(t, f.pp.URLs(token).Return+"?token="+token+"&PayerID=P1")
	q := returned(t, w)
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range q {
			if !yield(k) {
				return
			}
		}
	})
	if !slices.Equal(keys, []string{"code", "paypal_link", "state", "tab"}) {
		t.Errorf("the return carries %v; want the store app's own tab, and paypal_link, state and code", keys)
	}
	if q.Get("tab") != "pay" || q.Get("state") != state || !codePattern.MatchString(q.Get("code")) {
		t.Errorf("query %v", q)
	}
	if loc := w.Header().Get("Location"); strings.Contains(loc, token) {
		t.Errorf("the return %s carries the PayPal token", loc)
	}
}

// Fixed constraints, Verify before trusting; UC2 exception: PayPal is saved only at confirmation,
// and kept for the member only after PayPal saved it. A refused save keeps nothing.
func TestSaveOnlyAtConfirmationAndOnlyOnSuccess(t *testing.T) {
	f := newFixture(t)
	s, _ := f.checkin(t, "m_1")
	code := f.approvedCode(t, "m_1", "payer-1")
	if n := len(f.pp.Calls(paypaltest.CreatePaymentToken)); n != 0 || f.count(t, "SELECT count(*) FROM saved_paypals") != 0 {
		t.Fatal("PayPal was saved before the confirmation")
	}
	f.pp.Next(paypaltest.CreatePaymentToken, paypaltest.Refuse)
	assertError(t, f.confirm("m_1", code), http.StatusBadGateway, "paypal_save_failed")
	if f.getMember(t, "m_1").SavedPaypal != nil || len(f.events(t, "m_1", "member.paypal_linked")) != 0 ||
		len(f.events(t, s.SessionID, "session.paypal_linked")) != 0 || f.lookup(t, s.SessionID).PaymentMethod != "register" {
		t.Error("a refused save kept something")
	}
}

// UC2 exception: the member's PayPal account had a saved PayPal with the store when they approved,
// and it was deleted before the confirmation; PayPal refuses to save, and linking again saves.
func TestSaveFailsWhenTheEarlierSavedPayPalWasDeleted(t *testing.T) {
	f := newFixture(t)
	f.linked(t, "m_1", "payer-1")
	code := f.approvedCode(t, "m_2", "payer-1")
	if w := f.deleteSaved("m_1"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	assertError(t, f.confirm("m_2", code), http.StatusBadGateway, "paypal_save_failed")
	if v := f.linked(t, "m_2", "payer-1"); v.SavedPaypal == nil {
		t.Error("linking again did not save")
	}
}

// UC2 exception, Guarantees, Charges only for this visit: PayPal returns the saved PayPal another
// member holds when the same PayPal account approves; it is refused, nothing is saved for this
// member, and nothing is deleted in PayPal.
func TestOnePayPalAccountForOneMember(t *testing.T) {
	f := newFixture(t)
	s, _ := f.checkin(t, "m_2")
	f.linked(t, "m_1", "payer-1")
	code := f.approvedCode(t, "m_2", "payer-1")
	w := f.confirm("m_2", code)
	assertError(t, w, http.StatusConflict, "paypal_account_already_linked")
	if f.getMember(t, "m_2").SavedPaypal != nil || f.getMember(t, "m_1").SavedPaypal == nil {
		t.Error("the saved PayPal moved between the members")
	}
	if len(f.pp.Calls(paypaltest.DeletePaymentToken)) != 0 || len(f.pp.Vaults()) != 1 {
		t.Error("something was deleted in PayPal")
	}
	if len(f.events(t, "m_2", "member.paypal_linked")) != 0 || len(f.events(t, s.SessionID, "session.paypal_linked")) != 0 {
		t.Error("the refused link was notified")
	}
	assertError(t, f.confirm("m_2", code), http.StatusUnprocessableEntity, "link_code_invalid")
	// Another PayPal account links.
	if v := f.linked(t, "m_2", "payer-2"); v.SavedPaypal == nil {
		t.Error("m_2 could not link another PayPal account")
	}
}

// UC2 exception, Fixed constraints, The store's own PayPal: a saved PayPal PayPal returns that
// BarPay holds for no member is saved to the linking member.
func TestSavedPayPalHeldByNoMemberIsSaved(t *testing.T) {
	f := newFixture(t)
	f.linked(t, "m_1", "payer-1")
	vault := ""
	for v := range f.pp.Vaults() {
		vault = v
	}
	// BarPay lost its record, as a save whose answer was lost and whose code was used up leaves it.
	if _, err := f.st.Read().ExecContext(t.Context(), "DELETE FROM saved_paypals"); err != nil {
		t.Fatal(err)
	}
	f.linked(t, "m_2", "payer-1")
	var got string
	if err := f.st.Read().QueryRowContext(t.Context(), "SELECT vault_id FROM saved_paypals WHERE member_id = 'm_2'").Scan(&got); err != nil || got != vault {
		t.Errorf("m_2 holds %q, %v; want %s", got, err, vault)
	}
}

// UC2 exception: a code that is unknown, already used, expired or another member's is refused,
// saves nothing, and cannot be used again.
func TestConfirmWithAnInvalidCode(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		f := newFixture(t)
		assertError(t, f.confirm("m_1", newCode()), http.StatusUnprocessableEntity, "link_code_invalid")
	})
	t.Run("malformed", func(t *testing.T) {
		f := newFixture(t)
		assertError(t, f.confirm("m_1", "abc"), http.StatusBadRequest, "invalid_request")
	})
	t.Run("used", func(t *testing.T) {
		f := newFixture(t)
		code := f.approvedCode(t, "m_1", "payer-1")
		if w := f.confirm("m_1", code); w.Code != http.StatusCreated {
			t.Fatalf("first confirm: %d", w.Code)
		}
		assertError(t, f.confirm("m_1", code), http.StatusUnprocessableEntity, "link_code_invalid")
	})
	t.Run("another member's", func(t *testing.T) {
		f := newFixture(t)
		code := f.approvedCode(t, "m_1", "payer-1")
		assertError(t, f.confirm("m_2", code), http.StatusUnprocessableEntity, "link_code_invalid")
		assertError(t, f.confirm("m_1", code), http.StatusUnprocessableEntity, "link_code_invalid")
		if f.count(t, "SELECT count(*) FROM saved_paypals") != 0 || len(f.pp.Calls(paypaltest.CreatePaymentToken)) != 0 {
			t.Error("something was saved")
		}
	})
	for _, c := range []struct {
		after time.Duration
		ok    bool
	}{{10*time.Minute - time.Nanosecond, true}, {10 * time.Minute, false}} {
		t.Run("after "+c.after.String(), func(t *testing.T) {
			f := newFixture(t)
			code := f.approvedCode(t, "m_1", "payer-1")
			f.clk.Advance(c.after)
			w := f.confirm("m_1", code)
			if c.ok {
				if w.Code != http.StatusCreated {
					t.Errorf("just before 10 minutes: %d %s", w.Code, w.Body)
				}
				return
			}
			assertError(t, w, http.StatusUnprocessableEntity, "link_code_invalid")
			f.clk.Set(t0)
			assertError(t, f.confirm("m_1", code), http.StatusUnprocessableEntity, "link_code_invalid")
		})
	}
}

// UC2 exception: approving and never confirming saves nothing.
func TestApprovedButNeverConfirmed(t *testing.T) {
	f := newFixture(t)
	f.approvedCode(t, "m_1", "payer-1")
	f.clk.Advance(time.Hour)
	if f.getMember(t, "m_1").SavedPaypal != nil || len(f.pp.Vaults()) != 0 {
		t.Error("something was saved without a confirmation")
	}
}

// The link URL, PayPal's return and cancel need no key, only an unguessable link ID: an unknown ID
// is refused, a used link's ID calls nothing, and a key does not stand in for the ID.
func TestLinkURLsAcceptOnlyAKnownLinkID(t *testing.T) {
	f := newFixture(t)
	for _, suffix := range []string{"", "/return", "/cancel"} {
		for _, auth := range []string{noAuth, storeAuth, adminAuth} {
			w := f.do(call{method: http.MethodGet, path: "/v1/paypal-links/pll_UNKNOWNUNKNOWNUNKNOWN1234" + suffix, auth: auth})
			if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" {
				t.Errorf("unknown ID%s with %s: %d to %q", suffix, auth, w.Code, w.Header().Get("Location"))
			}
		}
	}
	u := f.linkURL(t, "m_1")
	token := f.toPayPal(t, u)
	returned(t, f.browse(t, f.pp.URLs(token).Cancel))
	before := len(f.pp.Calls(""))
	for _, v := range []string{u, u + "/return", u + "/cancel"} {
		if q := returned(t, f.browse(t, v)); q.Get("paypal_link") != "expired" {
			t.Errorf("used link %s: %v; want expired", v, q)
		}
	}
	if n := len(f.pp.Calls("")); n != before {
		t.Errorf("a used link made %d PayPal calls", n-before)
	}
	if _, err := url.Parse(u); err != nil || !strings.HasPrefix(u, publicURL+"/v1/paypal-links/pll_") {
		t.Errorf("link URL %s", u)
	}
}
