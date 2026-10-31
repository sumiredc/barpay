package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
)

func amountBody(value int64, cur string) string {
	return fmt.Sprintf(`{"amount_recorded":{"value":%d,"currency":%q}}`, value, cur)
}

func (f *fixture) recordPayment(id, body string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + id + "/register-payment", auth: adminAuth, body: body})
}

// receiptOf decodes the receipt of a session's JSON.
type receiptOf struct {
	Receipt *struct {
		Settlement     string                 `json:"settlement"`
		Total          struct{ Value int64 }  `json:"total"`
		AmountRecorded *struct{ Value int64 } `json:"amount_recorded"`
		UnpaidAmount   *struct{ Value int64 } `json:"unpaid_amount"`
		EndedAt        string                 `json:"ended_at"`
	} `json:"receipt"`
}

func decodeReceipt(t *testing.T, b []byte) receiptOf {
	t.Helper()
	var r receiptOf
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.Receipt == nil {
		t.Fatalf("no receipt in %s", b)
	}
	return r
}

func (f *fixture) staffActions(t *testing.T, id string) []staff.Action {
	t.Helper()
	a, err := staff.Actions(context.Background(), f.st.Read(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// atRegister checks in a guest who orders $13.00 at t0+5m and requests the register at t0+25m,
// for a total of $17.00.
func (f *fixture) atRegister(t *testing.T) View {
	t.Helper()
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(5 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 2, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(25 * time.Minute))
	return f.requested(t, v.SessionID)
}

// UC6 main flow and postcondition: recording the payment closes the session; the receipt shows
// the total and the amount recorded, session.settled_outside follows, and the staff member is
// recorded with it.
func TestRecordRegisterPaymentClosesTheSession(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	f.clk.Set(t0.Add(40 * time.Minute))
	w := f.recordPayment(v.SessionID, amountBody(1700, "USD"))
	if w.Code != http.StatusOK {
		t.Fatalf("recordRegisterPayment: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Session", w.Body.Bytes())
	got, r := decodeView(t, w.Body.Bytes()), decodeReceipt(t, w.Body.Bytes()).Receipt
	if got.State != Closed || got.PaymentMethod != "register" || got.Total.Value != 1700 || got.TotalFixedAt != "2026-10-31T18:25:00Z" || got.UnpaidAmount != nil {
		t.Errorf("session = %+v; want closed with the total 1700 fixed at the request", got)
	}
	if r.Settlement != "register" || r.Total.Value != 1700 || r.AmountRecorded == nil || r.AmountRecorded.Value != 1700 ||
		r.EndedAt != "2026-10-31T18:40:00Z" {
		t.Errorf("receipt = %+v; want register, total and amount recorded 1700, ended at 18:40", r)
	}
	events := f.events(t, v.SessionID, "session.settled_outside")
	if len(events) != 1 {
		t.Fatalf("%d session.settled_outside events, want 1", len(events))
	}
	spectest.Valid(t, "SessionSettledOutsideEvent", events[0])
	if a := f.staffActions(t, v.SessionID); len(a) != 1 || a[0].Staff != "staff_1" || a[0].Operation != "recordRegisterPayment" ||
		!a[0].At.Equal(t0.Add(40*time.Minute)) {
		t.Errorf("staff actions = %+v; want staff_1's recordRegisterPayment at 18:40", a)
	}
	if body := f.getSession(v.SessionID).Body.String(); body != w.Body.String() {
		t.Errorf("lookup %s; want the recorded session %s", body, w.Body)
	}
}

// UC6 alternative flow: a customer who goes straight to the register has the total fixed when
// staff record the payment.
func TestRecordRegisterPaymentForAnActiveSessionFixesTheTotal(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(25 * time.Minute))
	w := f.recordPayment(v.SessionID, amountBody(400, "USD"))
	if w.Code != http.StatusOK {
		t.Fatalf("recordRegisterPayment: %d %s", w.Code, w.Body)
	}
	got := decodeView(t, w.Body.Bytes())
	if got.State != Closed || got.TotalFixedAt != "2026-10-31T18:25:00Z" || got.CoverCharge.Value != 400 || got.Total.Value != 400 {
		t.Errorf("session = %+v; want closed with the cover charge 400 fixed at 18:25", got)
	}
	f.clk.Set(t0.Add(3 * time.Hour))
	if later := f.looked(t, v.SessionID); later.Total.Value != 400 || later.CoverChargeStatus != "stopped" {
		t.Errorf("later: %+v; want the total fixed at 400", later)
	}
}

// UC6 alternative flow: an amount other than the total is recorded as given, next to the total.
func TestRecordRegisterPaymentOfAnotherAmount(t *testing.T) {
	for _, amount := range []int64{1500, 2000, 0} {
		f := newFixture(t)
		v := f.atRegister(t)
		w := f.recordPayment(v.SessionID, amountBody(amount, "USD"))
		if w.Code != http.StatusOK {
			t.Fatalf("recordRegisterPayment %d: %d %s", amount, w.Code, w.Body)
		}
		r := decodeReceipt(t, w.Body.Bytes()).Receipt
		if r.Total.Value != 1700 || r.AmountRecorded.Value != amount {
			t.Errorf("receipt = total %d, recorded %d; want 1700 and %d", r.Total.Value, r.AmountRecorded.Value, amount)
		}
		if got := decodeView(t, w.Body.Bytes()); got.Total.Value != 1700 || got.UnpaidAmount != nil {
			t.Errorf("session = %+v; want the total unchanged and nothing unpaid", got)
		}
	}
}

// recordRegisterPayment: a session checking out, whose payment failed or that ended is refused,
// and nothing changes.
func TestRecordRegisterPaymentForASessionNotActive(t *testing.T) {
	for _, state := range []State{CheckingOut, PaymentFailed, Closed, ClosedUnpaid} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			v := f.checkedIn(t, "")
			f.setState(t, v.SessionID, state)
			before := f.notifications(t)
			assertError(t, f.recordPayment(v.SessionID, amountBody(400, "USD")), http.StatusConflict, "session_not_active")
			if f.state(t, v.SessionID) != state || f.notifications(t) != before || len(f.staffActions(t, v.SessionID)) != 0 ||
				f.count(t, "SELECT count(*) FROM receipts") != 0 {
				t.Error("the refused record changed something")
			}
		})
	}
}

// recordRegisterPayment: an amount in another currency than the store's is refused; nothing changes.
func TestRecordRegisterPaymentInAnotherCurrency(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	before := f.notifications(t)
	assertError(t, f.recordPayment(v.SessionID, amountBody(1700, "JPY")), http.StatusUnprocessableEntity, "currency_mismatch")
	if f.state(t, v.SessionID) != AtRegister || f.notifications(t) != before || len(f.staffActions(t, v.SessionID)) != 0 {
		t.Error("the refused record changed something")
	}
}

// recordRegisterPayment: a retry after a lost response is refused, and lookup shows that the
// first one closed the session.
func TestRecordRegisterPaymentRetriedAfterALostResponse(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	first := f.recordPayment(v.SessionID, amountBody(1700, "USD"))
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	assertError(t, f.recordPayment(v.SessionID, amountBody(1700, "USD")), http.StatusConflict, "session_not_active")
	if got := f.looked(t, v.SessionID); got.State != Closed {
		t.Errorf("lookup: %s, want closed", got.State)
	}
	if n := len(f.events(t, v.SessionID, "session.settled_outside")); n != 1 {
		t.Errorf("%d session.settled_outside events, want 1", n)
	}
}

// The PRD's Guarantees, Durability: the amount recorded is read after a restart, by lookup and
// in the receipt, as before it.
func TestRecordedAmountSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	before := f.recordPayment(v.SessionID, amountBody(1500, "USD")).Body.String()
	f.restart(t)
	f.clk.Set(t0.Add(48 * time.Hour))
	if got := f.getSession(v.SessionID).Body.String(); got != before {
		t.Errorf("after a restart: %s; want %s", got, before)
	}
}

// An ended session is shown from its receipt, also after the store's currency changed.
func TestEndedSessionIsShownAfterACurrencyChange(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	before := f.recordPayment(v.SessionID, amountBody(1700, "USD")).Body.String()
	f.clk.Set(t0.Add(time.Hour))
	jpy := settings.Settings{
		Currency: money.JPY, Floor: money.New(500, money.JPY), Ceiling: money.New(3000, money.JPY),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: money.New(3000, money.JPY), Goal: 50,
	}
	err := f.st.Tx(context.Background(), func(tx store.Querier) error {
		if err := settings.Save(context.Background(), tx, jpy, t0.Add(time.Hour)); err != nil {
			return err
		}
		_, _, err := pricing.PublishFirst(context.Background(), tx, money.New(1500, money.JPY), jpy, t0.Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	w := f.getSession(v.SessionID)
	if w.Code != http.StatusOK || w.Body.String() != before {
		t.Errorf("after the currency change: %d %s; want %s", w.Code, w.Body, before)
	}
}

func TestRecordRegisterPaymentForAnUnknownSession(t *testing.T) {
	f := newFixture(t)
	f.checkedIn(t, "")
	assertError(t, f.recordPayment("ses_AAAAAAAAAAAAAAAAAAAAAAAAAA", amountBody(400, "USD")), http.StatusNotFound, "session_not_found")
	if n := f.count(t, "SELECT count(*) FROM staff_actions"); n != 0 {
		t.Errorf("%d staff actions, want none", n)
	}
}

// A body that does not fit AmountRecordedRequest is refused before anything is read or changed.
func TestRecordRegisterPaymentWithABadBody(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	for body, field := range map[string]string{
		`{}`: "amount_recorded",
		`{"amount_recorded":{"value":-1,"currency":"USD"}}`:               "amount_recorded.value",
		`{"amount_recorded":{"value":1.5,"currency":"USD"}}`:              "amount_recorded.value",
		`{"amount_recorded":{"value":9007199254740992,"currency":"USD"}}`: "amount_recorded.value",
		`{"amount_recorded":{"value":1,"currency":"EUR"}}`:                "amount_recorded.currency",
		`{"amount_recorded":{"value":1}}`:                                 "amount_recorded.currency",
		`{"amount_recorded":{"value":1,"currency":"USD","x":1}}`:          "amount_recorded.x",
		`{"amount_recorded":{"value":1,"currency":"USD"},"note":"x"}`:     "note",
		`[]`:   "",
		`null`: "",
	} {
		w := f.recordPayment(v.SessionID, body)
		assertError(t, w, http.StatusBadRequest, "invalid_request")
		if _, got := errorOf(t, w); got != field {
			t.Errorf("%s: field %q, want %q", body, got, field)
		}
	}
	if w := f.recordPayment(v.SessionID, `{"amount_recorded":{"value":9007199254740991,"currency":"USD"}}`); w.Code != http.StatusOK {
		t.Errorf("the largest Total: %d %s, want 200", w.Code, w.Body)
	}
}

// UC7 postcondition: a record without BarPay-Staff-Id is refused and changes nothing.
func TestRecordRegisterPaymentNeedsTheStaffID(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	w := f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + v.SessionID + "/register-payment",
		auth: adminAuth, staff: noAuth, body: amountBody(1700, "USD")})
	assertError(t, w, http.StatusBadRequest, "invalid_request")
	if _, field := errorOf(t, w); field != "BarPay-Staff-Id" {
		t.Errorf("field %q, want BarPay-Staff-Id", field)
	}
	if f.state(t, v.SessionID) != AtRegister || len(f.events(t, v.SessionID, "session.settled_outside")) != 0 {
		t.Error("the refused record changed something")
	}
}

// Only the admin API key records a register payment.
func TestRecordRegisterPaymentNeedsTheAdminAPIKey(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	for _, auth := range []string{noAuth, storeAuth, edgeAuth} {
		w := f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + v.SessionID + "/register-payment",
			auth: auth, body: amountBody(1700, "USD")})
		assertError(t, w, http.StatusUnauthorized, "unauthorized")
	}
	if f.state(t, v.SessionID) != AtRegister {
		t.Error("an unauthorized request closed the session")
	}
}
