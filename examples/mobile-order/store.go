package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Data is everything the sample app keeps, written to one JSON file after each change so it
// survives a restart. It is a sample's store, not a database: one process, one lock.
type Data struct {
	Members  map[string]*MemberAccount `json:"members"`  // by member ID
	Browsers map[string]*Browser       `json:"browsers"` // by the browser session cookie
	// Visits holds what the app knows of each BarPay session: who it is for, the order lines
	// (BarPay keeps amounts, not the menu) and the latest session it read or was notified of.
	Visits map[string]*Visit `json:"visits"`
	// PendingKeys holds the Idempotency-Key of each request whose answer has not arrived yet, so
	// the next try sends the same key.
	PendingKeys map[string]string `json:"pending_keys"`
	// SeenEvents holds the notification event IDs already handled, with when they arrived.
	SeenEvents map[string]time.Time `json:"seen_events"`
	// LastSequence is the highest sequence handled per subject ("session:<id>", "member:<id>").
	LastSequence map[string]int `json:"last_sequence"`
	// Received is the log of notifications handled, newest last.
	Received []ReceivedEvent `json:"received"`
	// MemberNotices are messages for a member's settings screen, from member.* notifications.
	MemberNotices map[string]string `json:"member_notices"`
	// ToldByAnswer holds, per member, the member.* event type the member was already told of in
	// their own request's answer, so the notification of that event adds no second notice.
	ToldByAnswer map[string]string `json:"told_by_answer"`
}

type MemberAccount struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
	// OpenSessionID is the member's BarPay session that has not ended, if any.
	OpenSessionID string   `json:"open_session_id,omitempty"`
	SessionIDs    []string `json:"session_ids"`
}

// Browser is one browser session: who is logged in there, as a customer and as staff.
type Browser struct {
	CSRF      string    `json:"csrf"`
	ExpiresAt time.Time `json:"expires_at"`
	MemberID  string    `json:"member_id,omitempty"`
	Guest     bool      `json:"guest,omitempty"`
	SessionID string    `json:"session_id,omitempty"` // the customer's current BarPay session
	StaffID   string    `json:"staff_id,omitempty"`
	// LinkState is the state sent with createPaypalLink from this browser session; only a return
	// carrying it is confirmed, and only for LinkMemberID.
	LinkState     string    `json:"link_state,omitempty"`
	LinkMemberID  string    `json:"link_member_id,omitempty"`
	LinkExpiresAt time.Time `json:"link_expires_at,omitempty"`
	// PendingOrder is an order whose answer did not arrive; sending it again keeps its order_id.
	PendingOrder *PendingOrder `json:"pending_order,omitempty"`
	Flash        string        `json:"flash,omitempty"`
}

type PendingOrder struct {
	OrderID string      `json:"order_id"`
	Lines   []OrderLine `json:"lines"`
}

type Visit struct {
	SessionID string    `json:"session_id"`
	MemberID  string    `json:"member_id,omitempty"`
	Orders    []Order   `json:"orders"`
	Session   *Session  `json:"session,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ReceivedEvent struct {
	EventID    string    `json:"event_id"`
	Type       string    `json:"type"`
	Subject    string    `json:"subject"`
	Sequence   int       `json:"sequence"`
	CreatedAt  time.Time `json:"created_at"`
	ReceivedAt time.Time `json:"received_at"`
	// Note says how it was applied: in order, older than one already applied, or after a gap
	// that a lookup filled.
	Note string `json:"note"`
}

const (
	receivedKept = 500
	// seenKept is how long an event ID is remembered. BarPay retries a notification for 24 hours.
	seenKept = 7 * 24 * time.Hour
)

type Store struct {
	mu   sync.Mutex
	path string
	d    *Data
}

func openStore(path string) (*Store, error) {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw = []byte("{}")
	case err != nil:
		return nil, err
	}
	d, err := decode(raw)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, d: d}, nil
}

func decode(raw []byte) (*Data, error) {
	d := &Data{}
	if err := json.Unmarshal(raw, d); err != nil {
		return nil, err
	}
	if d.Members == nil {
		d.Members = map[string]*MemberAccount{}
	}
	if d.Browsers == nil {
		d.Browsers = map[string]*Browser{}
	}
	if d.Visits == nil {
		d.Visits = map[string]*Visit{}
	}
	if d.PendingKeys == nil {
		d.PendingKeys = map[string]string{}
	}
	if d.SeenEvents == nil {
		d.SeenEvents = map[string]time.Time{}
	}
	if d.LastSequence == nil {
		d.LastSequence = map[string]int{}
	}
	if d.MemberNotices == nil {
		d.MemberNotices = map[string]string{}
	}
	if d.ToldByAnswer == nil {
		d.ToldByAnswer = map[string]string{}
	}
	return d, nil
}

// update runs fn under the lock on a copy of the data, and keeps the copy only once it is saved.
// A change that fails, or that cannot be written, therefore leaves nothing behind: a notification
// that was answered with an error is not taken for a duplicate when BarPay sends it again.
func (s *Store) update(fn func(d *Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(s.d)
	if err != nil {
		return err
	}
	next, err := decode(raw)
	if err != nil {
		return err
	}
	if err := fn(next); err != nil {
		return err
	}
	if err := save(s.path, next); err != nil {
		return err
	}
	s.d = next
	return nil
}

// view runs fn under the lock without saving.
func (s *Store) view(fn func(d *Data)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.d)
}

func save(path string, d *Data) error {
	now := time.Now()
	for id, at := range d.SeenEvents {
		if now.Sub(at) > seenKept {
			delete(d.SeenEvents, id)
		}
	}
	for id, b := range d.Browsers {
		if now.After(b.ExpiresAt) {
			delete(d.Browsers, id)
		}
	}
	if n := len(d.Received); n > receivedKept {
		d.Received = append([]ReceivedEvent(nil), d.Received[n-receivedKept:]...)
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mobile-order-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// visit returns the visit for a session ID, creating it.
func (d *Data) visit(sessionID string) *Visit {
	v := d.Visits[sessionID]
	if v == nil {
		v = &Visit{SessionID: sessionID}
		d.Visits[sessionID] = v
	}
	return v
}

// remember stores a session read from BarPay or carried by a notification as the latest known
// one. A session that has ended never changes again, so a snapshot of it from before the end,
// such as a notification that arrives late, does not replace it.
func (d *Data) remember(s *Session) {
	v := d.visit(s.SessionID)
	if v.Session != nil && v.Session.ended() && !s.ended() {
		return
	}
	cp := *s
	v.Session = &cp
	v.UpdatedAt = time.Now()
	if s.MemberID != nil {
		v.MemberID = *s.MemberID
		if m := d.Members[*s.MemberID]; m != nil && s.ended() && m.OpenSessionID == s.SessionID {
			m.OpenSessionID = ""
		}
	}
}
