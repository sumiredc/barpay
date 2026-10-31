package alert

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

const noAuth = "-"

type listFixture struct {
	st     *store.Store
	router *httpapi.Router
}

func newListFixture(t *testing.T) *listFixture {
	t.Helper()
	st := storetest.Open(t)
	r := httpapi.NewRouter(httpapi.Keys{Store: secret.New("k-store"), Admin: secret.New("k-admin"), Edge: secret.New("k-edge")})
	API{Store: st, Log: slog.New(slog.DiscardHandler)}.Register(r)
	return &listFixture{st: st, router: r}
}

func (f *listFixture) get(query, auth, staffID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/alerts"+query, nil)
	if auth != noAuth {
		req.Header.Set("Authorization", auth)
	}
	if staffID != noAuth {
		req.Header.Set("BarPay-Staff-Id", staffID)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

type alertPage struct {
	Alerts     []json.RawMessage `json:"alerts"`
	NextCursor *string           `json:"next_cursor"`
}

// listed reads a page of listAlerts, failing the test unless it is 200 and every alert fits the
// spec's Alert.
func (f *listFixture) listed(t *testing.T, query string) alertPage {
	t.Helper()
	w := f.get(query, "Bearer k-admin", "staff_1")
	if w.Code != http.StatusOK {
		t.Fatalf("listAlerts%s: %d %s", query, w.Code, w.Body)
	}
	var p alertPage
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Alerts == nil {
		t.Fatalf("alerts is not an array: %s", w.Body)
	}
	for _, a := range p.Alerts {
		spectest.Valid(t, "Alert", a)
	}
	return p
}

func alertIDs(t *testing.T, p alertPage) []string {
	t.Helper()
	out := make([]string, len(p.Alerts))
	for i, a := range p.Alerts {
		var v struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(a, &v); err != nil {
			t.Fatal(err)
		}
		out[i] = v.ID
	}
	return out
}

// allPages reads every page of listAlerts, limit at a time.
func (f *listFixture) allPages(t *testing.T, limit int) []string {
	t.Helper()
	var out []string
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	for {
		p := f.listed(t, "?"+q.Encode())
		out = append(out, alertIDs(t, p)...)
		if p.NextCursor == nil {
			return out
		}
		q.Set("cursor", *p.NextCursor)
	}
}

func (f *listFixture) raised(t *testing.T, a Alert) Alert {
	t.Helper()
	got, err := raise(f.st, a)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The PRD's Guarantees, Observability: every alert raised is listed, newest first, as the spec's
// Alert, with the ID it was raised with and without the ID it was not.
func TestListAlertsNewestFirst(t *testing.T) {
	f := newListFixture(t)
	if p := f.listed(t, ""); len(p.Alerts) != 0 || p.NextCursor != nil {
		t.Errorf("with no alert: %d alerts, next %v", len(p.Alerts), p.NextCursor)
	}
	older := f.raised(t, Alert{Kind: DeletionFailing, RaisedAt: t0, MemberID: "m_1", Message: "Deletion failing."})
	newer := f.raised(t, Alert{Kind: RegisterRequested, RaisedAt: t0.Add(time.Minute), SessionID: "ses_8f3k2m9q1x7v4b6n", Message: "At the register."})
	p := f.listed(t, "")
	if len(p.Alerts) != 2 || p.NextCursor != nil {
		t.Fatalf("got %d alerts, next %v", len(p.Alerts), p.NextCursor)
	}
	for i, want := range []string{
		`{"id":"` + newer.ID + `","kind":"register_requested","raised_at":"2026-10-31T21:01:00Z","session_id":"ses_8f3k2m9q1x7v4b6n","message":"At the register."}`,
		`{"id":"` + older.ID + `","kind":"deletion_failing","raised_at":"2026-10-31T21:00:00Z","member_id":"m_1","message":"Deletion failing."}`,
	} {
		if string(p.Alerts[i]) != want {
			t.Errorf("alert %d = %s; want %s", i, p.Alerts[i], want)
		}
	}
}

// The spec's Cursor: alerts raised while paging, at the same instant as listed ones too, neither
// repeat nor drop an alert on the following pages; they are on a new first page. The order is the
// same on every read, also among alerts raised at the same instant.
func TestListAlertsPagesHoldWhileAlertsAreRaised(t *testing.T) {
	f := newListFixture(t)
	var want []string
	for range 5 {
		want = append([]string{f.raised(t, Alert{Kind: PaymentFailed, RaisedAt: t0, Message: "m"}).ID}, want...)
	}
	for _, limit := range []int{1, 2, 3, 5} {
		if got := f.allPages(t, limit); !slices.Equal(got, want) {
			t.Errorf("pages of %d = %v; want %v", limit, got, want)
		}
	}

	first := f.listed(t, "?limit=2")
	added := f.raised(t, Alert{Kind: PaymentFailed, RaisedAt: t0, Message: "m"})
	got := alertIDs(t, first)
	for c := first.NextCursor; c != nil; {
		p := f.listed(t, "?limit=2&cursor="+url.QueryEscape(*c))
		got = append(got, alertIDs(t, p)...)
		c = p.NextCursor
	}
	if !slices.Equal(got, want) {
		t.Errorf("pages with an alert raised after the first = %v; want %v", got, want)
	}
	if got := alertIDs(t, f.listed(t, "?limit=1")); !slices.Equal(got, []string{added.ID}) {
		t.Errorf("new first page = %v; want %s", got, added.ID)
	}
}

// The spec's Limit and Cursor: a limit out of range and a cursor that is malformed, from another
// list or names no alert are refused.
func TestListAlertsRefusesBadParameters(t *testing.T) {
	f := newListFixture(t)
	f.raised(t, Alert{Kind: PaymentFailed, RaisedAt: t0, Message: "m"})
	for _, c := range []struct{ query, field string }{
		{"?limit=0", "limit"},
		{"?limit=101", "limit"},
		{"?limit=-1", "limit"},
		{"?cursor=", "cursor"},
		{"?cursor=YWxlcnRzOjAx", "cursor"}, // "alerts:01", not as Cursor writes it
		{"?cursor=" + httpapi.Cursor("sessions", 1), "cursor"},
		{"?cursor=" + httpapi.Cursor("alerts", 99), "cursor"},
	} {
		w := f.get(c.query, "Bearer k-admin", "staff_1")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", c.query, w.Code, w.Body)
			continue
		}
		spectest.Valid(t, "Error", w.Body.Bytes())
		if want := `"code":"invalid_request","message":`; !bytes.Contains(w.Body.Bytes(), []byte(want)) ||
			!bytes.Contains(w.Body.Bytes(), []byte(`"field":"`+c.field+`"`)) {
			t.Errorf("%s: %s; want invalid_request for %s", c.query, w.Body, c.field)
		}
	}
}

// Guarantees → Access: only the admin API key lists alerts, and it needs the staff ID.
func TestListAlertsNeedsTheAdminKeyAndStaffID(t *testing.T) {
	f := newListFixture(t)
	a := f.raised(t, Alert{Kind: PaymentFailed, RaisedAt: t0, Message: "m"})
	for _, auth := range []string{noAuth, "Bearer wrong", "Bearer k-store", "Bearer k-edge"} {
		w := f.get("", auth, "staff_1")
		if w.Code != http.StatusUnauthorized || bytes.Contains(w.Body.Bytes(), []byte(a.ID)) {
			t.Errorf("with %s: %d %s; want 401", auth, w.Code, w.Body)
		}
	}
	for _, id := range []string{noAuth, "bad id"} {
		w := f.get("", "Bearer k-admin", id)
		if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(`"field":"BarPay-Staff-Id"`)) {
			t.Errorf("staff ID %q: %d %s; want 400 for BarPay-Staff-Id", id, w.Code, w.Body)
		}
	}
}
