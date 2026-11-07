package main

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// A guest's session that a member was attached to (UC10) is the member's visit from then on: the
// notification puts it in the member's visits, as their open one, and later notifications of
// it keep it there once.
func TestTheMemberAttachedNotificationMakesTheVisitTheMembers(t *testing.T) {
	a, appURL := notifiedApp(t)
	a.store.update(func(d *Data) error {
		d.Members["m_taro"] = &MemberAccount{ID: "m_taro", Name: "Taro"}
		return nil
	})
	at := time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)
	guest := Session{SessionID: "ses_1", State: "active", PaymentMethod: "register"}
	notify(t, appURL, "secret", "evt_1", "session.started", 1, at, guest, "")
	taro := "m_taro"
	attached := guest
	attached.MemberID = &taro
	notify(t, appURL, "secret", "evt_2", "session.member_attached", 2, at.Add(time.Minute), attached, "")
	notify(t, appURL, "secret", "evt_2", "session.member_attached", 2, at.Add(time.Minute), attached, "")
	linked := attached
	linked.PaymentMethod = "paypal"
	notify(t, appURL, "secret", "evt_3", "session.paypal_linked", 3, at.Add(2*time.Minute), linked, "")
	a.store.view(func(d *Data) {
		m := d.Members["m_taro"]
		if !slices.Equal(m.SessionIDs, []string{"ses_1"}) || m.OpenSessionID != "ses_1" {
			t.Errorf("the member's visits are %v, open %q; want ses_1 once, open", m.SessionIDs, m.OpenSessionID)
		}
		if v := d.Visits["ses_1"]; v.MemberID != "m_taro" {
			t.Errorf("the visit is for %q, want m_taro", v.MemberID)
		}
	})
}

const (
	chargedToPayPal  = "This visit&#39;s total will be charged to your saved PayPal when you leave through the exit gate, without asking you again"
	paidAtRegister   = "You pay at the register, or with PayPal once you link it in your account"
	takeoverQuestion = "<h1>Keep this visit on your account?</h1>"
	tookOverToast    = "<span>You&#39;re a member now. This visit carries on as yours</span>"
)

// inGuestVisit makes the browser a guest in a visit with an order of two beers, and returns the
// session ID.
func (s *shop) inGuestVisit() string {
	s.t.Helper()
	s.post("/", "/guest", url.Values{})
	id := s.browserState().SessionID
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	s.barpay.mu.Lock()
	ss := s.barpay.sessions[id]
	ss.OrdersTotal, ss.OrderCount, ss.CoverCharge, ss.Total = usd(1400), 1, usd(1200), usd(2600)
	s.barpay.mu.Unlock()
	s.a.store.update(func(d *Data) error {
		d.visit(id).Orders = []Order{{OrderID: "ord_1", Amount: usd(1400), RecordedAt: time.Now(),
			Lines: []OrderLine{{Name: "Draft beer", Quantity: 2, UnitPrice: usd(700)}}}}
		return nil
	})
	return id
}

// otherGuestVisit is a guest's session BarPay has for another browser.
func (s *shop) otherGuestVisit() string {
	s.barpay.mu.Lock()
	defer s.barpay.mu.Unlock()
	s.barpay.sessions["ses_other"] = &Session{SessionID: "ses_other", State: "active", PaymentMethod: "register"}
	return "ses_other"
}

// memberWithAccount signs up Taro, logs out, and returns the member ID, for a later login.
func (s *shop) memberWithAccount() string {
	s.t.Helper()
	id := s.signUp()
	s.post("/", "/logout", url.Values{})
	return id
}

// browserState is what the app keeps for this browser's session.
func (s *shop) browserState() Browser {
	u, _ := url.Parse(s.app.URL)
	var b Browser
	for _, c := range s.client.Jar.Cookies(u) {
		if c.Name == cookieName {
			s.a.store.view(func(d *Data) {
				if v := d.Browsers[c.Value]; v != nil {
					b = *v
				}
			})
		}
	}
	return b
}

// anotherBrowser is a second browser on the same app and BarPay.
func (s *shop) anotherBrowser() *shop {
	jar, _ := cookiejar.New(nil)
	return &shop{customer: &customer{t: s.t, app: s.app, client: &http.Client{Jar: jar}}, barpay: s.barpay, a: s.a}
}

// memberInVisitElsewhere signs up Taro in another browser and checks him in there with a total
// of $99.00, which tells his visit from a guest's. It returns the member and the session.
func (s *shop) memberInVisitElsewhere() (string, string) {
	s.t.Helper()
	other := s.anotherBrowser()
	member := other.signUp()
	other.setMember(&Member{})
	other.post("/", "/checkin", url.Values{})
	var visit string
	s.a.store.view(func(d *Data) { visit = d.Members[member].OpenSessionID })
	s.barpay.mu.Lock()
	s.barpay.sessions[visit].Total = Money{Value: 9900, Currency: "USD"}
	s.barpay.mu.Unlock()
	return member, visit
}

// postResp posts like post, and returns where it ends and the page there, whatever its status.
func (s *shop) postResp(from, path string, form url.Values) (string, string) {
	s.t.Helper()
	m := csrfPattern.FindStringSubmatch(s.open(from))
	if m == nil {
		s.t.Fatalf("%s has no form", from)
	}
	form.Set("csrf", m[1])
	resp, err := s.client.PostForm(s.app.URL+path, form)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.Request.URL.Path, string(b)
}

func (s *shop) logIn() (string, string) {
	s.t.Helper()
	return s.postResp("/login", "/login", url.Values{"email": {"taro@example.com"}, "password": {"Abcdef1!"}})
}

// agree posts the takeover's agreement.
func (s *shop) agree(form url.Values) (string, string) {
	s.t.Helper()
	return s.postResp(takeoverPage, takeoverPage, form)
}

func (s *shop) attaches() []attachRequest {
	s.barpay.mu.Lock()
	defer s.barpay.mu.Unlock()
	return append([]attachRequest(nil), s.barpay.attaches...)
}

func (s *shop) attachFails(code string, status int) {
	s.barpay.mu.Lock()
	defer s.barpay.mu.Unlock()
	s.barpay.attachErr, s.barpay.attachStatus = code, status
}

func (s *shop) memberVisits(id string) []string {
	var ids []string
	s.a.store.view(func(d *Data) { ids = append(ids, d.Members[id].SessionIDs...) })
	return ids
}

func (s *shop) pendingKeys() map[string]string {
	keys := map[string]string{}
	s.a.store.view(func(d *Data) {
		for k, v := range d.PendingKeys {
			keys[k] = v
		}
	})
	return keys
}

// loggedInGuestVisit is a guest visit in this browser, then the login of Taro there, who has no
// saved PayPal. It returns the member and the session.
func (s *shop) loggedInGuestVisit() (string, string) {
	s.t.Helper()
	member := s.memberWithAccount()
	visit := s.inGuestVisit()
	s.setMember(&Member{})
	if at, _ := s.logIn(); at != takeoverPage {
		s.t.Fatalf("logging in in a guest visit ends on %s", at)
	}
	return member, visit
}

func TestTheAccountOffersAGuestInAVisitToSignUpOrLogIn(t *testing.T) {
	s := newShop(t)
	s.inGuestVisit()
	page := s.open("/account")
	for _, want := range []string{
		`<a class="button" href="/signup">Sign up and keep this visit</a>`,
		`<a class="textbutton" href="/login" style="align-self: center">Already a member? Log in</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("a guest's account lacks %s", want)
		}
	}
	if !strings.Contains(s.open("/signup"), `<li><i data-icon="check"></i>Keep this visit as it is</li>`) {
		t.Error("the sign-up page does not say the visit is kept")
	}
	if !strings.Contains(s.open("/login"), "<span>Your current visit carries over to your account</span>") {
		t.Error("the login page does not say the visit carries over")
	}
}

func TestAMemberIsNotOfferedToKeepAVisit(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(&Member{})
	s.checkIn("/checkin")
	if strings.Contains(s.open("/account"), "keep this visit") || strings.Contains(s.open("/login"), "carries over") {
		t.Error("a member is offered to keep the visit")
	}
}

// Signing up leads to the question; nothing is attached before the member agrees.
func TestSigningUpInAGuestVisitAsksBeforeMovingIt(t *testing.T) {
	s := newShop(t)
	s.inGuestVisit()
	s.setMember(&Member{})
	at, page := s.postResp("/signup", "/signup", url.Values{"name": {"Taro"}, "email": {"taro@example.com"}, "password": {"Abcdef1!"}})
	if at != takeoverPage {
		t.Fatalf("signing up in a guest visit ends on %s", at)
	}
	for _, want := range []string{
		takeoverQuestion,
		`<div>Orders<span class="muted"> (1)</span></div>`,
		`<li><span>Draft beer</span><span class="muted">× 2</span></li>`,
		`<div class="bill-row total"><div>Total so far</div><div class="bill-amount">$26.00</div></div>`,
		paidAtRegister,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the question lacks %s", want)
		}
	}
	if strings.Contains(page, chargedToPayPal) {
		t.Error("the question says a PayPal the member has not saved will be charged")
	}
	if n := len(s.attaches()); n != 0 {
		t.Errorf("%d attaches before the member agreed", n)
	}
}

// A saved PayPal being unlinked does not link the visit (UC9), so only one in use is said to be
// charged.
func TestTheQuestionSaysASavedPayPalWillBeCharged(t *testing.T) {
	for status, charged := range map[string]bool{"active": true, "deletion_pending": false} {
		s := newShop(t)
		s.memberWithAccount()
		s.inGuestVisit()
		s.setMember(linked(nil, status))
		at, page := s.logIn()
		if at != takeoverPage {
			t.Fatalf("logging in in a guest visit ends on %s", at)
		}
		if strings.Contains(page, chargedToPayPal) != charged || strings.Contains(page, paidAtRegister) == charged {
			t.Errorf("a saved PayPal %s: the question says it is charged %v, want %v", status, !charged, charged)
		}
	}
}

// Whether a saved PayPal will be charged is part of what the member agrees to.
func TestTheQuestionIsNotAskedWithoutKnowingTheSavedPayPal(t *testing.T) {
	s := newShop(t)
	s.memberWithAccount()
	s.inGuestVisit()
	s.setMember(nil)
	_, page := s.logIn()
	if strings.Contains(page, takeoverQuestion) || !strings.Contains(page, "<h1>Couldn&#39;t load</h1>") {
		t.Error("the question is asked without knowing whether a saved PayPal is charged")
	}
}

func TestAgreeingMovesTheVisitToTheMember(t *testing.T) {
	s := newShop(t)
	member, visit := s.loggedInGuestVisit()
	at, page := s.agree(url.Values{})
	if at != "/account" || !strings.Contains(page, tookOverToast) {
		t.Errorf("agreeing ends on %s without the toast", at)
	}
	if !strings.Contains(page, "<strong>Taro</strong>") {
		t.Error("the account is not the member's")
	}
	got := s.attaches()
	if len(got) != 1 || got[0].SessionID != visit || got[0].MemberID != member || got[0].Key == "" {
		t.Errorf("attaches = %+v, want one of %s to %s with a key", got, member, visit)
	}
	if !slices.Equal(s.memberVisits(member), []string{visit}) {
		t.Errorf("the member's visits are %v, want %s", s.memberVisits(member), visit)
	}
	if !strings.Contains(s.open("/panel"), "$26.00") {
		t.Error("the bill does not show the visit going on")
	}
	if at, _ := s.visit(takeoverPage); at != "/account" {
		t.Errorf("the question is still asked after agreeing: %s", at)
	}
	s.end(visit, "register", 2600)
	s.open("/receipts/" + visit)
	if !strings.Contains(s.open("/receipts"), `href="/receipts/`+visit+`"`) {
		t.Error("the visit history does not list the visit after it ended")
	}
}

// The session and the member come from this browser session alone (UC10): a session or member ID
// in the query or the form names nothing.
func TestASessionIDFromTheClientIsIgnored(t *testing.T) {
	s := newShop(t)
	other := s.otherGuestVisit()
	member, visit := s.loggedInGuestVisit()
	if page := s.open(takeoverPage + "?session_id=" + other); !strings.Contains(page, "$26.00") {
		t.Error("the question does not show this browser's visit")
	}
	s.postResp(takeoverPage, takeoverPage+"?session_id="+other, url.Values{"session_id": {other}, "member_id": {"m_other"}})
	got := s.attaches()
	if len(got) != 1 || got[0].SessionID != visit || got[0].MemberID != member {
		t.Errorf("attaches = %+v, want only %s to this browser's %s", got, member, visit)
	}
}

// A member who did not just sign up or log in from a guest visit here has nothing to agree to,
// whatever session the form names.
func TestAgreeingWithoutAGuestVisitAttachesNothing(t *testing.T) {
	s := newShop(t)
	other := s.otherGuestVisit()
	s.signUp()
	s.setMember(&Member{})
	at, _ := s.postResp("/account", takeoverPage+"?session_id="+other, url.Values{"session_id": {other}})
	if at != "/account" || len(s.attaches()) != 0 {
		t.Errorf("agreeing without a guest visit ends on %s with attaches %+v", at, s.attaches())
	}
}

// A takeover holds only for the member who started it, on the visit this browser still has.
func TestATakeoverHoldsOnlyForItsMemberAndThisBrowsersVisit(t *testing.T) {
	t0 := &Takeover{SessionID: "ses_1", MemberID: "m_taro"}
	for _, tt := range []struct {
		b    Browser
		want bool
	}{
		{Browser{MemberID: "m_taro", SessionID: "ses_1", Takeover: t0}, true},
		{Browser{MemberID: "m_hana", SessionID: "ses_1", Takeover: t0}, false},
		{Browser{MemberID: "", SessionID: "ses_1", Takeover: t0}, false},
		{Browser{MemberID: "m_taro", SessionID: "ses_2", Takeover: t0}, false},
		{Browser{MemberID: "m_taro", SessionID: "ses_1"}, false},
	} {
		if got := pendingTakeover(tt.b) != nil; got != tt.want {
			t.Errorf("member %q, session %q, takeover %v: holds %v, want %v", tt.b.MemberID, tt.b.SessionID, tt.b.Takeover != nil, got, tt.want)
		}
	}
}

func TestAgreeingNeedsTheFormsCSRFToken(t *testing.T) {
	s := newShop(t)
	s.loggedInGuestVisit()
	resp, err := s.client.PostForm(s.app.URL+takeoverPage, url.Values{"csrf": {"forged"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(s.attaches()) != 0 {
		t.Errorf("a forged agreement answered %d with attaches %+v", resp.StatusCode, s.attaches())
	}
}

// Declining asks BarPay nothing. The member stays logged in, and the visit stays this browser's,
// as a guest visit paid at the register.
func TestDecliningKeepsTheVisitAGuestsAndTheMemberLoggedIn(t *testing.T) {
	s := newShop(t)
	member, _ := s.loggedInGuestVisit()
	at, page := s.postResp(takeoverPage, takeoverPage+"/decline", url.Values{})
	if at != "/account" || !strings.Contains(page, "<span>This visit stays a guest visit. Your account is used from your next visit</span>") {
		t.Errorf("declining ends on %s without saying the visit stays a guest's", at)
	}
	if !strings.Contains(page, "<strong>Taro</strong>") {
		t.Error("declining logged the member out")
	}
	if n := len(s.attaches()); n != 0 {
		t.Errorf("%d attaches after declining", n)
	}
	if len(s.memberVisits(member)) != 0 {
		t.Error("the declined visit is in the member's visits")
	}
	if !strings.Contains(s.open("/panel"), "$26.00") {
		t.Error("the guest visit is no longer this browser's")
	}
	if at, _ := s.visit(takeoverPage); at != "/account" {
		t.Errorf("the question is still asked after declining: %s", at)
	}
}

func TestEachRefusalIsToldAndTheVisitStaysAGuests(t *testing.T) {
	const other = "Couldn&#39;t move this visit to your account. It stays a guest visit. Please ask a staff member"
	for _, tt := range []struct {
		code   string
		status int
		want   string
	}{
		{"session_has_member", 409, "This visit is already on an account, so it can&#39;t move to yours. It stays a guest visit"},
		{"session_not_active", 409, "This visit is already being paid or has ended, so it can&#39;t move to your account. It stays a guest visit"},
		{"member_has_open_session", 409, "You&#39;re checked in on another device, so this visit can&#39;t move to your account. It stays a guest visit"},
		{"session_not_found", 404, "This visit couldn&#39;t be found, so nothing moved to your account"},
		{"idempotency_key_reused", 409, other},
		{"invalid_request", 400, other},
		{"unauthorized", 401, other},
	} {
		s := newShop(t)
		member, _ := s.loggedInGuestVisit()
		s.attachFails(tt.code, tt.status)
		at, page := s.agree(url.Values{})
		if at != "/account" || !strings.Contains(page, `<p class="notice bad" role="alert"><i data-icon="alert"></i><span>`+tt.want+`</span></p>`) {
			t.Errorf("%s: ends on %s without the red notice %q", tt.code, at, tt.want)
		}
		if len(s.memberVisits(member)) != 0 {
			t.Errorf("%s: the visit is in the member's visits", tt.code)
		}
		if at, _ := s.visit(takeoverPage); at != "/account" {
			t.Errorf("%s: the question is still asked", tt.code)
		}
		if keys := s.pendingKeys(); len(keys) != 0 {
			t.Errorf("%s: a key is kept after a definite answer: %v", tt.code, keys)
		}
	}
}

// A session that has already left active is not offered, so the member is not asked to agree to
// what BarPay would refuse.
func TestAVisitNoLongerActiveIsNotOffered(t *testing.T) {
	s := newShop(t)
	s.memberWithAccount()
	visit := s.inGuestVisit()
	s.setMember(&Member{})
	s.barpay.mu.Lock()
	s.barpay.sessions[visit].State = "at_register"
	s.barpay.mu.Unlock()
	at, page := s.logIn()
	if at != "/account" || strings.Contains(page, takeoverQuestion) ||
		!strings.Contains(page, "<span>This visit is already being paid or has ended, so it can&#39;t move to your account. It stays a guest visit</span>") {
		t.Errorf("a visit at the register ends on %s, offered or without the notice", at)
	}
}

// An answer that never arrived, or a request still in progress or failing in BarPay, leaves the
// outcome unknown: the key is kept, and trying again sends it unchanged.
func TestAnUnknownOutcomeIsTriedAgainWithTheSameKey(t *testing.T) {
	defer func(d []time.Duration) { resendDelays = d }(resendDelays)
	resendDelays = []time.Duration{0, 0, 0}
	for _, cause := range []string{"lost", "idempotency_key_in_progress", "internal_error", "redirect"} {
		s := newShop(t)
		member, visit := s.loggedInGuestVisit()
		switch cause {
		case "lost":
			s.barpay.attachLost = len(resendDelays) + 1
		case "idempotency_key_in_progress":
			s.attachFails(cause, http.StatusConflict)
		case "redirect":
			// Not a refusal BarPay's spec gives, though it carries an error code.
			s.attachFails("moved", http.StatusFound)
		default:
			s.attachFails(cause, http.StatusInternalServerError)
		}
		_, page := s.agree(url.Values{})
		if !strings.Contains(page, "<h1>Couldn&#39;t connect</h1>") || !strings.Contains(page, `<form method="post" action="/account/takeover">`) {
			t.Fatalf("%s: does not offer to try again", cause)
		}
		if len(s.pendingKeys()) != 1 {
			t.Errorf("%s: the key is not kept", cause)
		}
		s.attachFails("", 0)
		at, _ := s.agree(url.Values{})
		if at != "/account" || !slices.Equal(s.memberVisits(member), []string{visit}) {
			t.Errorf("%s: trying again ends on %s, the member's visits %v", cause, at, s.memberVisits(member))
		}
		got := s.attaches()
		for _, a := range got {
			if a.Key != got[0].Key {
				t.Errorf("%s: attaches = %+v, want every try with the same key", cause, got)
				break
			}
		}
	}
}

// A key left by an unknown outcome is dropped once the app learns the session ended, since no
// attach can be tried on it again; another session's key stays.
func TestAnAttachKeyIsDroppedWhenItsSessionEnds(t *testing.T) {
	defer func(d []time.Duration) { resendDelays = d }(resendDelays)
	resendDelays = []time.Duration{0, 0, 0}
	s := newShop(t)
	_, visit := s.loggedInGuestVisit()
	s.a.store.update(func(d *Data) error { d.PendingKeys["attach:"+visit+"0:m_x"] = "k"; return nil })
	s.barpay.attachLost = len(resendDelays) + 1
	s.agree(url.Values{})
	if len(s.pendingKeys()) != 2 {
		t.Fatalf("keys %v, want the attach's kept beside the other", s.pendingKeys())
	}
	s.end(visit, "register", 2600)
	s.open("/panel")
	if keys := s.pendingKeys(); len(keys) != 1 || keys["attach:"+visit+"0:m_x"] == "" {
		t.Errorf("keys %v after the session ended, want only the other session's", keys)
	}
}

// An agreement that went through although its answer was lost is found by reading the session,
// whether the member opens the question again or declines it.
func TestAnAgreementThatWentThroughWithoutAnAnswerIsFinished(t *testing.T) {
	defer func(d []time.Duration) { resendDelays = d }(resendDelays)
	resendDelays = []time.Duration{0, 0, 0}
	for _, then := range []string{"open", "decline"} {
		s := newShop(t)
		member, visit := s.loggedInGuestVisit()
		s.barpay.attachLost, s.barpay.attachLostLate = len(resendDelays)+1, true
		s.agree(url.Values{})
		var page string
		if then == "open" {
			page = s.open(takeoverPage)
		} else {
			_, page = s.postResp("/panel", takeoverPage+"/decline", url.Values{})
		}
		if !strings.Contains(page, tookOverToast) || !slices.Equal(s.memberVisits(member), []string{visit}) {
			t.Errorf("%s after a lost answer: the visit is not the member's", then)
		}
	}
}

// BarPay may have forgotten the key of an attach that went through: a session that is already the
// member's is the attach done.
func TestAVisitAlreadyTheMembersIsTheAttachDone(t *testing.T) {
	s := newShop(t)
	member, visit := s.loggedInGuestVisit()
	csrf := csrfPattern.FindStringSubmatch(s.open(takeoverPage))[1]
	s.barpay.mu.Lock()
	s.barpay.sessions[visit].MemberID = &member
	s.barpay.mu.Unlock()
	s.attachFails("session_has_member", http.StatusConflict)
	resp, err := s.client.PostForm(s.app.URL+takeoverPage, url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	at, page := resp.Request.URL.Path, s.read(resp)
	if at != "/account" || !strings.Contains(page, tookOverToast) {
		t.Errorf("an attach already done ends on %s without the toast", at)
	}
}

// After session_has_member, only the session's member tells done from refused: a session that
// cannot be read leaves the outcome unknown, and the member can try again.
func TestASessionHasMemberThatCannotBeCheckedIsUnknown(t *testing.T) {
	defer func(d []time.Duration) { resendDelays = d }(resendDelays)
	resendDelays = []time.Duration{0, 0, 0}
	s := newShop(t)
	member, visit := s.loggedInGuestVisit()
	csrf := csrfPattern.FindStringSubmatch(s.open(takeoverPage))[1]
	s.attachFails("session_has_member", http.StatusConflict)
	s.barpay.mu.Lock()
	s.barpay.sessions[visit].MemberID = &member
	s.barpay.sessionReadFails = true
	s.barpay.mu.Unlock()
	resp, err := s.client.PostForm(s.app.URL+takeoverPage, url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	if !strings.Contains(page, "<h1>Couldn&#39;t connect</h1>") || !strings.Contains(page, `<form method="post" action="/account/takeover">`) {
		t.Error("a session_has_member whose session cannot be read does not offer to try again")
	}
	if strings.Contains(page, "This visit is already on an account") {
		t.Error("a session_has_member whose session cannot be read is told as refused")
	}
	if s.browserState().Takeover == nil {
		t.Fatal("the takeover was dropped")
	}
	s.barpay.mu.Lock()
	s.barpay.sessionReadFails = false
	s.barpay.mu.Unlock()
	if at, page := s.visit(takeoverPage); at != "/account" || !strings.Contains(page, tookOverToast) {
		t.Errorf("opening the question again ends on %s without the toast", at)
	}
}

// A member in another visit would be refused by BarPay (member_has_open_session), so they are not
// asked, and the guest visit stays this browser's to be paid here.
func TestALoginInAnotherVisitKeepsTheGuestVisitWithoutAsking(t *testing.T) {
	s := newShop(t)
	s.memberInVisitElsewhere()
	visit := s.inGuestVisit()
	at, page := s.logIn()
	if at != "/account" || strings.Contains(page, takeoverQuestion) ||
		!strings.Contains(page, `<p class="notice bad" role="alert"><i data-icon="alert"></i><span>You&#39;re checked in on another device, so this visit can&#39;t move to your account. It stays a guest visit</span></p>`) {
		t.Errorf("a login in another visit ends on %s, asking or without the notice", at)
	}
	if b := s.browserState(); b.SessionID != visit || b.Takeover != nil {
		t.Errorf("the browser has visit %s and takeover %v, want the guest visit %s and none", b.SessionID, b.Takeover, visit)
	}
	if n := len(s.attaches()); n != 0 {
		t.Errorf("%d attaches", n)
	}
}

// A visit the app still holds as the member's open one but BarPay has ended does not stop the
// question.
func TestAnOtherVisitThatEndedDoesNotStopTheQuestion(t *testing.T) {
	s := newShop(t)
	member, elsewhere := s.memberInVisitElsewhere()
	s.end(elsewhere, "register", 2600)
	s.inGuestVisit()
	if at, _ := s.logIn(); at != takeoverPage {
		t.Errorf("a login after the other visit ended ends on %s", at)
	}
	s.a.store.view(func(d *Data) {
		if open := d.Members[member].OpenSessionID; open != "" {
			t.Errorf("the ended visit %s is still the member's open one", open)
		}
	})
}

// A guest visit the app knows has ended is not offered: the login follows the member's own open
// visit.
func TestALoginAfterTheGuestVisitEndedFollowsTheMembersOwnVisit(t *testing.T) {
	s := newShop(t)
	_, elsewhere := s.memberInVisitElsewhere()
	visit := s.inGuestVisit()
	s.end(visit, "register", 2600)
	s.open("/panel")
	at, _ := s.logIn()
	if at != "/" {
		t.Errorf("a login after the guest visit ended ends on %s", at)
	}
	if b := s.browserState(); b.SessionID != elsewhere || b.Takeover != nil {
		t.Errorf("the browser has visit %s and takeover %v, want the member's %s and none", b.SessionID, b.Takeover, elsewhere)
	}
}

func TestDecliningNeedsTheFormsCSRFToken(t *testing.T) {
	s := newShop(t)
	s.loggedInGuestVisit()
	for _, form := range []url.Values{{"csrf": {"forged"}}, {}} {
		resp, err := s.client.PostForm(s.app.URL+takeoverPage+"/decline", form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || s.browserState().Takeover == nil {
			t.Errorf("a decline with csrf %q answered %d, takeover kept %v", form.Get("csrf"), resp.StatusCode, s.browserState().Takeover != nil)
		}
	}
}
