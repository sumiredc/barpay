package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// failedUnlock is an unlock_failed alert for sessionID ("" for an unlock by hand for no
// particular customer), with an ID of the spec's pattern.
func failedUnlock(n int, sessionID string) Alert {
	return Alert{ID: fmt.Sprintf("alt_unlock%08d", n), Kind: "unlock_failed", RaisedAt: time.Now(), SessionID: sessionID, Message: "The gate did not open."}
}

// otherAlerts is a page of alerts that the alerts page leaves out.
func otherAlerts(n int) []Alert {
	var page []Alert
	for i := range n {
		page = append(page, Alert{ID: fmt.Sprintf("alt_payment%08d", i), Kind: "payment_failed", RaisedAt: time.Now(), SessionID: "ses_01hx39p4s6d8f0g2h4j6", Message: "PayPal refused."})
	}
	return page
}

// alertRows returns the rows of the alerts table on a page.
func alertRows(page string) []string {
	_, table, _ := strings.Cut(page, "<tbody>")
	table, _, _ = strings.Cut(table, "</tbody>")
	return strings.Split(table, "</tr>")[:strings.Count(table, "</tr>")]
}

func TestTheAlertsPageListsOnlyTheGatesFailedUnlocks(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.barpay.alerts = append(s.barpay.alerts, otherAlerts(1)...)
	got := alertRows(s.open("/admin/alerts"))
	if len(got) != 1 || !strings.Contains(got[0], "Gate didn't unlock") || !strings.Contains(got[0], "Yui Tanaka") {
		t.Errorf("rows %q, want the failed unlock of Yui Tanaka only", got)
	}
}

func TestAFailedUnlockOnTheSecondPageIsRead(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.barpay.alertPages = [][]Alert{otherAlerts(20), {failedUnlock(1, "")}}
	got := alertRows(s.open("/admin/alerts"))
	if len(got) != 1 || !strings.Contains(got[0], "Unlocked by hand") {
		t.Errorf("rows %q, want the failed unlock of the second page", got)
	}
}

func TestTheAlertsPageStopsAfterTenPagesAndGoesOnWithOlderAlerts(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	for range 10 {
		s.barpay.alertPages = append(s.barpay.alertPages, otherAlerts(20))
	}
	s.barpay.alertPages = append(s.barpay.alertPages, []Alert{failedUnlock(1, "")})
	before := s.alertReads()
	page := s.open("/admin/alerts")
	if got := s.alertReads() - before; got != 10 {
		t.Errorf("read %d pages of alerts, want 10", got)
	}
	if got := alertRows(page); len(got) != 1 || !strings.Contains(got[0], "No open alerts") {
		t.Errorf("rows %q, want none", got)
	}
	if !strings.Contains(page, `<a class="textbutton" href="/admin/alerts?cursor=page-10">Older alerts`) {
		t.Errorf("no Older alerts from the 11th page:\n%s", page)
	}
	if got := alertRows(s.open("/admin/alerts?cursor=page-10")); len(got) != 1 || !strings.Contains(got[0], "Unlocked by hand") {
		t.Errorf("Older alerts shows %q, want the failed unlock of the 11th page", got)
	}
}

func TestTheAlertsPageEndsWithoutOlderAlertsAtTheEndOfTheList(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.barpay.alertPages = [][]Alert{otherAlerts(20), {failedUnlock(1, "")}}
	if page := s.open("/admin/alerts"); strings.Contains(page, "Older alerts") {
		t.Errorf("Older alerts after the end of the list:\n%s", page)
	}
}

func TestTheDetailOfAFailedUnlockTellsHowTheCustomerPaid(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	receipt := s.barpay.sessions["ses_01hx36w1e3r5t7y9u1i3"].Receipt
	receipt.Total = Money{Value: 3300, Currency: "USD"}
	y, m, d := time.Now().Date()
	receipt.EndedAt = time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	page := s.open("/admin/alerts")
	for _, want := range []string{
		"<h2>Yui Tanaka: Paid, but the exit gate didn&#39;t open</h2>",
		"The customer paid $33.00 with PayPal at 00:00. If they're waiting at the gate, open it with Unlock gate in the menu.",
		"<dt>Visit</dt><dd>ses_01hx36w1e3r5t7y9u1i3</dd>",
		`data-close>Close</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the detail lacks %s:\n%s", want, page)
		}
	}
}

func TestTheDetailOfAFailedUnlockTellsAPaymentAtTheRegister(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	ended := time.Date(2026, 11, 5, 20, 15, 0, 0, time.Local)
	recorded := Money{Value: 1800, Currency: "USD"}
	s.barpay.sessions["ses_01hx2x4c6v8b0n2m4k1q"] = &Session{SessionID: "ses_01hx2x4c6v8b0n2m4k1q", State: "closed",
		Receipt: &Receipt{Settlement: "register", Total: Money{Value: 1750, Currency: "USD"}, AmountRecorded: &recorded, EndedAt: ended}}
	s.barpay.alerts = []Alert{failedUnlock(1, "ses_01hx2x4c6v8b0n2m4k1q")}
	page := s.open("/admin/alerts")
	if !strings.Contains(page, "The customer paid $18.00 at the register at Nov 5 20:15.") {
		t.Errorf("the detail does not tell the payment at the register:\n%s", page)
	}
	if got := alertRows(page); len(got) != 1 || !strings.Contains(got[0], "Guest") {
		t.Errorf("rows %q, want a Guest", got)
	}
}

func TestAFailedUnlockWithoutASessionHasNoCustomerAndNoReceipt(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.barpay.alerts = []Alert{failedUnlock(1, "")}
	page := s.open("/admin/alerts")
	if got := alertRows(page); len(got) != 1 || !strings.Contains(got[0], `<td class="nowrap">—</td>`) {
		t.Errorf("rows %q, want one whose customer is —", got)
	}
	_, detail, _ := strings.Cut(page, `<dialog class="sheet" id="alert-alt_unlock00000001">`)
	detail, _, _ = strings.Cut(detail, "</dialog>")
	if strings.Contains(detail, "The customer paid") || strings.Contains(detail, "<dt>Visit</dt>") {
		t.Errorf("the detail has a receipt or a visit:\n%s", detail)
	}
}

// A slow BarPay cuts the screen short rather than losing it: the gate dialog offers the customers
// of the pages read before the time ran out.
func TestTheGateDialogKeepsTheCustomersReadBeforeBarPayStoppedAnswering(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.barpay.mu.Lock()
	s.barpay.alertPages = [][]Alert{s.barpay.alerts, otherAlerts(20)}
	s.barpay.laterPagesHang = true
	s.barpay.mu.Unlock()
	took, dialog := s.openGateDialog("/admin")
	if took > gateLookupTimeout+2*time.Second {
		t.Errorf("the page took %s", took)
	}
	if !strings.Contains(dialog, "Yui Tanaka") || strings.Contains(dialog, "BarPay didn't answer") {
		t.Errorf("the dialog does not offer Yui Tanaka of the first page:\n%s", dialog)
	}
}

// The gate dialog's read of a session can be cut short by its time limit; the detail of the alert
// reads it again rather than going without the payment.
func TestTheDetailReadsTheSessionAgainWhenTheGateDialogsReadFailed(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.barpay.mu.Lock()
	s.barpay.sessionReadsFailing = 1
	s.barpay.mu.Unlock()
	if page := s.open("/admin/alerts"); !strings.Contains(page, "The customer paid") {
		t.Errorf("the detail does not tell the payment:\n%s", page)
	}
}

func TestTheAlertsPageWhoseReadFailedDoesNotReadAgainForTheGateDialog(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.barpay.mu.Lock()
	s.barpay.down = true
	s.barpay.mu.Unlock()
	before := s.alertReads()
	_, dialog := s.openGateDialog("/admin/alerts")
	if got := s.alertReads() - before; got != 1 {
		t.Errorf("the alerts were read %d times, want once", got)
	}
	if !strings.Contains(dialog, "BarPay didn't answer. Wait a moment, then") {
		t.Errorf("the dialog does not say BarPay didn't answer:\n%s", dialog)
	}
}

// markDone presses Done on the row of alertID on the page at from, and returns the page it goes
// back to.
func (s *staff) markDone(from, alertID string) string {
	s.t.Helper()
	return s.post(from, "/admin/alerts/"+alertID+"/done", url.Values{"back": {from}})
}

const yuisAlert = "alt_01hx36w1e3r5t7y9u1i3"

var (
	alertsBadgePattern  = regexp.MustCompile(`<span>Alerts</span><span class="badge" aria-label="[^"]*">([^<]*)</span>`)
	openTabBadgePattern = regexp.MustCompile(`>Open<span class="badge" aria-label="[^"]*">([^<]*)</span>`)
)

// badge returns the count of the first badge that pattern finds on a page, or "" when there is
// none.
func badge(pattern *regexp.Regexp, page string) string {
	m := pattern.FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestTwentyOpenAlertsWithMorePagesAreCountedAsTwentyPlus(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	var first []Alert
	for i := range 20 {
		first = append(first, failedUnlock(i, ""))
	}
	s.barpay.alertPages = [][]Alert{first, {failedUnlock(20, "")}}
	for _, p := range adminPages {
		if got := badge(alertsBadgePattern, s.open(p.path)); got != "20+" {
			t.Errorf("%s: the badge beside Alerts is %q, want 20+", p.path, got)
		}
	}
	if got := badge(openTabBadgePattern, s.open("/admin/alerts?tab=all")); got != "20+" {
		t.Errorf("the Open tab's badge is %q, want 20+", got)
	}
}

func TestAnAlertMarkedDoneLeavesOpenAndTheGateDialogAndShowsWhoInAll(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.barpay.alerts = append(s.barpay.alerts, failedUnlock(1, ""))
	before := s.open("/admin/alerts")
	if badge(alertsBadgePattern, before) != "2" || badge(openTabBadgePattern, before) != "2" || len(alertRows(before)) != 2 {
		t.Fatalf("before: badge %q, tab %q, %d rows, want 2 open",
			badge(alertsBadgePattern, before), badge(openTabBadgePattern, before), len(alertRows(before)))
	}

	page := s.markDone("/admin/alerts", yuisAlert)
	at := time.Now().Format("15:04")
	if got := alertRows(page); len(got) != 1 || strings.Contains(got[0], "Yui Tanaka") {
		t.Errorf("Open still lists Yui Tanaka: %q", got)
	}
	if badge(alertsBadgePattern, page) != "1" || badge(openTabBadgePattern, page) != "1" {
		t.Errorf("badge %q, tab %q, want 1", badge(alertsBadgePattern, page), badge(openTabBadgePattern, page))
	}
	if _, dialog, _ := strings.Cut(page, gateDialogStart); strings.Contains(dialog, "Yui Tanaka") {
		t.Errorf("the gate dialog still offers Yui Tanaka:\n%s", dialog)
	}
	if _, dialog, _ := strings.Cut(s.open("/admin"), gateDialogStart); strings.Contains(dialog, "Yui Tanaka") {
		t.Errorf("the gate dialog of the visits page still offers Yui Tanaka:\n%s", dialog)
	}

	all := s.open("/admin/alerts?tab=all")
	var yui string
	for _, r := range alertRows(all) {
		if strings.Contains(r, "Yui Tanaka") {
			yui = r
		}
	}
	if !strings.Contains(yui, `<tr class="done"`) || !strings.Contains(yui, "Done<br>staff-01, "+at) {
		t.Errorf("All lists Yui Tanaka as %q, want done by staff-01 at %s", yui, at)
	}
	if !strings.Contains(all, "· Done (staff-01, "+at+")</p>") {
		t.Errorf("the detail does not say who marked it done:\n%s", all)
	}
}

func TestMarkingAnAlertDoneAgainKeepsTheFirstMark(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	first := AlertDone{StaffID: "staff-02", At: time.Date(2026, 11, 5, 20, 16, 0, 0, time.Local)}
	s.a.store.update(func(d *Data) error { d.AlertsDone[yuisAlert] = first; return nil })
	s.markDone("/admin/alerts?tab=all", yuisAlert)
	var got AlertDone
	s.a.store.view(func(d *Data) { got = d.AlertsDone[yuisAlert] })
	if got.StaffID != first.StaffID || !got.At.Equal(first.At) {
		t.Errorf("the mark is %+v, want the first %+v", got, first)
	}
}

func TestAnAlertMarkedDoneIsStillDoneAfterARestart(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.markDone("/admin/alerts", yuisAlert)
	restarted, err := openStore(s.a.store.path)
	if err != nil {
		t.Fatal(err)
	}
	s.a.store = restarted
	if got := alertRows(s.open("/admin/alerts")); len(got) != 1 || !strings.Contains(got[0], "No open alerts") {
		t.Errorf("after a restart Open lists %q, want none", got)
	}
}

func TestAnIDThatIsNotAnAlertsIsNotMarkedDone(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	if status, _ := s.send("/admin/alerts", "/admin/alerts/x/done", url.Values{"back": {"/admin/alerts"}}); status != 404 {
		t.Errorf("status %d, want 404", status)
	}
	s.a.store.view(func(d *Data) {
		if len(d.AlertsDone) != 0 {
			t.Errorf("marked %v", d.AlertsDone)
		}
	})
}

func TestTheAlertsPageHasTheOpenAndAllTabsAndTheDoneButtons(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	page := s.open("/admin/alerts")
	for _, want := range []string{
		`<a href="/admin/alerts" aria-current="page">Open<span class="badge"`,
		`<a href="/admin/alerts?tab=all">All</a>`,
		"<th>Raised</th><th>Kind</th><th>What happened</th><th>Customer</th>",
		`<button class="textbutton muted" type="submit">Done</button>`,
		`<button class="button secondary" type="submit">Mark as done</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the alerts page lacks %s", want)
		}
	}
	s.markDone("/admin/alerts", yuisAlert)
	if page := s.open("/admin/alerts"); !strings.Contains(page, "<span>No open alerts</span>") {
		t.Errorf("no word that nothing is open:\n%s", page)
	}
}

func TestAScreenCutShortByASlowBarPayIsCountedWithAPlus(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.barpay.mu.Lock()
	s.barpay.alertPages = [][]Alert{{failedUnlock(1, "")}, otherAlerts(20)}
	s.barpay.laterPagesHang = true
	s.barpay.mu.Unlock()
	if got := badge(alertsBadgePattern, s.open("/admin")); got != "1+" {
		t.Errorf("the badge beside Alerts is %q, want 1+", got)
	}
}

func TestNoCountIsShownWhenTheAlertsCannotBeRead(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.barpay.mu.Lock()
	s.barpay.down = true
	s.barpay.mu.Unlock()
	for _, p := range []string{"/admin", "/admin/alerts"} {
		if page := s.open(p); strings.Contains(page, `class="badge"`) {
			t.Errorf("%s shows a count:\n%s", p, page)
		}
	}
}

// An ID BarPay sent in a form other than an alert's cannot be marked done, so no Done is offered
// and it is not put into a form's path.
func TestAnAlertWhoseIDIsNotOfTheAlertsFormHasNoDone(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.barpay.alerts = []Alert{{ID: "alt_x/../../gate", Kind: "unlock_failed", RaisedAt: time.Now(), Message: "The gate did not open."}}
	page := s.open("/admin/alerts")
	if got := alertRows(page); len(got) != 1 || strings.Contains(got[0], ">Done</button>") {
		t.Errorf("rows %q, want one without Done", got)
	}
	if strings.Contains(page, "Mark as done") || strings.Contains(page, "/done") {
		t.Errorf("the page offers to mark it done:\n%s", page)
	}
}
