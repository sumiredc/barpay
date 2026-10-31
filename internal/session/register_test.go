package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/spectest"
)

const (
	adminAuth = "Bearer k-admin"
	edgeAuth  = "Bearer k-edge"
)

func (f *fixture) requestRegister(id string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/sessions/" + id + "/register-request"})
}

// requested requests the register and returns the session, failing the test unless it is 200.
func (f *fixture) requested(t *testing.T, id string) View {
	t.Helper()
	w := f.requestRegister(id)
	if w.Code != http.StatusOK {
		t.Fatalf("requestRegister: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Session", w.Body.Bytes())
	return decodeView(t, w.Body.Bytes())
}

// events returns the bodies of the session's notifications of type typ, oldest first.
func (f *fixture) events(t *testing.T, sessionID, typ string) [][]byte {
	t.Helper()
	rows, err := f.st.Read().QueryContext(context.Background(),
		"SELECT body FROM notifications WHERE subject_id = ? AND type = ? ORDER BY seq", sessionID, typ)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) alerts(t *testing.T, sessionID, kind string) int {
	return f.count(t, "SELECT count(*) FROM alerts WHERE session_id = ? AND kind = ?", sessionID, kind)
}

func (f *fixture) state(t *testing.T, id string) State {
	t.Helper()
	s, err := get(context.Background(), f.st.Read(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s.State
}

// UC6 main flow: the request fixes the final total and stops the cover charge, sets the payment
// method to the register and waits at the register, with an alert and session.register_requested.
func TestRequestRegisterFixesTheTotalAndWaitsAtTheRegister(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(5 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 2, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(25 * time.Minute))
	got := f.requested(t, v.SessionID)
	if got.State != AtRegister || got.PaymentMethod != "register" || got.CoverChargeStatus != "stopped" ||
		got.TotalFixedAt != "2026-10-31T18:25:00Z" || got.CoverCharge.Value != 400 || got.Total.Value != 1300+400 {
		t.Errorf("session = %+v; want at_register by the register, stopped at 18:25 with cover charge 400 and total 1700", got)
	}
	if n := f.alerts(t, v.SessionID, "register_requested"); n != 1 {
		t.Errorf("%d register_requested alerts, want 1", n)
	}
	events := f.events(t, v.SessionID, "session.register_requested")
	if len(events) != 1 {
		t.Fatalf("%d session.register_requested events, want 1", len(events))
	}
	spectest.Valid(t, "SessionRegisterRequestedEvent", events[0])
	if f.wake.n.Load() == 0 {
		t.Error("the dispatcher was not woken")
	}
	if f.state(t, v.SessionID) != AtRegister {
		t.Error("the stored session is not at the register")
	}
}

// UC6 main flow: a block that ends after the request is not charged, however long the customer stays.
func TestBlockEndingAfterTheRequestIsNotCharged(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(25 * time.Minute))
	want := f.requested(t, v.SessionID)
	for _, d := range []time.Duration{30 * time.Minute, 3 * time.Hour} {
		f.clk.Set(t0.Add(d))
		if got := f.looked(t, v.SessionID); got.CoverCharge.Value != want.CoverCharge.Value || got.Total.Value != want.Total.Value {
			t.Errorf("at t0+%v: cover charge %d, total %d; want %d, %d as at the request",
				d, got.CoverCharge.Value, got.Total.Value, want.CoverCharge.Value, want.Total.Value)
		}
	}
}

// UC6 precondition and exception: a linked session pays by PayPal until it requests the register,
// and by the register from then on.
// TODO(#18): request the register for a linked session end to end once linking exists.
func TestPaymentMethodOfALinkedSessionAtTheRegister(t *testing.T) {
	for _, c := range []struct {
		linked bool
		state  State
		want   string
	}{
		{true, Active, "paypal"},
		{true, AtRegister, "register"},
		{false, Active, "register"},
		{false, AtRegister, "register"},
	} {
		if got := paymentMethod(c.linked, c.state); got != c.want {
			t.Errorf("linked %v, %s: %s, want %s", c.linked, c.state, got, c.want)
		}
	}
}

// UC6 exception: the request cannot be withdrawn; repeating it returns the session as it is and
// notifies nothing more.
func TestRequestRegisterAgainReturnsTheSessionAsItIs(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "m_1")
	f.clk.Set(t0.Add(25 * time.Minute))
	first := f.requestRegister(v.SessionID).Body.String()
	f.clk.Set(t0.Add(time.Hour))
	w := f.requestRegister(v.SessionID)
	if w.Code != http.StatusOK || w.Body.String() != first {
		t.Errorf("repeat: %d %s; want 200 %s", w.Code, w.Body, first)
	}
	if n := len(f.events(t, v.SessionID, "session.register_requested")); n != 1 {
		t.Errorf("%d session.register_requested events, want 1", n)
	}
	if n := f.alerts(t, v.SessionID, "register_requested"); n != 1 {
		t.Errorf("%d register_requested alerts, want 1", n)
	}
	if f.state(t, v.SessionID) != AtRegister {
		t.Error("the session left the register")
	}
}

// Two requests at once move the session once and notify once; both get the session at the register.
func TestConcurrentRegisterRequestsMoveOnce(t *testing.T) {
	for range 20 {
		f := newFixture(t)
		v := f.checkedIn(t, "")
		var wg sync.WaitGroup
		codes := make([]int, 4)
		for i := range codes {
			wg.Go(func() { codes[i] = f.requestRegister(v.SessionID).Code })
		}
		wg.Wait()
		for _, c := range codes {
			if c != http.StatusOK {
				t.Fatalf("codes = %v, want all 200", codes)
			}
		}
		if n := len(f.events(t, v.SessionID, "session.register_requested")); n != 1 {
			t.Fatalf("%d session.register_requested events, want 1", n)
		}
	}
}

// requestRegister: a session checking out, whose payment failed or that ended is refused, and
// nothing changes.
func TestRequestRegisterForASessionNotActive(t *testing.T) {
	for _, state := range []State{CheckingOut, PaymentFailed, Closed, ClosedUnpaid} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			v := f.checkedIn(t, "")
			f.setState(t, v.SessionID, state)
			before := f.notifications(t)
			assertError(t, f.requestRegister(v.SessionID), http.StatusConflict, "session_not_active")
			if f.state(t, v.SessionID) != state || f.notifications(t) != before || f.alerts(t, v.SessionID, "register_requested") != 0 {
				t.Error("the refused request changed something")
			}
		})
	}
}

func TestRequestRegisterForAnUnknownSession(t *testing.T) {
	f := newFixture(t)
	f.checkedIn(t, "")
	assertError(t, f.requestRegister("ses_AAAAAAAAAAAAAAAAAAAAAAAAAA"), http.StatusNotFound, "session_not_found")
}

// Only the store API key requests the register.
func TestRequestRegisterNeedsTheStoreAPIKey(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	for _, auth := range []string{noAuth, adminAuth, edgeAuth} {
		w := f.do(call{method: http.MethodPost, path: "/v1/sessions/" + v.SessionID + "/register-request", auth: auth})
		assertError(t, w, http.StatusUnauthorized, "unauthorized")
	}
	if f.state(t, v.SessionID) != Active {
		t.Error("an unauthorized request moved the session")
	}
}
