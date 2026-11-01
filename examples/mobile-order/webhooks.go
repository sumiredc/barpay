package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// signatureTolerance is how far the signature's t may be from now (the spec's Signature).
const signatureTolerance = 5 * time.Minute

// verifySignature checks BarPay-Signature, "t=<unix seconds>,v1=<hex>", against the raw body.
func verifySignature(header string, body []byte, secret string, now time.Time) bool {
	var ts, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > signatureTolerance || d < -signatureTolerance {
		return false
	}
	got, err := hex.DecodeString(v1)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

type event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Sequence  int       `json:"sequence"`
	Data      struct {
		Session *Session `json:"session"`
		Member  *Member  `json:"member"`
		Reason  string   `json:"reason"`
	} `json:"data"`
}

// handleNotification receives BarPay's signed notifications. It acknowledges with 204 only after
// the event is stored, so one it could not store is sent again. An event ID already handled is
// acknowledged and changes nothing, since BarPay may send the same event more than once.
func (a *App) handleNotification(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if !verifySignature(r.Header.Get("BarPay-Signature"), body, a.cfg.SigningSecret, time.Now()) {
		log.Printf("notification refused: bad signature")
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var e event
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" || e.Sequence < 1 {
		http.Error(w, "unreadable event", http.StatusBadRequest)
		return
	}
	subject := ""
	switch {
	case e.Data.Session != nil:
		subject = "session:" + e.Data.Session.SessionID
	case e.Data.Member != nil:
		subject = "member:" + e.Data.Member.MemberID
	default:
		http.Error(w, "event without a subject", http.StatusBadRequest)
		return
	}

	var duplicate, gap bool
	err = a.store.update(func(d *Data) error {
		if _, seen := d.SeenEvents[e.ID]; seen {
			duplicate = true
			return nil
		}
		last := d.LastSequence[subject]
		note := "applied in order"
		switch {
		case e.Sequence <= last:
			// An older event arriving after a newer one: the newer state is already kept.
			note = "older than one already applied; state kept"
		case e.Sequence > last+1:
			gap = true
			note = "arrived after a gap; state read from BarPay"
			fallthrough
		default:
			d.LastSequence[subject] = e.Sequence
			a.apply(d, &e)
		}
		d.SeenEvents[e.ID] = time.Now()
		d.Received = append(d.Received, ReceivedEvent{
			EventID: e.ID, Type: e.Type, Subject: subject, Sequence: e.Sequence,
			CreatedAt: e.CreatedAt, ReceivedAt: time.Now(), Note: note,
		})
		return nil
	})
	if err != nil {
		log.Printf("notification %s not stored: %v", e.ID, err)
		http.Error(w, "not stored", http.StatusInternalServerError)
		return
	}
	if duplicate {
		log.Printf("notification %s %s already handled", e.ID, e.Type)
	} else if gap {
		// Events in between have not arrived (yet): read the subject's current state instead
		// of waiting for them.
		go a.refreshSubject(subject)
	}
	w.WriteHeader(http.StatusNoContent)
}

const (
	paypalLinkedNotice       = "PayPal is linked. Your next visits are paid by walking out."
	savedPaypalDeletedNotice = "Your saved PayPal was deleted."
)

// apply records what an event says, in the order of its subject's sequence.
func (a *App) apply(d *Data, e *event) {
	if s := e.Data.Session; s != nil {
		d.remember(s)
		return
	}
	m := e.Data.Member
	told := d.ToldByAnswer[m.MemberID]
	// Linking and deleting alternate, so this event also ends a mark left for the other kind
	// whose notification was never applied (it arrived after a newer one).
	delete(d.ToldByAnswer, m.MemberID)
	switch e.Type {
	case "member.paypal_linked":
		if told != e.Type {
			d.MemberNotices[m.MemberID] = paypalLinkedNotice
		}
	case "member.saved_paypal_deleted":
		switch {
		case e.Data.Reason == "no_longer_valid":
			d.MemberNotices[m.MemberID] = "PayPal no longer has your saved PayPal, so we removed it. You can link PayPal again or pay at the register."
		case told != e.Type:
			d.MemberNotices[m.MemberID] = savedPaypalDeletedNotice
		}
	}
}

// toldByAnswer records that the member read in their own request's answer what the eventType
// notification says, whether that notification arrived already (its notice is dropped) or comes
// later (it adds none).
func (d *Data) toldByAnswer(memberID, eventType, notice string) {
	if d.MemberNotices[memberID] == notice {
		delete(d.MemberNotices, memberID)
	} else {
		d.ToldByAnswer[memberID] = eventType
	}
}

func (a *App) refreshSubject(subject string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	kind, id, _ := strings.Cut(subject, ":")
	if kind != "session" {
		// A member's saved PayPal is read from BarPay each time the settings screen is drawn.
		return
	}
	s, err := a.barpay.getSession(ctx, id)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("reading session after a notification gap: %v", err)
		}
		return
	}
	a.store.update(func(d *Data) error { d.remember(s); return nil })
}
