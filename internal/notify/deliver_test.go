package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

// A distinguishable signing secret, so a leak is found by a substring search.
const signingSecret = "SECRET-signing-9c41e7"

// received is one request the fake store app got.
type received struct {
	body      []byte
	signature string
}

// storeApp is a fake store app. It answers each request with the next status of statuses
// (the last one repeats), or with answer when set.
type storeApp struct {
	*httptest.Server
	mu       sync.Mutex
	got      []received
	statuses []int
	answer   func(w http.ResponseWriter, r *http.Request)
}

func newStoreApp(t *testing.T, statuses ...int) *storeApp {
	t.Helper()
	a := &storeApp{statuses: statuses}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.got = append(a.got, received{body: b, signature: r.Header.Get(SignatureHeader)})
		answer := a.answer
		status := http.StatusOK
		if len(a.statuses) > 0 {
			status = a.statuses[min(len(a.got), len(a.statuses))-1]
		}
		a.mu.Unlock()
		if answer != nil {
			answer(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *storeApp) setAnswer(fn func(w http.ResponseWriter, r *http.Request)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answer = fn
}

func (a *storeApp) requests() []received {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]received(nil), a.got...)
}

type fixture struct {
	st  *store.Store
	clk *clock.Fake
	log *bytes.Buffer
	d   *Dispatcher
}

func newFixture(t *testing.T, app *storeApp) *fixture {
	t.Helper()
	f := &fixture{st: storetest.Open(t), clk: clock.NewFake(t0), log: &bytes.Buffer{}}
	f.d = f.dispatcher(app.URL, 0)
	return f
}

func (f *fixture) dispatcher(url string, timeout time.Duration) *Dispatcher {
	return NewDispatcher(f.st, Options{
		URL: url, Secret: secret.New(signingSecret), Clock: f.clk,
		Log: slog.New(slog.NewTextHandler(f.log, nil)), Timeout: timeout,
	})
}

// restart drops every in-memory object and builds a new Dispatcher from the reopened store.
func (f *fixture) restart(t *testing.T, app *storeApp) {
	t.Helper()
	f.st = storetest.Reopen(t, f.st)
	f.d = f.dispatcher(app.URL, 0)
}

func (f *fixture) deliver(t *testing.T) int {
	t.Helper()
	n, err := f.d.DeliverDue(context.Background())
	if err != nil {
		t.Fatalf("DeliverDue: %v", err)
	}
	return n
}

type record struct {
	status   Status
	attempts int
	last     time.Time
}

func (f *fixture) record(t *testing.T, id string) record {
	t.Helper()
	var r record
	var status string
	var last *int64
	err := f.st.Read().QueryRowContext(context.Background(),
		"SELECT status, attempts, last_attempt_at FROM notifications WHERE id = ?", id).Scan(&status, &r.attempts, &last)
	if err != nil {
		t.Fatal(err)
	}
	r.status = Status(status)
	if last != nil {
		r.last = time.Unix(0, *last).UTC()
	}
	return r
}

func (f *fixture) alerts(t *testing.T) []alert.Alert {
	t.Helper()
	rows, err := f.st.Read().QueryContext(context.Background(), "SELECT id FROM alerts ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []alert.Alert
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		a, err := alert.Get(context.Background(), f.st.Read(), id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

var signaturePattern = regexp.MustCompile(`^t=[0-9]+,v1=[0-9a-f]{64}$`)

// verify checks a request the way the spec's Signature parameter tells the store app to, and
// returns the signed time.
func verify(t *testing.T, r received) time.Time {
	t.Helper()
	if !signaturePattern.MatchString(r.signature) {
		t.Fatalf("BarPay-Signature %q does not fit the spec's pattern", r.signature)
	}
	ts, v1, _ := strings.Cut(strings.TrimPrefix(r.signature, "t="), ",v1=")
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(ts + "." + string(r.body)))
	got, _ := hex.DecodeString(v1)
	if !hmac.Equal(got, mac.Sum(nil)) {
		t.Fatalf("signature %q does not verify against the body %s", r.signature, r.body)
	}
	sec, _ := strconv.ParseInt(ts, 10, 64)
	return time.Unix(sec, 0).UTC()
}

func eventID(t *testing.T, b []byte) string {
	t.Helper()
	var e struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e.ID
}

// The signature verifies with the spec's steps and is made for the time of each attempt; a
// retry carries the same ID and body with a new signature.
func TestSignatureAndRetryCarrySameEvent(t *testing.T) {
	app := newStoreApp(t, http.StatusInternalServerError, http.StatusOK)
	f := newFixture(t, app)
	f.clk.Set(t0.Add(400 * time.Millisecond))
	ev := enqueue(t, f.st, f.clk.Now(), sessionEvent(SessionStarted, sesA))

	f.deliver(t)
	f.clk.Advance(retryAfter[0])
	f.deliver(t)

	got := app.requests()
	if len(got) != 2 {
		t.Fatalf("%d requests, want 2", len(got))
	}
	if at := verify(t, got[0]); !at.Equal(t0) {
		t.Errorf("first attempt signed for %v, want %v", at, t0)
	}
	if at := verify(t, got[1]); !at.Equal(t0.Add(retryAfter[0])) {
		t.Errorf("retry signed for %v, want %v", at, t0.Add(retryAfter[0]))
	}
	if !bytes.Equal(got[0].body, got[1].body) || !bytes.Equal(got[0].body, storedBody(t, f.st, ev.ID)) {
		t.Errorf("bodies differ:\n%s\n%s", got[0].body, got[1].body)
	}
	if eventID(t, got[1].body) != ev.ID {
		t.Errorf("retry has event ID %q, want %q", eventID(t, got[1].body), ev.ID)
	}
	if got[0].signature == got[1].signature {
		t.Error("the retry was not signed again")
	}
}

func TestSignatureNeedsTheSecret(t *testing.T) {
	body := []byte(`{"id":"evt_x"}`)
	if sign(secret.New("one"), t0, body) == sign(secret.New("other"), t0, body) {
		t.Error("two secrets give the same signature")
	}
	if sign(secret.New("one"), t0, body) == sign(secret.New("one"), t0, []byte(`{"id":"evt_y"}`)) {
		t.Error("two bodies give the same signature")
	}
}

// Every retry time of the PRD: nothing is sent just before it, one attempt exactly at it, and
// after the last one fails the notification is given up with an alert naming its session.
func TestRetryScheduleAndGiveUp(t *testing.T) {
	app := newStoreApp(t, http.StatusServiceUnavailable)
	f := newFixture(t, app)
	ev := enqueue(t, f.st, t0, sessionEvent(SessionPaymentFailed, sesA))
	if n := f.deliver(t); n != 1 {
		t.Fatalf("first attempt: %d sent, want 1", n)
	}
	for i, d := range retryAfter {
		f.clk.Set(t0.Add(d - time.Nanosecond))
		if n := f.deliver(t); n != 0 {
			t.Fatalf("1ns before retry %d (%v): %d sent", i+1, d, n)
		}
		f.clk.Set(t0.Add(d))
		if n := f.deliver(t); n != 1 {
			t.Fatalf("at retry %d (%v): %d sent, want 1", i+1, d, n)
		}
		if i < len(retryAfter)-1 {
			if r := f.record(t, ev.ID); r.status != Retrying || r.attempts != i+2 || !r.last.Equal(t0.Add(d)) {
				t.Fatalf("after retry %d: %+v", i+1, r)
			}
			if len(f.alerts(t)) != 0 {
				t.Fatalf("alert raised before the last retry")
			}
		}
	}
	if r := f.record(t, ev.ID); r.status != GivenUp || r.attempts != len(retryAfter)+1 {
		t.Errorf("after the last retry: %+v, want given_up after %d attempts", r, len(retryAfter)+1)
	}
	as := f.alerts(t)
	if len(as) != 1 || as[0].Kind != alert.NotificationGivenUp || as[0].SessionID != sesA || as[0].MemberID != "" ||
		!as[0].RaisedAt.Equal(t0.Add(24*time.Hour)) || !strings.Contains(as[0].Message, ev.ID) {
		t.Errorf("alerts = %+v; want one notification_given_up for %s at 24h naming %s", as, sesA, ev.ID)
	}
	f.clk.Advance(48 * time.Hour)
	if n := f.deliver(t); n != 0 || len(app.requests()) != len(retryAfter)+1 {
		t.Errorf("a given-up notification was sent again")
	}
}

// A member event that is given up names the member, not a session.
func TestGiveUpOfMemberEventNamesTheMember(t *testing.T) {
	app := newStoreApp(t, http.StatusInternalServerError)
	f := newFixture(t, app)
	enqueue(t, f.st, t0, Event{Type: MemberSavedPaypalDeleted, SubjectID: "m_1024", Data: map[string]any{"reason": "member_request"}})
	for _, d := range append([]time.Duration{0}, retryAfter...) {
		f.clk.Set(t0.Add(d))
		f.deliver(t)
	}
	as := f.alerts(t)
	if len(as) != 1 || as[0].MemberID != "m_1024" || as[0].SessionID != "" {
		t.Errorf("alerts = %+v; want one naming member m_1024", as)
	}
}

// Any 2xx acknowledges: nothing is sent after it.
func TestAcknowledgedIsNotSentAgain(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent, 299} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			app := newStoreApp(t, http.StatusBadGateway, status)
			f := newFixture(t, app)
			ev := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
			f.deliver(t)
			f.clk.Set(t0.Add(retryAfter[0]))
			f.deliver(t)
			for _, d := range retryAfter[1:] {
				f.clk.Set(t0.Add(d))
				f.deliver(t)
			}
			if n := len(app.requests()); n != 2 {
				t.Errorf("%d requests, want 2", n)
			}
			if r := f.record(t, ev.ID); r.status != Delivered || r.attempts != 2 {
				t.Errorf("record = %+v, want delivered after 2 attempts", r)
			}
			if len(f.alerts(t)) != 0 {
				t.Error("an alert was raised")
			}
		})
	}
}

// Anything but a 2xx is retried at the next retry time and not before.
func TestNotAcknowledgedIsRetried(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound,
		http.StatusInternalServerError, http.StatusMovedPermanently, http.StatusFound, http.StatusNotModified,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			app := newStoreApp(t, status)
			f := newFixture(t, app)
			ev := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
			f.deliver(t)
			if r := f.record(t, ev.ID); r.status != Retrying || r.attempts != 1 {
				t.Errorf("record = %+v, want retrying after 1 attempt", r)
			}
			f.clk.Set(t0.Add(retryAfter[0] - time.Nanosecond))
			if n := f.deliver(t); n != 0 {
				t.Errorf("sent again before the first retry time")
			}
			f.clk.Set(t0.Add(retryAfter[0]))
			if n := f.deliver(t); n != 1 {
				t.Errorf("not sent again at the first retry time")
			}
		})
	}
}

// A redirect is not followed: the place it points to receives nothing.
func TestRedirectIsNotFollowed(t *testing.T) {
	other := newStoreApp(t, http.StatusOK)
	app := newStoreApp(t)
	app.setAnswer(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusTemporaryRedirect)
	})
	f := newFixture(t, app)
	ev := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
	f.deliver(t)
	if n := len(other.requests()); n != 0 {
		t.Errorf("the redirect target received %d requests", n)
	}
	if r := f.record(t, ev.ID); r.status != Retrying {
		t.Errorf("record = %+v, want retrying", r)
	}
}

// No answer within the timeout is not an acknowledgement, even if the store app answers 200 later.
func TestNoAnswerInTimeIsRetried(t *testing.T) {
	release := make(chan struct{})
	app := newStoreApp(t)
	app.setAnswer(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	})
	defer close(release)
	f := newFixture(t, app)
	f.d = f.dispatcher(app.URL, 50*time.Millisecond)
	ev := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
	f.deliver(t)
	if r := f.record(t, ev.ID); r.status != Retrying || r.attempts != 1 {
		t.Errorf("record = %+v, want retrying after 1 attempt", r)
	}
	if !strings.Contains(f.log.String(), "no answer within") {
		t.Errorf("the log does not say why:\n%s", f.log)
	}
}

func TestUnreachableStoreAppIsRetried(t *testing.T) {
	app := newStoreApp(t, http.StatusOK)
	f := newFixture(t, app)
	app.Close()
	ev := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
	f.deliver(t)
	if r := f.record(t, ev.ID); r.status != Retrying || r.attempts != 1 {
		t.Errorf("record = %+v, want retrying after 1 attempt", r)
	}
}

// The outbox: a change that rolls back sends nothing; one that commits is sent.
func TestOnlyCommittedEventsAreSent(t *testing.T) {
	app := newStoreApp(t, http.StatusOK)
	f := newFixture(t, app)
	_ = f.st.Tx(context.Background(), func(tx store.Querier) error {
		if _, err := Enqueue(context.Background(), tx, t0, sessionEvent(SessionStarted, sesA)); err != nil {
			t.Fatal(err)
		}
		return errors.New("the check-in failed")
	})
	f.deliver(t)
	if n := len(app.requests()); n != 0 {
		t.Fatalf("a rolled-back event was sent (%d requests)", n)
	}
	ev := enqueue(t, f.st, t0, sessionEvent(SessionStarted, sesA))
	f.deliver(t)
	got := app.requests()
	if len(got) != 1 || eventID(t, got[0].body) != ev.ID {
		t.Errorf("requests = %d; want the committed event once", len(got))
	}
}

// A notification stored just before BarPay stopped is sent after the restart.
func TestNeverAttemptedIsSentAfterRestart(t *testing.T) {
	app := newStoreApp(t, http.StatusOK)
	f := newFixture(t, app)
	ev := enqueue(t, f.st, t0, sessionEvent(SessionStarted, sesA))
	f.clk.Set(t0.Add(3 * time.Hour))
	f.restart(t, app)
	f.deliver(t)
	got := app.requests()
	if len(got) != 1 || eventID(t, got[0].body) != ev.ID {
		t.Fatalf("requests = %d; want the stored event once", len(got))
	}
	if at := verify(t, got[0]); !at.Equal(t0.Add(3 * time.Hour)) {
		t.Errorf("signed for %v, want the restart time", at)
	}
}

// Retry times come from the stored first attempt, so a restart keeps them.
func TestRetryResumesAfterRestart(t *testing.T) {
	app := newStoreApp(t, http.StatusInternalServerError, http.StatusOK)
	f := newFixture(t, app)
	ev := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
	f.deliver(t)

	f.clk.Set(t0.Add(30 * time.Second))
	f.restart(t, app)
	f.clk.Set(t0.Add(retryAfter[0] - time.Nanosecond))
	if n := f.deliver(t); n != 0 {
		t.Fatalf("sent before the first retry time after a restart")
	}
	f.clk.Set(t0.Add(retryAfter[0]))
	if n := f.deliver(t); n != 1 {
		t.Fatalf("not sent at the first retry time after a restart")
	}
	got := app.requests()
	if !bytes.Equal(got[0].body, got[1].body) {
		t.Error("the body changed across the restart")
	}
	if r := f.record(t, ev.ID); r.status != Delivered || r.attempts != 2 {
		t.Errorf("record = %+v, want delivered after 2 attempts", r)
	}
}

// The PRD's rule for retry times passed while BarPay was stopped: one attempt at the start,
// then the next retry time still ahead.
func TestMissedRetriesAreSentOnceAtStart(t *testing.T) {
	app := newStoreApp(t, http.StatusInternalServerError)
	f := newFixture(t, app)
	ev := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
	f.deliver(t)

	f.clk.Set(t0.Add(40 * time.Minute)) // past the 1, 5 and 30 minute retries
	f.restart(t, app)
	if n := f.deliver(t); n != 1 {
		t.Fatalf("at start: %d attempts, want 1", n)
	}
	if n := f.deliver(t); n != 0 {
		t.Fatalf("a second attempt for the same missed retries")
	}
	if r := f.record(t, ev.ID); r.status != Retrying || r.attempts != 2 {
		t.Fatalf("record = %+v, want retrying after 2 attempts", r)
	}
	f.clk.Set(t0.Add(2*time.Hour - time.Nanosecond))
	if n := f.deliver(t); n != 0 {
		t.Fatalf("sent before the 2 hour retry")
	}
	f.clk.Set(t0.Add(2 * time.Hour))
	if n := f.deliver(t); n != 1 {
		t.Fatalf("not sent at the 2 hour retry")
	}
}

// When the last retry time passed while BarPay was stopped, the attempt at the start is the last.
func TestStartAfterLastRetryTimeMakesOneLastAttempt(t *testing.T) {
	cases := map[string]struct {
		status int
		want   Status
		alerts int
	}{
		"acknowledged":     {http.StatusOK, Delivered, 0},
		"not acknowledged": {http.StatusInternalServerError, GivenUp, 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			app := newStoreApp(t, http.StatusInternalServerError, c.status)
			f := newFixture(t, app)
			ev := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
			f.deliver(t)

			f.clk.Set(t0.Add(30 * time.Hour))
			f.restart(t, app)
			if n := f.deliver(t); n != 1 {
				t.Fatalf("at start: %d attempts, want 1", n)
			}
			if r := f.record(t, ev.ID); r.status != c.want || r.attempts != 2 {
				t.Errorf("record = %+v, want %s after 2 attempts", r, c.want)
			}
			if n := len(f.alerts(t)); n != c.alerts {
				t.Errorf("%d alerts, want %d", n, c.alerts)
			}
			f.clk.Advance(time.Hour)
			if n := f.deliver(t); n != 0 {
				t.Error("sent again after the last attempt")
			}
		})
	}
}

// An attempt cut off by BarPay stopping does not count; the notification goes out after the restart.
func TestAttemptCutOffByStopIsNotCounted(t *testing.T) {
	arrived := make(chan struct{}, 1)
	app := newStoreApp(t)
	app.setAnswer(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done()
	})
	f := newFixture(t, app)
	ev := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := f.d.DeliverDue(ctx); done <- err }()
	<-arrived
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("DeliverDue = %v, want context.Canceled", err)
	}
	if r := f.record(t, ev.ID); r.status != Retrying || r.attempts != 0 {
		t.Errorf("record = %+v, want retrying with no attempt counted", r)
	}
}

// Run sends a stored notification as soon as it is woken.
func TestRunSendsWhenWoken(t *testing.T) {
	got := make(chan struct{}, 4)
	app := newStoreApp(t)
	app.setAnswer(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		got <- struct{}{}
	})
	f := newFixture(t, app)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { f.d.Run(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()

	enqueue(t, f.st, t0, sessionEvent(SessionStarted, sesA))
	f.d.Wake()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("not sent within 5s of Wake")
	}
}

// The signing secret appears in no log line and no stored record.
func TestSigningSecretIsNotLoggedOrStored(t *testing.T) {
	app := newStoreApp(t, http.StatusInternalServerError)
	f := newFixture(t, app)
	enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
	for _, d := range append([]time.Duration{0}, retryAfter...) {
		f.clk.Set(t0.Add(d))
		f.deliver(t)
	}
	if strings.Contains(f.log.String(), signingSecret) {
		t.Errorf("the log contains the signing secret:\n%s", f.log)
	}
	for _, table := range []string{"notifications", "alerts"} {
		rows, err := f.st.Read().QueryContext(context.Background(), "SELECT * FROM "+table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if s, ok := v.(string); ok && strings.Contains(s, signingSecret) ||
					func() bool { b, ok := v.([]byte); return ok && bytes.Contains(b, []byte(signingSecret)) }() {
					t.Errorf("%s.%s contains the signing secret", table, cols[i])
				}
			}
		}
		rows.Close()
	}
}
