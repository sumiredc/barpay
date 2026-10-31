package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/store"
)

// line is one order line of value cents in USD.
func line(name string, quantity, value int64) string {
	return fmt.Sprintf(`{"name":%q,"quantity":%d,"unit_price":{"value":%d,"currency":"USD"}}`, name, quantity, value)
}

func orderBody(orderID string, lines ...string) string {
	return fmt.Sprintf(`{"order_id":%q,"lines":[%s]}`, orderID, strings.Join(lines, ","))
}

func (f *fixture) order(sessionID, body string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/sessions/" + sessionID + "/orders", body: body})
}

// lines keeps the lines as sent, so two records compare with ==.
type lines string

func (l *lines) UnmarshalJSON(b []byte) error { *l = lines(b); return nil }

type recorded struct {
	OrderID    string                `json:"order_id"`
	Lines      lines                 `json:"lines"`
	Amount     struct{ Value int64 } `json:"amount"`
	RecordedAt string                `json:"recorded_at"`
}

func decodeOrder201(t *testing.T, w *httptest.ResponseRecorder, status int) recorded {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, body %s; want %d", w.Code, w.Body, status)
	}
	spectest.Valid(t, "Order", w.Body.Bytes())
	var r recorded
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) orderCount(t *testing.T) int { return f.count(t, "SELECT count(*) FROM orders") }

func (f *fixture) lateOrderAlerts(t *testing.T, sessionID string) int {
	return f.count(t, "SELECT count(*) FROM alerts WHERE kind = 'late_order_rejected' AND session_id = ?", sessionID)
}

// UC4 main flow and postcondition: the order is recorded with its amount and is in the total.
func TestOrderIsRecordedAndInTheTotal(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(12 * time.Minute))
	r := decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 2, 650), line("Fries", 1, 500))), http.StatusCreated)
	if r.OrderID != "o-1" || r.Amount.Value != 1800 || r.RecordedAt != "2026-10-31T18:12:00Z" ||
		string(r.Lines) != "["+line("Beer", 2, 650)+","+line("Fries", 1, 500)+"]" {
		t.Errorf("order = %+v, lines %s", r, r.Lines)
	}
	got := f.looked(t, v.SessionID)
	if got.OrdersTotal.Value != 1800 || got.OrderCount != 1 || got.CoverCharge.Value != 200 || got.Total.Value != 2000 {
		t.Errorf("session = %+v; want orders 1800 (1 order) + cover charge 200 = 2000", got)
	}
}

// The first order of a visit does not pause the cover charge; the second does.
func TestSecondOrderPausesTheCoverCharge(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(5 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	if got := f.looked(t, v.SessionID); got.CoverChargeStatus != "running" {
		t.Errorf("after the first order: %s, want running", got.CoverChargeStatus)
	}
	f.clk.Set(t0.Add(10 * time.Minute))
	if got := f.looked(t, v.SessionID).CoverCharge.Value; got != 200 {
		t.Errorf("block ending after the first order: cover charge %d, want 200", got)
	}
	f.clk.Set(t0.Add(15 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-2", line("Beer", 1, 650))), http.StatusCreated)
	if got := f.looked(t, v.SessionID); got.CoverChargeStatus != "paused" {
		t.Errorf("after the second order: %s, want paused", got.CoverChargeStatus)
	}
	f.clk.Set(t0.Add(20 * time.Minute))
	if got := f.looked(t, v.SessionID).CoverCharge.Value; got != 200 {
		t.Errorf("block ending in the pause: cover charge %d, want still 200", got)
	}
}

// An order for a session that is not active is refused, not recorded, and raised as an alert
// for that session.
func TestOrderForASessionNotActive(t *testing.T) {
	for _, state := range States {
		if state == Active {
			continue
		}
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			v := f.checkedIn(t, "")
			f.setState(t, v.SessionID, state)
			assertError(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusConflict, "session_not_active")
			if f.orderCount(t) != 0 {
				t.Error("the order was recorded")
			}
			if n := f.lateOrderAlerts(t, v.SessionID); n != 1 {
				t.Errorf("%d late_order_rejected alerts for the session, want 1", n)
			}
		})
	}
}

// The same order reported again is answered as first recorded, does not restart the pause, and
// raises no alert even once the session is no longer active.
func TestSameOrderAgain(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(time.Minute))
	second := orderBody("o-2", line("Beer", 1, 650))
	first := decodeOrder201(t, f.order(v.SessionID, second), http.StatusCreated)

	f.clk.Set(t0.Add(20 * time.Minute))
	again := decodeOrder201(t, f.order(v.SessionID, second), http.StatusOK)
	if again.RecordedAt != first.RecordedAt || again.Amount != first.Amount {
		t.Errorf("again = %+v, want the first record %+v", again, first)
	}
	// The pause runs from the first report of o-2 for 30 minutes, not from the repeat.
	f.clk.Set(t0.Add(31 * time.Minute))
	if got := f.looked(t, v.SessionID); got.CoverChargeStatus != "running" || got.OrderCount != 2 || got.OrdersTotal.Value != 1300 {
		t.Errorf("after the pause: %+v; want running with 2 orders of 1300", got)
	}

	f.setState(t, v.SessionID, Closed)
	decodeOrder201(t, f.order(v.SessionID, second), http.StatusOK)
	if f.lateOrderAlerts(t, v.SessionID) != 0 || f.orderCount(t) != 2 {
		t.Errorf("after the session closed: %d alerts, %d orders; want none and 2", f.lateOrderAlerts(t, v.SessionID), f.orderCount(t))
	}
}

func TestSameOrderIDWithOtherLines(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	body := orderBody("o-1", line("Beer", 1, 650))
	first := decodeOrder201(t, f.order(v.SessionID, body), http.StatusCreated)
	for _, other := range []string{
		orderBody("o-1", line("Beer", 2, 650)),
		orderBody("o-1", line("Beer", 1, 650), line("Fries", 1, 500)),
		orderBody("o-1", line("Ale", 1, 650)),
	} {
		assertError(t, f.order(v.SessionID, other), http.StatusConflict, "order_conflict")
	}
	if again := decodeOrder201(t, f.order(v.SessionID, body), http.StatusOK); again != first {
		t.Errorf("recorded order = %+v, want unchanged %+v", again, first)
	}
	if f.orderCount(t) != 1 {
		t.Errorf("%d orders, want 1", f.orderCount(t))
	}
}

// The order ID is unique within a session only: another session may use it.
func TestOrderIDIsPerSession(t *testing.T) {
	f := newFixture(t)
	a, b := f.checkedIn(t, ""), f.checkedIn(t, "")
	body := orderBody("o-1", line("Beer", 1, 650))
	decodeOrder201(t, f.order(a.SessionID, body), http.StatusCreated)
	decodeOrder201(t, f.order(b.SessionID, body), http.StatusCreated)
}

func TestOrderRefusesBodiesOutsideTheSchema(t *testing.T) {
	ok := line("Beer", 1, 650)
	cases := []struct{ name, body, field string }{
		{"not JSON", `order`, ""},
		{"null", `null`, ""},
		{"an array", `[]`, ""},
		{"two values", orderBody("o-1", ok) + `{}`, ""},
		{"order_id missing", `{"lines":[` + ok + `]}`, "order_id"},
		{"order_id empty", orderBody("", ok), "order_id"},
		{"order_id a number", `{"order_id":1,"lines":[` + ok + `]}`, "order_id"},
		{"order_id with a space", orderBody("o 1", ok), "order_id"},
		{"order_id too long", orderBody(strings.Repeat("o", 129), ok), "order_id"},
		{"lines missing", `{"order_id":"o-1"}`, "lines"},
		{"lines null", `{"order_id":"o-1","lines":null}`, "lines"},
		{"no lines", orderBody("o-1"), "lines"},
		{"101 lines", orderBody("o-1", strings.Split(strings.Repeat(ok+"\x00", 101), "\x00")[:101]...), "lines"},
		{"a line not an object", `{"order_id":"o-1","lines":[1]}`, "lines[0]"},
		{"name missing", `{"order_id":"o-1","lines":[{"quantity":1,"unit_price":{"value":1,"currency":"USD"}}]}`, "lines[0].name"},
		{"name empty", orderBody("o-1", ok, line("", 1, 650)), "lines[1].name"},
		{"name too long", orderBody("o-1", line(strings.Repeat("é", 201), 1, 650)), "lines[0].name"},
		{"quantity 0", orderBody("o-1", line("Beer", 0, 650)), "lines[0].quantity"},
		{"quantity 100", orderBody("o-1", line("Beer", 100, 650)), "lines[0].quantity"},
		{"quantity a fraction", strings.Replace(orderBody("o-1", ok), `"quantity":1`, `"quantity":1.5`, 1), "lines[0].quantity"},
		{"quantity a string", strings.Replace(orderBody("o-1", ok), `"quantity":1`, `"quantity":"1"`, 1), "lines[0].quantity"},
		{"negative unit price", orderBody("o-1", line("Beer", 1, -1)), "lines[0].unit_price.value"},
		{"unit price above Money", orderBody("o-1", line("Beer", 1, 100_000_001)), "lines[0].unit_price.value"},
		{"unit price null", `{"order_id":"o-1","lines":[{"name":"x","quantity":1,"unit_price":null}]}`, "lines[0].unit_price"},
		{"currency missing", `{"order_id":"o-1","lines":[{"name":"x","quantity":1,"unit_price":{"value":1}}]}`, "lines[0].unit_price.currency"},
		{"currency not in the enum", strings.Replace(orderBody("o-1", ok), "USD", "EUR", 1), "lines[0].unit_price.currency"},
		{"unknown field", strings.Replace(orderBody("o-1", ok), `"order_id"`, `"note":"x","order_id"`, 1), "note"},
		{"unknown line field", strings.Replace(orderBody("o-1", ok, ok), `{"name":"Beer"`, `{"Name":"Beer"`, 1), "lines[0].Name"},
		{"duplicate key", strings.Replace(orderBody("o-1", ok), `"quantity":1`, `"quantity":1,"quantity":2`, 1), "lines[0].quantity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			v := f.checkedIn(t, "")
			w := f.order(v.SessionID, c.body)
			assertError(t, w, http.StatusBadRequest, "invalid_request")
			if _, field := errorOf(t, w); field != c.field {
				t.Errorf("details.field = %q, want %q", field, c.field)
			}
			if f.orderCount(t) != 0 {
				t.Error("the order was recorded")
			}
		})
	}
	// The schema's bounds themselves pass.
	f := newFixture(t)
	v := f.checkedIn(t, "")
	lines := strings.Split(strings.Repeat(line(strings.Repeat("é", 200), 99, 100_000_000)+"\x00", 100), "\x00")[:100]
	w := f.order(v.SessionID, orderBody(strings.Repeat("o", 128), lines...))
	if r := decodeOrder201(t, w, http.StatusCreated); r.Amount.Value != 100*99*100_000_000 {
		t.Errorf("amount = %d", r.Amount.Value)
	}
	decodeOrder201(t, f.order(v.SessionID, orderBody("free", line("Water", 1, 0))), http.StatusCreated)
	// A number with an integer value is an integer, as the schema counts it, and is recorded as one.
	body := strings.Replace(strings.Replace(orderBody("whole", line("Beer", 1, 650)), `"quantity":1`, `"quantity":2.0`, 1), `"value":650`, `"value":6.5e2`, 1)
	if r := decodeOrder201(t, f.order(v.SessionID, body), http.StatusCreated); r.Amount.Value != 1300 || string(r.Lines) != "["+line("Beer", 2, 650)+"]" {
		t.Errorf("2.0 × 6.5e2: %+v", r)
	}
}

// A number is an integer when its exact value is one, however it is written.
func TestIntegerReadsTheExactValue(t *testing.T) {
	for raw, want := range map[string]int64{"2": 2, "2.0": 2, "2e0": 2, "0.5e1": 5, "20E-1": 2, "-3": -3, "650.000": 650} {
		if got, ok := integer(json.RawMessage(raw)); !ok || got != want {
			t.Errorf("%s: %d, %v; want %d", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"2.0000000000000001", "99.99999999999999999", "1.5", "0.5e0", "1e-1",
		"1e100", "1e999999999", "2." + strings.Repeat("0", 63), "9223372036854775808", `"2"`, "true", "null", "[2]"} {
		if got, ok := integer(json.RawMessage(raw)); ok {
			t.Errorf("%s: read as %d, want refused", raw, got)
		}
	}
}

// Each check comes before the next in the order the spec lists, shown by a request that fails
// both of two adjacent checks.
func TestOrderChecksInTheSpecOrder(t *testing.T) {
	f := newFixture(t)
	jpy := strings.Replace(line("Sake", 1, 800), "USD", "JPY", 1)

	// The body before the session.
	assertError(t, f.order("ses_AAAAAAAAAAAAAAAAAAAAAAAAAA", `{"order_id":""}`), http.StatusBadRequest, "invalid_request")

	// A recorded order_id before the currency.
	v := f.checkedIn(t, "")
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	assertError(t, f.order(v.SessionID, orderBody("o-1", jpy)), http.StatusConflict, "order_conflict")

	// The currency before the state: no alert.
	closed := f.checkedIn(t, "")
	f.setState(t, closed.SessionID, Closed)
	assertError(t, f.order(closed.SessionID, orderBody("o-2", jpy)), http.StatusUnprocessableEntity, "currency_mismatch")
	if n := f.lateOrderAlerts(t, closed.SessionID); n != 0 {
		t.Errorf("%d alerts after a currency mismatch, want 0", n)
	}

	// The state before the total: refused as not active, with an alert.
	err := f.st.Tx(context.Background(), func(tx store.Querier) error {
		return insertOrder(context.Background(), tx, closed.SessionID,
			order{id: "big", lines: []byte(`[]`), amount: maxOrdersTotal, recordedAt: f.clk.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	assertError(t, f.order(closed.SessionID, orderBody("o-3", line("Beer", 1, 650))), http.StatusConflict, "session_not_active")
	if n := f.lateOrderAlerts(t, closed.SessionID); n != 1 {
		t.Errorf("%d alerts, want 1", n)
	}
}

func TestOrderInAnotherCurrency(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	body := orderBody("o-1", line("Beer", 1, 650), strings.Replace(line("Sake", 1, 800), "USD", "JPY", 1))
	assertError(t, f.order(v.SessionID, body), http.StatusUnprocessableEntity, "currency_mismatch")
	if f.orderCount(t) != 0 {
		t.Error("the order was recorded")
	}
}

// An order for an ID no session has is 404, and raises no alert.
func TestOrderForAnUnknownSession(t *testing.T) {
	f := newFixture(t)
	f.checkedIn(t, "")
	for _, id := range []string{"ses_AAAAAAAAAAAAAAAAAAAAAAAAAA", "nope"} {
		assertError(t, f.order(id, orderBody("o-1", line("Beer", 1, 650))), http.StatusNotFound, "session_not_found")
	}
	if n := f.count(t, "SELECT count(*) FROM alerts"); n != 0 || f.orderCount(t) != 0 {
		t.Errorf("%d alerts, %d orders; want none", n, f.orderCount(t))
	}
}

// The same order sent several times at once is recorded once.
func TestConcurrentSameOrder(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	body := orderBody("o-1", line("Beer", 1, 650))
	const n = 10
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { codes <- f.order(v.SessionID, body).Code })
	}
	wg.Wait()
	close(codes)
	created := 0
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Errorf("status %d", c)
		}
	}
	if created != 1 || f.orderCount(t) != 1 {
		t.Errorf("created = %d, orders = %d; want 1", created, f.orderCount(t))
	}
}

// An order that would bring the total above maxOrdersTotal is refused; one that reaches it
// exactly is recorded.
func TestOrderAboveTheLargestTotal(t *testing.T) {
	f := newFixture(t)
	// big checks in with an order recorded directly, 1000 below maxOrdersTotal.
	big := func() View {
		t.Helper()
		v := f.checkedIn(t, "")
		err := f.st.Tx(context.Background(), func(tx store.Querier) error {
			return insertOrder(context.Background(), tx, v.SessionID,
				order{id: "big", lines: []byte(`[]`), amount: maxOrdersTotal - 1000, recordedAt: f.clk.Now()})
		})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	a := big()
	assertError(t, f.order(a.SessionID, orderBody("o-1", line("Beer", 1, 1001))), http.StatusUnprocessableEntity, "amount_too_large")
	if f.orderCount(t) != 1 {
		t.Fatal("the refused order was recorded")
	}
	decodeOrder201(t, f.order(a.SessionID, orderBody("o-2", line("Beer", 1, 1000))), http.StatusCreated)
	if got := f.looked(t, a.SessionID).Total.Value; got != maxOrdersTotal {
		t.Errorf("total = %d, want %d", got, int64(maxOrdersTotal))
	}

	// The cover charge so far counts: after a block of 200, 801 more is too much.
	b := big()
	f.clk.Advance(10 * time.Minute)
	assertError(t, f.order(b.SessionID, orderBody("o-1", line("Beer", 1, 801))), http.StatusUnprocessableEntity, "amount_too_large")
	decodeOrder201(t, f.order(b.SessionID, orderBody("o-2", line("Beer", 1, 800))), http.StatusCreated)
}

// ticking is a clock that moves forward a microsecond at every reading, so every operation
// gets its own instant.
type ticking struct{ *clock.Fake }

func (c ticking) Now() time.Time {
	c.Advance(time.Microsecond)
	return c.Fake.Now()
}

// Orders racing the moment the total is fixed are either recorded before it, and in the fixed
// total, or refused as not active; none is added after it.
func TestOrdersRacingTheFixedTotal(t *testing.T) {
	for range 5 {
		f := newFixture(t)
		clk := ticking{f.clk}
		f.buildWith(clk)
		v := f.checkedIn(t, "")

		const n = 20
		codes := make([]int, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				codes[i] = f.order(v.SessionID, orderBody(fmt.Sprintf("o-%d", i), line("Beer", 1, 100))).Code
			})
		}
		wg.Go(func() {
			err := f.st.Tx(context.Background(), func(tx store.Querier) error {
				_, err := tx.ExecContext(context.Background(), "UPDATE sessions SET state = 'checking_out', total_fixed_at = ? WHERE id = ?",
					clk.Now().UnixNano(), v.SessionID)
				return err
			})
			if err != nil {
				t.Error(err)
			}
		})
		wg.Wait()

		recorded := 0
		for i, c := range codes {
			var inStore, beforeFixed int
			err := f.st.Read().QueryRowContext(context.Background(), `SELECT count(*), coalesce(sum(o.recorded_at < s.total_fixed_at), 0)
				FROM orders o JOIN sessions s ON s.id = o.session_id WHERE o.order_id = ?`, fmt.Sprintf("o-%d", i)).Scan(&inStore, &beforeFixed)
			if err != nil {
				t.Fatal(err)
			}
			switch c {
			case http.StatusCreated:
				recorded++
				if inStore != 1 || beforeFixed != 1 {
					t.Errorf("o-%d: 201 but stored %d, before the fixed total %d", i, inStore, beforeFixed)
				}
			case http.StatusConflict:
				if inStore != 0 {
					t.Errorf("o-%d: refused but stored", i)
				}
			default:
				t.Errorf("o-%d: status %d", i, c)
			}
		}
		if got := f.looked(t, v.SessionID); got.OrdersTotal.Value != int64(100*recorded) || got.OrderCount != recorded {
			t.Errorf("fixed total has %d orders of %d; want the %d recorded", got.OrderCount, got.OrdersTotal.Value, recorded)
		}
	}
}

// Each operation is refused without the store API key, and changes nothing.
func TestSessionOperationsNeedTheStoreAPIKey(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	ops := map[string]call{
		"createCheckin": {method: http.MethodPost, path: "/v1/checkins", key: "key-x", body: `{}`},
		"getSession":    {method: http.MethodGet, path: "/v1/sessions/" + v.SessionID},
		"reportOrder":   {method: http.MethodPost, path: "/v1/sessions/" + v.SessionID + "/orders", body: orderBody("o-1", line("Beer", 1, 650))},
	}
	for name, c := range ops {
		for _, auth := range []string{noAuth, "Basic k-store", "Bearer wrong", "Bearer k-admin", "Bearer k-edge"} {
			c.auth = auth
			w := f.do(c)
			assertError(t, w, http.StatusUnauthorized, "unauthorized")
			if strings.Contains(w.Body.String(), v.SessionID) {
				t.Errorf("%s with %s: the body shows the session", name, auth)
			}
		}
	}
	if f.sessions(t) != 1 || f.orderCount(t) != 0 || f.notifications(t) != 1 || f.count(t, "SELECT count(*) FROM idempotency_keys") != 1 {
		t.Error("something changed")
	}
}

// Sessions, orders and the kept Idempotency-Key responses survive a restart.
func TestSessionsOrdersAndKeysSurviveARestart(t *testing.T) {
	f := newFixture(t)
	first := f.checkin("key-1", `{"member_id":"m_1"}`)
	v := decodeView(t, first.Body.Bytes())
	f.clk.Set(t0.Add(time.Minute))
	ordered := decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(15 * time.Minute))
	before := f.getSession(v.SessionID).Body.String()

	f.restart(t)
	if after := f.getSession(v.SessionID).Body.String(); after != before {
		t.Errorf("after restart: %s; want %s", after, before)
	}
	if again := f.checkin("key-1", `{"member_id":"m_1"}`); again.Code != http.StatusCreated || again.Body.String() != first.Body.String() {
		t.Errorf("the same key after restart: %d %s; want the first response", again.Code, again.Body)
	}
	if again := decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusOK); again != ordered {
		t.Errorf("the same order after restart: %+v; want %+v", again, ordered)
	}
	if f.sessions(t) != 1 || f.orderCount(t) != 1 {
		t.Errorf("sessions = %d, orders = %d; want 1 each", f.sessions(t), f.orderCount(t))
	}
}
