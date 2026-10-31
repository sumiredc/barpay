package session

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/spectest"
)

// requestedAt is when atRegister requests the register; its 3-day limit is 3 days later.
var (
	requestedAt = t0.Add(25 * time.Minute)
	limit       = requestedAt.Add(72 * time.Hour)
)

func (f *fixture) closeDue(t *testing.T) int {
	t.Helper()
	n, err := f.api.CloseDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// UC6 exception: a session waiting at the register is closed as unpaid from exactly 3 days after
// the request, with the unpaid total recorded and session.closed_unpaid; not a moment before.
func TestSessionAtTheRegisterIsClosedUnpaidAtThreeDays(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	f.clk.Set(limit.Add(-time.Nanosecond))
	if n := f.closeDue(t); n != 0 || f.state(t, v.SessionID) != AtRegister {
		t.Fatalf("just before 3 days: closed %d, state %s; want none closed", n, f.state(t, v.SessionID))
	}
	w0 := f.wake.n.Load()
	f.clk.Set(limit)
	if n := f.closeDue(t); n != 1 {
		t.Fatalf("at exactly 3 days: closed %d, want 1", n)
	}
	got := f.looked(t, v.SessionID)
	r := decodeReceipt(t, f.getSession(v.SessionID).Body.Bytes()).Receipt
	if got.State != ClosedUnpaid || got.UnpaidAmount == nil || got.UnpaidAmount.Value != 1700 ||
		r.Settlement != "unpaid" || r.UnpaidAmount.Value != 1700 || r.EndedAt != "2026-11-03T18:25:00Z" {
		t.Errorf("session = %+v, receipt %+v; want closed_unpaid with 1700 unpaid at the limit", got, r)
	}
	events := f.events(t, v.SessionID, "session.closed_unpaid")
	if len(events) != 1 {
		t.Fatalf("%d session.closed_unpaid events, want 1", len(events))
	}
	spectest.Valid(t, "SessionClosedUnpaidEvent", events[0])
	if f.wake.n.Load() <= w0 {
		t.Error("the dispatcher was not woken")
	}
	if len(f.staffActions(t, v.SessionID)) != 0 {
		t.Error("a staff member was recorded for the limit")
	}
	f.clk.Set(limit.Add(time.Hour))
	if n := f.closeDue(t); n != 0 || f.endEvents(t, v.SessionID) != 1 {
		t.Errorf("later: closed %d again, %d end notifications; want 0 and 1", n, f.endEvents(t, v.SessionID))
	}
}

// Just after the limit, the session is closed too: the loop catches up on one it missed.
func TestSessionPastThreeDaysIsClosedUnpaid(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	f.clk.Set(limit.Add(time.Nanosecond))
	if n := f.closeDue(t); n != 1 || f.state(t, v.SessionID) != ClosedUnpaid {
		t.Errorf("just after 3 days: closed %d, state %s; want closed_unpaid", n, f.state(t, v.SessionID))
	}
}

// Staff can record a payment or mark the session unpaid until just before the limit.
func TestStaffActJustBeforeThreeDays(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	f.clk.Set(limit.Add(-time.Nanosecond))
	if w := f.recordPayment(v.SessionID, amountBody(1700, "USD")); w.Code != http.StatusOK {
		t.Errorf("record just before 3 days: %d %s, want 200", w.Code, w.Body)
	}

	f = newFixture(t)
	v = f.atRegister(t)
	f.clk.Set(limit.Add(-time.Nanosecond))
	if w := f.markUnpaid(v.SessionID); w.Code != http.StatusOK {
		t.Errorf("unpaid mark just before 3 days: %d %s, want 200", w.Code, w.Body)
	}
}

// From the limit on, a staff operation is refused as for a session that has ended, even before
// the loop closed it, and the session is closed as unpaid once, without a staff member.
func TestStaffActAtThreeDays(t *testing.T) {
	for _, op := range []struct {
		name string
		do   func(f *fixture, id string) *httptest.ResponseRecorder
	}{
		{"record", func(f *fixture, id string) *httptest.ResponseRecorder {
			return f.recordPayment(id, amountBody(1700, "USD"))
		}},
		{"unpaid", (*fixture).markUnpaid},
	} {
		t.Run(op.name, func(t *testing.T) {
			f := newFixture(t)
			v := f.atRegister(t)
			w0 := f.wake.n.Load()
			f.clk.Set(limit)
			want := map[string]string{"record": "session_not_active", "unpaid": "session_not_at_register"}[op.name]
			assertError(t, op.do(f, v.SessionID), http.StatusConflict, want)
			if got := f.looked(t, v.SessionID); got.State != ClosedUnpaid || got.Receipt == nil {
				t.Errorf("lookup: %+v; want closed_unpaid with its receipt", got)
			}
			if f.endEvents(t, v.SessionID) != 1 || len(f.events(t, v.SessionID, "session.closed_unpaid")) != 1 ||
				len(f.staffActions(t, v.SessionID)) != 0 {
				t.Error("want exactly one session.closed_unpaid and no staff member recorded")
			}
			if f.wake.n.Load() <= w0 {
				t.Error("the dispatcher was not woken for the close")
			}
		})
	}
}

// The judge refuses a staff operation from the limit on with errPastLimit, even when the session
// read is still at the register, as when the limit passes between the read and the operation.
func TestJudgeRefusesStaffOperationsFromTheLimit(t *testing.T) {
	s := Session{State: AtRegister, TotalFixedAt: requestedAt}
	for _, c := range []struct {
		t    trigger
		at   time.Time
		want error
	}{
		{registerPaymentRecorded, limit.Add(-time.Nanosecond), nil},
		{registerPaymentRecorded, limit, errPastLimit},
		{markedUnpaid, limit.Add(-time.Nanosecond), nil},
		{markedUnpaid, limit, errPastLimit},
		{unpaidLimit, limit.Add(-time.Nanosecond), errRefused},
		{unpaidLimit, limit, nil},
		{registerRequested, limit, errRefused}, // no edge leaves the register for it
	} {
		if got := judge(s, c.t, c.at); got != c.want {
			t.Errorf("%s at %v: %v, want %v", c.t, c.at, got, c.want)
		}
	}
	if !errors.Is(errPastLimit, errRefused) {
		t.Error("errPastLimit is not an errRefused")
	}
}

// From the limit on, a repeated register request is refused: the session has ended, and lookup
// shows it closed as unpaid once.
func TestRequestRegisterAtThreeDays(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	f.clk.Set(limit)
	assertError(t, f.requestRegister(v.SessionID), http.StatusConflict, "session_not_active")
	if got := f.looked(t, v.SessionID); got.State != ClosedUnpaid {
		t.Errorf("lookup: %s, want closed_unpaid", got.State)
	}
	if n := len(f.events(t, v.SessionID, "session.closed_unpaid")); n != 1 {
		t.Errorf("%d session.closed_unpaid events, want 1", n)
	}
}

// The PRD's UC6 exception, "from that instant": a lookup at the limit closes the session and
// shows it closed as unpaid before the loop has run; just before it, it is still at the register.
func TestGetSessionAtThreeDaysShowsItClosedUnpaid(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	f.clk.Set(limit.Add(-time.Nanosecond))
	if got := f.looked(t, v.SessionID); got.State != AtRegister {
		t.Fatalf("just before 3 days: %s, want at_register", got.State)
	}
	w0 := f.wake.n.Load()
	f.clk.Set(limit)
	got := f.looked(t, v.SessionID)
	if got.State != ClosedUnpaid || got.UnpaidAmount == nil || got.UnpaidAmount.Value != 1700 {
		t.Errorf("at 3 days: %+v; want closed_unpaid with 1700 unpaid", got)
	}
	if f.state(t, v.SessionID) != ClosedUnpaid || len(f.events(t, v.SessionID, "session.closed_unpaid")) != 1 {
		t.Error("the lookup did not store the close and its one notification")
	}
	if f.wake.n.Load() <= w0 {
		t.Error("the dispatcher was not woken")
	}
}

// Run closes, as soon as it starts, a session that reached its limit while BarPay was stopped.
func TestRunClosesAtStart(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	f.restart(t)
	f.clk.Set(limit)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.api.Run(ctx, slog.New(slog.DiscardHandler)); close(done) }()
	defer func() { cancel(); <-done }()
	give := time.Now().Add(5 * time.Second)
	for f.state(t, v.SessionID) != ClosedUnpaid {
		if time.Now().After(give) {
			t.Fatal("not closed within 5 seconds of starting")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The PRD's Guarantees, Durability: the limit is computed from the stored request time, so a
// session that reached it while BarPay was stopped is closed at the next start.
func TestThreeDaysAcrossARestart(t *testing.T) {
	f := newFixture(t)
	v := f.atRegister(t)
	f.restart(t)
	f.clk.Set(limit.Add(-time.Nanosecond))
	if n := f.closeDue(t); n != 0 {
		t.Fatalf("before the limit after a restart: closed %d", n)
	}
	f.restart(t)
	f.clk.Set(limit.Add(6 * time.Hour))
	if n := f.closeDue(t); n != 1 || f.state(t, v.SessionID) != ClosedUnpaid {
		t.Errorf("after a restart past the limit: closed %d, state %s; want closed_unpaid", n, f.state(t, v.SessionID))
	}
}

// The limit is the register's: an active session, however old, is not closed by it.
func TestThreeDaysDoNotCloseAnActiveSession(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(30 * 24 * time.Hour))
	if n := f.closeDue(t); n != 0 || f.state(t, v.SessionID) != Active {
		t.Errorf("closed %d, state %s; want the active session left as it is", n, f.state(t, v.SessionID))
	}
}
