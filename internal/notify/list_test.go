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
	"github.com/sumiredc/barpay/internal/spectest"
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
	SessionID     *string `json:"session_id"`
	MemberID      *string `json:"member_id"`
	Status        string  `json:"status"`
	Attempts      int     `json:"attempts"`
	CreatedAt     string  `json:"created_at"`
	LastAttemptAt *string `json:"last_attempt_at"`
	NextAttemptAt *string `json:"next_attempt_at"`
}

type listed struct {
	Notifications []listedRecord `json:"notifications"`
	NextCursor    *string        `json:"next_cursor"`
}

// list reads a page of listNotifications, failing the test unless it is 200 and every record fits
// the spec's NotificationRecord.
func list(t *testing.T, r http.Handler, query string) listed {
	t.Helper()
	l, _ := listRows(t, r, query)
	return l
}

// listRows is list that also returns each record's fields as sent, so a test can tell a field
// that is absent from one that is null.
func listRows(t *testing.T, r http.Handler, query string) (listed, []map[string]json.RawMessage) {
	t.Helper()
	w := get(r, query, "Bearer k-admin", "staff_1")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body %s", query, w.Code, w.Body)
	}
	var raw struct {
		Notifications []json.RawMessage `json:"notifications"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body %s: %v", w.Body, err)
	}
	rows := make([]map[string]json.RawMessage, len(raw.Notifications))
	for i, n := range raw.Notifications {
		spectest.Valid(t, "NotificationRecord", n)
		if err := json.Unmarshal(n, &rows[i]); err != nil {
			t.Fatal(err)
		}
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
	return l, rows
}

// field returns the field as sent, or "absent".
func field(row map[string]json.RawMessage, name string) string {
	v, ok := row[name]
	if !ok {
		return "absent"
	}
	return string(v)
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
		{EventID: notYet.ID, Type: "session.started", Status: "retrying", Attempts: 0, CreatedAt: "2026-11-01T22:00:01Z"},
		{EventID: retrying.ID, Type: "member.paypal_linked", Status: "retrying", Attempts: 1, CreatedAt: "2026-11-01T22:00:00Z", LastAttemptAt: str("2026-11-01T22:00:00Z")},
		{EventID: givenUp.ID, Type: "session.closed", Status: "given_up", Attempts: len(retryAfter) + 1, CreatedAt: "2026-10-31T21:00:00Z", LastAttemptAt: str("2026-11-01T21:00:00Z")},
		{EventID: delivered.ID, Type: "session.started", Status: "delivered", Attempts: 1, CreatedAt: "2026-10-31T21:00:00Z", LastAttemptAt: str("2026-10-31T21:00:00Z")},
	}
	if len(got) != len(want) {
		t.Fatalf("%d records, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.EventID != w.EventID || g.Type != w.Type || g.Status != w.Status || g.Attempts != w.Attempts ||
			g.CreatedAt != w.CreatedAt || deref(g.LastAttemptAt) != deref(w.LastAttemptAt) {
			t.Errorf("record %d = %+v (last %v), want %+v (last %v)", i, g, deref(g.LastAttemptAt), w, deref(w.LastAttemptAt))
		}
	}
}

// A record names its session for session.* and gate.*, its member for member.*, and never both.
func TestListShowsSubject(t *testing.T) {
	app := newStoreApp(t)
	f := newFixture(t, app)
	enqueue(t, f.st, t0, sessionEvent(SessionClosed, sesA))
	enqueue(t, f.st, t0, sessionEvent(GateUnlockFailed, sesB))
	enqueue(t, f.st, t0, Event{Type: MemberPaypalLinked, SubjectID: "m_1024", Data: map[string]any{}})

	got, rows := listRows(t, listRouter(f), "")
	want := []struct{ typ, session, member string }{
		{"member.paypal_linked", "absent", `"m_1024"`},
		{"gate.unlock_failed", `"` + sesB + `"`, "absent"},
		{"session.closed", `"` + sesA + `"`, "absent"},
	}
	if len(rows) != len(want) {
		t.Fatalf("%d records, want %d: %+v", len(rows), len(want), got)
	}
	for i, w := range want {
		g := rows[i]
		typ := got.Notifications[i].Type
		if typ != w.typ || field(g, "session_id") != w.session || field(g, "member_id") != w.member {
			t.Errorf("record %d: %s session_id %s member_id %s; want %s session_id %s member_id %s",
				i, typ, field(g, "session_id"), field(g, "member_id"), w.typ, w.session, w.member)
		}
	}
}

// next_attempt_at is the stored time of the next attempt while retrying, before the first attempt
// too, and null once delivered or given up.
func TestListShowsNextAttempt(t *testing.T) {
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

	stored := func(id string) string {
		var next int64
		if err := f.st.Read().QueryRowContext(t.Context(), "SELECT next_attempt_at FROM notifications WHERE id = ?", id).Scan(&next); err != nil {
			t.Fatal(err)
		}
		return `"` + formatTime(time.Unix(0, next)) + `"`
	}
	want := map[string]string{
		notYet.ID:    stored(notYet.ID),
		retrying.ID:  stored(retrying.ID),
		givenUp.ID:   "null",
		delivered.ID: "null",
	}
	// The stored times are the ones the schedule gives, not some other column read by mistake.
	if want[notYet.ID] != `"2026-11-01T22:00:01Z"` || want[retrying.ID] != `"2026-11-01T22:01:00Z"` {
		t.Fatalf("stored next attempts %s and %s, want 22:00:01 and 22:01:00", want[notYet.ID], want[retrying.ID])
	}
	l, rows := listRows(t, listRouter(f), "")
	if len(rows) != len(want) {
		t.Fatalf("%d records, want %d: %+v", len(rows), len(want), l.Notifications)
	}
	for i, g := range l.Notifications {
		if got := field(rows[i], "next_attempt_at"); got != want[g.EventID] {
			t.Errorf("%s (%s): next_attempt_at %s, want %s", g.Type, g.Status, got, want[g.EventID])
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
