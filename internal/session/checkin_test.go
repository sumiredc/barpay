package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/store"
)

// UC1 main flow and postcondition: a member or a guest gets 201 and an active session with no
// cover charge, and session.started is stored with sequence 1, the session as its data and a
// body that fits the spec.
func TestCheckinStartsAnActiveSession(t *testing.T) {
	for name, c := range map[string]struct {
		body   string
		member *string
	}{
		"member": {`{"member_id":"m_1024"}`, ptr("m_1024")},
		"guest":  {`{}`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.clk.Advance(3 * time.Minute)
			w := f.checkin("key-1", c.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			spectest.Valid(t, "Session", w.Body.Bytes())
			v := decodeView(t, w.Body.Bytes())
			if v.State != Active || v.CheckedInAt != "2026-10-31T18:03:00Z" || v.TotalFixedAt != "" ||
				v.CoverCharge.Value != 0 || v.Total.Value != 0 || v.OrderCount != 0 || v.CoverChargeStatus != "running" ||
				v.RatePerHour.Value != 1200 || v.PaymentMethod != "register" || v.UnpaidAmount != nil || v.Receipt != nil {
				t.Errorf("session = %+v", v)
			}
			if (v.MemberID == nil) != (c.member == nil) || (v.MemberID != nil && *v.MemberID != *c.member) {
				t.Errorf("member_id = %v, want %v", v.MemberID, c.member)
			}

			var typ, subject string
			var sequence int64
			var body []byte
			err := f.st.Read().QueryRowContext(context.Background(),
				"SELECT type, subject_id, sequence, body FROM notifications").Scan(&typ, &subject, &sequence, &body)
			if err != nil {
				t.Fatal(err)
			}
			if typ != "session.started" || subject != v.SessionID || sequence != 1 {
				t.Errorf("notification %s for %s, sequence %d", typ, subject, sequence)
			}
			spectest.Valid(t, "SessionStartedEvent", body)
			var ev struct {
				Data struct {
					Session json.RawMessage `json:"session"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &ev); err != nil {
				t.Fatal(err)
			}
			if string(ev.Data.Session) != w.Body.String() {
				t.Errorf("the notification's session %s differs from the response %s", ev.Data.Session, w.Body)
			}
			if f.wake.n.Load() != 1 {
				t.Errorf("the dispatcher was woken %d times, want 1", f.wake.n.Load())
			}
		})
	}
}

func ptr(s string) *string { return &s }

// UC1 postcondition: the first block ends one block length after check-in, and not before.
func TestFirstBlockEndsOneBlockAfterCheckin(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	in, _ := time.Parse(time.RFC3339Nano, v.CheckedInAt)
	cover := func(at time.Time) int64 {
		t.Helper()
		var got View
		err := f.st.View(context.Background(), func(q store.Querier) error {
			s, err := get(context.Background(), q, v.SessionID)
			if err != nil {
				return err
			}
			got, err = view(context.Background(), q, s, at)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got.CoverCharge.Value
	}
	if got := cover(in.Add(10*time.Minute - time.Nanosecond)); got != 0 {
		t.Errorf("just before the block ends: cover charge %d, want 0", got)
	}
	if got := cover(in.Add(10 * time.Minute)); got != 200 { // $12.00/h for 10 minutes
		t.Errorf("when the block ends: cover charge %d, want 200", got)
	}
}

// Every way to start a session without the store API key is refused, and nothing is created.
func TestCheckinNeedsTheStoreAPIKey(t *testing.T) {
	for name, auth := range map[string]string{
		"missing": "-", "wrong": "Bearer nope", "admin API key": "Bearer k-admin", "edge shared secret": "Bearer k-edge",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if auth == "-" {
				auth = ""
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/checkins", strings.NewReader(`{}`))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			req.Header.Set("Idempotency-Key", "key-1")
			w := httptest.NewRecorder()
			f.router.ServeHTTP(w, req)
			assertError(t, w, http.StatusUnauthorized, "unauthorized")
			if f.sessions(t) != 0 || f.notifications(t) != 0 {
				t.Errorf("sessions = %d, notifications = %d; want none", f.sessions(t), f.notifications(t))
			}
		})
	}
}

// The same key and body return the first response; no second session or notification.
func TestCheckinRepeatedWithTheSameKey(t *testing.T) {
	f := newFixture(t)
	first := f.checkin("key-1", `{"member_id":"m_1"}`)
	f.clk.Advance(time.Minute)
	again := f.checkin("key-1", `{"member_id":"m_1"}`)
	if again.Code != http.StatusCreated || again.Body.String() != first.Body.String() {
		t.Errorf("again: %d %s; want the first response %s", again.Code, again.Body, first.Body)
	}
	if f.sessions(t) != 1 || f.notifications(t) != 1 {
		t.Errorf("sessions = %d, notifications = %d; want 1 each", f.sessions(t), f.notifications(t))
	}
}

func TestCheckinWithTheSameKeyAndAnotherBody(t *testing.T) {
	f := newFixture(t)
	f.checkin("key-1", `{"member_id":"m_1"}`)
	assertError(t, f.checkin("key-1", `{"member_id":"m_2"}`), http.StatusConflict, "idempotency_key_reused")
	if f.sessions(t) != 1 {
		t.Errorf("sessions = %d, want 1", f.sessions(t))
	}
}

// While a check-in with a key is still being processed, the same key is refused at once.
func TestCheckinWithAKeyInProgress(t *testing.T) {
	f := newFixture(t)
	// Hold the write lock, so the first check-in waits inside its transaction.
	locked, unlock := make(chan struct{}), make(chan struct{})
	held := make(chan error)
	go func() {
		held <- f.st.Tx(context.Background(), func(store.Querier) error {
			close(locked)
			<-unlock
			return nil
		})
	}()
	<-locked
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { results <- f.checkin("key-1", `{}`) }()
	}
	// One of the two holds the key and waits for the lock; the other is refused without waiting.
	assertError(t, <-results, http.StatusConflict, "idempotency_key_in_progress")
	close(unlock)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if w := <-results; w.Code != http.StatusCreated {
		t.Errorf("the other: %d %s, want 201", w.Code, w.Body)
	}
	if f.sessions(t) != 1 {
		t.Errorf("sessions = %d, want 1", f.sessions(t))
	}
}

// A refusal that changed nothing keeps nothing: once the member's session has ended, the same
// key starts a session.
func TestCheckinRefusedThenSentAgainWithTheSameKey(t *testing.T) {
	f := newFixture(t)
	open := f.checkedIn(t, "m_1")
	assertError(t, f.checkin("key-2", `{"member_id":"m_1"}`), http.StatusConflict, "member_has_open_session")
	f.setState(t, open.SessionID, Closed)
	if w := f.checkin("key-2", `{"member_id":"m_1"}`); w.Code != http.StatusCreated {
		t.Errorf("sent again: %d %s, want 201", w.Code, w.Body)
	}
}

func TestCheckinWithoutAValidKey(t *testing.T) {
	for name, key := range map[string]string{"missing": "", "malformed": "a b"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			w := f.checkin(key, `{}`)
			assertError(t, w, http.StatusBadRequest, "invalid_request")
			if _, field := errorOf(t, w); field != "Idempotency-Key" {
				t.Errorf("details.field = %q", field)
			}
			if f.sessions(t) != 0 || f.notifications(t) != 0 {
				t.Error("something was created")
			}
		})
	}
}

func TestCheckinRefusesBodiesOutsideTheSchema(t *testing.T) {
	cases := []struct{ name, body, field string }{
		{"empty", ``, ""},
		{"not JSON", `member=m_1`, ""},
		{"null", `null`, ""},
		{"an array", `[]`, ""},
		{"two values", `{}{}`, ""},
		{"member_id null", `{"member_id":null}`, "member_id"},
		{"member_id empty", `{"member_id":""}`, "member_id"},
		{"member_id a number", `{"member_id":1}`, "member_id"},
		{"member_id too long", `{"member_id":"` + strings.Repeat("a", 65) + `"}`, "member_id"},
		{"member_id with a space", `{"member_id":"m 1"}`, "member_id"},
		{"another case", `{"Member_ID":"m_1"}`, "Member_ID"},
		{"twice", `{"member_id":"m_1","member_id":"m_2"}`, "member_id"},
		{"unknown field", `{"name":"x"}`, "name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			w := f.checkin("key-1", c.body)
			assertError(t, w, http.StatusBadRequest, "invalid_request")
			if _, field := errorOf(t, w); field != c.field {
				t.Errorf("details.field = %q, want %q", field, c.field)
			}
			if f.sessions(t) != 0 {
				t.Error("a session was created")
			}
		})
	}
	f := newFixture(t)
	if w := f.checkin("key-1", `{"member_id":"`+strings.Repeat("Z", 64)+`"}`); w.Code != http.StatusCreated {
		t.Errorf("a 64-character member ID: %d %s", w.Code, w.Body)
	}
}

// ended are the states in which a session has ended (the PRD's state diagram in Use cases).
var ended = map[State]bool{Closed: true, ClosedUnpaid: true}

// A member with a session in any open state cannot check in again; with only ended ones they can.
func TestMemberWithAnOpenSessionIsRefused(t *testing.T) {
	for _, state := range States {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			first := f.checkedIn(t, "m_1")
			f.setState(t, first.SessionID, state)
			w := f.checkin("key-2", `{"member_id":"m_1"}`)
			if ended[state] {
				if w.Code != http.StatusCreated {
					t.Errorf("after %s: %d %s, want 201", state, w.Code, w.Body)
				}
				return
			}
			assertError(t, w, http.StatusConflict, "member_has_open_session")
			if f.sessions(t) != 1 || f.notifications(t) != 1 {
				t.Errorf("sessions = %d, notifications = %d; want 1 each", f.sessions(t), f.notifications(t))
			}
		})
	}
	// Another member, and guests, are not affected.
	f := newFixture(t)
	f.checkedIn(t, "m_1")
	f.checkedIn(t, "m_2")
	f.checkedIn(t, "")
	f.checkedIn(t, "")
}

// Check-ins of the same member with different keys at the same time start one session.
func TestConcurrentCheckinsOfOneMember(t *testing.T) {
	f := newFixture(t)
	const n = 10
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { codes <- f.checkin("key-"+string(rune('a'+i)), `{"member_id":"m_1"}`).Code })
	}
	wg.Wait()
	close(codes)
	created := 0
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Errorf("status %d", c)
		}
	}
	if created != 1 || f.sessions(t) != 1 {
		t.Errorf("created = %d, sessions = %d; want 1", created, f.sessions(t))
	}
}

// The store holds the rule by itself: a second open session of a member is refused even when
// written directly, and a member's ended sessions do not count.
func TestStoreHoldsOneOpenSessionPerMember(t *testing.T) {
	f := newFixture(t)
	first := f.checkedIn(t, "m_1")
	write := func(state string) error {
		return f.st.Tx(context.Background(), func(tx store.Querier) error {
			_, err := tx.ExecContext(context.Background(), `INSERT INTO sessions (id, member_id, state, currency, checked_in_at, total_fixed_at)
				VALUES (?, 'm_1', ?, 'USD', 0, CASE WHEN ? = 'active' THEN NULL ELSE 0 END)`, newID(), state, state)
			return err
		})
	}
	if err := write("at_register"); err == nil {
		t.Error("a second open session was stored")
	}
	if err := write("closed"); err != nil {
		t.Errorf("an ended session: %v", err)
	}
	f.setState(t, first.SessionID, ClosedUnpaid)
	if err := write("active"); err != nil {
		t.Errorf("after the first ended: %v", err)
	}
}
