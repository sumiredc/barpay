package idempotency

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var (
	t0   = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	keys = httpapi.Keys{Store: secret.New("k-store"), Admin: secret.New("k-admin"), Edge: secret.New("k-edge")}
)

// fixture serves test operations that count their runs and change a table, so a test can tell
// whether an operation ran again.
type fixture struct {
	st     *store.Store
	clk    *clock.Fake
	router *httpapi.Router
	runs   atomic.Int64
	// gate, if not nil, holds the operation until it is closed.
	gate chan struct{}
	// started receives once the operation has begun.
	started chan struct{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := storetest.Open(t)
	err := st.Tx(context.Background(), func(tx store.Querier) error {
		_, err := tx.ExecContext(context.Background(), "CREATE TABLE things (n INTEGER NOT NULL)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, clk: clock.NewFake(t0), started: make(chan struct{}, 10)}
	f.build()
	return f
}

// build makes a new Keeper and router on f's store, as a restart does.
func (f *fixture) build() {
	k := New(f.st, f.clk)
	r := httpapi.NewRouter(keys)
	// change records a thing and keeps a response with status; a 4xx stands for an error given
	// after something changed, such as a failed charge.
	change := func(status int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			n := f.runs.Add(1)
			f.started <- struct{}{}
			if f.gate != nil {
				<-f.gate
			}
			body, _ := io.ReadAll(req.Body)
			out := fmt.Appendf(nil, `{"run":%d,"body":%q}`, n, body)
			at := f.clk.Now()
			err := f.st.Tx(req.Context(), func(tx store.Querier) error {
				if _, err := tx.ExecContext(req.Context(), "INSERT INTO things VALUES (?)", n); err != nil {
					return err
				}
				return FromRequest(req).Keep(req.Context(), tx, at, status, out)
			})
			if err != nil {
				httpapi.WriteInternal(w)
				return
			}
			Write(w, status, out)
		})
	}
	// refuse changes nothing and keeps nothing.
	refuse := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.runs.Add(1)
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, "refused", "")
	})
	r.Store("POST /things", k.Require(change(http.StatusCreated)))
	r.Store("POST /other", k.Require(change(http.StatusCreated)))
	r.Store("POST /failing", k.Require(change(http.StatusConflict)))
	r.Store("POST /refused", k.Require(refuse))
	r.Admin("POST /things", k.Require(change(http.StatusCreated)))
	f.router = r
}

type request struct {
	path, auth, key, body, staff string
	noKey                        bool
}

func (f *fixture) do(r request) *httptest.ResponseRecorder {
	if r.path == "" {
		r.path = "/v1/things"
	}
	if r.auth == "" {
		r.auth = "Bearer k-store"
	}
	if r.key == "" && !r.noKey {
		r.key = "key-1"
	}
	req := httptest.NewRequest(http.MethodPost, r.path, strings.NewReader(r.body))
	req.Header.Set("Authorization", r.auth)
	if r.staff == "" {
		r.staff = "staff_1"
	}
	req.Header.Set("BarPay-Staff-Id", r.staff)
	if !r.noKey {
		req.Header.Set(Header, r.key)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func (f *fixture) things(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.Read().QueryRowContext(context.Background(), "SELECT count(*) FROM things").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) (code, field string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Field string `json:"field"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	return body.Error.Code, body.Error.Details.Field
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, body %s; want %d", w.Code, w.Body, status)
	}
	if got, _ := errorCode(t, w); got != code {
		t.Errorf("code = %q, want %q", got, code)
	}
}

func TestSameKeyAndRequestGetTheFirstResponse(t *testing.T) {
	f := newFixture(t)
	first := f.do(request{body: `{"a":1}`})
	again := f.do(request{body: `{"a":1}`})
	if first.Code != http.StatusCreated || again.Code != first.Code || again.Body.String() != first.Body.String() {
		t.Errorf("first %d %s, again %d %s; want the same 201", first.Code, first.Body, again.Code, again.Body)
	}
	if f.runs.Load() != 1 || f.things(t) != 1 {
		t.Errorf("runs = %d, things = %d; want the operation run once", f.runs.Load(), f.things(t))
	}
}

// The first response is returned up to exactly Retention after it was kept; after that the key
// is forgotten and the request runs as a new one.
func TestResponseIsKeptForRetentionInclusive(t *testing.T) {
	f := newFixture(t)
	first := f.do(request{})
	f.clk.Set(t0.Add(Retention - time.Nanosecond))
	if w := f.do(request{}); w.Body.String() != first.Body.String() {
		t.Errorf("just before: %s, want the first response", w.Body)
	}
	f.clk.Set(t0.Add(Retention))
	if w := f.do(request{}); w.Body.String() != first.Body.String() {
		t.Errorf("exactly at Retention: %s, want the first response", w.Body)
	}
	if f.runs.Load() != 1 {
		t.Fatalf("runs = %d up to Retention, want 1", f.runs.Load())
	}
	f.clk.Set(t0.Add(Retention + time.Nanosecond))
	if w := f.do(request{}); w.Code != http.StatusCreated || w.Body.String() == first.Body.String() {
		t.Errorf("after Retention: %d %s, want a new run", w.Code, w.Body)
	}
	if f.runs.Load() != 2 {
		t.Errorf("runs = %d, want 2", f.runs.Load())
	}
}

// An error given after something changed is kept like a success: a resend gets the same error
// and the operation does not run again.
func TestErrorAfterAChangeIsKept(t *testing.T) {
	f := newFixture(t)
	first := f.do(request{path: "/v1/failing"})
	again := f.do(request{path: "/v1/failing"})
	if first.Code != http.StatusConflict || again.Code != first.Code || again.Body.String() != first.Body.String() {
		t.Errorf("first %d %s, again %d %s; want the same kept error", first.Code, first.Body, again.Code, again.Body)
	}
	if f.runs.Load() != 1 {
		t.Errorf("runs = %d, want 1", f.runs.Load())
	}
}

// After an error given before anything changed nothing is kept: the same key runs again.
func TestErrorBeforeAChangeIsNotKept(t *testing.T) {
	f := newFixture(t)
	for range 2 {
		assertError(t, f.do(request{path: "/v1/refused"}), http.StatusBadRequest, "invalid_request")
	}
	if f.runs.Load() != 2 {
		t.Errorf("runs = %d, want 2", f.runs.Load())
	}
}

// With the same API key, a key is one request: another body, another operation or another path
// is refused as reused, and the operation does not run.
func TestSameKeyForAnotherRequestIsRefused(t *testing.T) {
	for name, other := range map[string]request{
		"another body":      {body: `{"a":2}`},
		"another operation": {path: "/v1/other", body: `{"a":1}`},
		"another query":     {path: "/v1/things?x=1", body: `{"a":1}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.do(request{body: `{"a":1}`})
			assertError(t, f.do(other), http.StatusConflict, "idempotency_key_reused")
			if f.runs.Load() != 1 || f.things(t) != 1 {
				t.Errorf("runs = %d, things = %d; want 1", f.runs.Load(), f.things(t))
			}
		})
	}
}

// Under the admin API key, the staff member is part of the request: another staff member with
// the same key is refused as reused, and never gets the first one's response.
func TestSameKeyForAnotherStaffMemberIsRefused(t *testing.T) {
	f := newFixture(t)
	first := request{path: "/v1/admin/things", auth: "Bearer k-admin", body: `{"a":1}`, staff: "staff_a"}
	if w := f.do(first); w.Code != http.StatusCreated {
		t.Fatalf("staff_a: %d %s", w.Code, w.Body)
	}
	other := first
	other.staff = "staff_b"
	assertError(t, f.do(other), http.StatusConflict, "idempotency_key_reused")
	if w := f.do(first); w.Code != http.StatusCreated || f.runs.Load() != 1 {
		t.Errorf("staff_a again: %d, runs %d; want the first response", w.Code, f.runs.Load())
	}
}

// Each API key has its own keys: the same key under the admin API key is independent.
func TestKeysArePerAPIKey(t *testing.T) {
	f := newFixture(t)
	store := f.do(request{body: `{"a":1}`})
	admin := f.do(request{path: "/v1/admin/things", auth: "Bearer k-admin", body: `{"a":1}`})
	if store.Code != http.StatusCreated || admin.Code != http.StatusCreated || store.Body.String() == admin.Body.String() {
		t.Errorf("store %d %s, admin %d %s; want two runs", store.Code, store.Body, admin.Code, admin.Body)
	}
	if f.runs.Load() != 2 {
		t.Errorf("runs = %d, want 2", f.runs.Load())
	}
}

func TestMissingOrMalformedKeyIsRefused(t *testing.T) {
	cases := map[string]request{
		"missing":   {noKey: true},
		"empty":     {key: " "},
		"too long":  {key: strings.Repeat("a", 129)},
		"bad char":  {key: "a/b"},
		"non-ASCII": {key: "kéy"},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			w := f.do(r)
			assertError(t, w, http.StatusBadRequest, "invalid_request")
			if _, field := errorCode(t, w); field != Header {
				t.Errorf("details.field = %q, want %s", field, Header)
			}
			if f.runs.Load() != 0 {
				t.Error("the operation ran")
			}
		})
	}
	f := newFixture(t)
	if w := f.do(request{key: strings.Repeat("aZ0_.:-", 19)[:128]}); w.Code != http.StatusCreated {
		t.Errorf("a 128-character key: %d %s", w.Code, w.Body)
	}
}

// A request whose key is still being processed is refused at once, and does not run.
func TestKeyInProgressIsRefused(t *testing.T) {
	f := newFixture(t)
	f.gate = make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- f.do(request{}) }()
	<-f.started
	assertError(t, f.do(request{}), http.StatusConflict, "idempotency_key_in_progress")
	close(f.gate)
	if w := <-done; w.Code != http.StatusCreated {
		t.Errorf("the first request: %d %s", w.Code, w.Body)
	}
	if f.runs.Load() != 1 {
		t.Errorf("runs = %d, want 1", f.runs.Load())
	}
	// Once it finished, the same key gets its response.
	if w := f.do(request{}); w.Code != http.StatusCreated || f.runs.Load() != 1 {
		t.Errorf("after it finished: %d, runs %d", w.Code, f.runs.Load())
	}
}

// Kept responses survive a restart; a request running when BarPay stopped holds nothing after it.
func TestKeptResponsesSurviveARestart(t *testing.T) {
	f := newFixture(t)
	first := f.do(request{})
	f.st = storetest.Reopen(t, f.st)
	f.build()
	if w := f.do(request{}); w.Body.String() != first.Body.String() || f.runs.Load() != 1 {
		t.Errorf("after restart: %s, runs %d; want the first response", w.Body, f.runs.Load())
	}
}

// The key is checked after the API key: without it the answer is 401 and nothing runs.
func TestAPIKeyIsCheckedFirst(t *testing.T) {
	f := newFixture(t)
	w := f.do(request{auth: "Bearer wrong", noKey: true})
	if w.Code != http.StatusUnauthorized || f.runs.Load() != 0 {
		t.Errorf("status = %d, runs %d; want 401 and no run", w.Code, f.runs.Load())
	}
}
