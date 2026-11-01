package session

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/spectest"
)

func (f *fixture) listSessions(query string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodGet, path: "/v1/admin/sessions" + query, auth: adminAuth})
}

// sessionPage is listSessions' response, each session kept as sent.
type sessionPage struct {
	Sessions   []json.RawMessage `json:"sessions"`
	NextCursor *string           `json:"next_cursor"`
}

// listed reads a page of listSessions, failing the test unless it is 200 and every session fits
// the spec's Session.
func (f *fixture) listed(t *testing.T, query string) sessionPage {
	t.Helper()
	w := f.listSessions(query)
	if w.Code != http.StatusOK {
		t.Fatalf("listSessions%s: %d %s", query, w.Code, w.Body)
	}
	var p sessionPage
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Sessions == nil {
		t.Fatalf("sessions is not an array: %s", w.Body)
	}
	for _, s := range p.Sessions {
		spectest.Valid(t, "Session", s)
	}
	return p
}

// ids returns the session IDs of a page, in order.
func ids(t *testing.T, p sessionPage) []string {
	t.Helper()
	out := make([]string, len(p.Sessions))
	for i, s := range p.Sessions {
		out[i] = decodeView(t, s).SessionID
	}
	return out
}

// listedIDs reads every page of listSessions with query (which has no cursor), limit at a time.
func (f *fixture) listedIDs(t *testing.T, query url.Values, limit int) []string {
	t.Helper()
	var out []string
	query.Set("limit", strconv.Itoa(limit))
	for {
		p := f.listed(t, "?"+query.Encode())
		out = append(out, ids(t, p)...)
		if p.NextCursor == nil {
			return out
		}
		query.Set("cursor", *p.NextCursor)
	}
}

// UC7 main flow: every session, ended ones too, newest check-in first; a state filter of one or
// more states lists only the sessions in them.
func TestListSessionsNewestFirstAndByState(t *testing.T) {
	f := newFixture(t)
	closed := f.checkedIn(t, "")
	f.requested(t, closed.SessionID)
	if w := f.recordPayment(closed.SessionID, amountBody(0, "USD")); w.Code != http.StatusOK {
		t.Fatalf("recordRegisterPayment: %d %s", w.Code, w.Body)
	}
	f.clk.Set(t0.Add(time.Minute))
	register := f.checkedIn(t, "m_1")
	f.requested(t, register.SessionID)
	f.clk.Set(t0.Add(2 * time.Minute))
	active := f.checkedIn(t, "m_2")

	for _, c := range []struct {
		query string
		want  []string
	}{
		{"", []string{active.SessionID, register.SessionID, closed.SessionID}},
		{"?state=active", []string{active.SessionID}},
		{"?state=closed", []string{closed.SessionID}},
		{"?state=closed,at_register", []string{register.SessionID, closed.SessionID}},
		{"?state=active,closed,active", []string{active.SessionID, closed.SessionID}},
		{"?state=active&state=at_register", []string{active.SessionID, register.SessionID}},
		// Repeats bind once each, so the query stays under SQLite's limit on bound parameters.
		{"?state=" + strings.Repeat("active,", 40000) + "active", []string{active.SessionID}},
		{"?state=payment_failed", []string{}},
	} {
		p := f.listed(t, c.query)
		if got := ids(t, p); !slices.Equal(got, c.want) || p.NextCursor != nil {
			t.Errorf("listSessions%s = %v, next %v; want %v, no next", c.query, got, p.NextCursor, c.want)
		}
	}
}

// Each listed session is what getSession shows at the same instant, in every SessionState, and
// for an active session in every CoverChargeStatus it can have, so the admin screen and the
// customer's panel never disagree on a total.
func TestListedSessionIsItsLookup(t *testing.T) {
	f := newFixture(t)
	// Checked in at t0 with no order: 16 blocks of $2.00 reach the cap of $30.00 by t0+160m.
	capped := f.checkedIn(t, "")
	// payment_failed: checked in at t0, checkout begun and refused at t0+25m.
	f.failed(t, "m_1")
	atRegister := f.checkedIn(t, "")
	f.requested(t, atRegister.SessionID)
	closed := f.checkedIn(t, "")
	f.requested(t, closed.SessionID)
	if w := f.recordPayment(closed.SessionID, amountBody(500, "USD")); w.Code != http.StatusOK {
		t.Fatalf("recordRegisterPayment: %d %s", w.Code, w.Body)
	}
	unpaid := f.checkedIn(t, "")
	f.requested(t, unpaid.SessionID)
	if w := f.markUnpaid(unpaid.SessionID); w.Code != http.StatusOK {
		t.Fatalf("markUnpaid: %d %s", w.Code, w.Body)
	}
	checkingOut := f.checkedIn(t, "m_2")
	f.setState(t, checkingOut.SessionID, CheckingOut)
	f.clk.Set(t0.Add(100 * time.Minute))
	f.checkedIn(t, "") // running at t0+160m: 6 blocks, under the cap
	f.clk.Set(t0.Add(140 * time.Minute))
	paused := f.checkedIn(t, "") // its second order pauses it from t0+150m to t0+180m
	decodeOrder201(t, f.order(paused.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(150 * time.Minute))
	decodeOrder201(t, f.order(paused.SessionID, orderBody("o-2", line("Fries", 1, 500))), http.StatusCreated)
	f.propose(t, 1500, t0.Add(155*time.Minute))
	f.clk.Set(t0.Add(160 * time.Minute))

	p := f.listed(t, "?limit=100")
	states := map[State]bool{}
	statuses := map[pricing.Status]bool{}
	for _, s := range p.Sessions {
		v := decodeView(t, s)
		states[v.State] = true
		if v.State == Active {
			statuses[v.CoverChargeStatus] = true
		}
		var want bytes.Buffer
		if err := json.Compact(&want, f.getSession(v.SessionID).Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(s, want.Bytes()) {
			t.Errorf("listed %s\nlookup %s", s, want.Bytes())
		}
	}
	for _, s := range States {
		if !states[s] {
			t.Errorf("no session in state %s", s)
		}
	}
	for _, s := range []pricing.Status{pricing.Running, pricing.Paused, pricing.Capped} {
		if !statuses[s] {
			t.Errorf("no active session with cover_charge_status %s", s)
		}
	}
	if got := decodeView(t, p.Sessions[len(p.Sessions)-1]); got.SessionID != capped.SessionID || got.CoverChargeStatus != pricing.Capped {
		t.Errorf("oldest = %s %s; want %s capped", got.SessionID, got.CoverChargeStatus, capped.SessionID)
	}
}

// UC6 and UC7 exceptions: a session that reached its 3-day limit is listed closed unpaid, as its
// lookup shows it, and no longer under the state it left.
func TestListSessionsClosesThoseAtTheirLimit(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.requested(t, v.SessionID)
	f.clk.Set(t0.Add(unpaidAfter))
	if got := ids(t, f.listed(t, "?state=at_register")); len(got) != 0 {
		t.Errorf("at_register lists %v at the limit; want none", got)
	}
	p := f.listed(t, "?state=closed_unpaid")
	if got := ids(t, p); !slices.Equal(got, []string{v.SessionID}) {
		t.Fatalf("closed_unpaid lists %v; want %s", got, v.SessionID)
	}
	if n := len(f.events(t, v.SessionID, "session.closed_unpaid")); n != 1 {
		t.Errorf("%d session.closed_unpaid; want 1", n)
	}
	var want bytes.Buffer
	if err := json.Compact(&want, f.getSession(v.SessionID).Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Sessions[0], want.Bytes()) {
		t.Errorf("listed %s\nlookup %s", p.Sessions[0], want.Bytes())
	}
}

// The spec's Cursor: check-ins while paging, at the same instant as listed ones too, neither
// repeat nor drop a session on the following pages; they are on a new first page. The order is
// the same on every read, also among sessions checked in at the same instant.
func TestListSessionsPagesHoldWhileSessionsAreAdded(t *testing.T) {
	f := newFixture(t)
	var want []string
	for range 5 { // all at t0
		want = append([]string{f.checkedIn(t, "").SessionID}, want...)
	}
	if got := f.listedIDs(t, url.Values{}, 2); !slices.Equal(got, want) {
		t.Fatalf("pages = %v; want %v", got, want)
	}
	if got := f.listedIDs(t, url.Values{}, 3); !slices.Equal(got, want) {
		t.Errorf("pages of 3 = %v; want %v", got, want)
	}

	first := f.listed(t, "?limit=2")
	added := f.checkedIn(t, "") // at t0, like every listed one
	var rest []string
	for c := first.NextCursor; c != nil; {
		p := f.listed(t, "?limit=2&cursor="+url.QueryEscape(*c))
		rest = append(rest, ids(t, p)...)
		c = p.NextCursor
	}
	if got := append(ids(t, first), rest...); !slices.Equal(got, want) {
		t.Errorf("pages with a check-in after the first = %v; want %v", got, want)
	}
	if got := ids(t, f.listed(t, "?limit=1")); !slices.Equal(got, []string{added.SessionID}) {
		t.Errorf("new first page = %v; want %s", got, added.SessionID)
	}
}

// A page continues after its cursor's session also with a state filter, while that session
// leaves the filter's states.
func TestListSessionsCursorHoldsWhenItsSessionChangesState(t *testing.T) {
	f := newFixture(t)
	a := f.checkedIn(t, "")
	b := f.checkedIn(t, "")
	c := f.checkedIn(t, "")
	first := f.listed(t, "?state=active&limit=1")
	if got := ids(t, first); !slices.Equal(got, []string{c.SessionID}) || first.NextCursor == nil {
		t.Fatalf("first page = %v, next %v", got, first.NextCursor)
	}
	f.requested(t, c.SessionID)
	p := f.listed(t, "?state=active&limit=5&cursor="+url.QueryEscape(*first.NextCursor))
	if got := ids(t, p); !slices.Equal(got, []string{b.SessionID, a.SessionID}) {
		t.Errorf("next page = %v; want %s, %s", got, b.SessionID, a.SessionID)
	}
}

// The spec's Limit, Cursor and state: a value out of range, a cursor that is malformed, from
// another list or names no session, and a state that is not a SessionState are refused.
func TestListSessionsRefusesBadParameters(t *testing.T) {
	f := newFixture(t)
	f.checkedIn(t, "")
	for _, c := range []struct{ query, field string }{
		{"?limit=0", "limit"},
		{"?limit=101", "limit"},
		{"?limit=x", "limit"},
		{"?cursor=", "cursor"},
		{"?cursor=not-a-cursor", "cursor"},
		{"?cursor=" + httpapi.Cursor("alerts", 1), "cursor"},
		{"?cursor=" + httpapi.Cursor("sessions", 99), "cursor"},
		{"?state=open", "state"},
		{"?state=", "state"},
		{"?state=active,", "state"},
		{"?state=Active", "state"},
	} {
		w := f.listSessions(c.query)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", c.query, w.Code, w.Body)
			continue
		}
		spectest.Valid(t, "Error", w.Body.Bytes())
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Details struct {
					Field string `json:"field"`
				} `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Error.Code != "invalid_request" || e.Error.Details.Field != c.field {
			t.Errorf("%s: %s %s; want invalid_request %s", c.query, e.Error.Code, e.Error.Details.Field, c.field)
		}
	}
}

// An unreadable state is refused rather than answered with every state.
func TestListSessionsRefusesUnreadableState(t *testing.T) {
	f := newFixture(t)
	f.checkedIn(t, "")
	for _, q := range []string{"?state=%zz", "?state=active&state=%zz", "?state=active;closed"} {
		w := f.listSessions(q)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", q, w.Code, w.Body)
			continue
		}
		spectest.Valid(t, "Error", w.Body.Bytes())
		if !bytes.Contains(w.Body.Bytes(), []byte(`"code":"invalid_request"`)) ||
			!bytes.Contains(w.Body.Bytes(), []byte(`"details":{"field":"state"}`)) {
			t.Errorf("%s: %s; want invalid_request for state", q, w.Body)
		}
	}
}

// Guarantees → Access: only the admin API key lists sessions, and it needs the staff ID.
func TestListSessionsNeedsTheAdminKeyAndStaffID(t *testing.T) {
	f := newFixture(t)
	f.checkedIn(t, "")
	for _, auth := range []string{noAuth, "Bearer wrong", storeAuth, edgeAuth} {
		w := f.do(call{method: http.MethodGet, path: "/v1/admin/sessions", auth: auth})
		if w.Code != http.StatusUnauthorized || bytes.Contains(w.Body.Bytes(), []byte("ses_")) {
			t.Errorf("with %s: %d %s; want 401", auth, w.Code, w.Body)
		}
	}
	for _, id := range []string{noAuth, "bad id"} {
		w := f.do(call{method: http.MethodGet, path: "/v1/admin/sessions", auth: adminAuth, staff: id})
		if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(`"field":"BarPay-Staff-Id"`)) {
			t.Errorf("staff ID %q: %d %s; want 400 for BarPay-Staff-Id", id, w.Code, w.Body)
		}
	}
}
