package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/spectest"
)

func (f *fixture) attach(id, key, member string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/sessions/" + id + "/member", key: key, body: `{"member_id":"` + member + `"}`})
}

// memberOf is the member_id stored for the session, empty for a guest's.
func (f *fixture) memberOf(t *testing.T, id string) string {
	t.Helper()
	var m *string
	if err := f.st.Read().QueryRowContext(context.Background(), "SELECT member_id FROM sessions WHERE id = ?", id).Scan(&m); err != nil {
		t.Fatal(err)
	}
	if m == nil {
		return ""
	}
	return *m
}

// UC10 main flow: a member attached to a guest's active session gets 200 and the session as the
// member's, fitting the spec, and lookup shows it as the member's from then on.
func TestAttachMemberMakesTheGuestSessionTheMembers(t *testing.T) {
	f := newFixture(t)
	g := f.checkedIn(t, "")
	w := f.attach(g.SessionID, "key-a", "m_1")
	if w.Code != http.StatusOK {
		t.Fatalf("attachMember: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Session", w.Body.Bytes())
	if v := decodeView(t, w.Body.Bytes()); v.MemberID == nil || *v.MemberID != "m_1" || v.State != Active {
		t.Errorf("response = %+v; want active and m_1's", v)
	}
	if v := f.looked(t, g.SessionID); v.MemberID == nil || *v.MemberID != "m_1" {
		t.Errorf("lookup member_id = %v, want m_1", v.MemberID)
	}
}

// UC10 main flow and postcondition: the visit carries on unchanged. Its check-in time, orders,
// cover charge, block ends and pause stay those of a guest's session that checked in and ordered
// at the same instants, before the attach and at each minute after it.
func TestAttachMemberKeepsTheVisitAsItWas(t *testing.T) {
	f := newFixture(t)
	g, same := f.checkedIn(t, ""), f.checkedIn(t, "")
	f.clk.Advance(12 * time.Minute)
	for _, id := range []string{g.SessionID, same.SessionID} {
		if w := f.order(id, orderBody("o-1", line("Beer", 1, 650))); w.Code != http.StatusCreated {
			t.Fatalf("order: %d %s", w.Code, w.Body)
		}
	}
	f.clk.Advance(time.Minute)
	before := f.looked(t, g.SessionID)
	attached := decodeView(t, f.attach(g.SessionID, "key-a", "m_1").Body.Bytes())
	attached.MemberID = nil
	if !reflect.DeepEqual(attached, before) {
		t.Errorf("after the attach: %+v\nbefore: %+v; want the same but member_id", attached, before)
	}
	for at := 13 * time.Minute; at <= 2*time.Hour; at += time.Minute {
		f.clk.Set(t0.Add(at))
		got, want := f.looked(t, g.SessionID), f.looked(t, same.SessionID)
		got.SessionID, got.MemberID = want.SessionID, nil
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("at %s: %+v\nthe guest's: %+v", at, got, want)
		}
	}
}

// UC10 main flow: session.member_attached, fitting the spec, is stored with the session as the
// response showed it, and the dispatcher is woken once.
func TestAttachMemberNotifiesTheStoreApp(t *testing.T) {
	f := newFixture(t)
	g := f.checkedIn(t, "")
	woken := f.wake.n.Load()
	w := f.attach(g.SessionID, "key-a", "m_1")
	evs := f.events(t, g.SessionID, "session.member_attached")
	if len(evs) != 1 {
		t.Fatalf("%d session.member_attached events, want 1", len(evs))
	}
	spectest.Valid(t, "SessionMemberAttachedEvent", evs[0])
	var ev struct {
		Sequence int64 `json:"sequence"`
		Data     struct {
			Session json.RawMessage `json:"session"`
		} `json:"data"`
	}
	if err := json.Unmarshal(evs[0], &ev); err != nil {
		t.Fatal(err)
	}
	if string(ev.Data.Session) != w.Body.String() || ev.Sequence != 2 {
		t.Errorf("event sequence %d, session %s; want 2 and the response %s", ev.Sequence, ev.Data.Session, w.Body)
	}
	if n := f.wake.n.Load() - woken; n != 1 {
		t.Errorf("the dispatcher was woken %d times, want 1", n)
	}
}

// UC10 exception: a session checking out, at the register, payment failed, closed or closed
// unpaid is refused with session_not_active and stays a guest's, with nothing sent.
func TestAttachMemberToASessionNotActive(t *testing.T) {
	for _, state := range []State{CheckingOut, AtRegister, PaymentFailed, Closed, ClosedUnpaid} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			g := f.checkedIn(t, "")
			f.setState(t, g.SessionID, state)
			notes := f.notifications(t)
			assertError(t, f.attach(g.SessionID, "key-a", "m_1"), http.StatusConflict, "session_not_active")
			if f.memberOf(t, g.SessionID) != "" || f.notifications(t) != notes {
				t.Error("a member was attached or a notification was stored")
			}
			if n := f.count(t, "SELECT count(*) FROM sessions WHERE id = ? AND state = ?", g.SessionID, string(state)); n != 1 {
				t.Errorf("the session is no longer %s", state)
			}
		})
	}
}

// UC10 exception: a session that already has a member, the same one or another, is refused with
// session_has_member and keeps its member, with nothing sent.
func TestAttachMemberToASessionThatHasOne(t *testing.T) {
	for name, attached := range map[string]string{"checked in as a member": "", "attached before": "m_1"} {
		for _, again := range []string{"m_1", "m_2"} {
			t.Run(name+", then "+again, func(t *testing.T) {
				f := newFixture(t)
				var id string
				if attached == "" {
					id = f.checkedIn(t, "m_1").SessionID
				} else {
					id = f.checkedIn(t, "").SessionID
					if w := f.attach(id, "key-a", attached); w.Code != http.StatusOK {
						t.Fatalf("first attach: %d %s", w.Code, w.Body)
					}
				}
				notes := f.notifications(t)
				assertError(t, f.attach(id, "key-b", again), http.StatusConflict, "session_has_member")
				if f.memberOf(t, id) != "m_1" || f.notifications(t) != notes {
					t.Error("the member changed or a notification was stored")
				}
			})
		}
	}
}

// UC10 exception: a member with another session in any open state is refused with
// member_has_open_session, and the guest's session stays a guest's; one that has only ended
// sessions is attached.
func TestAttachMemberWithAnotherOpenSession(t *testing.T) {
	for _, state := range States {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			other := f.checkedIn(t, "m_1")
			f.setState(t, other.SessionID, state)
			g := f.checkedIn(t, "")
			w := f.attach(g.SessionID, "key-a", "m_1")
			if ended[state] {
				if w.Code != http.StatusOK {
					t.Errorf("after %s: %d %s, want 200", state, w.Code, w.Body)
				}
				return
			}
			assertError(t, w, http.StatusConflict, "member_has_open_session")
			if f.memberOf(t, g.SessionID) != "" || len(f.events(t, g.SessionID, "session.member_attached")) != 0 {
				t.Error("the guest's session changed or a notification was stored")
			}
		})
	}
}

// UC10 postcondition, UC1: a member attached to a guest's session cannot check in again until it
// is closed.
func TestAttachedMemberCannotCheckIn(t *testing.T) {
	f := newFixture(t)
	g := f.checkedIn(t, "")
	f.attach(g.SessionID, "key-a", "m_1")
	assertError(t, f.checkin("key-b", `{"member_id":"m_1"}`), http.StatusConflict, "member_has_open_session")
	f.setState(t, g.SessionID, Closed)
	if w := f.checkin("key-c", `{"member_id":"m_1"}`); w.Code != http.StatusCreated {
		t.Errorf("after the session closed: %d %s, want 201", w.Code, w.Body)
	}
}

// UC10 exception: no session has the ID → 404, nothing stored.
func TestAttachMemberToNoSession(t *testing.T) {
	f := newFixture(t)
	assertError(t, f.attach("ses_AAAAAAAAAAAAAAAAAAAAAAAAAA", "key-a", "m_1"), http.StatusNotFound, "session_not_found")
	if f.notifications(t) != 0 || f.count(t, "SELECT count(*) FROM idempotency_keys") != 0 {
		t.Error("something was stored")
	}
}

func TestAttachMemberRefusesBodiesOutsideTheSchema(t *testing.T) {
	cases := []struct{ name, body, field string }{
		{"empty", ``, ""},
		{"not JSON", `member_id=m_1`, ""},
		{"null", `null`, ""},
		{"an array", `[]`, ""},
		{"two values", `{"member_id":"m_1"}{}`, ""},
		{"no member_id", `{}`, "member_id"},
		{"member_id null", `{"member_id":null}`, "member_id"},
		{"member_id empty", `{"member_id":""}`, "member_id"},
		{"member_id a number", `{"member_id":1}`, "member_id"},
		{"member_id too long", `{"member_id":"` + strings.Repeat("a", 65) + `"}`, "member_id"},
		{"member_id with a space", `{"member_id":"m 1"}`, "member_id"},
		{"twice", `{"member_id":"m_1","member_id":"m_2"}`, "member_id"},
		{"unknown field", `{"member_id":"m_1","name":"x"}`, "name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			g := f.checkedIn(t, "")
			w := f.do(call{method: http.MethodPost, path: "/v1/sessions/" + g.SessionID + "/member", key: "key-a", body: c.body})
			assertError(t, w, http.StatusBadRequest, "invalid_request")
			if _, field := errorOf(t, w); field != c.field {
				t.Errorf("details.field = %q, want %q", field, c.field)
			}
			if f.memberOf(t, g.SessionID) != "" {
				t.Error("a member was attached")
			}
		})
	}
	f := newFixture(t)
	g := f.checkedIn(t, "")
	if w := f.attach(g.SessionID, "key-a", strings.Repeat("Z", 64)); w.Code != http.StatusOK {
		t.Errorf("a 64-character member ID: %d %s", w.Code, w.Body)
	}
}

// The spec's check order: body, session, a member already attached, state, the member's other
// sessions; the first failure is returned.
func TestAttachMemberChecksInOrder(t *testing.T) {
	t.Run("the body before the session", func(t *testing.T) {
		f := newFixture(t)
		w := f.do(call{method: http.MethodPost, path: "/v1/sessions/ses_AAAAAAAAAAAAAAAAAAAAAAAAAA/member", key: "key-a", body: `{}`})
		assertError(t, w, http.StatusBadRequest, "invalid_request")
	})
	t.Run("a member attached before the state", func(t *testing.T) {
		f := newFixture(t)
		s := f.checkedIn(t, "m_1")
		f.setState(t, s.SessionID, Closed)
		assertError(t, f.attach(s.SessionID, "key-a", "m_2"), http.StatusConflict, "session_has_member")
	})
	t.Run("the state before the member's other sessions", func(t *testing.T) {
		f := newFixture(t)
		f.checkedIn(t, "m_1")
		g := f.checkedIn(t, "")
		f.setState(t, g.SessionID, AtRegister)
		assertError(t, f.attach(g.SessionID, "key-a", "m_1"), http.StatusConflict, "session_not_active")
	})
}

// UC10 exception: the same key and body return the first response, byte for byte; the member is
// attached once and the store app is notified once.
func TestAttachMemberRepeatedWithTheSameKey(t *testing.T) {
	f := newFixture(t)
	g := f.checkedIn(t, "")
	first := f.attach(g.SessionID, "key-a", "m_1")
	f.clk.Advance(time.Minute)
	again := f.attach(g.SessionID, "key-a", "m_1")
	if again.Code != http.StatusOK || again.Body.String() != first.Body.String() {
		t.Errorf("again: %d %s; want the first response %s", again.Code, again.Body, first.Body)
	}
	if n := len(f.events(t, g.SessionID, "session.member_attached")); n != 1 {
		t.Errorf("%d session.member_attached events, want 1", n)
	}
}

// The same key with another body is refused as reused, and nothing changes.
func TestAttachMemberWithTheSameKeyAndAnotherBody(t *testing.T) {
	f := newFixture(t)
	g := f.checkedIn(t, "")
	f.attach(g.SessionID, "key-a", "m_1")
	assertError(t, f.attach(g.SessionID, "key-a", "m_2"), http.StatusConflict, "idempotency_key_reused")
	if f.memberOf(t, g.SessionID) != "m_1" {
		t.Error("the member changed")
	}
}

// A refusal that changed nothing keeps nothing: once the member's other session has ended, the
// same key attaches.
func TestAttachMemberRefusedThenSentAgainWithTheSameKey(t *testing.T) {
	f := newFixture(t)
	other := f.checkedIn(t, "m_1")
	g := f.checkedIn(t, "")
	assertError(t, f.attach(g.SessionID, "key-a", "m_1"), http.StatusConflict, "member_has_open_session")
	f.setState(t, other.SessionID, Closed)
	if w := f.attach(g.SessionID, "key-a", "m_1"); w.Code != http.StatusOK {
		t.Errorf("sent again: %d %s, want 200", w.Code, w.Body)
	}
}

func TestAttachMemberWithoutAValidKey(t *testing.T) {
	for name, key := range map[string]string{"missing": "", "malformed": "a b"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			g := f.checkedIn(t, "")
			w := f.attach(g.SessionID, key, "m_1")
			assertError(t, w, http.StatusBadRequest, "invalid_request")
			if _, field := errorOf(t, w); field != "Idempotency-Key" {
				t.Errorf("details.field = %q", field)
			}
			if f.memberOf(t, g.SessionID) != "" {
				t.Error("a member was attached")
			}
		})
	}
}

// race runs a and b at the same moment and returns their responses.
func race(a, b func() *httptest.ResponseRecorder) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
	var wa, wb *httptest.ResponseRecorder
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Go(func() { <-start; wa = a() })
	wg.Go(func() { <-start; wb = b() })
	close(start)
	wg.Wait()
	return wa, wb
}

// raceRuns is how many times each race is run, so an order that loses only sometimes shows.
const raceRuns = 20

// UC10 exception: of attaches of several members to the same session at the same time, one goes
// through and the others get session_has_member; one notification is stored.
func TestConcurrentAttachesToOneSession(t *testing.T) {
	for range raceRuns {
		f := newFixture(t)
		g := f.checkedIn(t, "")
		a, b := race(
			func() *httptest.ResponseRecorder { return f.attach(g.SessionID, "key-a", "m_1") },
			func() *httptest.ResponseRecorder { return f.attach(g.SessionID, "key-b", "m_2") },
		)
		won, lost, member := a, b, "m_1"
		if b.Code == http.StatusOK {
			won, lost, member = b, a, "m_2"
		}
		if won.Code != http.StatusOK {
			t.Fatalf("neither went through: %d %s, %d %s", a.Code, a.Body, b.Code, b.Body)
		}
		assertError(t, lost, http.StatusConflict, "session_has_member")
		if f.memberOf(t, g.SessionID) != member || len(f.events(t, g.SessionID, "session.member_attached")) != 1 {
			t.Fatalf("member %q, want %s, with one notification", f.memberOf(t, g.SessionID), member)
		}
	}
}

// UC10 exception: of attaches of the same member to two guests' sessions at the same time, one
// goes through and the other gets member_has_open_session.
func TestConcurrentAttachesOfOneMember(t *testing.T) {
	for range raceRuns {
		f := newFixture(t)
		g1, g2 := f.checkedIn(t, ""), f.checkedIn(t, "")
		a, b := race(
			func() *httptest.ResponseRecorder { return f.attach(g1.SessionID, "key-a", "m_1") },
			func() *httptest.ResponseRecorder { return f.attach(g2.SessionID, "key-b", "m_1") },
		)
		lost := b
		if b.Code == http.StatusOK {
			lost = a
		}
		if a.Code != http.StatusOK && b.Code != http.StatusOK {
			t.Fatalf("neither went through: %d %s, %d %s", a.Code, a.Body, b.Code, b.Body)
		}
		assertError(t, lost, http.StatusConflict, "member_has_open_session")
		if n := f.count(t, "SELECT count(*) FROM sessions WHERE member_id = 'm_1'"); n != 1 {
			t.Fatalf("m_1 has %d sessions, want 1", n)
		}
	}
}

// UC10 exception: of an attach and a check-in of the same member at the same time, one goes
// through and the other gets member_has_open_session.
func TestConcurrentAttachAndCheckinOfOneMember(t *testing.T) {
	for range raceRuns {
		f := newFixture(t)
		g := f.checkedIn(t, "")
		a, c := race(
			func() *httptest.ResponseRecorder { return f.attach(g.SessionID, "key-a", "m_1") },
			func() *httptest.ResponseRecorder { return f.checkin("key-c", `{"member_id":"m_1"}`) },
		)
		switch {
		case a.Code == http.StatusOK:
			assertError(t, c, http.StatusConflict, "member_has_open_session")
		case c.Code == http.StatusCreated:
			assertError(t, a, http.StatusConflict, "member_has_open_session")
		default:
			t.Fatalf("neither went through: %d %s, %d %s", a.Code, a.Body, c.Code, c.Body)
		}
		if n := f.count(t, "SELECT count(*) FROM sessions WHERE member_id = 'm_1'"); n != 1 {
			t.Fatalf("m_1 has %d sessions, want 1", n)
		}
	}
}

// UC10 exception: when the session leaves active at the same moment (the register request, or a
// register payment staff record), whichever is taken first holds: the session leaves active as
// the member's, or the attach gets session_not_active and the session stays a guest's.
func TestAttachRacesTheSessionLeavingActive(t *testing.T) {
	for name, c := range map[string]struct {
		leave func(f *fixture, id string) *httptest.ResponseRecorder
		to    State
	}{
		"register request": {func(f *fixture, id string) *httptest.ResponseRecorder { return f.requestRegister(id) }, AtRegister},
		"register payment": {func(f *fixture, id string) *httptest.ResponseRecorder {
			return f.recordPayment(id, amountBody(0, "USD"))
		}, Closed},
	} {
		t.Run(name, func(t *testing.T) {
			for range raceRuns {
				f := newFixture(t)
				g := f.checkedIn(t, "")
				a, l := race(
					func() *httptest.ResponseRecorder { return f.attach(g.SessionID, "key-a", "m_1") },
					func() *httptest.ResponseRecorder { return c.leave(f, g.SessionID) },
				)
				if l.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", name, l.Code, l.Body)
				}
				v := decodeView(t, l.Body.Bytes())
				switch a.Code {
				case http.StatusOK:
					if f.memberOf(t, g.SessionID) != "m_1" || v.MemberID == nil || *v.MemberID != "m_1" {
						t.Fatalf("attached first, but the session left active as %v's", v.MemberID)
					}
				default:
					assertError(t, a, http.StatusConflict, "session_not_active")
					if f.memberOf(t, g.SessionID) != "" || v.MemberID != nil {
						t.Fatal("refused, but the session has a member")
					}
				}
				if v.State != c.to {
					t.Fatalf("state %s, want %s", v.State, c.to)
				}
			}
		})
	}
}
