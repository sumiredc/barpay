package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// visitsBarPay is adminBarPay with the sessions of the visits page: it lists them by state and
// answers the checkout requests, keeping what it received.
type visitsBarPay struct {
	*adminBarPay
	vmu sync.Mutex
	// list is every session, newest check-in first; a read lists those of the states asked for.
	// full answers a read with a next cursor.
	list []Session
	full bool
	// reads are the state filters of the reads of the list, in order.
	reads []string
	// answers are the sessions the checkout requests answer with, by action; an action without
	// one is refused with 409. An answered session replaces its own in the list.
	answers map[string]Session
	sent    []sentRequest
	// beforeAnswer, when set, runs before a checkout request is answered.
	beforeAnswer func()
}

type sentRequest struct {
	Action, SessionID, IdempotencyKey string
	Body                              map[string]Money
}

func (f *visitsBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	f.vmu.Lock()
	defer f.vmu.Unlock()
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == "GET" && p == "/v1/admin/sessions":
		states := r.URL.Query().Get("state")
		f.reads = append(f.reads, states)
		page := []Session{}
		for _, s := range f.list {
			if states == "" || strings.Contains(","+states+",", ","+s.State+",") {
				page = append(page, s)
			}
		}
		var next any
		if f.full {
			next = "cursor-2"
		}
		write(http.StatusOK, map[string]any{"sessions": page, "next_cursor": next})
	case r.Method == "POST" && strings.HasPrefix(p, "/v1/admin/sessions/"):
		id, action, _ := strings.Cut(strings.TrimPrefix(p, "/v1/admin/sessions/"), "/")
		var body map[string]Money
		json.NewDecoder(r.Body).Decode(&body)
		f.sent = append(f.sent, sentRequest{Action: action, SessionID: id, IdempotencyKey: r.Header.Get("Idempotency-Key"), Body: body})
		s, ok := f.answers[action]
		if !ok {
			write(http.StatusConflict, map[string]any{"error": map[string]string{"code": "session_not_active", "message": "refused for the test"}})
			return
		}
		for i := range f.list {
			if f.list[i].SessionID == s.SessionID {
				f.list[i] = s
			}
		}
		if hook := f.beforeAnswer; hook != nil {
			f.vmu.Unlock()
			hook()
			f.vmu.Lock()
		}
		write(http.StatusOK, s)
	case r.Method == "POST" && strings.HasPrefix(p, "/v1/sessions/"):
		id, action, _ := strings.Cut(strings.TrimPrefix(p, "/v1/sessions/"), "/")
		f.sent = append(f.sent, sentRequest{Action: action, SessionID: id})
		write(http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": p}})
	default:
		f.vmu.Unlock()
		f.adminBarPay.ServeHTTP(w, r)
		f.vmu.Lock()
	}
}

func (f *visitsBarPay) received() []sentRequest {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	return append([]sentRequest(nil), f.sent...)
}

// newVisitsStaff is newStaff whose BarPay is a visitsBarPay holding sessions.
func newVisitsStaff(t *testing.T, sessions ...Session) (*staff, *visitsBarPay) {
	t.Helper()
	f := &visitsBarPay{adminBarPay: &adminBarPay{sessions: map[string]*Session{}}, list: sessions, answers: map[string]Session{}}
	barpay := httptest.NewServer(f)
	t.Cleanup(barpay.Close)
	st, err := openStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("http://127.0.0.1")
	cfg := &Config{StoreName: "The Copper Fox", PublicURL: public, BarPayURL: barpay.URL, SigningSecret: "secret",
		Staff: map[string]string{"staff-01": hashPassword(staffPassword)}}
	a := &App{cfg: cfg, barpay: newBarPay(cfg), store: st, pages: loadPages(), dummyHash: hashPassword("x")}
	app := httptest.NewServer(a.routes())
	t.Cleanup(app.Close)
	jar, _ := cookiejar.New(nil)
	s := &staff{customer: &customer{t: t, app: app, client: &http.Client{Jar: jar}}, barpay: f.adminBarPay, a: a}
	s.logIn()
	f.vmu.Lock()
	f.reads = nil
	f.vmu.Unlock()
	return s, f
}

func usdOf(v int64) Money { return Money{Value: v, Currency: "USD"} }

var visitCheckedIn = time.Date(2026, 11, 5, 20, 5, 0, 0, time.Local)

// visitIn is a session in state, checked in at 20:05 with a total of $52.00; a session past
// active has its total fixed at 21:45.
func visitIn(id, state string, memberID string) Session {
	s := Session{SessionID: id, State: state, PaymentMethod: "register", CheckedInAt: visitCheckedIn,
		CoverChargeStatus: "running", OrdersTotal: usdOf(3700), CoverCharge: usdOf(1500), Total: usdOf(5200)}
	if memberID != "" {
		s.MemberID = &memberID
		s.PaymentMethod = "paypal"
	}
	if state != "active" {
		fixed := time.Date(2026, 11, 5, 21, 45, 0, 0, time.Local)
		s.TotalFixedAt = &fixed
		s.CoverChargeStatus = "stopped"
	}
	if state == "payment_failed" || state == "closed_unpaid" {
		unpaid := s.Total
		s.UnpaidAmount = &unpaid
	}
	return s
}

// checkout sends the checkout dialog of a session from the page at from, with the form fields,
// and returns the page it comes back to.
func (s *staff) checkout(from, sessionID string, form url.Values) string {
	s.t.Helper()
	form.Set("back", from)
	if form.Get("currency") == "" {
		form.Set("currency", "USD")
	}
	return s.post(from, "/admin/sessions/"+sessionID+"/checkout", form)
}

// ways are the ways the checkout dialog of a session offers, in order; nil without the dialog.
func ways(page, sessionID string) []string {
	d := sheet(page, "pay-"+sessionID)
	if d == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(d, `name="how" value="`)[1:] {
		out = append(out, part[:strings.Index(part, `"`)])
	}
	return out
}

func TestTheNeedsCheckoutTabAsksForThePaymentFailedAndTheAtRegisterVisits(t *testing.T) {
	s, f := newVisitsStaff(t)
	page := s.open("/admin")
	f.vmu.Lock()
	reads := append([]string(nil), f.reads...)
	f.vmu.Unlock()
	if len(reads) != 1 || reads[0] != "payment_failed,at_register" {
		t.Errorf("the visits page read the sessions with %q, want once with payment_failed,at_register", reads)
	}
	mustContain(t, page, `aria-current="page">Needs checkout`, "No one is waiting to check out")
}

func TestEachTabListsItsStates(t *testing.T) {
	s, f := newVisitsStaff(t)
	for tab, want := range map[string]string{"here": "active,checking_out", "done": "closed,closed_unpaid"} {
		s.open("/admin?tab=" + tab)
		f.vmu.Lock()
		reads := append([]string(nil), f.reads...)
		f.vmu.Unlock()
		if !slices.Contains(reads, want) {
			t.Errorf("tab %s read the sessions with %q, want %s among them", tab, reads, want)
		}
	}
}

func TestAChargePayPalRefusedOffersToChargeAgainFirst(t *testing.T) {
	s, _ := newVisitsStaff(t, visitIn("ses_k", "payment_failed", "mem_ken"))
	s.member("mem_ken", "Ken Sato")
	failed := visitIn("ses_k", "payment_failed", "mem_ken")
	notify(t, s.app.URL, "secret", "evt_1", "session.payment_failed", 1, time.Date(2026, 11, 5, 21, 46, 0, 0, time.Local), failed, "refused")
	page := s.open("/admin")
	if got := ways(page, "ses_k"); strings.Join(got, ",") != "retry,outside" {
		t.Errorf("ways = %v, want retry then outside", got)
	}
	d := sheet(page, "pay-ses_k")
	mustContain(t, d, "Check out Ken Sato", "<dt>Last result</dt><dd>PayPal declined</dd>", "Charge PayPal again", "Settled outside BarPay",
		"This fails if the customer has deleted their saved PayPal.")
	mustLack(t, d, "PayPal dashboard")
}

func TestAChargeWhoseOutcomeIsUnknownSaysToCheckPayPalFirst(t *testing.T) {
	for _, reason := range []string{"unconfirmed", "no notification"} {
		s, _ := newVisitsStaff(t, visitIn("ses_s", "payment_failed", "mem_sora"))
		if reason != "no notification" {
			notify(t, s.app.URL, "secret", "evt_1", "session.payment_failed", 1, time.Now(), visitIn("ses_s", "payment_failed", "mem_sora"), reason)
		}
		page := s.open("/admin")
		if got := ways(page, "ses_s"); strings.Join(got, ",") != "outside,retry" {
			t.Errorf("%s: ways = %v, want outside then retry", reason, got)
		}
		d := sheet(page, "pay-ses_s")
		mustContain(t, d, "The last charge&#39;s outcome is unknown. First check in the PayPal dashboard whether it went through.",
			"It went through (settled outside BarPay)", "It didn&#39;t (charge PayPal again)", "If the last charge went through, it isn&#39;t charged twice.")
		if strings.Index(d, "PayPal dashboard") > strings.Index(d, `name="how"`) {
			t.Errorf("%s: the dashboard comes after the ways", reason)
		}
	}
}

func TestARetryAfterTheLastRefusalIsTakenAsUnknown(t *testing.T) {
	refused := VisitEvent{Type: "session.payment_failed", Sequence: 1, At: time.Date(2026, 11, 5, 21, 46, 0, 0, time.Local), Reason: "refused"}
	rec := Visit{
		Events:       []VisitEvent{refused},
		StaffActions: []StaffAction{{Action: "retry", StaffID: "staff-01", At: time.Date(2026, 11, 5, 21, 50, 0, 0, time.Local), AfterSequence: 1}},
	}
	if got := paymentFailedReason(rec); got != "unconfirmed" {
		t.Errorf("reason = %s, want unconfirmed: the retry's own outcome has not been notified", got)
	}
	rec.StaffActions = nil
	if got := paymentFailedReason(rec); got != "refused" {
		t.Errorf("reason = %s, want refused", got)
	}
}

// refusedRetry is a member's visit whose charge PayPal refused (sequence 3), with the payment
// failure BarPay notifies of it.
func refusedRetry(t *testing.T) (*staff, *visitsBarPay, Session) {
	failed := visitIn("ses_k", "payment_failed", "mem_ken")
	s, f := newVisitsStaff(t, failed)
	s.member("mem_ken", "Ken Sato")
	notify(t, s.app.URL, "secret", "evt_3", "session.payment_failed", 3, time.Date(2026, 11, 5, 21, 40, 0, 0, time.Local), failed, "refused")
	f.answers["retry"] = failed
	return s, f, failed
}

// The result of a retry is the payment failure of a higher sequence, whether it arrives before
// or after the retry's answer: BarPay stores it before it answers. It is told by the sequence,
// not the time: here BarPay's clock is two minutes behind the app's.
func TestARetryRefusedAgainOffersToChargeAgainWheneverItsResultArrives(t *testing.T) {
	for _, when := range []string{"before the answer", "after the answer"} {
		s, f, failed := refusedRetry(t)
		result := func() {
			notify(t, s.app.URL, "secret", "evt_4", "session.payment_failed", 4, time.Now().Add(-2*time.Minute), failed, "refused")
		}
		if when == "before the answer" {
			f.beforeAnswer = result
		}
		s.checkout("/admin", "ses_k", url.Values{"how": {"retry"}})
		if when == "after the answer" {
			result()
		}
		page := s.open("/admin")
		if got := ways(page, "ses_k"); strings.Join(got, ",") != "retry,outside" {
			t.Errorf("%s: ways = %v, want retry then outside", when, got)
		}
		history := sheet(page, "d-ses_k")
		if !regexp.MustCompile(`charged PayPal again → (\d\d:\d\d )?PayPal declined</dd>`).MatchString(history) {
			t.Errorf("%s: the history does not end with the retry, then its refusal: %s", when, history)
		}
	}
}

func TestARetryWhoseResultHasNotArrivedIsTakenAsUnknown(t *testing.T) {
	s, _, _ := refusedRetry(t)
	s.checkout("/admin", "ses_k", url.Values{"how": {"retry"}})
	page := s.open("/admin")
	if got := ways(page, "ses_k"); strings.Join(got, ",") != "outside,retry" {
		t.Errorf("ways = %v, want outside then retry", got)
	}
	mustContain(t, sheet(page, "d-ses_k"), "charged PayPal again → failed, outcome unknown</dd>")
}

// guestHere is a guest checked in at 21:05 with $18.00 so far.
func guestHere() Session {
	s := visitIn("ses_g", "active", "")
	s.CheckedInAt = time.Date(2026, 11, 5, 21, 5, 0, 0, time.Local)
	s.OrdersTotal, s.CoverCharge, s.Total = usdOf(900), usdOf(900), usdOf(1800)
	return s
}

// paidAtTheRegister is guestHere closed with a register payment of took at a total of total.
func paidAtTheRegister(total, took int64) Session {
	s := guestHere()
	fixed := time.Date(2026, 11, 5, 22, 0, 0, 0, time.Local)
	s.State, s.TotalFixedAt, s.CoverChargeStatus = "closed", &fixed, "stopped"
	s.CoverCharge, s.Total = usdOf(total-900), usdOf(total)
	amount := usdOf(took)
	s.Receipt = &Receipt{Settlement: "register", OrdersTotal: usdOf(900), CoverCharge: usdOf(total - 900), Total: usdOf(total), EndedAt: fixed, AmountRecorded: &amount}
	return s
}

func TestAGuestPaidAtTheRegisterIsOneRegisterPaymentOfTheTotalShown(t *testing.T) {
	s, f := newVisitsStaff(t, guestHere())
	f.answers["register-payment"] = paidAtTheRegister(1800, 1800)
	page := s.open("/admin?tab=here")
	if got := ways(page, "ses_g"); strings.Join(got, ",") != "register" {
		t.Errorf("ways of a guest = %v, want register only", got)
	}
	mustContain(t, sheet(page, "pay-ses_g"), "Check out Guest (checked in 21:05)", `<dt>Total so far</dt><dd>$18.00</dd>`,
		`name="amount" value="18.00" required> Took $18.00 (total so far)`, "Another amount",
		"The total is fixed when BarPay receives this. If a block ends in between and the total changes, both the fixed total and the amount you took are kept.")
	after := s.checkout("/admin?tab=here", "ses_g", url.Values{"how": {"register"}, "amount": {"18.00"}})
	sent := f.received()
	if len(sent) != 1 || sent[0].Action != "register-payment" || sent[0].Body["amount_recorded"] != usdOf(1800) {
		t.Fatalf("BarPay got %+v, want one register-payment with amount_recorded $18.00 and no register-request", sent)
	}
	mustContain(t, after, `<span>Checked out.</span>`)
}

func TestATotalFixedAboveTheAmountTakenIsToldWithBoth(t *testing.T) {
	s, f := newVisitsStaff(t, guestHere())
	f.answers["register-payment"] = paidAtTheRegister(1850, 1800)
	after := s.checkout("/admin?tab=here", "ses_g", url.Values{"how": {"register"}, "amount": {"18.00"}})
	mustContain(t, after, "Checked out at a total of $18.50. You took $18.00.")
	done := sheet(s.open("/admin?tab=done"), "d-ses_g")
	mustContain(t, done, "<dt>Total</dt><dd>$18.50 (cover charge $9.50, orders $9.00)</dd>", "<dd>$18.00 at the register (22:00, staff-01)</dd>")
}

func TestAnotherAmountIsTheAmountTyped(t *testing.T) {
	s, f := newVisitsStaff(t, guestHere())
	f.answers["register-payment"] = paidAtTheRegister(1800, 2000)
	s.checkout("/admin?tab=here", "ses_g", url.Values{"how": {"register"}, "amount": {"other"}, "other_amount": {"20"}})
	if sent := f.received(); len(sent) != 1 || sent[0].Body["amount_recorded"] != usdOf(2000) {
		t.Errorf("BarPay got %+v, want amount_recorded $20.00", sent)
	}
}

// guestHereInYen is guestHere in a store that takes yen: ¥1,800 so far.
func guestHereInYen() Session {
	s := guestHere()
	yen := func(v int64) Money { return Money{Value: v, Currency: "JPY"} }
	s.OrdersTotal, s.CoverCharge, s.Total = yen(900), yen(900), yen(1800)
	return s
}

func TestAnAmountInYenIsWholeYen(t *testing.T) {
	s, f := newVisitsStaff(t, guestHereInYen())
	f.answers["register-payment"] = guestHereInYen()
	d := sheet(s.open("/admin?tab=here"), "pay-ses_g")
	mustContain(t, d, `name="amount" value="1800" required> Took ¥1,800 (total so far)`, `placeholder="0"`, `name="currency" value="JPY"`)
	for typed, want := range map[string]int64{"1800": 1800, "2000": 2000, "¥2000": 2000} {
		before := len(f.received())
		amount := url.Values{"how": {"register"}, "currency": {"JPY"}, "amount": {"other"}, "other_amount": {typed}}
		if typed == "1800" {
			amount.Set("amount", "1800")
		}
		s.checkout("/admin?tab=here", "ses_g", amount)
		sent := f.received()
		if len(sent) != before+1 || sent[before].Body["amount_recorded"] != (Money{Value: want, Currency: "JPY"}) {
			t.Errorf("%q: BarPay got %+v, want amount_recorded ¥%d", typed, sent[before:], want)
		}
	}
	before := len(f.received())
	after := s.checkout("/admin?tab=here", "ses_g", url.Values{"how": {"register"}, "currency": {"JPY"}, "amount": {"other"}, "other_amount": {"18.00"}})
	mustContain(t, after, "Enter the amount taken, such as 1250.")
	if len(f.received()) != before {
		t.Error("an amount in yen with a fraction was sent")
	}
}

func TestAnAmountInDollarsIsInCents(t *testing.T) {
	s, f := newVisitsStaff(t, guestHere())
	f.answers["register-payment"] = paidAtTheRegister(1800, 1850)
	mustContain(t, sheet(s.open("/admin?tab=here"), "pay-ses_g"), `placeholder="0.00"`)
	s.checkout("/admin?tab=here", "ses_g", url.Values{"how": {"register"}, "amount": {"other"}, "other_amount": {"18.5"}})
	if sent := f.received(); len(sent) != 1 || sent[0].Body["amount_recorded"] != usdOf(1850) {
		t.Errorf("BarPay got %+v, want amount_recorded $18.50", sent)
	}
}

func TestAnAmountThatIsNotOneIsNotSent(t *testing.T) {
	s, f := newVisitsStaff(t, guestHere())
	after := s.checkout("/admin?tab=here", "ses_g", url.Values{"how": {"register"}, "amount": {"other"}, "other_amount": {"twenty"}})
	mustContain(t, after, `class="notice bad"`, "Enter the amount taken, such as 12.50.")
	if sent := f.received(); len(sent) != 0 {
		t.Errorf("BarPay got %+v", sent)
	}
}

func TestTheStaffWhoRecordedARegisterPaymentIsShownOnThePaidVisit(t *testing.T) {
	s, f := newVisitsStaff(t, guestHere())
	f.answers["register-payment"] = paidAtTheRegister(1800, 1800)
	s.checkout("/admin?tab=here", "ses_g", url.Values{"how": {"register"}, "amount": {"18.00"}})
	page := s.open("/admin?tab=done")
	mustContain(t, sheet(page, "d-ses_g"), "<dt>Payment</dt><dd>$18.00 at the register (22:00, staff-01)</dd>")
	if sheet(page, "pay-ses_g") != "" || strings.Contains(page, `data-sheet="pay-ses_g"`) {
		t.Error("a paid visit has a Check out button")
	}
}

func TestEachStateOffersOnlyItsWaysToCheckOut(t *testing.T) {
	checking := visitIn("ses_x", "checking_out", "mem_x")
	closed := visitIn("ses_x", "closed", "mem_x")
	closed.Receipt = &Receipt{Settlement: "paypal", Total: usdOf(5200), EndedAt: *closed.TotalFixedAt}
	unpaid := visitIn("ses_x", "closed_unpaid", "")
	unpaid.Receipt = &Receipt{Settlement: "unpaid", Total: usdOf(5200), EndedAt: *unpaid.TotalFixedAt}
	notLinked := visitIn("ses_x", "active", "mem_x")
	notLinked.PaymentMethod = "register"
	cases := []struct {
		name string
		s    Session
		tab  string
		want string
	}{
		{"here, a guest", visitIn("ses_x", "active", ""), "here", "register"},
		{"here, a member not linked", notLinked, "here", "register"},
		{"here, a member linked", visitIn("ses_x", "active", "mem_x"), "here", "register,close"},
		{"at the register", visitIn("ses_x", "at_register", ""), "needs", "register,unpaid"},
		{"checking out", checking, "here", ""},
		{"paid", closed, "done", ""},
		{"closed unpaid", unpaid, "done", ""},
	}
	// One app lists every case, each as its own session.
	var sessions []Session
	for i := range cases {
		cases[i].s.SessionID = fmt.Sprintf("ses_%d", i)
		sessions = append(sessions, cases[i].s)
	}
	s, _ := newVisitsStaff(t, sessions...)
	for _, c := range cases {
		id := c.s.SessionID
		page := s.open("/admin?tab=" + c.tab)
		if !strings.Contains(page, `id="d-`+id+`"`) {
			t.Fatalf("%s: the visit is not listed", c.name)
		}
		if got := strings.Join(ways(page, id), ","); got != c.want {
			t.Errorf("%s: ways = %q, want %q", c.name, got, c.want)
		}
		if c.want == "" && strings.Contains(page, `data-sheet="pay-`+id+`"`) {
			t.Errorf("%s: there is a Check out button", c.name)
		}
	}
}

func TestTheDeadlineIsThreeDaysAfterCheckoutBegan(t *testing.T) {
	s, _ := newVisitsStaff(t, visitIn("ses_payment_failed", "payment_failed", ""), visitIn("ses_at_register", "at_register", ""))
	page := s.open("/admin")
	for _, state := range []string{"payment_failed", "at_register"} {
		mustContain(t, sheet(page, "d-ses_"+state), "<dt>Deadline</dt><dd>Closes unpaid on 11/8 at 21:45</dd>")
	}
}

func TestAChargeWhoseAnswerWasLostIsSentAgainWithTheSameKey(t *testing.T) {
	s, f := newVisitsStaff(t, visitIn("ses_t", "active", "mem_taro"))
	closed := visitIn("ses_t", "checking_out", "mem_taro")
	f.answers["close"] = closed
	name := chargeKeyName("close", "ses_t", "staff-01")
	s.a.store.update(func(d *Data) error { d.PendingKeys[name] = "key-of-the-lost-answer"; return nil })
	after := s.checkout("/admin?tab=here", "ses_t", url.Values{"how": {"close"}})
	sent := f.received()
	if len(sent) != 1 || sent[0].Action != "close" || sent[0].IdempotencyKey != "key-of-the-lost-answer" {
		t.Fatalf("BarPay got %+v, want one close with the stored key", sent)
	}
	mustContain(t, after, "The result comes shortly; until then the visit shows Checking out.")
	s.a.store.view(func(d *Data) {
		if _, ok := d.PendingKeys[name]; ok {
			t.Error("the key is kept after the answer arrived")
		}
	})
}

func TestARefusedCheckoutIsARedNoticeAndRecordsNoStaff(t *testing.T) {
	s, _ := newVisitsStaff(t, visitIn("ses_a", "at_register", ""))
	after := s.checkout("/admin", "ses_a", url.Values{"how": {"unpaid"}})
	mustContain(t, after, `class="notice bad"`, "BarPay refused")
	s.a.store.view(func(d *Data) {
		if v := d.Visits["ses_a"]; v != nil && len(v.StaffActions) > 0 {
			t.Errorf("staff actions = %+v", v.StaffActions)
		}
	})
}

func TestTheListShowsTheCustomerAndTheHeadAndTailOfTheSession(t *testing.T) {
	s, _ := newVisitsStaff(t, visitIn("ses_01hx3b2n7k4m9p8w", "payment_failed", "mem_sora"), visitIn("ses_01hx3a7d5f2g8h3j6k4l", "at_register", ""))
	s.member("mem_sora", "Sora Kato")
	page := s.open("/admin")
	mustContain(t, page, `<span class="who">Sora Kato</span><span class="id">ses_01hx3b2n…9p8w</span>`,
		`<span class="who">Guest</span><span class="id">ses_01hx3a7d…6k4l</span>`, `<span class="pill payment_failed">Payment failed</span>`,
		`<span class="pill at_register">At the register</span>`, "<th>Customer</th><th>Checked in</th><th class=\"num\">Total</th><th>State</th>")
	mustContain(t, sheet(page, "d-ses_01hx3b2n7k4m9p8w"), "<dt>Checked in</dt><dd>20:05 (1 h 40 min)</dd>",
		"<dt>Total</dt><dd>$52.00 (cover charge $15.00, orders $37.00)</dd>", "<dt>Payment</dt><dd>PayPal</dd>", "<dt>ID</dt><dd>ses_01hx3b2n7k4m9p8w</dd>")
}

func TestAFullPageOfVisitsWaitingIsCountedAsMore(t *testing.T) {
	var list []Session
	for i := 0; i < 20; i++ {
		list = append(list, visitIn("ses_"+string(rune('a'+i)), "at_register", ""))
	}
	s, f := newVisitsStaff(t, list...)
	f.full = true
	page := s.open("/admin?tab=here")
	mustContain(t, page, `Needs checkout<span class="badge" aria-label="More than 20 waiting to check out">20+</span>`,
		`<span>Visits</span><span class="badge" aria-label="More than 20 waiting to check out">20+</span>`, "Older visits")
}

func TestTheHistoryTellsHowTheVisitCameToWait(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 5, h, m, 0, 0, time.Local) }
	failed := visitIn("ses_s", "payment_failed", "mem_sora")
	register := visitIn("ses_a", "at_register", "")
	register.TotalFixedAt = visitPtr(at(21, 31))
	paid := visitIn("ses_y", "closed", "mem_yui")
	paid.TotalFixedAt = visitPtr(at(21, 59))
	cases := []struct {
		name string
		s    Session
		rec  Visit
		want string
	}{
		{"outcome unknown", failed, Visit{Events: []VisitEvent{{Type: "session.payment_failed", Sequence: 1, At: at(21, 47), Reason: "unconfirmed"}}},
			"21:45 charged PayPal → 21:47 failed, outcome unknown"},
		{"declined within the minute", failed, Visit{Events: []VisitEvent{{Type: "session.payment_failed", Sequence: 1, At: at(21, 45), Reason: "refused"}}},
			"21:45 charged PayPal → PayPal declined"},
		{"no notification", failed, Visit{}, "21:45 charged PayPal → failed, outcome unknown"},
		{"declined, then retried", failed, Visit{
			Events:       []VisitEvent{{Type: "session.payment_failed", Sequence: 1, At: at(21, 45), Reason: "refused"}},
			StaffActions: []StaffAction{{Action: "retry", StaffID: "staff-01", At: at(21, 50), AfterSequence: 1}}},
			"21:45 charged PayPal → PayPal declined → 21:50 charged PayPal again → failed, outcome unknown"},
		{"at the register", register, Visit{}, "21:31 the customer chose the register; the total was fixed"},
		{"paid at the gate, which did not open", paid, Visit{Events: []VisitEvent{
			{Type: "session.closed", Sequence: 2, At: at(21, 59)}, {Type: "gate.unlock_failed", Sequence: 3, At: at(21, 59)}}},
			"21:59 charged PayPal at the exit gate → paid. The gate didn't open"},
		{"closed by staff", paid, Visit{Events: []VisitEvent{{Type: "session.closed", Sequence: 2, At: at(21, 59)}},
			StaffActions: []StaffAction{{Action: "close", StaffID: "staff-01", At: at(21, 59)}}},
			"21:59 charged PayPal → paid"},
	}
	for _, c := range cases {
		if got := visitHistory(c.s, c.rec); got != c.want {
			t.Errorf("%s: history = %q, want %q", c.name, got, c.want)
		}
	}
}

func visitPtr[T any](v T) *T { return &v }

func TestAVisitHereShowsWhetherItCanBeChargedWithPayPal(t *testing.T) {
	paused := guestHere()
	paused.CoverChargeStatus = "paused"
	paused.CoverChargeResumesAt = visitPtr(time.Date(2026, 11, 5, 22, 10, 0, 0, time.Local))
	notLinked := visitIn("ses_h", "active", "mem_h")
	notLinked.PaymentMethod = "register"
	cases := []struct {
		s     Session
		wants []string
	}{
		{paused, []string{"<dt>Total so far</dt><dd>$18.00 (cover charge $9.00, orders $9.00)</dd>", "<dt>Cover charge</dt><dd>free after an order until 22:10</dd>", "<dt>PayPal</dt><dd>not linked (guest)</dd>"}},
		{notLinked, []string{"<dt>PayPal</dt><dd>not linked</dd>"}},
		{visitIn("ses_t", "active", "mem_t"), []string{"<dt>PayPal</dt><dd>linked</dd>"}},
	}
	for _, c := range cases {
		row := visitRowOf(c.s, Visit{}, "", c.s.MemberID != nil, c.s.CheckedInAt.Add(55*time.Minute))
		var b strings.Builder
		for _, l := range row.Detail {
			b.WriteString("<dt>" + l.Label + "</dt><dd>" + l.Value + "</dd>")
		}
		mustContain(t, b.String(), c.wants...)
		mustContain(t, b.String(), "(55 min)")
	}
}
