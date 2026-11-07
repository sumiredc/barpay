package main

import (
	"slices"
	"testing"
	"time"
)

// A guest's session that a member was attached to (UC10) is the member's visit from then on: the
// notification puts it in the member's visits, as their open one, and later notifications of
// it keep it there once.
func TestTheMemberAttachedNotificationMakesTheVisitTheMembers(t *testing.T) {
	a, url := notifiedApp(t)
	a.store.update(func(d *Data) error {
		d.Members["m_taro"] = &MemberAccount{ID: "m_taro", Name: "Taro"}
		return nil
	})
	at := time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)
	guest := Session{SessionID: "ses_1", State: "active", PaymentMethod: "register"}
	notify(t, url, "secret", "evt_1", "session.started", 1, at, guest, "")
	taro := "m_taro"
	attached := guest
	attached.MemberID = &taro
	notify(t, url, "secret", "evt_2", "session.member_attached", 2, at.Add(time.Minute), attached, "")
	notify(t, url, "secret", "evt_2", "session.member_attached", 2, at.Add(time.Minute), attached, "")
	linked := attached
	linked.PaymentMethod = "paypal"
	notify(t, url, "secret", "evt_3", "session.paypal_linked", 3, at.Add(2*time.Minute), linked, "")
	a.store.view(func(d *Data) {
		m := d.Members["m_taro"]
		if !slices.Equal(m.SessionIDs, []string{"ses_1"}) || m.OpenSessionID != "ses_1" {
			t.Errorf("the member's visits are %v, open %q; want ses_1 once, open", m.SessionIDs, m.OpenSessionID)
		}
		if v := d.Visits["ses_1"]; v.MemberID != "m_taro" {
			t.Errorf("the visit is for %q, want m_taro", v.MemberID)
		}
	})
}
