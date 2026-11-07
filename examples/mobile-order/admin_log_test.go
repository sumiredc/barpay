package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBarPay answers BarPay's notification records from pages, and passes every other read to
// the adminBarPay of the staff's other pages.
type logBarPay struct {
	inner *adminBarPay
	mu    sync.Mutex
	// pages are the records from the newest; the cursor of page i is "p<i>".
	pages [][]NotificationRecord
	// cursors are the cursors of the reads of the records, "" for the first page.
	cursors []string
}

func (f *logBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.inner.mu.Lock()
	down := f.inner.down
	f.inner.mu.Unlock()
	if r.URL.Path != "/v1/admin/notifications" || down {
		f.inner.ServeHTTP(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor := r.URL.Query().Get("cursor")
	f.cursors = append(f.cursors, cursor)
	i := 0
	if cursor != "" {
		i, _ = strconv.Atoi(strings.TrimPrefix(cursor, "p"))
	}
	page := map[string]any{"notifications": []NotificationRecord{}, "next_cursor": nil}
	if i < len(f.pages) {
		page["notifications"] = f.pages[i]
		if i+1 < len(f.pages) {
			page["next_cursor"] = "p" + strconv.Itoa(i+1)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(page)
}

func newLogStaff(t *testing.T) (*staff, *logBarPay) {
	t.Helper()
	s := newStaff(t)
	f := &logBarPay{inner: s.barpay}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s.a.cfg.BarPayURL = srv.URL
	s.a.barpay = newBarPay(s.a.cfg)
	s.logIn()
	return s, f
}

// today is the time of day h:m today.
func today(h, m int) time.Time {
	y, mo, d := time.Now().Date()
	return time.Date(y, mo, d, h, m, 0, 0, time.Local)
}

func at(t time.Time) *time.Time { return &t }

// member makes a member account of the app, with the session IDs of their visits.
func (s *staff) member(id, name string, sessions ...string) {
	s.t.Helper()
	s.a.store.update(func(d *Data) error {
		d.Members[id] = &MemberAccount{ID: id, Name: name, SessionIDs: sessions}
		return nil
	})
}

func (s *staff) received(e ReceivedEvent) {
	s.t.Helper()
	s.a.store.update(func(d *Data) error { d.Received = append(d.Received, e); return nil })
}

var logRowPattern = regexp.MustCompile(`<tr data-sheet="(log-\d+)" tabindex="0">(.*?)</tr>`)
var tagPattern = regexp.MustCompile(`<[^>]+>`)

// logRows are the rows of the log, each as its dialog's ID and its cells' text joined by " | ".
func logRows(page string) [][2]string {
	var rows [][2]string
	for _, m := range logRowPattern.FindAllStringSubmatch(page, -1) {
		cells := strings.Split(m[2], "</td>")
		var text []string
		for _, c := range cells[:len(cells)-1] {
			text = append(text, html.UnescapeString(tagPattern.ReplaceAllString(c, "")))
		}
		rows = append(rows, [2]string{m[1], strings.Join(text, " | ")})
	}
	return rows
}

// detail is the text of a row's dialog, each item as "Name: value" on a line.
func detail(page, id string) string {
	d := sheet(page, id)
	d = strings.NewReplacer("</dd>", "\n", "</dt>", ": ").Replace(d)
	return html.UnescapeString(tagPattern.ReplaceAllString(d, ""))
}

func TestARecordAndWhatThisAppReceivedOfItAreOneRow(t *testing.T) {
	s, f := newLogStaff(t)
	s.member("mem_yui", "Yui Tanaka", "ses_yui")
	f.pages = [][]NotificationRecord{{{
		EventID: "evt_closed", Type: "session.closed", SessionID: "ses_yui", Status: "delivered",
		Attempts: 2, CreatedAt: today(21, 59), LastAttemptAt: at(today(22, 0)),
	}}}
	s.received(ReceivedEvent{EventID: "evt_closed", Type: "session.closed", Subject: "session:ses_yui", Sequence: 3,
		CreatedAt: today(21, 59), ReceivedAt: today(22, 0), Note: "older than one already applied; state kept"})

	page := s.open("/admin/notifications")
	rows := logRows(page)
	if len(rows) != 1 || rows[0][1] != "21:59 | Paid | Yui Tanaka | Delivered" {
		t.Fatalf("rows = %q", rows)
	}
	want := "Type: session.closed\nCustomer: Yui Tanaka (ses_yui)\n" +
		"Sent by BarPay: 21:59, delivered on try 2 at 22:00\n" +
		"This app: Received at 22:00, number 3. Older than the state already read, so nothing changed\n" +
		"Event ID: evt_closed\n"
	if got := detail(page, rows[0][0]); !strings.Contains(got, want) {
		t.Errorf("the dialog does not say\n%s\nit says\n%s", want, got)
	}
}

func TestARecordNotReceivedYetNamesTheCustomerFromItsSession(t *testing.T) {
	s, f := newLogStaff(t)
	s.member("mem_sora", "Sora Kato", "ses_sora")
	f.pages = [][]NotificationRecord{{{
		EventID: "evt_failed", Type: "session.payment_failed", SessionID: "ses_sora", Status: "retrying",
		Attempts: 2, CreatedAt: today(21, 47), LastAttemptAt: at(today(21, 48)), NextAttemptAt: at(today(21, 52)),
	}}}

	page := s.open("/admin/notifications")
	rows := logRows(page)
	if len(rows) != 1 || rows[0][1] != "21:47 | Payment failed | Sora Kato | Retrying" {
		t.Fatalf("rows = %q", rows)
	}
	want := "Type: session.payment_failed\nCustomer: Sora Kato (ses_sora)\n" +
		"Sent by BarPay: 21:47, sent twice (last at 21:48). Next try at 21:52\n" +
		"This app: Not received yet\n"
	if got := detail(page, rows[0][0]); !strings.Contains(got, want) {
		t.Errorf("the dialog does not say\n%s\nit says\n%s", want, got)
	}
}

func TestTheDialogSaysHowEachNotificationWasSentAndApplied(t *testing.T) {
	s, f := newLogStaff(t)
	s.member("mem_yui", "Yui Tanaka", "ses_yui")
	y, mo, d := time.Now().AddDate(0, 0, -1).Date()
	yesterday := time.Date(y, mo, d, 19, 0, 0, 0, time.Local)
	f.pages = [][]NotificationRecord{{
		{EventID: "evt_gate", Type: "gate.unlock_failed", SessionID: "ses_yui", Status: "delivered",
			Attempts: 1, CreatedAt: today(21, 59), LastAttemptAt: at(today(21, 59))},
		{EventID: "evt_ken", Type: "session.payment_failed", SessionID: "ses_ken", Status: "delivered",
			Attempts: 1, CreatedAt: today(21, 40), LastAttemptAt: at(today(21, 40))},
		{EventID: "evt_linked", Type: "member.paypal_linked", MemberID: "mem_yui", Status: "given_up",
			Attempts: 8, CreatedAt: yesterday, LastAttemptAt: at(yesterday.Add(24 * time.Hour))},
	}}
	s.received(ReceivedEvent{EventID: "evt_ken", Type: "session.payment_failed", Subject: "session:ses_ken", Sequence: 3,
		CreatedAt: today(21, 40), ReceivedAt: today(21, 40), Note: "applied in order", Reason: "refused"})
	s.received(ReceivedEvent{EventID: "evt_gate", Type: "gate.unlock_failed", Subject: "session:ses_yui", Sequence: 4,
		CreatedAt: today(21, 59), ReceivedAt: today(21, 59), Note: "arrived after a gap; state read from BarPay"})

	page := s.open("/admin/notifications")
	rows := logRows(page)
	if len(rows) != 3 {
		t.Fatalf("rows = %q", rows)
	}
	day := yesterday.Format("1/2")
	gaveUp := yesterday.Add(24 * time.Hour).Format("1/2")
	for i, want := range []string{
		"Sent by BarPay: 21:59, delivered on the first try\n" +
			"This app: Received at 21:59, number 4. One was missing, so the state was read again from BarPay\n",
		"Type: session.payment_failed (refused)\nCustomer: Guest (ses_ken)\n" +
			"Sent by BarPay: 21:40, delivered on the first try\nThis app: Received at 21:40, number 3. Applied in order\n",
		"Type: member.paypal_linked\nCustomer: Yui Tanaka (member)\n" +
			"Sent by BarPay: Sent 8 times from " + day + " 19:00; gave up on " + gaveUp + " at 19:00\n" +
			"This app: Not received. The member's PayPal is read from BarPay instead\n",
	} {
		if got := detail(page, rows[i][0]); !strings.Contains(got, want) {
			t.Errorf("dialog %d does not say\n%s\nit says\n%s", i, want, got)
		}
	}
	if rows[2][1] != day+" 19:00 | Linked PayPal | Yui Tanaka | Not delivered" {
		t.Errorf("the given-up row is %q", rows[2][1])
	}
}

// sign is the BarPay-Signature of body with the app's signing secret.
func sign(secret string, body []byte) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestAFailedPaymentThisAppReceivedSaysWhy(t *testing.T) {
	for reason, want := range map[string]string{"refused": "(refused)", "unconfirmed": "(outcome unknown)"} {
		t.Run(reason, func(t *testing.T) {
			s, f := newLogStaff(t)
			f.pages = [][]NotificationRecord{{{EventID: "evt_failed", Type: "session.payment_failed", SessionID: "ses_ken",
				Status: "delivered", Attempts: 1, CreatedAt: today(21, 40), LastAttemptAt: at(today(21, 40))}}}
			body, _ := json.Marshal(map[string]any{"id": "evt_failed", "type": "session.payment_failed", "created_at": today(21, 40), "sequence": 1,
				"data": map[string]any{"reason": reason, "session": Session{SessionID: "ses_ken", State: "payment_failed"}}})
			req, _ := http.NewRequest("POST", s.app.URL+"/barpay/notifications", bytes.NewReader(body))
			req.Header.Set("BarPay-Signature", sign(s.a.cfg.SigningSecret, body))
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusNoContent {
				t.Fatalf("the notification was not taken: %v %v", resp, err)
			}
			page := s.open("/admin/notifications")
			if got := detail(page, "log-1"); !strings.Contains(got, "Type: session.payment_failed "+want+"\n") {
				t.Errorf("the dialog does not say %s:\n%s", want, got)
			}
		})
	}
}

func TestAPaymentFailureNotReceivedGivesNoReason(t *testing.T) {
	s, f := newLogStaff(t)
	f.pages = [][]NotificationRecord{{{EventID: "evt_failed", Type: "session.payment_failed", SessionID: "ses_ken",
		Status: "retrying", Attempts: 0, CreatedAt: today(21, 40), NextAttemptAt: at(today(21, 40))}}}
	if got := detail(s.open("/admin/notifications"), "log-1"); !strings.Contains(got, "Type: session.payment_failed\n") {
		t.Errorf("the dialog gives a reason it does not have:\n%s", got)
	}
}

func TestSearchingAMembersNameFindsTheirOwnNotificationsAndTheirVisits(t *testing.T) {
	s, f := newLogStaff(t)
	s.member("mem_yui", "Yui Tanaka", "ses_yui")
	s.member("mem_sora", "Sora Kato", "ses_sora")
	rec := func(id, typ, sessionID, memberID string) NotificationRecord {
		return NotificationRecord{EventID: id, Type: typ, SessionID: sessionID, MemberID: memberID, Status: "delivered",
			Attempts: 1, CreatedAt: today(21, 0), LastAttemptAt: at(today(21, 0))}
	}
	f.pages = [][]NotificationRecord{{
		rec("evt_gate", "gate.unlock_failed", "ses_yui", ""),
		rec("evt_sora", "session.closed", "ses_sora", ""),
		rec("evt_guest", "session.closed", "ses_guest", ""),
		rec("evt_linked", "member.paypal_linked", "", "mem_yui"),
		rec("evt_sora_linked", "member.paypal_linked", "", "mem_sora"),
	}}

	page := s.open("/admin/notifications?q=yui")
	var got []string
	for _, r := range logRows(page) {
		got = append(got, r[1])
	}
	want := []string{"21:00 | Gate didn't open | Yui Tanaka | Delivered", "21:00 | Linked PayPal | Yui Tanaka | Delivered"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("searching yui finds\n%s", strings.Join(got, "\n"))
	}
	if !strings.Contains(page, `name="q" value="yui"`) {
		t.Error("the search box does not keep what was searched")
	}
}

func TestSearchFindsTheSessionIDTheEventIDAndTheWordsOfWhatHappened(t *testing.T) {
	s, f := newLogStaff(t)
	f.pages = [][]NotificationRecord{{
		{EventID: "evt_a1", Type: "gate.unlock_failed", SessionID: "ses_x1", Status: "delivered", Attempts: 1, CreatedAt: today(21, 0)},
		{EventID: "evt_b2", Type: "session.closed", SessionID: "ses_y2", Status: "delivered", Attempts: 1, CreatedAt: today(21, 0)},
	}}
	for q, want := range map[string]int{"SES_X1": 1, "evt_b2": 1, "gate didn": 1, "paid": 1, "guest": 2, "nothing": 0} {
		if got := len(logRows(s.open("/admin/notifications?q=" + strings.ReplaceAll(q, " ", "+")))); got != want {
			t.Errorf("searching %q finds %d rows, want %d", q, got, want)
		}
	}
}

func TestASearchReadsTenPagesAndNoMore(t *testing.T) {
	s, f := newLogStaff(t)
	for i := 0; i < 11; i++ {
		var page []NotificationRecord
		for j := 0; j < 20; j++ {
			page = append(page, NotificationRecord{EventID: "evt_" + strconv.Itoa(i*20+j), Type: "session.closed",
				SessionID: "ses_guest", Status: "delivered", Attempts: 1, CreatedAt: today(20, 0)})
		}
		f.pages = append(f.pages, page)
	}
	s.member("mem_hanako", "Hanako Mori", "ses_hanako")
	f.pages[10][0].SessionID = "ses_hanako"

	page := s.open("/admin/notifications?q=Hanako")
	if rows := logRows(page); len(rows) != 0 {
		t.Errorf("the search found %q past ten pages", rows)
	}
	if !strings.Contains(page, `Nothing matches "Hanako"`) {
		t.Errorf("the search does not say nothing matches:\n%s", page)
	}
	f.mu.Lock()
	reads := len(f.cursors)
	f.mu.Unlock()
	if reads != 10 {
		t.Errorf("the search read %d pages, want 10", reads)
	}
	if !strings.Contains(page, `href="/admin/notifications?cursor=p10&amp;q=Hanako">Older entries`) {
		t.Errorf("the search offers no way on to older entries:\n%s", page)
	}
}

func TestTheLogReadsOnePageWithoutASearch(t *testing.T) {
	s, f := newLogStaff(t)
	f.pages = [][]NotificationRecord{
		{{EventID: "evt_new", Type: "session.closed", SessionID: "ses_a", Status: "delivered", Attempts: 1, CreatedAt: today(21, 0)}},
		{{EventID: "evt_old", Type: "session.closed", SessionID: "ses_b", Status: "delivered", Attempts: 1, CreatedAt: today(20, 0)}},
	}
	page := s.open("/admin/notifications")
	if rows := logRows(page); len(rows) != 1 {
		t.Errorf("rows = %q", rows)
	}
	if !strings.Contains(page, `href="/admin/notifications?cursor=p1">Older entries`) {
		t.Errorf("no link to older entries:\n%s", page)
	}
	if older := logRows(s.open("/admin/notifications?cursor=p1")); len(older) != 1 || !strings.Contains(older[0][1], "20:00") {
		t.Errorf("the older entries are %q", older)
	}
}

func TestALogBarPayDoesNotGiveShowsWhatThisAppReceived(t *testing.T) {
	s, _ := newLogStaff(t)
	s.member("mem_yui", "Yui Tanaka", "ses_yui")
	s.received(ReceivedEvent{EventID: "evt_closed", Type: "session.closed", Subject: "session:ses_yui", Sequence: 3,
		CreatedAt: today(21, 59), ReceivedAt: today(21, 59), Note: "applied in order"})
	s.received(ReceivedEvent{EventID: "evt_linked", Type: "member.paypal_linked", Subject: "member:mem_yui", Sequence: 1,
		CreatedAt: today(22, 0), ReceivedAt: today(22, 0), Note: "applied in order"})
	// Made before the others and received after them, as a notification BarPay retried.
	s.received(ReceivedEvent{EventID: "evt_ken", Type: "session.payment_failed", Subject: "session:ses_ken", Sequence: 3,
		CreatedAt: today(21, 40), ReceivedAt: today(22, 1), Note: "applied in order", Reason: "refused"})
	s.barpay.mu.Lock()
	s.barpay.down = true
	s.barpay.mu.Unlock()

	page := s.open("/admin/notifications")
	var got []string
	for _, r := range logRows(page) {
		got = append(got, r[1])
	}
	want := []string{"22:00 | Linked PayPal | Yui Tanaka | Delivered", "21:59 | Paid | Yui Tanaka | Delivered",
		"21:40 | Payment failed | Guest | Delivered"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows, newest made first, are\n%s", strings.Join(got, "\n"))
	}
	body, _, _ := strings.Cut(page, gateDialogStart)
	notice := `<span>BarPay didn't answer, so this shows only what this app received. Wait a moment, then <a href="/admin/notifications">reload</a>.</span>`
	if strings.Count(body, notice) != 1 {
		t.Errorf("the log does not say once what it shows:\n%s", body)
	}
	if strings.Contains(body, "Older entries") {
		t.Error("the log offers older entries BarPay did not give")
	}
	if rows := logRows(s.open("/admin/notifications?q=member")); len(rows) != 0 {
		t.Errorf("a search while BarPay is down finds %q", rows)
	}
	if rows := logRows(s.open("/admin/notifications?q=paid")); len(rows) != 1 {
		t.Errorf("a search while BarPay is down finds %q", rows)
	}
}

func TestTheLogIsInTheWordsOfTheMockup(t *testing.T) {
	s, _ := newLogStaff(t)
	page := s.open("/admin/notifications")
	for _, want := range []string{
		"<h1>Log</h1>",
		"Notifications from BarPay. You don't need to check this day to day.",
		`<input type="search" name="q" value="" placeholder="Customer name, session ID or event ID" aria-label="Search">`,
		`<button class="button icon-only" type="submit" aria-label="Search">`,
		"<th>Time</th><th>What happened</th><th>Customer</th><th>Status</th>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the log has no %s", want)
		}
	}
}

func TestEachKindOfNotificationIsSaidInWords(t *testing.T) {
	for typ, want := range map[string]string{
		"gate.unlock_failed": "Gate didn't open", "session.closed": "Paid", "session.payment_failed": "Payment failed",
		"member.paypal_linked": "Linked PayPal", "session.member_attached": "Guest visit moved to a member",
		"something.new": "something.new",
	} {
		if got := eventWords(typ); got != want {
			t.Errorf("%s is said as %q, want %q", typ, got, want)
		}
	}
	for status, want := range map[string]string{"delivered": "Delivered", "retrying": "Retrying", "given_up": "Not delivered"} {
		if got, _ := statusWords(status); got != want {
			t.Errorf("%s is said as %q, want %q", status, got, want)
		}
	}
}

func TestAnOlderPageBarPayDoesNotGiveOffersTheNewestInsteadOfRows(t *testing.T) {
	s, _ := newLogStaff(t)
	s.received(ReceivedEvent{EventID: "evt_closed", Type: "session.closed", Subject: "session:ses_yui", Sequence: 3,
		CreatedAt: today(21, 59), ReceivedAt: today(21, 59), Note: "applied in order"})
	s.barpay.mu.Lock()
	s.barpay.down = true
	s.barpay.mu.Unlock()

	page := s.open("/admin/notifications?cursor=p3&q=yui")
	body, _, _ := strings.Cut(page, gateDialogStart)
	if rows := logRows(body); len(rows) != 0 {
		t.Errorf("an older page shows %q, which are the newest", rows)
	}
	if !strings.Contains(body, `<span>BarPay didn't answer. Wait a moment, then <a href="/admin/notifications?cursor=p3&amp;q=yui">reload</a>.</span>`) {
		t.Errorf("an older page does not say BarPay didn't answer:\n%s", body)
	}
	if !strings.Contains(body, `href="/admin/notifications?q=yui">Newest entries`) {
		t.Errorf("an older page offers no way back to the newest:\n%s", body)
	}
	for _, not := range []string{"this shows only what this app received", "Older entries", "<table"} {
		if strings.Contains(body, not) {
			t.Errorf("an older page has %q", not)
		}
	}
}

func TestADeliveryAfterSeveralTriesNeverSaysTheFirstTry(t *testing.T) {
	rec := NotificationRecord{Status: "delivered", Attempts: 3, CreatedAt: today(21, 0)}
	if got := sentText(rec, time.Now()); got != "21:00, delivered on try 3" {
		t.Errorf("without the last attempt's time it says %q", got)
	}
	rec.LastAttemptAt = at(today(21, 5))
	if got := sentText(rec, time.Now()); got != "21:00, delivered on try 3 at 21:05" {
		t.Errorf("with the last attempt's time it says %q", got)
	}
}
