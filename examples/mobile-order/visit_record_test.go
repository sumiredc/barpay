package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// notify sends BarPay's signed notification of a session to the app, as event id with seq, and
// fails the test unless the app acknowledges it.
func notify(t *testing.T, appURL, secret, id, eventType string, seq int, at time.Time, s Session, reason string) {
	t.Helper()
	data := map[string]any{"session": s}
	if reason != "" {
		data["reason"] = reason
	}
	body, _ := json.Marshal(map[string]any{"id": id, "type": eventType, "created_at": at, "sequence": seq, "data": data})
	ts := fmt.Sprint(time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	req, _ := http.NewRequest("POST", appURL+"/barpay/notifications", bytes.NewReader(body))
	req.Header.Set("BarPay-Signature", "t="+ts+",v1="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("notification %s answered %d", id, resp.StatusCode)
	}
}

// notifiedApp is the app that BarPay's notifications signed with "secret" are sent to. Its
// BarPay answers nothing: a notification is stored without a call.
func notifiedApp(t *testing.T) (*App, string) {
	t.Helper()
	barpay := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(barpay.Close)
	st, err := openStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{StoreName: "The Copper Fox", BarPayURL: barpay.URL, SigningSecret: "secret"}
	a := &App{cfg: cfg, barpay: newBarPay(cfg), store: st, pages: loadPages()}
	app := httptest.NewServer(a.routes())
	t.Cleanup(app.Close)
	return a, app.URL
}

func eventsOf(a *App, sessionID string) []VisitEvent {
	var out []VisitEvent
	a.store.view(func(d *Data) {
		if v := d.Visits[sessionID]; v != nil {
			out = append(out, v.Events...)
		}
	})
	return out
}

func TestAVisitKeepsTheKindTimeAndReasonOfEachNotificationOfItsSession(t *testing.T) {
	a, url := notifiedApp(t)
	at := time.Date(2026, 11, 5, 21, 47, 0, 0, time.UTC)
	ses := Session{SessionID: "ses_1", State: "payment_failed", PaymentMethod: "paypal"}
	notify(t, url, "secret", "evt_1", "session.payment_failed", 1, at, ses, "unconfirmed")
	notify(t, url, "secret", "evt_1", "session.payment_failed", 1, at, ses, "unconfirmed")
	got := eventsOf(a, "ses_1")
	want := []VisitEvent{{Type: "session.payment_failed", Sequence: 1, At: at, Reason: "unconfirmed"}}
	if len(got) != 1 || !got[0].At.Equal(want[0].At) || got[0].Type != want[0].Type || got[0].Reason != want[0].Reason {
		t.Errorf("events = %+v, want %+v (the same event sent twice is kept once)", got, want)
	}
}

func TestANotificationArrivingLateIsKeptInItsPlace(t *testing.T) {
	a, url := notifiedApp(t)
	at := time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)
	ses := Session{SessionID: "ses_1", State: "closed", PaymentMethod: "paypal"}
	notify(t, url, "secret", "evt_2", "gate.unlock_failed", 2, at.Add(time.Minute), ses, "")
	notify(t, url, "secret", "evt_1", "session.closed", 1, at, ses, "")
	got := eventsOf(a, "ses_1")
	if len(got) != 2 || got[0].Type != "session.closed" || got[1].Type != "gate.unlock_failed" {
		t.Errorf("events = %+v, want session.closed then gate.unlock_failed", got)
	}
}
