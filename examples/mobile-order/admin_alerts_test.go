package main

import (
	"fmt"
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
	if got := alertRows(page); len(got) != 1 || !strings.Contains(got[0], "No alerts") {
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
