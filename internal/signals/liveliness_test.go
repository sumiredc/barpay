package signals

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func (f *fixture) liveliness(t *testing.T, at time.Time) (float64, bool) {
	t.Helper()
	v, ok, err := LivelinessAt(context.Background(), f.st.Read(), at)
	if err != nil {
		t.Fatal(err)
	}
	return v, ok
}

func (f *fixture) sendSignals(body string) *http.Response {
	return f.do(http.MethodPost, "/v1/edge/signals", edgeAuth, "", body).Result()
}

// The last value is used until LivelinessUse after it was received: just before, it is; exactly
// then and later, there is no liveliness signal. A restart keeps it.
func TestLivelinessIsUsedForLivelinessUse(t *testing.T) {
	f := newFixture(t)
	if _, ok := f.liveliness(t, t0); ok {
		t.Fatal("a liveliness signal before any was sent")
	}
	if r := f.sendSignals(`{"liveliness":0.25}`); r.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", r.StatusCode)
	}
	f.clk.Advance(5 * time.Second)
	if r := f.sendSignals(`{"liveliness":0.75}`); r.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", r.StatusCode)
	}
	f.restart(t)
	received := t0.Add(5 * time.Second)
	for at, want := range map[time.Time]bool{
		received: true,
		received.Add(LivelinessUse - time.Nanosecond): true,
		received.Add(LivelinessUse):                   false,
		received.Add(time.Hour):                       false,
	} {
		v, ok := f.liveliness(t, at)
		if ok != want || (ok && v != 0.75) {
			t.Errorf("at %s: %v %v; want used: %v (0.75)", at.Sub(received), v, ok, want)
		}
	}
}

// The edge device sends several values within one use.
func TestLivelinessIntervalFitsInItsUse(t *testing.T) {
	if LivelinessUse < 3*LivelinessInterval {
		t.Errorf("use %s holds fewer than 3 sends every %s", LivelinessUse, LivelinessInterval)
	}
}

// Numbers only, 0 to 1, and only the fields edge.yaml defines; a refused request keeps nothing.
func TestSendSignalsRefusesAnythingButANumberFromZeroToOne(t *testing.T) {
	f := newFixture(t)
	for body, field := range map[string]string{
		`{}`:                                  "liveliness",
		`{"liveliness":"0.5"}`:                "liveliness",
		`{"liveliness":null}`:                 "liveliness",
		`{"liveliness":true}`:                 "liveliness",
		`{"liveliness":[0.5]}`:                "liveliness",
		`{"liveliness":{"value":0.5}}`:        "liveliness",
		`{"liveliness":-0.01}`:                "liveliness",
		`{"liveliness":1.0001}`:               "liveliness",
		`{"liveliness":1e400}`:                "liveliness",
		`{"liveliness":0.5,"audio":"UklGRg"}`: "audio",
		`{"liveliness":0.5,"people":3}`:       "people",
		`{"Liveliness":0.5}`:                  "Liveliness",
		`{"liveliness":0.5,"liveliness":0.6}`: "liveliness",
		`[0.5]`:                               "",
		`0.5`:                                 "",
		`{"liveliness":0.5} {}`:               "",
		``:                                    "",
	} {
		w := f.do(http.MethodPost, "/v1/edge/signals", edgeAuth, "", body)
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Details struct {
					Field string `json:"field"`
				} `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != http.StatusBadRequest || e.Error.Code != "invalid_request" || e.Error.Details.Field != field {
			t.Errorf("%s: %d %s; want 400 invalid_request on %q", body, w.Code, w.Body, field)
		}
	}
	if n := f.count(t, "liveliness"); n != 0 {
		t.Errorf("%d liveliness rows stored", n)
	}
	for _, body := range []string{`{"liveliness":0}`, `{"liveliness":1}`, `{"liveliness":1e-3}`, ` {"liveliness": 0.5 } `} {
		if r := f.sendSignals(body); r.StatusCode != http.StatusNoContent {
			t.Errorf("%s: %d, want 204", body, r.StatusCode)
		}
	}
}

// Only the edge device's shared secret.
func TestSendSignalsAccess(t *testing.T) {
	f := newFixture(t)
	for name, auth := range map[string]string{"no key": "", "store API key": storeAuth, "admin API key": adminAuth} {
		w := f.do(http.MethodPost, "/v1/edge/signals", auth, "staff_1", `{"liveliness":0.5}`)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "unauthorized") {
			t.Errorf("%s: %d %s, want 401", name, w.Code, w.Body)
		}
	}
	if n := f.count(t, "liveliness"); n != 0 {
		t.Errorf("%d liveliness rows stored", n)
	}
}
