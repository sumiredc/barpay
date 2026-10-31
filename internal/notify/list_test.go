package notify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/secret"
)

var apiKeys = httpapi.Keys{Store: secret.New("k-store"), Admin: secret.New("k-admin"), Edge: secret.New("k-edge")}

func listRouter(f *fixture) *httpapi.Router {
	r := httpapi.NewRouter(apiKeys)
	r.Admin("GET /notifications", ListHandler(f.st, slog.New(slog.NewTextHandler(f.log, nil))))
	return r
}

func get(r http.Handler, query, auth, staffID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/notifications"+query, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if staffID != "" {
		req.Header.Set("BarPay-Staff-Id", staffID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type listedRecord struct {
	EventID       string  `json:"event_id"`
	Type          string  `json:"type"`
	Status        string  `json:"status"`
	Attempts      int     `json:"attempts"`
	CreatedAt     string  `json:"created_at"`
	LastAttemptAt *string `json:"last_attempt_at"`
}

type listed struct {
	Notifications []listedRecord `json:"notifications"`
	NextCursor    *string        `json:"next_cursor"`
}

func list(t *testing.T, r http.Handler, query string) listed {
	t.Helper()
	w := get(r, query, "Bearer k-admin", "staff_1")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body %s", query, w.Code, w.Body)
	}
	var l listed
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		t.Fatalf("body %s: %v", w.Body, err)
	}
	if l.Notifications == nil {
		t.Fatalf("notifications is not an array: %s", w.Body)
	}
	return l
}

// Newest first, with each record's status and number of attempts.
func TestListShowsStatusAndAttempts(t *testing.T) {
	app := newStoreApp(t)
	f := newFixture(t, app)

	delivered := enqueue(t, f.st, t0, sessionEvent(SessionStarted, sesA))
	f.deliver(t)
	app.setAnswer(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	givenUp := enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesB))
	for _, d := range append([]time.Duration{0}, retryAfter...) {
		f.clk.Set(t0.Add(d))
		f.deliver(t)
	}
	later := t0.Add(25 * time.Hour)
	f.clk.Set(later)
	retrying := enqueue(t, f.st, later, Event{Type: MemberPaypalLinked, SubjectID: "m_1024", Data: map[string]any{}})
	f.deliver(t)
	notYet := enqueue(t, f.st, later.Add(time.Second), sessionEvent(SessionStarted, sesB))

	got := list(t, listRouter(f), "").Notifications
	str := func(s string) *string { return &s }
	want := []listedRecord{
		{notYet.ID, "session.started", "retrying", 0, "2026-11-01T22:00:01Z", nil},
		{retrying.ID, "member.paypal_linked", "retrying", 1, "2026-11-01T22:00:00Z", str("2026-11-01T22:00:00Z")},
		{givenUp.ID, "session.closed", "given_up", len(retryAfter) + 1, "2026-10-31T21:00:00Z", str("2026-11-01T21:00:00Z")},
		{delivered.ID, "session.started", "delivered", 1, "2026-10-31T21:00:00Z", str("2026-10-31T21:00:00Z")},
	}
	if len(got) != len(want) {
		t.Fatalf("%d records, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.EventID != w.EventID || g.Type != w.Type || g.Status != w.Status || g.Attempts != w.Attempts ||
			g.CreatedAt != w.CreatedAt || (g.LastAttemptAt == nil) != (w.LastAttemptAt == nil) ||
			(g.LastAttemptAt != nil && *g.LastAttemptAt != *w.LastAttemptAt) {
			t.Errorf("record %d = %+v (last %v), want %+v (last %v)", i, g, deref(g.LastAttemptAt), w, deref(w.LastAttemptAt))
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

// limit and cursor walk the log page by page; an event stored meanwhile does not shift the pages.
func TestListPages(t *testing.T) {
	app := newStoreApp(t)
	f := newFixture(t, app)
	var ids []string
	for i := range 5 {
		ids = append(ids, enqueue(t, f.st, t0.Add(time.Duration(i)*time.Second), sessionEvent(SessionStarted, sesA)).ID)
	}
	r := listRouter(f)

	var seen []string
	page := list(t, r, "?limit=2")
	for range 5 {
		for _, n := range page.Notifications {
			seen = append(seen, n.EventID)
		}
		if page.NextCursor == nil {
			break
		}
		if len(seen) == 2 {
			enqueue(t, f.st, t0.Add(time.Minute), sessionEvent(SessionClosed, sesA))
		}
		page = list(t, r, "?limit=2&cursor="+url.QueryEscape(*page.NextCursor))
	}
	want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("pages listed %v, want %v", seen, want)
	}
}

func TestListLastPageHasNoCursor(t *testing.T) {
	app := newStoreApp(t)
	f := newFixture(t, app)
	r := listRouter(f)
	if l := list(t, r, ""); len(l.Notifications) != 0 || l.NextCursor != nil {
		t.Errorf("empty log: %+v", l)
	}
	for range 2 {
		enqueue(t, f.st, t0, sessionEvent(SessionStarted, sesA))
	}
	if l := list(t, r, "?limit=2"); len(l.Notifications) != 2 || l.NextCursor != nil {
		t.Errorf("exactly one full page: %d records, next_cursor %v", len(l.Notifications), deref(l.NextCursor))
	}
}

func TestListDefaultLimit(t *testing.T) {
	app := newStoreApp(t)
	f := newFixture(t, app)
	for range httpapi.DefaultLimit + 1 {
		enqueue(t, f.st, t0, sessionEvent(SessionStarted, sesA))
	}
	if l := list(t, listRouter(f), ""); len(l.Notifications) != httpapi.DefaultLimit || l.NextCursor == nil {
		t.Errorf("%d records, next_cursor %v; want %d and a cursor", len(l.Notifications), deref(l.NextCursor), httpapi.DefaultLimit)
	}
}

func TestListRefusesBadLimitOrCursor(t *testing.T) {
	app := newStoreApp(t)
	f := newFixture(t, app)
	enqueue(t, f.st, t0, sessionEvent(SessionStarted, sesA))
	r := listRouter(f)
	cases := map[string]string{
		"?limit=0":                                 "limit",
		"?limit=101":                               "limit",
		"?limit=x":                                 "limit",
		"?cursor=" + url.QueryEscape("@@"):         "cursor",
		"?cursor=" + httpapi.Cursor("alerts", 1):   "cursor",
		"?cursor=" + httpapi.Cursor(listName, 999): "cursor", // well formed, names nothing
		"?cursor=" + base64.RawURLEncoding.EncodeToString([]byte(listName+":1")) + "AAAA": "cursor",
	}
	for q, field := range cases {
		t.Run(q, func(t *testing.T) {
			w := get(r, q, "Bearer k-admin", "staff_1")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Details struct {
						Field string `json:"field"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "invalid_request" || body.Error.Details.Field != field {
				t.Errorf("body = %s; want invalid_request on %s", w.Body, field)
			}
		})
	}
}

// Only the admin API key reads the log, and only with a staff ID.
func TestListAccess(t *testing.T) {
	app := newStoreApp(t)
	f := newFixture(t, app)
	r := listRouter(f)
	for name, auth := range map[string]string{
		"no key":             "",
		"wrong key":          "Bearer k-wrong",
		"store API key":      "Bearer k-store",
		"edge shared secret": "Bearer k-edge",
		"not bearer":         "Basic k-admin",
	} {
		t.Run(name, func(t *testing.T) {
			if w := get(r, "", auth, "staff_1"); w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
		})
	}
	w := get(r, "", "Bearer k-admin", "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"field":"BarPay-Staff-Id"`) {
		t.Errorf("without a staff ID: status = %d, body %s; want 400 on BarPay-Staff-Id", w.Code, w.Body)
	}
}

// A log that cannot be read answers 500 and says why in BarPay's own log.
func TestListFailureIsLogged(t *testing.T) {
	app := newStoreApp(t)
	f := newFixture(t, app)
	r := listRouter(f)
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	if w := get(r, "", "Bearer k-admin", "staff_1"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(f.log.String(), "notification log not read") {
		t.Errorf("the failure is not logged:\n%s", f.log)
	}
}

// The signing secret is not in the log the admin API returns.
func TestListDoesNotShowTheSigningSecret(t *testing.T) {
	app := newStoreApp(t, http.StatusInternalServerError)
	f := newFixture(t, app)
	enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
	for _, d := range append([]time.Duration{0}, retryAfter...) {
		f.clk.Set(t0.Add(d))
		f.deliver(t)
	}
	enqueue(t, f.st, f.clk.Now(), sessionEvent(SessionStarted, sesB))
	f.deliver(t)
	w := get(listRouter(f), "", "Bearer k-admin", "staff_1")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), signingSecret) {
		t.Errorf("status = %d; the response contains the signing secret or failed:\n%s", w.Code, w.Body)
	}
}
