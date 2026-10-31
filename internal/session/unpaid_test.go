package session

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/spectest"
)

func (f *fixture) markUnpaid(id string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + id + "/mark-unpaid", auth: adminAuth})
}

// endEvents counts the session's notifications that report how it ended.
func (f *fixture) endEvents(t *testing.T, id string) int {
	return f.count(t, `SELECT count(*) FROM notifications WHERE subject_id = ?
		AND type IN ('session.closed', 'session.settled_outside', 'session.closed_unpaid')`, id)
}

// UC6 exception, UC7: marking a session at the register unpaid records the unpaid total and
// closes it, session.closed_unpaid follows, and the staff member is recorded with it.
func TestMarkUnpaidClosesTheSessionWithTheUnpaidTotal(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	f.clk.Set(t0.Add(2 * time.Hour))
	w := f.markUnpaid(v.SessionID)
	if w.Code != http.StatusOK {
		t.Fatalf("markUnpaid: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Session", w.Body.Bytes())
	got, r := decodeView(t, w.Body.Bytes()), decodeReceipt(t, w.Body.Bytes()).Receipt
	if got.State != ClosedUnpaid || got.PaymentMethod != "register" || got.UnpaidAmount == nil || got.UnpaidAmount.Value != 1700 {
		t.Errorf("session = %+v; want closed_unpaid with 1700 unpaid", got)
	}
	if r.Settlement != "unpaid" || r.Total.Value != 1700 || r.UnpaidAmount == nil || r.UnpaidAmount.Value != 1700 ||
		r.AmountRecorded != nil || r.EndedAt != "2026-10-31T20:00:00Z" {
		t.Errorf("receipt = %+v; want unpaid 1700 of 1700, ended at 20:00", r)
	}
	events := f.events(t, v.SessionID, "session.closed_unpaid")
	if len(events) != 1 {
		t.Fatalf("%d session.closed_unpaid events, want 1", len(events))
	}
	spectest.Valid(t, "SessionClosedUnpaidEvent", events[0])
	if a := f.staffActions(t, v.SessionID); len(a) != 1 || a[0].Staff != "staff_1" || a[0].Operation != "markUnpaid" {
		t.Errorf("staff actions = %+v; want staff_1's markUnpaid", a)
	}
}

// markUnpaid: a session that is not waiting at the register is refused, and nothing changes.
func TestMarkUnpaidForASessionNotAtTheRegister(t *testing.T) {
	for _, state := range []State{Active, CheckingOut, PaymentFailed, Closed, ClosedUnpaid} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			v := f.checkedIn(t, "")
			f.setState(t, v.SessionID, state)
			before := f.notifications(t)
			assertError(t, f.markUnpaid(v.SessionID), http.StatusConflict, "session_not_at_register")
			if f.state(t, v.SessionID) != state || f.notifications(t) != before || len(f.staffActions(t, v.SessionID)) != 0 {
				t.Error("the refused request changed something")
			}
		})
	}
}

// UC6, UC7: a record and an unpaid mark at the same time: one goes through, the other is refused
// with its own code, and the store app is told of one end only.
func TestRecordAndMarkUnpaidAtOnce(t *testing.T) {
	for range 30 {
		f := newFixture(t)
		v := f.atRegister(t)
		var record, unpaid *httptest.ResponseRecorder
		var wg sync.WaitGroup
		wg.Go(func() { record = f.recordPayment(v.SessionID, amountBody(1700, "USD")) })
		wg.Go(func() { unpaid = f.markUnpaid(v.SessionID) })
		wg.Wait()
		switch {
		case record.Code == http.StatusOK && unpaid.Code != http.StatusOK:
			assertError(t, unpaid, http.StatusConflict, "session_not_at_register")
			if s := f.state(t, v.SessionID); s != Closed {
				t.Errorf("state %s after the record won, want closed", s)
			}
		case unpaid.Code == http.StatusOK && record.Code != http.StatusOK:
			assertError(t, record, http.StatusConflict, "session_not_active")
			if s := f.state(t, v.SessionID); s != ClosedUnpaid {
				t.Errorf("state %s after the unpaid mark won, want closed_unpaid", s)
			}
		default:
			t.Fatalf("record %d %s, unpaid %d %s; want exactly one 200", record.Code, record.Body, unpaid.Code, unpaid.Body)
		}
		if n := f.endEvents(t, v.SessionID); n != 1 {
			t.Fatalf("%d end notifications, want 1", n)
		}
		if n := f.count(t, "SELECT count(*) FROM staff_actions"); n != 1 {
			t.Fatalf("%d staff actions, want 1", n)
		}
	}
}

// The PRD's Guarantees, Durability: the unpaid amount is read after a restart.
func TestUnpaidAmountSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	before := f.markUnpaid(v.SessionID).Body.String()
	f.restart(t)
	if got := f.getSession(v.SessionID).Body.String(); got != before {
		t.Errorf("after a restart: %s; want %s", got, before)
	}
}

func TestMarkUnpaidForAnUnknownSession(t *testing.T) {
	f := newFixture(t)
	f.checkedIn(t, "")
	assertError(t, f.markUnpaid("ses_AAAAAAAAAAAAAAAAAAAAAAAAAA"), http.StatusNotFound, "session_not_found")
}

// UC7 postcondition: an unpaid mark without BarPay-Staff-Id is refused and changes nothing.
func TestMarkUnpaidNeedsTheStaffID(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	w := f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + v.SessionID + "/mark-unpaid", auth: adminAuth, staff: noAuth})
	assertError(t, w, http.StatusBadRequest, "invalid_request")
	if _, field := errorOf(t, w); field != "BarPay-Staff-Id" {
		t.Errorf("field %q, want BarPay-Staff-Id", field)
	}
	if f.state(t, v.SessionID) != AtRegister || f.endEvents(t, v.SessionID) != 0 {
		t.Error("the refused request changed something")
	}
}

// Only the admin API key marks a session unpaid.
func TestMarkUnpaidNeedsTheAdminAPIKey(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	for _, auth := range []string{noAuth, storeAuth, edgeAuth} {
		w := f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + v.SessionID + "/mark-unpaid", auth: auth})
		assertError(t, w, http.StatusUnauthorized, "unauthorized")
	}
	if f.state(t, v.SessionID) != AtRegister {
		t.Error("an unauthorized request closed the session")
	}
}
